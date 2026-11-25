package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrNotConfigured is returned when the GitHub App configuration has not been
// saved yet.
var ErrNotConfigured = errors.New("github app not configured")

// ErrTeamNameTaken is returned when a rename would collide with another team's
// name or slug.
var ErrTeamNameTaken = errors.New("team name already taken")

// ErrTeamInUse is returned when a team still has repositories assigned.
var ErrTeamInUse = errors.New("team has repositories assigned")

// TeamWithMembers is a team plus the users assigned to it and their roles.
type TeamWithMembers struct {
	Team
	Members []struct {
		User User
		Role string
	}
	RepositoryCount int
}

// UpsertUserFromGitHub inserts a GitHub user or refreshes its profile. The
// first user in the table becomes the administrator. An exclusive table lock
// keeps concurrent first logins from both claiming that role.
func (s *Store) UpsertUserFromGitHub(ctx context.Context, ghID int64, login, avatar string) (User, error) {
	var user User
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("LOCK TABLE users IN ACCESS EXCLUSIVE MODE").Error; err != nil {
			return fmt.Errorf("lock users: %w", err)
		}

		var count int64
		if err := tx.Model(&User{}).Count(&count).Error; err != nil {
			return fmt.Errorf("count users: %w", err)
		}

		var existing User
		err := tx.Where("github_id = ?", ghID).Take(&existing).Error
		switch {
		case err == nil:
			existing.Login = login
			existing.AvatarURL = avatar
			if err := tx.Save(&existing).Error; err != nil {
				return fmt.Errorf("update user: %w", err)
			}
			user = existing
			return nil
		case errors.Is(err, gorm.ErrRecordNotFound):
			user = User{GitHubID: ghID, Login: login, AvatarURL: avatar, IsAdmin: count == 0}
			if err := tx.Create(&user).Error; err != nil {
				return fmt.Errorf("insert user: %w", err)
			}
			return nil
		default:
			return fmt.Errorf("find user: %w", err)
		}
	})
	if err != nil {
		return User{}, fmt.Errorf("upsert user %d: %w", ghID, err)
	}
	return user, nil
}

// CreateSession starts a session for userID and returns the clear token. Only
// its sha256 hex digest is persisted.
func (s *Store) CreateSession(ctx context.Context, userID uint, ttl time.Duration) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	session := Session{
		TokenHash: sessionTokenHash(token),
		UserID:    userID,
		ExpiresAt: time.Now().UTC().Add(ttl),
	}
	if err := s.DB.WithContext(ctx).Create(&session).Error; err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return token, nil
}

// UserBySessionToken resolves a live session token to its user. Expired and
// unknown tokens are reported as ErrNotFound.
func (s *Store) UserBySessionToken(ctx context.Context, token string) (User, error) {
	var session Session
	err := s.DB.WithContext(ctx).
		Where("token_hash = ? AND expires_at > ?", sessionTokenHash(token), time.Now().UTC()).
		Take(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return User{}, fmt.Errorf("session token: %w", ErrNotFound)
	}
	if err != nil {
		return User{}, fmt.Errorf("find session token: %w", err)
	}

	var user User
	if err := s.DB.WithContext(ctx).Take(&user, session.UserID).Error; err != nil {
		return User{}, fmt.Errorf("find session user %d: %w", session.UserID, err)
	}
	return user, nil
}

// DeleteSession removes the session identified by its clear token.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	res := s.DB.WithContext(ctx).Where("token_hash = ?", sessionTokenHash(token)).Delete(&Session{})
	if res.Error != nil {
		return fmt.Errorf("delete session: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("delete session: %w", ErrNotFound)
	}
	return nil
}

func sessionTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ListUsers returns every user, oldest first.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	var users []User
	if err := s.DB.WithContext(ctx).Order("id").Find(&users).Error; err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return users, nil
}

// UserByGitHubID resolves the local user behind a GitHub account. Users who
// never signed in with OAuth are reported as ErrNotFound.
func (s *Store) UserByGitHubID(ctx context.Context, githubID int64) (User, error) {
	var user User
	err := quietDB(s.DB.WithContext(ctx)).Where("github_id = ?", githubID).Take(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return User{}, fmt.Errorf("user with GitHub id %d: %w", githubID, ErrNotFound)
	}
	if err != nil {
		return User{}, fmt.Errorf("find user with GitHub id %d: %w", githubID, err)
	}
	return user, nil
}

// CreateTeam inserts a team with a URL-safe slug derived from its name.
// Unique name or slug violations are returned to the caller.
func (s *Store) CreateTeam(ctx context.Context, name string) (Team, error) {
	team := Team{Name: name, Slug: slugify(name)}
	if err := s.DB.WithContext(ctx).Create(&team).Error; err != nil {
		return Team{}, fmt.Errorf("create team %q: %w", name, err)
	}
	return team, nil
}

// RenameTeam changes a team's name and the slug derived from it. Renaming to
// the current name is a no-op; a name or slug already used by another team is
// ErrTeamNameTaken.
func (s *Store) RenameTeam(ctx context.Context, teamID uint, name string) (Team, error) {
	var team Team
	err := quietDB(s.DB.WithContext(ctx)).Take(&team, teamID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Team{}, fmt.Errorf("team %d: %w", teamID, ErrNotFound)
	}
	if err != nil {
		return Team{}, fmt.Errorf("find team %d: %w", teamID, err)
	}
	if name == team.Name {
		return team, nil
	}

	slug := slugify(name)
	var taken int64
	if err := s.DB.WithContext(ctx).Model(&Team{}).
		Where("id <> ? AND (name = ? OR slug = ?)", teamID, name, slug).
		Count(&taken).Error; err != nil {
		return Team{}, fmt.Errorf("check team name %q: %w", name, err)
	}
	if taken > 0 {
		return Team{}, fmt.Errorf("rename team %d to %q: %w", teamID, name, ErrTeamNameTaken)
	}

	now := time.Now().UTC()
	if err := s.DB.WithContext(ctx).Model(&Team{}).Where("id = ?", teamID).
		Updates(map[string]any{"name": name, "slug": slug, "updated_at": now}).Error; err != nil {
		return Team{}, fmt.Errorf("rename team %d to %q: %w", teamID, name, err)
	}
	team.Name, team.Slug, team.UpdatedAt = name, slug, now
	return team, nil
}

// TeamRepositories lists the repositories assigned to a team, ordered by name.
func (s *Store) TeamRepositories(ctx context.Context, teamID uint) ([]Repository, error) {
	var repos []Repository
	err := s.DB.WithContext(ctx).Model(&Repository{}).
		Select("id", "full_name").
		Where("team_id = ?", teamID).
		Order("full_name").
		Find(&repos).Error
	if err != nil {
		return nil, fmt.Errorf("list team %d repositories: %w", teamID, err)
	}
	return repos, nil
}

// DeleteTeam removes a team and, through the database cascade, its members and
// QA policy. A team that still owns repositories is ErrTeamInUse; the check
// runs inside the transaction so a late assignment is not missed. It returns
// the number of members that were removed.
func (s *Store) DeleteTeam(ctx context.Context, teamID uint) (int64, error) {
	var members int64
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var repos int64
		if err := tx.Model(&Repository{}).Where("team_id = ?", teamID).Count(&repos).Error; err != nil {
			return fmt.Errorf("count team %d repositories: %w", teamID, err)
		}
		if repos > 0 {
			return fmt.Errorf("delete team %d: %w", teamID, ErrTeamInUse)
		}

		if err := tx.Model(&TeamMember{}).Where("team_id = ?", teamID).Count(&members).Error; err != nil {
			return fmt.Errorf("count team %d members: %w", teamID, err)
		}

		res := tx.Delete(&Team{}, teamID)
		if res.Error != nil {
			return fmt.Errorf("delete team %d: %w", teamID, res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("delete team %d: %w", teamID, ErrNotFound)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return members, nil
}

func slugify(name string) string {
	var folded strings.Builder
	for _, r := range strings.ToLower(name) {
		folded.WriteString(foldLatin(r))
	}

	var slug strings.Builder
	dash := false
	for _, r := range folded.String() {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			slug.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			slug.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(slug.String(), "-")
}

// foldLatin transliterates the accented letters common in Latin text so team
// slugs stay ASCII and URL-safe.
func foldLatin(r rune) string {
	switch r {
	case 'à', 'á', 'â', 'ã', 'ä', 'å', 'ā', 'ă', 'ą':
		return "a"
	case 'ç', 'ć', 'č', 'ĉ', 'ċ':
		return "c"
	case 'ď', 'đ', 'ð':
		return "d"
	case 'è', 'é', 'ê', 'ë', 'ē', 'ĕ', 'ė', 'ę', 'ě':
		return "e"
	case 'ì', 'í', 'î', 'ï', 'ĩ', 'ī', 'ĭ', 'į', 'ı':
		return "i"
	case 'ł':
		return "l"
	case 'ñ', 'ń', 'ň', 'ņ':
		return "n"
	case 'ò', 'ó', 'ô', 'õ', 'ö', 'ø', 'ō', 'ŏ', 'ő':
		return "o"
	case 'ř', 'ŕ':
		return "r"
	case 'ś', 'š', 'ş', 'ș':
		return "s"
	case 'ť', 'ţ', 'ț':
		return "t"
	case 'ù', 'ú', 'û', 'ü', 'ũ', 'ū', 'ŭ', 'ů', 'ű':
		return "u"
	case 'ý', 'ÿ', 'ŷ':
		return "y"
	case 'ź', 'ż', 'ž':
		return "z"
	case 'ß':
		return "ss"
	case 'æ':
		return "ae"
	case 'œ':
		return "oe"
	default:
		return string(r)
	}
}

// teamMemberRow is the flattened result of joining team members with users.
type teamMemberRow struct {
	TeamID    uint
	ID        uint
	GitHubID  int64
	Login     string
	AvatarURL string
	IsAdmin   bool
	Role      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ListTeams returns every team, with member roles and repository counts, in a
// deterministic order.
func (s *Store) ListTeams(ctx context.Context) ([]TeamWithMembers, error) {
	var teams []Team
	if err := s.DB.WithContext(ctx).Order("name, id").Find(&teams).Error; err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}

	var rows []teamMemberRow
	err := s.DB.WithContext(ctx).
		Model(&TeamMember{}).
		Select(strings.Join([]string{
			"team_members.team_id AS team_id",
			"team_members.role AS role",
			"users.id AS id",
			"users.github_id AS github_id",
			"users.login AS login",
			"users.avatar_url AS avatar_url",
			"users.is_admin AS is_admin",
			"users.created_at AS created_at",
			"users.updated_at AS updated_at",
		}, ", ")).
		Joins("JOIN users ON users.id = team_members.user_id").
		Order("users.login, users.id").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list team members: %w", err)
	}

	type teamRepositoryCount struct {
		TeamID uint
		Repos  int
	}
	var counts []teamRepositoryCount
	err = s.DB.WithContext(ctx).Model(&Repository{}).
		Select("team_id, count(*) AS repos").
		Where("team_id IS NOT NULL").
		Group("team_id").
		Scan(&counts).Error
	if err != nil {
		return nil, fmt.Errorf("count team repositories: %w", err)
	}
	repositoryCount := make(map[uint]int, len(counts))
	for _, c := range counts {
		repositoryCount[c.TeamID] = c.Repos
	}

	result := make([]TeamWithMembers, 0, len(teams))
	for _, team := range teams {
		item := TeamWithMembers{
			Team:            team,
			RepositoryCount: repositoryCount[team.ID],
			Members: []struct {
				User User
				Role string
			}{},
		}
		for _, row := range rows {
			if row.TeamID != team.ID {
				continue
			}
			item.Members = append(item.Members, struct {
				User User
				Role string
			}{
				User: User{
					ID:        row.ID,
					GitHubID:  row.GitHubID,
					Login:     row.Login,
					AvatarURL: row.AvatarURL,
					IsAdmin:   row.IsAdmin,
					CreatedAt: row.CreatedAt,
					UpdatedAt: row.UpdatedAt,
				},
				Role: row.Role,
			})
		}
		result = append(result, item)
	}
	return result, nil
}

// SetTeamMember assigns a user to a team with one of the accepted roles,
// replacing the role if the membership already exists.
func (s *Store) SetTeamMember(ctx context.Context, teamID, userID uint, role string) error {
	switch role {
	case "lead", "qa", "developer":
	default:
		return fmt.Errorf("invalid team role %q", role)
	}

	member := TeamMember{TeamID: teamID, UserID: userID, Role: role}
	err := s.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "team_id"}, {Name: "user_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"role", "updated_at"}),
	}).Create(&member).Error
	if err != nil {
		return fmt.Errorf("set team %d member %d: %w", teamID, userID, err)
	}
	return nil
}

// RemoveTeamMember removes a user from a team.
func (s *Store) RemoveTeamMember(ctx context.Context, teamID, userID uint) error {
	res := s.DB.WithContext(ctx).
		Where("team_id = ? AND user_id = ?", teamID, userID).
		Delete(&TeamMember{})
	if res.Error != nil {
		return fmt.Errorf("remove team %d member %d: %w", teamID, userID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("remove team %d member %d: %w", teamID, userID, ErrNotFound)
	}
	return nil
}

// TeamRole returns the user's role in teamID, or "" if they are not a member.
func (s *Store) TeamRole(ctx context.Context, userID, teamID uint) (string, error) {
	var member TeamMember
	err := s.DB.WithContext(ctx).
		Where("team_id = ? AND user_id = ?", teamID, userID).
		Take(&member).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find team %d role for user %d: %w", teamID, userID, err)
	}
	return member.Role, nil
}

// RepoRole returns the user's role in the repo's owning team. Repos without a
// team and non-members yield "".
func (s *Store) RepoRole(ctx context.Context, userID uint, repoID int64) (string, error) {
	var repo Repository
	if err := s.DB.WithContext(ctx).Take(&repo, repoID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", fmt.Errorf("repository %d: %w", repoID, ErrNotFound)
		}
		return "", fmt.Errorf("find repository %d: %w", repoID, err)
	}
	if repo.TeamID == nil {
		return "", nil
	}
	return s.TeamRole(ctx, userID, *repo.TeamID)
}

// SetRepositoryEnabled flips the local enabled flag of a repository.
func (s *Store) SetRepositoryEnabled(ctx context.Context, repoID int64, enabled bool) error {
	res := s.DB.WithContext(ctx).Model(&Repository{}).Where("id = ?", repoID).Update("enabled", enabled)
	if res.Error != nil {
		return fmt.Errorf("set repository %d enabled: %w", repoID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("set repository %d enabled: %w", repoID, ErrNotFound)
	}
	return nil
}

// AssignRepositoryTeam sets the owning team of a repository; a nil teamID
// clears the assignment.
func (s *Store) AssignRepositoryTeam(ctx context.Context, repoID int64, teamID *uint) error {
	var value any
	if teamID != nil {
		value = *teamID
	}
	res := s.DB.WithContext(ctx).Model(&Repository{}).Where("id = ?", repoID).Update("team_id", value)
	if res.Error != nil {
		return fmt.Errorf("assign repository %d team: %w", repoID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("assign repository %d team: %w", repoID, ErrNotFound)
	}
	return nil
}

// ListRepositories returns all repositories for admins and only repositories
// of the caller's teams for everyone else.
func (s *Store) ListRepositories(ctx context.Context, u User) ([]Repository, error) {
	q := s.DB.WithContext(ctx).Model(&Repository{})
	if !u.IsAdmin {
		teams := s.DB.WithContext(ctx).Model(&TeamMember{}).
			Select("team_id").Where("user_id = ?", u.ID)
		q = q.Where("team_id IN (?)", teams)
	}

	var repos []Repository
	if err := q.Order("id").Find(&repos).Error; err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	return repos, nil
}

// RepositoryWithPolicy is a repository plus whether its assigned team has a QA
// policy and whether the repository overrides it. A repository without a team
// always reports false.
type RepositoryWithPolicy struct {
	Repository
	TeamHasPolicy     bool
	HasPolicyOverride bool
	PolicyObservation bool
}

// ListRepositoriesWithPolicy applies the same visibility rules as
// ListRepositories and annotates each row with its team policy status and the
// presence of a repository override (#153).
func (s *Store) ListRepositoriesWithPolicy(ctx context.Context, u User) ([]RepositoryWithPolicy, error) {
	q := s.DB.WithContext(ctx).Model(&Repository{}).
		Select(`repositories.*,
			EXISTS (SELECT 1 FROM qa_policies p WHERE p.team_id = repositories.team_id) AS team_has_policy,
			EXISTS (SELECT 1 FROM repository_policies rp WHERE rp.repository_id = repositories.id) AS has_policy_override,
			COALESCE((SELECT rp.observation_mode FROM repository_policies rp WHERE rp.repository_id = repositories.id), false) AS policy_observation`)
	if !u.IsAdmin {
		teams := s.DB.WithContext(ctx).Model(&TeamMember{}).
			Select("team_id").Where("user_id = ?", u.ID)
		q = q.Where("repositories.team_id IN (?)", teams)
	}

	var repos []RepositoryWithPolicy
	if err := q.Order("repositories.id").Find(&repos).Error; err != nil {
		return nil, fmt.Errorf("list repositories with policy: %w", err)
	}
	return repos, nil
}

// Audit appends an administrative action to the audit log. actorID may be nil
// for actions not performed by a signed-in user.
func (s *Store) Audit(ctx context.Context, actorID *uint, action, targetType, targetID string, data any) error {
	encoded, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal audit data: %w", err)
	}
	row := AuditLog{
		ActorUserID: actorID,
		Action:      action,
		TargetType:  targetType,
		TargetID:    targetID,
		Data:        datatypes.JSON(encoded),
	}
	if err := s.DB.WithContext(ctx).Create(&row).Error; err != nil {
		return fmt.Errorf("write audit row: %w", err)
	}
	return nil
}

// SaveAppConfig upserts the single GitHub App configuration row (id = 1).
func (s *Store) SaveAppConfig(ctx context.Context, c AppConfig) error {
	c.ID = 1
	err := s.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"app_id", "slug", "html_url", "client_id", "client_secret_enc",
			"private_key_enc", "webhook_secret_enc", "updated_at",
		}),
	}).Create(&c).Error
	if err != nil {
		return fmt.Errorf("save app config: %w", err)
	}
	return nil
}

// LoadAppConfig returns the saved configuration or ErrNotConfigured.
func (s *Store) LoadAppConfig(ctx context.Context) (AppConfig, error) {
	var c AppConfig
	err := s.DB.WithContext(ctx).Where("id = 1").Take(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return AppConfig{}, ErrNotConfigured
	}
	if err != nil {
		return AppConfig{}, fmt.Errorf("load app config: %w", err)
	}
	return c, nil
}

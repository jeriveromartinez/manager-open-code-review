package store

import (
	"time"

	"gorm.io/datatypes"
)

// AppConfig is the single-row instance configuration (id = 1) that holds the
// GitHub App credentials. Secrets are stored encrypted.
type AppConfig struct {
	ID               uint      `gorm:"column:id;primaryKey"`
	AppID            int64     `gorm:"column:app_id"`
	Slug             string    `gorm:"column:slug"`
	HTMLURL          string    `gorm:"column:html_url"`
	ClientID         string    `gorm:"column:client_id"`
	ClientSecretEnc  string    `gorm:"column:client_secret_enc"`
	PrivateKeyEnc    string    `gorm:"column:private_key_enc"`
	WebhookSecretEnc string    `gorm:"column:webhook_secret_enc"`
	CreatedAt        time.Time `gorm:"column:created_at"`
	UpdatedAt        time.Time `gorm:"column:updated_at"`
}

// User is a person who signed in with GitHub OAuth.
type User struct {
	ID        uint      `gorm:"column:id;primaryKey"`
	GitHubID  int64     `gorm:"column:github_id;uniqueIndex"`
	Login     string    `gorm:"column:login"`
	AvatarURL string    `gorm:"column:avatar_url"`
	IsAdmin   bool      `gorm:"column:is_admin"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// Session is an authenticated browser session. Only the sha256 hex hash of the
// token is stored.
type Session struct {
	ID        uint      `gorm:"column:id;primaryKey"`
	TokenHash string    `gorm:"column:token_hash;uniqueIndex"`
	UserID    uint      `gorm:"column:user_id"`
	ExpiresAt time.Time `gorm:"column:expires_at"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// Installation is a GitHub App installation. Its primary key is GitHub's
// installation ID.
type Installation struct {
	ID           int64      `gorm:"column:id;primaryKey;autoIncrement:false"`
	AccountLogin string     `gorm:"column:account_login"`
	AccountType  string     `gorm:"column:account_type"`
	SuspendedAt  *time.Time `gorm:"column:suspended_at"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
}

// Team groups repositories and users. A repository belongs to exactly one team.
type Team struct {
	ID        uint      `gorm:"column:id;primaryKey"`
	Name      string    `gorm:"column:name;uniqueIndex"`
	Slug      string    `gorm:"column:slug;uniqueIndex"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TeamMember links a user to a team with a role (lead, qa or developer).
type TeamMember struct {
	TeamID    uint      `gorm:"column:team_id;primaryKey"`
	UserID    uint      `gorm:"column:user_id;primaryKey"`
	Role      string    `gorm:"column:role"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// Repository is a GitHub repository tracked by the instance. Its primary key is
// GitHub's repository ID.
type Repository struct {
	ID             int64     `gorm:"column:id;primaryKey;autoIncrement:false"`
	InstallationID int64     `gorm:"column:installation_id"`
	FullName       string    `gorm:"column:full_name;uniqueIndex"`
	Private        bool      `gorm:"column:private"`
	DefaultBranch  string    `gorm:"column:default_branch"`
	Enabled        bool      `gorm:"column:enabled"`
	TeamID         *uint     `gorm:"column:team_id"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
}

// PullRequest is a GitHub pull request with its QA workflow state.
type PullRequest struct {
	ID                uint      `gorm:"column:id;primaryKey"`
	RepositoryID      int64     `gorm:"column:repository_id"`
	Number            int       `gorm:"column:number"`
	Title             string    `gorm:"column:title"`
	AuthorLogin       string    `gorm:"column:author_login"`
	AuthorAssociation string    `gorm:"column:author_association"`
	HeadSHA           string    `gorm:"column:head_sha"`
	BaseRef           string    `gorm:"column:base_ref"`
	HeadRef           string    `gorm:"column:head_ref"`
	HTMLURL           string    `gorm:"column:html_url"`
	Draft             bool      `gorm:"column:draft"`
	State             string    `gorm:"column:state"`
	QAStatus          string    `gorm:"column:qa_status"`
	GitHubUpdatedAt   time.Time `gorm:"column:github_updated_at"`
	CreatedAt         time.Time `gorm:"column:created_at"`
	UpdatedAt         time.Time `gorm:"column:updated_at"`
}

// AuditLog records an administrative action.
type AuditLog struct {
	ID          uint           `gorm:"column:id;primaryKey"`
	ActorUserID *uint          `gorm:"column:actor_user_id"`
	Action      string         `gorm:"column:action"`
	TargetType  string         `gorm:"column:target_type"`
	TargetID    string         `gorm:"column:target_id"`
	Data        datatypes.JSON `gorm:"column:data"`
	CreatedAt   time.Time      `gorm:"column:created_at"`
	UpdatedAt   time.Time      `gorm:"column:updated_at"`
}

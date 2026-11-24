package store

import (
	"strings"
	"time"

	"gorm.io/datatypes"
)

// Statuses and kinds used by the Phase 2 review pipeline.
const (
	RunQueued       = "queued"
	RunRunning      = "running"
	RunSucceeded    = "succeeded"
	RunInconclusive = "inconclusive"
	RunFailed       = "failed"
	RunSuperseded   = "superseded"

	// RunErrorInterrupted marks a run the watchdog failed because the worker
	// that claimed it disappeared before writing a terminal state.
	RunErrorInterrupted = "interrupted"

	// Phases of a running review (#123). They are stable identifiers: the panel
	// translates them and the log groups by milestone, so the values must not
	// carry prose. Empty means the run is queued, finished or predates phases.
	RunPhaseCloning   = "cloning"
	RunPhasePreparing = "preparing"
	RunPhaseReviewing = "reviewing"
	RunPhaseChunking  = "chunking"
	RunPhaseVerifying = "verifying"
	RunPhaseFinishing = "finishing"

	HealthUnknown = "unknown"
	HealthOK      = "ok"
	HealthError   = "error"

	FindingOpen      = "open"
	FindingResolved  = "resolved"
	FindingWithdrawn = "withdrawn"

	// IdentityVersionLegacy marks findings written before the identity change
	// (#144); IdentityVersionCurrent is the fingerprint scheme every new row
	// carries. Legacy rows migrate in place the first time a review matches them.
	IdentityVersionLegacy  int16 = 1
	IdentityVersionCurrent int16 = 2

	VerdictConfirmed     = "confirmed"
	VerdictFalsePositive = "false_positive"

	// Auto-triage modes of a team. DryRun records verdicts without changing any
	// finding; Apply lets the verifier triage under the policy's limits.
	AutoTriageOff    = "off"
	AutoTriageDryRun = "dry_run"
	AutoTriageApply  = "apply"

	// TriageSource records who produced the last triage of a finding.
	TriageSourceHuman    = "human"
	TriageSourceVerifier = "verifier"

	ArtifactSummary = "summary_comment"
	ArtifactReview  = "review"
	ArtifactCheck   = "check_run"
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
	ID                uint   `gorm:"column:id;primaryKey"`
	RepositoryID      int64  `gorm:"column:repository_id"`
	Number            int    `gorm:"column:number"`
	Title             string `gorm:"column:title"`
	AuthorLogin       string `gorm:"column:author_login"`
	AuthorAssociation string `gorm:"column:author_association"`
	HeadSHA           string `gorm:"column:head_sha"`
	BaseRef           string `gorm:"column:base_ref"`
	// BaseSHA is the base branch tip last announced by GitHub. MergeBaseSHA is
	// the comparison identity (#141): the real base of the three-dot diff,
	// resolved through the Compare API and cleared whenever the head or the
	// base moves.
	BaseSHA           string    `gorm:"column:base_sha"`
	MergeBaseSHA      string    `gorm:"column:merge_base_sha"`
	HeadRef           string    `gorm:"column:head_ref"`
	HTMLURL           string    `gorm:"column:html_url"`
	Draft             bool      `gorm:"column:draft"`
	State             string    `gorm:"column:state"`
	QAStatus          string    `gorm:"column:qa_status"`
	GitHubUpdatedAt   time.Time `gorm:"column:github_updated_at"`
	IsFork            bool      `gorm:"column:is_fork"`
	HeadRepoFullName  string    `gorm:"column:head_repo_full_name"`
	CreatedAt         time.Time `gorm:"column:created_at"`
	UpdatedAt         time.Time `gorm:"column:updated_at"`
	ValidationsPassed int       `gorm:"column:validations_passed"`
	ValidationsFailed int       `gorm:"column:validations_failed"`
	ValidationsTotal  int       `gorm:"column:validations_total"`
	// Maintained by a database trigger whenever qa_status changes, so GORM
	// must never write it.
	QAStatusChangedAt time.Time `gorm:"column:qa_status_changed_at;<-:false"`
}

// IsOpen reports whether GitHub still has the pull request open. A merged or
// closed PR must not start a review, even if a scheduled trigger is pending.
func (p PullRequest) IsOpen() bool { return p.State == stateOpen }

// Comparison is the identity of one base–head comparison (#141). BaseRef is part
// of it because the prompt context names the base branch; MergeBaseSHA is the
// real base of the three-dot diff, not the base branch tip.
type Comparison struct {
	HeadSHA      string
	BaseRef      string
	MergeBaseSHA string
}

// Comparison returns the comparison the pull request is currently at.
func (p PullRequest) Comparison() Comparison {
	return Comparison{HeadSHA: p.HeadSHA, BaseRef: p.BaseRef, MergeBaseSHA: p.MergeBaseSHA}
}

// Comparison returns the comparison a review run was created for. The run's
// BaseSHA is the merge base it resolved, so it doubles as the comparison key.
func (r ReviewRun) Comparison() Comparison {
	return Comparison{HeadSHA: r.HeadSHA, BaseRef: r.BaseRef, MergeBaseSHA: r.BaseSHA}
}

// Covers reports whether c (the key of a run) covers o (the current key). A
// run with another head or base branch never covers it, and a run that resolved
// a merge base stops covering it as soon as that merge base is no longer the
// current one. A run without a base branch and without a merge base predates
// #141 and is a one-time wildcard; a run with a known merge base is compared
// even when its legacy base branch is empty.
func (c Comparison) Covers(o Comparison) bool {
	if !strings.EqualFold(c.HeadSHA, o.HeadSHA) {
		return false
	}
	if c.BaseRef == "" && c.MergeBaseSHA == "" {
		// Pre-#141 row: no part of the comparison is known.
		return true
	}
	if c.BaseRef != "" && c.BaseRef != o.BaseRef {
		return false
	}
	if c.MergeBaseSHA == "" {
		// An unknown merge base only covers an unresolved current comparison;
		// claiming any resolved one would keep a stale result forever.
		return o.MergeBaseSHA == ""
	}
	return o.MergeBaseSHA != "" && strings.EqualFold(c.MergeBaseSHA, o.MergeBaseSHA)
}

// CoveredBy reports whether the run belongs to the pull request's current
// comparison. It backs both deduplication and the publication guard.
func (p PullRequest) CoveredBy(r ReviewRun) bool {
	return r.Comparison().Covers(p.Comparison())
}

// ComparisonMoved reports whether a synced payload changed the comparison
// identity: the head, the base branch, the base tip, or the resolved merge
// base (which SyncPullRequest clears when either side moves).
func ComparisonMoved(prev, next PullRequest) bool {
	return !strings.EqualFold(prev.HeadSHA, next.HeadSHA) ||
		prev.BaseRef != next.BaseRef ||
		!strings.EqualFold(prev.BaseSHA, next.BaseSHA) ||
		!strings.EqualFold(prev.MergeBaseSHA, next.MergeBaseSHA)
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

// LLMProfile is a configured LLM backend used by the review worker. Its API key
// is encrypted with the worker key, separately from the app credentials.
type LLMProfile struct {
	ID              uint       `gorm:"column:id;primaryKey"`
	Name            string     `gorm:"column:name;uniqueIndex"`
	Provider        string     `gorm:"column:provider"`
	Model           string     `gorm:"column:model"`
	Variant         *string    `gorm:"column:variant"`
	Mode            string     `gorm:"column:mode"`
	APIKeyEnc       string     `gorm:"column:api_key_enc"`
	BaseURL         *string    `gorm:"column:base_url"`
	Concurrency     int        `gorm:"column:concurrency"`
	Enabled         bool       `gorm:"column:enabled"`
	HealthStatus    string     `gorm:"column:health_status"`
	HealthCheckedAt *time.Time `gorm:"column:health_checked_at"`
	HealthError     string     `gorm:"column:health_error"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

// LLMHealthCheck is one append-only probe of an LLM profile. The profile row
// keeps only the latest verdict, so this history carries the token usage of
// every check outside a review. Token columns stay NULL when the provider did
// not report usage or the probe failed (#73).
type LLMHealthCheck struct {
	ID               uint      `gorm:"column:id;primaryKey"`
	ProfileID        uint      `gorm:"column:profile_id"`
	Status           string    `gorm:"column:status"`
	ErrorMessage     string    `gorm:"column:error_message"`
	InputTokens      *int64    `gorm:"column:input_tokens"`
	OutputTokens     *int64    `gorm:"column:output_tokens"`
	ReasoningTokens  *int64    `gorm:"column:reasoning_tokens"`
	CacheReadTokens  *int64    `gorm:"column:cache_read_tokens"`
	CacheWriteTokens *int64    `gorm:"column:cache_write_tokens"`
	TotalTokens      *int64    `gorm:"column:total_tokens"`
	MaxContextTokens *int64    `gorm:"column:max_context_tokens"`
	CheckedAt        time.Time `gorm:"column:checked_at"`
}

// QAPolicy holds the review settings of a team. ExcludeGlobs is a JSON array of
// path patterns stored in a jsonb column.
type QAPolicy struct {
	ID                  uint           `gorm:"column:id;primaryKey"`
	TeamID              uint           `gorm:"column:team_id;uniqueIndex"`
	LLMProfileID        uint           `gorm:"column:llm_profile_id"`
	BlockingThreshold   string         `gorm:"column:blocking_threshold"`
	ExtraInstructions   string         `gorm:"column:extra_instructions"`
	ExcludeGlobs        datatypes.JSON `gorm:"column:exclude_globs"`
	MaxFilesPerPass     int            `gorm:"column:max_files_per_pass"`
	MaxDiffLinesPerPass int            `gorm:"column:max_diff_lines_per_pass"`
	MaxPasses           int            `gorm:"column:max_passes"`
	RequireGreenCI      bool           `gorm:"column:require_green_ci"`
	// ReviewContextCeiling overrides the global review_context_ceiling for this
	// team. Nil inherits the global setting.
	ReviewContextCeiling *int   `gorm:"column:review_context_ceiling"`
	ReviewEffort         string `gorm:"column:review_effort;default:low"`
	// ReviewTokenBudget is the optional cap on the run's total tokens. Zero
	// disables it.
	ReviewTokenBudget int64 `gorm:"column:review_token_budget"`
	// AutoTriageMode turns the second OCR pass on. VerifyTokenMax is the token
	// cap used when ReviewTokenBudget disables the run budget.
	AutoTriageMode    string    `gorm:"column:auto_triage_mode;default:off"`
	AutoWithdraw      bool      `gorm:"column:auto_withdraw"`
	VerifyMaxFindings int       `gorm:"column:verify_max_findings"`
	VerifyMaxFiles    int       `gorm:"column:verify_max_files"`
	VerifyTokenShare  int       `gorm:"column:verify_token_share"`
	VerifyTimeShare   int       `gorm:"column:verify_time_share"`
	VerifyTokenMax    int64     `gorm:"column:verify_token_max"`
	CreatedAt         time.Time `gorm:"column:created_at"`
	UpdatedAt         time.Time `gorm:"column:updated_at"`
}

// ReviewRun is one execution of the review pipeline for a pull request. At
// most one run per PR can be queued or running at a time.
type ReviewRun struct {
	ID            uint   `gorm:"column:id;primaryKey"`
	PullRequestID uint   `gorm:"column:pull_request_id"`
	ProfileID     uint   `gorm:"column:profile_id"`
	HeadSHA       string `gorm:"column:head_sha"`
	BaseSHA       string `gorm:"column:base_sha"`
	// BaseRef and BaseTipSHA snapshot the comparison the run belongs to (#141).
	// BaseSHA above is the merge base of that comparison; runs created before
	// the comparison identity existed carry empty values and act as wildcards
	// once.
	BaseRef       string         `gorm:"column:base_ref"`
	BaseTipSHA    string         `gorm:"column:base_tip_sha"`
	Trigger       string         `gorm:"column:trigger"`
	Status        string         `gorm:"column:status"`
	Attempt       int            `gorm:"column:attempt"`
	Summary       string         `gorm:"column:summary"`
	Coverage      datatypes.JSON `gorm:"column:coverage"`
	Usage         datatypes.JSON `gorm:"column:usage"`
	ErrorCode     string         `gorm:"column:error_code"`
	ErrorMessage  string         `gorm:"column:error_message"`
	CloneTokenEnc string         `gorm:"column:clone_token_enc"`
	// CloneTokenExpiresAt lets the control process renew the token of a run
	// that waits in the queue longer than the token lives. Nil on runs created
	// before the expiry was stored.
	CloneTokenExpiresAt *time.Time `gorm:"column:clone_token_expires_at"`
	// Structured token consumption, summed over every model call of the run.
	// Nil on runs created before consumption was recorded (#73).
	InputTokens      *int64 `gorm:"column:input_tokens"`
	OutputTokens     *int64 `gorm:"column:output_tokens"`
	ReasoningTokens  *int64 `gorm:"column:reasoning_tokens"` // hidden part of OutputTokens (#65)
	CacheReadTokens  *int64 `gorm:"column:cache_read_tokens"`
	CacheWriteTokens *int64 `gorm:"column:cache_write_tokens"`
	TotalTokens      *int64 `gorm:"column:total_tokens"`
	// MaxContextTokens is the largest prompt of the run, used to watch the
	// 270k context ceiling (#72).
	MaxContextTokens *int64 `gorm:"column:max_context_tokens"`
	// Phase is the current step of a running run and PhaseStartedAt when it
	// began. Both stay empty on queued and terminal runs and on runs created
	// before #123, which the panel renders as "—".
	Phase          string     `gorm:"column:phase"`
	PhaseStartedAt *time.Time `gorm:"column:phase_started_at"`
	// VerifySessionID and VerifyUsage carry the second OCR pass: its session for
	// traceability and its consumption so the panel can show the extra cost.
	VerifySessionID string         `gorm:"column:verify_session_id"`
	VerifyUsage     datatypes.JSON `gorm:"column:verify_usage"`
	// HeartbeatAt is refreshed by the executing worker every minute. Nil on
	// runs claimed before the heartbeat existed (#130).
	HeartbeatAt *time.Time `gorm:"column:heartbeat_at"`
	StartedAt   *time.Time `gorm:"column:started_at"`
	FinishedAt  *time.Time `gorm:"column:finished_at"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
}

// ReviewRunLog is one line of a review run's live log. Seq orders the lines of
// one run and (run_id, seq) is the primary key, so a retried flush can insert
// the same line twice without duplicating it.
type ReviewRunLog struct {
	RunID      uint      `gorm:"column:run_id;primaryKey"`
	Seq        int64     `gorm:"column:seq;primaryKey"`
	TS         time.Time `gorm:"column:ts"`
	Source     string    `gorm:"column:source"`
	Invocation string    `gorm:"column:invocation"`
	Level      string    `gorm:"column:level"`
	Phase      string    `gorm:"column:phase"`
	Line       string    `gorm:"column:line"`
}

// Finding is a review comment deduplicated by (pull request, fingerprint).
// IDs are generated by the review package, not by the database.
type Finding struct {
	ID            string `gorm:"column:id;primaryKey;autoIncrement:false"`
	PullRequestID uint   `gorm:"column:pull_request_id"`
	Fingerprint   string `gorm:"column:fingerprint"`
	// IdentityVersion is the fingerprint scheme of the row. It stays 1 on rows
	// written before #144 until a review matches them and migrates them in place.
	IdentityVersion int16  `gorm:"column:identity_version;default:2"`
	Path            string `gorm:"column:path"`
	StartLine       int    `gorm:"column:start_line"`
	EndLine         int    `gorm:"column:end_line"`
	Severity        string `gorm:"column:severity"`
	Category        string `gorm:"column:category"`
	Title           string `gorm:"column:title"`
	Body            string `gorm:"column:body"`
	Suggestion      string `gorm:"column:suggestion"`
	Confidence      string `gorm:"column:confidence"`
	// ExistingCode is the code OCR cited when reporting the finding. It decides
	// whether a stored verdict still refers to the same code.
	ExistingCode string         `gorm:"column:existing_code"`
	Verification datatypes.JSON `gorm:"column:verification"`
	// TriageSource is who produced the last triage: human or verifier.
	TriageSource        string    `gorm:"column:triage_source"`
	Status              string    `gorm:"column:status"`
	FirstSeenRunID      uint      `gorm:"column:first_seen_run_id"`
	LastSeenRunID       uint      `gorm:"column:last_seen_run_id"`
	GithubCommentID     int64     `gorm:"column:github_comment_id"`
	GithubCommentNodeID string    `gorm:"column:github_comment_node_id"`
	GithubThreadID      string    `gorm:"column:github_thread_id"`
	CreatedAt           time.Time `gorm:"column:created_at"`
	UpdatedAt           time.Time `gorm:"column:updated_at"`
	// TriageVerdict, TriagedBy and TriagedAt record the last panel decision on
	// the finding. Only TriageFinding writes them: a review run or a /qa command
	// changes Status but leaves QA's verdict as it was.
	TriageVerdict string     `gorm:"column:triage_verdict"`
	TriagedBy     string     `gorm:"column:triaged_by"`
	TriagedAt     *time.Time `gorm:"column:triaged_at"`
}

// FindingEvent records a lifecycle transition of a finding.
type FindingEvent struct {
	ID          uint           `gorm:"column:id;primaryKey"`
	FindingID   string         `gorm:"column:finding_id"`
	ReviewRunID uint           `gorm:"column:review_run_id"`
	Kind        string         `gorm:"column:kind"`
	Actor       string         `gorm:"column:actor"`
	Data        datatypes.JSON `gorm:"column:data"`
	CreatedAt   time.Time      `gorm:"column:created_at"`
}

// GitHubArtifact tracks a comment, review or check run published for a PR.
type GitHubArtifact struct {
	ID            uint      `gorm:"column:id;primaryKey"`
	PullRequestID uint      `gorm:"column:pull_request_id"`
	Kind          string    `gorm:"column:kind"`
	HeadSHA       string    `gorm:"column:head_sha"`
	ExternalID    string    `gorm:"column:external_id"`
	URL           string    `gorm:"column:url"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
}

// TableName keeps the GORM default from splitting the GitHub initialism into
// "git_hub_artifacts".
func (GitHubArtifact) TableName() string { return "github_artifacts" }

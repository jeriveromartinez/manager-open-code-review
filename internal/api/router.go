// Package api exposes the QA Control Center HTTP API and the SPA fallback.
package api

import (
	"context"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/jeriveromartinez/qa-control-center/internal/auth"
	"github.com/jeriveromartinez/qa-control-center/internal/config"
	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/jobs"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// HealthEnqueuer schedules an asynchronous health check for an LLM profile.
// The review worker's control client implements it; the HTTP layer only needs
// this narrow contract.
type HealthEnqueuer interface {
	EnqueueLLMHealth(ctx context.Context, profileID uint) error
}

// ReviewRetrier schedules a fresh review run for a pull request. Keeping this
// interface here avoids coupling the API package to the review package.
type ReviewRetrier interface {
	Retry(ctx context.Context, prID uint) error
}

// FindingsPublisher mirrors a finding change made in the panel on GitHub: it
// resolves or reopens the published thread and recomputes the check.
type FindingsPublisher interface {
	Republish(ctx context.Context, prID uint) error
	SetFindingThread(ctx context.Context, prID uint, finding store.Finding, resolved bool) (bool, error)
}

// Deps groups every dependency the HTTP layer needs.
type Deps struct {
	Cfg        config.Config
	Store      *store.Store
	Box        *crypto.Box
	WorkerBox  *crypto.Box
	Apps       github.AppProvider
	Auth       *auth.Auth
	Webhooks   http.Handler
	Broker     *events.Broker
	Events     events.Publisher
	Jobs       jobs.Enqueuer
	Health     HealthEnqueuer
	Reviews    ReviewRetrier
	Findings   FindingsPublisher
	Models     LLMModelLister
	SetupToken string
	Static     http.Handler
}

type server struct {
	Deps
	// settingsMu serialises the read-validate-write cycle of a settings save.
	// Without it two admins saving different keys of an interdependent pair can
	// each validate against the pre-save state and persist an incoherent pair.
	// The serialisation is per process, which is sound because the deployment
	// runs a single app instance; scaling out would need a lock in PostgreSQL.
	settingsMu sync.Mutex
}

// NewRouter builds the complete HTTP handler: public setup/auth routes, the
// authenticated API, and a catch-all that serves the embedded SPA.
func NewRouter(d Deps) http.Handler {
	s := &server{Deps: d}
	r := chi.NewRouter()

	// Session resolution is global so public routes can also see a signed-in
	// user when they care; route guards enforce authentication separately.
	if d.Auth != nil {
		r.Use(d.Auth.Middleware)
	}

	r.Get("/healthz", s.healthz)

	r.Get("/api/setup/status", s.setupStatus)
	r.Post("/api/setup/manifest", s.setupManifest)
	r.Get("/setup/callback", s.setupCallback)
	r.Get("/setup/installed", s.setupInstalled)

	r.Get("/auth/login", d.Auth.Login)
	r.Get("/auth/callback", d.Auth.Callback)
	r.Post("/auth/logout", d.Auth.Logout)

	// The setting is read at startup because the route table is built here: with
	// the webhook disabled the request falls through to the static fallback,
	// which rejects the POST method, so a delivery GitHub still sends finds
	// nothing to process.
	if d.Webhooks != nil && d.Cfg.WebhooksEnabled {
		r.Post("/webhooks/github", d.Webhooks.ServeHTTP)
	}

	r.Route("/api", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireUser)

			r.Get("/me", s.me)
			r.Get("/stats", s.stats)
			r.Get("/qa/summary", s.qaSummary)
			r.Get("/prs", s.prs)
			r.Get("/prs/{id}", s.prDetail)
			// The run log is scoped through its pull request, so a user who
			// cannot see the PR cannot read the log either.
			r.Get("/review-runs", s.activeRuns)
			// /review-runs/history must stay before the {id}/logs pattern so chi
			// resolves the static segment first.
			r.Get("/review-runs/history", s.runHistory)
			r.Get("/review-runs/{id}/logs", s.reviewRunLogs)
			r.Get("/repos", s.repos)
			r.Get("/teams", s.teams)
			// /api/users returns minimal public profile fields (id, login,
			// avatar_url, is_admin). Leads need it to manage their team members.
			r.Get("/users", s.users)
			r.Get("/events", s.events)

			// Team membership allows admins and the team's lead; the handler
			// performs that finer-grained check.
			r.Put("/teams/{id}/members/{userId}", s.setTeamMember)
			r.Delete("/teams/{id}/members/{userId}", s.removeTeamMember)

			// Policies allow admins and the lead of that particular team.
			r.Get("/teams/{id}/policy", s.getPolicy)
			r.Put("/teams/{id}/policy", s.putPolicy)

			// Repository overrides follow the same rule: admins and the lead
			// of the repository's team (#153).
			r.Get("/repos/{id}/policy", s.getRepoPolicy)
			r.Put("/repos/{id}/policy", s.putRepoPolicy)
			r.Delete("/repos/{id}/policy", s.deleteRepoPolicy)

			// Retry, finding actions and the human verdict of an inconclusive
			// review are limited to admins and the lead or QA of the repo's team.
			r.Post("/prs/{id}/retry", s.retryPR)
			r.Post("/prs/{id}/human-verdict", s.recordHumanVerdict)
			r.Post("/prs/{id}/findings/{findingID}/withdraw", s.withdrawFinding)
			r.Post("/prs/{id}/findings/{findingID}/confirm", s.confirmFinding)

			r.Group(func(r chi.Router) {
				r.Use(auth.RequireAdmin)

				r.Get("/installations", s.installations)
				r.Post("/installations/{id}/sync", s.syncInstallation)
				r.Get("/readiness", s.readiness)
				r.Patch("/repos/{id}", s.patchRepo)
				r.Post("/teams", s.createTeam)
				r.Patch("/teams/{id}", s.patchTeam)
				r.Delete("/teams/{id}", s.deleteTeam)

				r.Get("/llm-profiles", s.llmProfiles)
				r.Get("/llm-profiles/usage", s.llmProfileUsage)
				r.Post("/llm-profiles", s.createLLMProfile)
				r.Post("/llm-profiles/models", s.llmModels)
				r.Patch("/llm-profiles/{id}", s.patchLLMProfile)
				r.Delete("/llm-profiles/{id}", s.deleteLLMProfile)
				r.Post("/llm-profiles/{id}/health", s.enqueueLLMHealth)

				r.Get("/settings", s.getSettings)
				r.Put("/settings", s.putSettings)
			})
		})
	})

	static := d.Static
	if static == nil {
		static = http.NotFoundHandler()
	}
	r.NotFound(static.ServeHTTP)

	return r
}

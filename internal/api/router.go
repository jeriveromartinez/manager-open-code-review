// Package api exposes the QA Control Center HTTP API and the SPA fallback.
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/jeriveromartinez/qa-control-center/internal/auth"
	"github.com/jeriveromartinez/qa-control-center/internal/config"
	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/jobs"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// Deps groups every dependency the HTTP layer needs.
type Deps struct {
	Cfg        config.Config
	Store      *store.Store
	Box        *crypto.Box
	Apps       github.AppProvider
	Auth       *auth.Auth
	Webhooks   http.Handler
	Broker     *events.Broker
	Events     events.Publisher
	Jobs       jobs.Enqueuer
	SetupToken string
	Static     http.Handler
}

type server struct {
	Deps
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

	if d.Webhooks != nil {
		r.Post("/webhooks/github", d.Webhooks.ServeHTTP)
	}

	r.Route("/api", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireUser)

			r.Get("/me", s.me)
			r.Get("/stats", s.stats)
			r.Get("/prs", s.prs)
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

			r.Group(func(r chi.Router) {
				r.Use(auth.RequireAdmin)

				r.Patch("/repos/{id}", s.patchRepo)
				r.Post("/teams", s.createTeam)
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

package api

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/jeriveromartinez/qa-control-center/internal/github"
)

// setupInstalledPage answers an installation the organisation still has to
// approve. GitHub redirects the browser here before any account exists, so it
// cannot be a SPA route: the page ships its own styles.
//
//go:embed setup_installed.html
var setupInstalledPage string

type setupStatusResponse struct {
	Configured    bool   `json:"configured"`
	InstallURL    string `json:"install_url"`
	TokenRequired bool   `json:"token_required"`
}

type setupManifestResponse struct {
	ActionURL string          `json:"action_url"`
	Manifest  github.Manifest `json:"manifest"`
}

func (s *server) isConfigured(r *http.Request) bool {
	_, err := s.Store.LoadAppConfig(r.Context())
	return err == nil
}

// setupStatus tells the SPA whether the App has been created and where to send
// the administrator for installation.
func (s *server) setupStatus(w http.ResponseWriter, r *http.Request) {
	response := setupStatusResponse{TokenRequired: s.SetupToken != ""}
	if _, err := s.Store.LoadAppConfig(r.Context()); err == nil {
		response.Configured = true
		app, err := s.Apps.App(r.Context())
		if err == nil && app != nil {
			response.InstallURL = app.InstallURL()
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// setupManifest returns the GitHub App manifest and the URL that submits it.
func (s *server) setupManifest(w http.ResponseWriter, r *http.Request) {
	if s.isConfigured(r) {
		writeAPIError(w, http.StatusConflict, "already configured")
		return
	}

	var body struct {
		Name       string `json:"name"`
		SetupToken string `json:"setup_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeAPIError(w, http.StatusBadRequest, "name is required")
		return
	}
	// The token is optional: it only exists for operators who want to lock the
	// setup window down. Leaving it unset keeps the first-run wizard honest
	// instead of asking for a value copied from the logs.
	if s.SetupToken != "" && body.SetupToken != s.SetupToken {
		writeAPIError(w, http.StatusForbidden, "invalid setup token")
		return
	}

	configuredURL := strings.TrimRight(strings.TrimSpace(s.Cfg.PublicURL), "/")
	publicURL := configuredURL
	if publicURL == "" {
		publicURL = s.requestPublicURL(r)
	}
	if publicURL == "" {
		writeAPIError(w, http.StatusBadRequest, "cannot determine the public URL")
		return
	}
	if configuredURL == "" {
		if err := s.Store.SaveSettings(r.Context(), nil, map[string]string{"public_url": publicURL}); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "save public url")
			return
		}
	}

	manifest := github.BuildManifest(publicURL, s.Cfg.WebhooksEnabled, name)
	actionURL := strings.TrimRight(s.Cfg.GitHubURL, "/") + "/settings/apps/new"
	writeJSON(w, http.StatusOK, setupManifestResponse{ActionURL: actionURL, Manifest: manifest})
}

// setupCallback exchanges the manifest code for credentials, persists them and
// redirects to the App's installation page.
func (s *server) setupCallback(w http.ResponseWriter, r *http.Request) {
	if s.isConfigured(r) {
		writeAPIError(w, http.StatusConflict, "already configured")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		writeAPIError(w, http.StatusBadRequest, "missing manifest code")
		return
	}

	client := s.Auth.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	creds, err := github.ConvertManifest(r.Context(), client, s.Cfg.GitHubAPIURL, code)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "convert manifest")
		return
	}
	if err := github.SaveCredentials(r.Context(), s.Store, s.Box, creds); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "save app credentials")
		return
	}
	if err := s.Store.Audit(r.Context(), nil, "app.configured", "app", "1", map[string]any{"slug": creds.Slug}); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "write audit log")
		return
	}

	app := &github.App{Creds: creds, APIURL: s.Cfg.GitHubAPIURL}
	http.Redirect(w, r, app.InstallURL(), http.StatusFound)
}

// setupInstalled schedules the initial sync after the administrator installs
// the App and GitHub redirects back to the setup URL.
func (s *server) setupInstalled(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("setup_action") == "request" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(setupInstalledPage))
		return
	}

	id, err := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid installation_id")
		return
	}
	if s.Jobs == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "job queue unavailable")
		return
	}
	if err := s.Jobs.EnqueueSync(r.Context(), id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "enqueue sync")
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

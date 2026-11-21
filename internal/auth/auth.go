// Package auth implements GitHub OAuth sign-in, browser sessions and the
// role-based guards used by the HTTP API.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	// SessionCookie is the name of the cookie holding the clear session token.
	SessionCookie = "qa_session"
	stateCookie   = "qa_oauth_state"

	sessionTTL = 7 * 24 * time.Hour
	stateTTL   = 10 * time.Minute
)

// Auth holds the dependencies needed to sign users in through GitHub and to
// resolve their sessions on later requests.
type Auth struct {
	Store     *store.Store
	Apps      github.AppProvider
	GitHubURL string
	APIURL    string
	PublicURL string
	HTTP      *http.Client
	Events    events.Publisher
}

type userContextKey struct{}

// UserFrom returns the signed-in user placed in the request context by
// Middleware, if any.
func UserFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userContextKey{}).(store.User)
	return u, ok
}

func (a *Auth) client() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return http.DefaultClient
}

func (a *Auth) sessionCookie(value string, maxAge int, publicURL string) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.secureCookies(publicURL),
		MaxAge:   maxAge,
	}
}

func (a *Auth) stateCookie(value string, maxAge int, publicURL string) *http.Cookie {
	return &http.Cookie{
		Name:     stateCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.secureCookies(publicURL),
		MaxAge:   maxAge,
	}
}

func randomHex(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// Login starts the OAuth flow. Without a configured GitHub App it returns 503
// so the SPA can direct the administrator to /setup.
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	app, err := a.Apps.App(r.Context())
	if errors.Is(err, store.ErrNotConfigured) || (err == nil && app == nil) {
		http.Error(w, "github app is not configured", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		http.Error(w, "load github app", http.StatusInternalServerError)
		return
	}

	state, err := randomHex(32)
	if err != nil {
		http.Error(w, "generate oauth state", http.StatusInternalServerError)
		return
	}
	publicURL := a.PublicURLFor(r.Context(), r)
	http.SetCookie(w, a.stateCookie(state, int(stateTTL.Seconds()), publicURL))

	q := url.Values{}
	q.Set("client_id", app.Creds.ClientID)
	q.Set("state", state)
	q.Set("redirect_uri", callbackURL(publicURL))
	target := strings.TrimRight(a.GitHubURL, "/") + "/login/oauth/authorize?" + q.Encode()
	http.Redirect(w, r, target, http.StatusFound)
}

// Callback validates the OAuth state, exchanges the code for a token, loads the
// GitHub profile and starts a local session.
func (a *Auth) Callback(w http.ResponseWriter, r *http.Request) {
	publicURL := a.PublicURLFor(r.Context(), r)
	// The state cookie is single use, clear it even when validation fails.
	http.SetCookie(w, a.stateCookie("", -1, publicURL))

	cookie, err := r.Cookie(stateCookie)
	if err != nil || cookie.Value == "" || r.URL.Query().Get("state") != cookie.Value {
		http.Error(w, "invalid oauth state", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing oauth code", http.StatusBadRequest)
		return
	}

	app, err := a.Apps.App(r.Context())
	if errors.Is(err, store.ErrNotConfigured) || (err == nil && app == nil) {
		http.Error(w, "github app is not configured", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		http.Error(w, "load github app", http.StatusInternalServerError)
		return
	}

	token, err := a.exchangeCode(r.Context(), app, code, publicURL)
	if err != nil {
		http.Error(w, "oauth token exchange failed", http.StatusBadGateway)
		return
	}
	profile, err := a.fetchUser(r.Context(), token)
	if err != nil {
		http.Error(w, "load github user failed", http.StatusBadGateway)
		return
	}

	user, err := a.Store.UpsertUserFromGitHub(r.Context(), profile.ID, profile.Login, profile.AvatarURL)
	if err != nil {
		http.Error(w, "save user", http.StatusInternalServerError)
		return
	}
	// The upsert does not say whether the user is new, so every login notifies;
	// a lost notification must never block a sign-in.
	if a.Events != nil {
		ev := events.Event{Type: "user.updated", ID: strconv.FormatUint(uint64(user.ID), 10)}
		if err := a.Events.Publish(r.Context(), ev); err != nil {
			slog.Warn("publish event", "type", ev.Type, "id", ev.ID, "error", err)
		}
	}
	session, err := a.Store.CreateSession(r.Context(), user.ID, sessionTTL)
	if err != nil {
		http.Error(w, "create session", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, a.sessionCookie(session, int(sessionTTL.Seconds()), publicURL))
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *Auth) exchangeCode(ctx context.Context, app *github.App, code, publicURL string) (string, error) {
	form := url.Values{}
	form.Set("client_id", app.Creds.ClientID)
	form.Set("client_secret", app.Creds.ClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", callbackURL(publicURL))

	endpoint := strings.TrimRight(a.GitHubURL, "/") + "/login/oauth/access_token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange oauth code: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("exchange oauth code: unexpected HTTP status %s", resp.Status)
	}

	var result struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode oauth token: %w", err)
	}
	if result.AccessToken == "" {
		return "", errors.New("oauth token response has no access_token")
	}
	return result.AccessToken, nil
}

type githubProfile struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

func (a *Auth) fetchUser(ctx context.Context, token string) (githubProfile, error) {
	endpoint := strings.TrimRight(a.APIURL, "/") + "/user"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return githubProfile{}, fmt.Errorf("build user request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := a.client().Do(req)
	if err != nil {
		return githubProfile{}, fmt.Errorf("fetch github user: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return githubProfile{}, fmt.Errorf("fetch github user: unexpected HTTP status %s", resp.Status)
	}

	var profile githubProfile
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		return githubProfile{}, fmt.Errorf("decode github user: %w", err)
	}
	if profile.ID == 0 || profile.Login == "" {
		return githubProfile{}, errors.New("github user response is missing id or login")
	}
	return profile, nil
}

// Logout deletes the persisted session and clears the browser cookie.
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(SessionCookie); err == nil && cookie.Value != "" {
		_ = a.Store.DeleteSession(r.Context(), cookie.Value)
	}
	http.SetCookie(w, a.sessionCookie("", -1, a.PublicURLFor(r.Context(), r)))
	w.WriteHeader(http.StatusNoContent)
}

// Middleware resolves a valid session cookie into a user on the request
// context. Requests without a valid cookie continue unauthenticated; route
// guards decide whether that is allowed.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Store != nil {
			if cookie, err := r.Cookie(SessionCookie); err == nil && cookie.Value != "" {
				if user, err := a.Store.UserBySessionToken(r.Context(), cookie.Value); err == nil {
					ctx := context.WithValue(r.Context(), userContextKey{}, user)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSONError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

// RequireUser rejects requests that are not signed in.
func RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := UserFrom(r.Context()); !ok {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAdmin rejects anonymous requests with 401 and non-admin users with
// 403.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := UserFrom(r.Context())
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !user.IsAdmin {
			writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		next.ServeHTTP(w, r)
	})
}

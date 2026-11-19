package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/auth"
	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

type fakeAppProvider struct {
	app *ghapp.App
	err error
}

func (p fakeAppProvider) App(context.Context) (*ghapp.App, error) { return p.app, p.err }

// tokenExchange captures what the fake GitHub OAuth server received so the test
// can assert the exact request the callback must send.
type tokenExchange struct {
	mu           sync.Mutex
	method       string
	accept       string
	clientID     string
	clientSecret string
	code         string
	redirectURI  string
	userAuth     string
}

type exchangeSnapshot struct {
	method       string
	accept       string
	clientID     string
	clientSecret string
	code         string
	redirectURI  string
	userAuth     string
}

func (x *tokenExchange) snapshot() exchangeSnapshot {
	x.mu.Lock()
	defer x.mu.Unlock()
	return exchangeSnapshot{
		method:       x.method,
		accept:       x.accept,
		clientID:     x.clientID,
		clientSecret: x.clientSecret,
		code:         x.code,
		redirectURI:  x.redirectURI,
		userAuth:     x.userAuth,
	}
}

func newFakeGitHub(t *testing.T, exchange *tokenExchange) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		exchange.mu.Lock()
		exchange.method = r.Method
		exchange.accept = r.Header.Get("Accept")
		exchange.clientID = r.PostForm.Get("client_id")
		exchange.clientSecret = r.PostForm.Get("client_secret")
		exchange.code = r.PostForm.Get("code")
		exchange.redirectURI = r.PostForm.Get("redirect_uri")
		exchange.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"gho_testtoken","token_type":"bearer","scope":"read:user"}`))
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		exchange.mu.Lock()
		exchange.userAuth = r.Header.Get("Authorization")
		exchange.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":12345,"login":"octocat","avatar_url":"https://avatars.example/octocat.png"}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newAuthForTest(t *testing.T, srv *httptest.Server) (*auth.Auth, fakeAppProvider) {
	t.Helper()

	provider := fakeAppProvider{app: &ghapp.App{Creds: ghapp.AppCredentials{
		ClientID:     "Iv1.testclient",
		ClientSecret: "test-client-secret",
		HTMLURL:      "https://github.test/apps/qa-control-center",
	}}}
	return &auth.Auth{
		Store:     testutil.NewStore(t),
		Apps:      provider,
		GitHubURL: srv.URL,
		APIURL:    srv.URL,
		PublicURL: "http://qa.example.test",
		HTTP:      srv.Client(),
	}, provider
}

func cookieByName(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestOAuthFlow(t *testing.T) {
	exchange := &tokenExchange{}
	srv := newFakeGitHub(t, exchange)
	a, _ := newAuthForTest(t, srv)
	ctx := context.Background()

	// GET /auth/login: state cookie plus a redirect carrying the App's client id.
	loginRec := httptest.NewRecorder()
	a.Login(loginRec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if loginRec.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302 (body %q)", loginRec.Code, loginRec.Body.String())
	}

	stateCookie := cookieByName(loginRec.Result().Cookies(), "qa_oauth_state")
	if stateCookie == nil || stateCookie.Value == "" {
		t.Fatalf("login did not set a non-empty qa_oauth_state cookie: %v", loginRec.Result().Cookies())
	}
	if !stateCookie.HttpOnly || stateCookie.SameSite != http.SameSiteLaxMode || stateCookie.Path != "/" {
		t.Errorf("state cookie attributes = %+v, want HttpOnly, SameSite=Lax, Path=/", stateCookie)
	}
	if stateCookie.MaxAge != 600 {
		t.Errorf("state cookie MaxAge = %d, want 600", stateCookie.MaxAge)
	}

	location, err := url.Parse(loginRec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse login Location %q: %v", loginRec.Header().Get("Location"), err)
	}
	if got, want := location.Scheme+"://"+location.Host+location.Path, srv.URL+"/login/oauth/authorize"; got != want {
		t.Errorf("authorize URL = %q, want %q", got, want)
	}
	q := location.Query()
	if q.Get("client_id") != "Iv1.testclient" {
		t.Errorf("authorize client_id = %q, want Iv1.testclient", q.Get("client_id"))
	}
	if q.Get("state") != stateCookie.Value {
		t.Errorf("authorize state = %q, want %q", q.Get("state"), stateCookie.Value)
	}
	if q.Get("redirect_uri") != "http://qa.example.test/auth/callback" {
		t.Errorf("authorize redirect_uri = %q", q.Get("redirect_uri"))
	}

	// A mismatched state is rejected before any token exchange.
	badReq := httptest.NewRequest(http.MethodGet, "/auth/callback?state=wrong&code=abc123", nil)
	badReq.AddCookie(stateCookie)
	badRec := httptest.NewRecorder()
	a.Callback(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("callback with wrong state = %d, want 400 (body %q)", badRec.Code, badRec.Body.String())
	}
	if c := cookieByName(badRec.Result().Cookies(), auth.SessionCookie); c != nil {
		t.Error("callback with wrong state issued a session cookie")
	}

	// The correct state exchanges the code, creates the first user as admin and
	// issues the session cookie.
	goodReq := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+url.QueryEscape(stateCookie.Value)+"&code=abc123", nil)
	goodReq.AddCookie(stateCookie)
	goodRec := httptest.NewRecorder()
	a.Callback(goodRec, goodReq)
	if goodRec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302 (body %q)", goodRec.Code, goodRec.Body.String())
	}
	if got := goodRec.Header().Get("Location"); got != "/" {
		t.Errorf("callback Location = %q, want /", got)
	}

	session := cookieByName(goodRec.Result().Cookies(), auth.SessionCookie)
	if session == nil || session.Value == "" {
		t.Fatalf("callback did not set a %s cookie: %v", auth.SessionCookie, goodRec.Result().Cookies())
	}
	if !session.HttpOnly || session.SameSite != http.SameSiteLaxMode || session.Path != "/" {
		t.Errorf("session cookie attributes = %+v, want HttpOnly, SameSite=Lax, Path=/", session)
	}
	if session.MaxAge != 7*24*60*60 {
		t.Errorf("session cookie MaxAge = %d, want 604800", session.MaxAge)
	}
	if cleared := cookieByName(goodRec.Result().Cookies(), "qa_oauth_state"); cleared == nil || cleared.MaxAge >= 0 {
		t.Errorf("state cookie was not cleared after callback: %+v", cleared)
	}

	users, err := a.Store.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("users after OAuth = %d, want 1", len(users))
	}
	if users[0].Login != "octocat" || users[0].GitHubID != 12345 || !users[0].IsAdmin {
		t.Errorf("created user = %+v, want octocat/12345/admin", users[0])
	}
	loaded, err := a.Store.UserBySessionToken(ctx, session.Value)
	if err != nil {
		t.Fatalf("session cookie is not a valid session: %v", err)
	}
	if loaded.ID != users[0].ID {
		t.Errorf("session user = %d, want %d", loaded.ID, users[0].ID)
	}

	got := exchange.snapshot()
	if got.method != http.MethodPost {
		t.Errorf("token exchange method = %q, want POST", got.method)
	}
	if got.accept != "application/json" {
		t.Errorf("token exchange Accept = %q, want application/json", got.accept)
	}
	if got.clientID != "Iv1.testclient" || got.clientSecret != "test-client-secret" {
		t.Errorf("token exchange credentials = %q/%q", got.clientID, got.clientSecret)
	}
	if got.code != "abc123" || got.redirectURI != "http://qa.example.test/auth/callback" {
		t.Errorf("token exchange code/redirect = %q/%q", got.code, got.redirectURI)
	}
	if got.userAuth != "Bearer gho_testtoken" {
		t.Errorf("/user Authorization = %q, want Bearer gho_testtoken", got.userAuth)
	}
}

func TestMiddlewareLoadsUser(t *testing.T) {
	exchange := &tokenExchange{}
	srv := newFakeGitHub(t, exchange)
	a, _ := newAuthForTest(t, srv)
	ctx := context.Background()

	user, err := a.Store.UpsertUserFromGitHub(ctx, 42, "lead-one", "https://avatars.example/42")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	token, err := a.Store.CreateSession(ctx, user.ID, 24*time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	var seen bool
	handler := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := auth.UserFrom(r.Context())
		seen = ok
		if ok {
			_, _ = w.Write([]byte(got.Login))
		}
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/any", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: token})
	handler.ServeHTTP(rec, req)
	if !seen || rec.Body.String() != "lead-one" {
		t.Errorf("Middleware user = %q (found=%v), want lead-one", rec.Body.String(), seen)
	}

	// No cookie continues unauthenticated rather than blocking the request.
	seen = true
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/any", nil))
	if seen {
		t.Error("UserFrom reported a user for a request without a session cookie")
	}
}

func TestLogoutDeletesSession(t *testing.T) {
	exchange := &tokenExchange{}
	srv := newFakeGitHub(t, exchange)
	a, _ := newAuthForTest(t, srv)
	ctx := context.Background()

	user, err := a.Store.UpsertUserFromGitHub(ctx, 7, "quitter", "")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	token, err := a.Store.CreateSession(ctx, user.ID, 24*time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: token})
	rec := httptest.NewRecorder()
	a.Logout(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", rec.Code)
	}
	if c := cookieByName(rec.Result().Cookies(), auth.SessionCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("session cookie was not cleared: %+v", c)
	}
	if _, err := a.Store.UserBySessionToken(ctx, token); err == nil {
		t.Error("session is still valid after logout")
	}
}

func TestLoginNotConfigured503(t *testing.T) {
	exchange := &tokenExchange{}
	srv := newFakeGitHub(t, exchange)
	a, _ := newAuthForTest(t, srv)
	a.Apps = fakeAppProvider{err: store.ErrNotConfigured}

	rec := httptest.NewRecorder()
	a.Login(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("login when App is not configured = %d, want 503 (body %q)", rec.Code, rec.Body.String())
	}
}

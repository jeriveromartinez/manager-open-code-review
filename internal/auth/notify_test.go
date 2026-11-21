package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"sync"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/auth"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
)

type recordingPublisher struct {
	mu     sync.Mutex
	events []events.Event
	err    error
}

func (p *recordingPublisher) Publish(_ context.Context, ev events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.events = append(p.events, ev)
	return nil
}

func (p *recordingPublisher) snapshot() []events.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]events.Event(nil), p.events...)
}

// signIn drives the OAuth round trip and returns the callback response.
func signIn(t *testing.T, a *auth.Auth) *httptest.ResponseRecorder {
	t.Helper()
	loginRec := httptest.NewRecorder()
	a.Login(loginRec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	state := cookieByName(loginRec.Result().Cookies(), "qa_oauth_state")
	if state == nil {
		t.Fatal("login did not set qa_oauth_state")
	}
	req := httptest.NewRequest(http.MethodGet,
		"/auth/callback?state="+url.QueryEscape(state.Value)+"&code=abc123", nil)
	req.AddCookie(state)
	rec := httptest.NewRecorder()
	a.Callback(rec, req)
	return rec
}

func TestLoginNotifiesTheUser(t *testing.T) {
	srv := newFakeGitHub(t, &tokenExchange{})
	a, _ := newAuthForTest(t, srv)
	pub := &recordingPublisher{}
	a.Events = pub

	rec := signIn(t, a)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}

	users, err := a.Store.ListUsers(context.Background())
	if err != nil || len(users) != 1 {
		t.Fatalf("ListUsers() = %v, %v; want one user", users, err)
	}
	want := []events.Event{{Type: "user.updated", ID: strconv.FormatUint(uint64(users[0].ID), 10)}}
	if got := pub.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("published events = %+v, want %+v", got, want)
	}
}

func TestLoginSurvivesAFailedNotification(t *testing.T) {
	srv := newFakeGitHub(t, &tokenExchange{})
	a, _ := newAuthForTest(t, srv)
	a.Events = &recordingPublisher{err: errors.New("notify unavailable")}

	rec := signIn(t, a)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}
	if session := cookieByName(rec.Result().Cookies(), auth.SessionCookie); session == nil || session.Value == "" {
		t.Error("a failed notification prevented the session cookie")
	}
}

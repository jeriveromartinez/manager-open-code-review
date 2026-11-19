package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
)

// testAppPEM generates the RSA key ghinstallation needs to sign App JWTs.
func testAppPEM(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

func TestRepositoryReadToken(t *testing.T) {
	expires := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	const wantBody = `{"repositories":["repo"],"permissions":{"contents":"read","metadata":"read"}}`

	var (
		mu        sync.Mutex
		gotMethod string
		gotPath   string
		gotAuth   string
		gotBody   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotMethod, gotPath, gotAuth, gotBody = r.Method, r.URL.Path, r.Header.Get("Authorization"), string(body)
		mu.Unlock()

		if r.URL.Path != "/app/installations/9/access_tokens" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"token":"t","expires_at":%q}`, expires.Format(time.RFC3339))
	}))
	defer srv.Close()

	a := &ghapp.App{
		Creds:  ghapp.AppCredentials{AppID: 12345, PEM: testAppPEM(t)},
		APIURL: srv.URL,
	}
	token, gotExpires, err := a.RepositoryReadToken(context.Background(), 9, "acme/repo")
	if err != nil {
		t.Fatalf("RepositoryReadToken() error = %v", err)
	}
	if token != "t" {
		t.Errorf("token = %q, want %q", token, "t")
	}
	if !gotExpires.Equal(expires) {
		t.Errorf("expires = %v, want %v", gotExpires, expires)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/app/installations/9/access_tokens" {
		t.Errorf("path = %q, want /app/installations/9/access_tokens", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(gotAuth, "Bearer ")) == "" {
		t.Errorf("Authorization = %q, want Bearer <App JWT>", gotAuth)
	}
	if gotBody != wantBody {
		t.Errorf("body = %q, want %q", gotBody, wantBody)
	}
}

func TestRepositoryReadTokenHTTPErrorBoundsBody(t *testing.T) {
	longBody := strings.Repeat("x", 2000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, longBody)
	}))
	defer srv.Close()

	a := &ghapp.App{
		Creds:  ghapp.AppCredentials{AppID: 12345, PEM: testAppPEM(t)},
		APIURL: srv.URL,
	}
	_, _, err := a.RepositoryReadToken(context.Background(), 9, "acme/repo")
	if err == nil {
		t.Fatal("RepositoryReadToken() error = nil, want an HTTP error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %q, want it to include the HTTP status", err)
	}
	if strings.Contains(err.Error(), longBody) {
		t.Error("error includes the full response body")
	}
	if len(err.Error()) > 450 {
		t.Errorf("error length = %d, want the body excerpt bounded to ~300 bytes: %q", len(err.Error()), err)
	}
}

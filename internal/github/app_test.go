package github_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

func newTestBox(t *testing.T) *crypto.Box {
	t.Helper()

	box, err := crypto.NewBox(bytes.Repeat([]byte{0x11}, crypto.KeySize))
	if err != nil {
		t.Fatalf("NewBox() error = %v", err)
	}
	return box
}

func TestProviderRoundTrip(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()
	box := newTestBox(t)
	const apiURL = "https://api.github.com"

	// An empty database has no App configuration yet.
	empty := ghapp.NewStoreAppProvider(s, box, apiURL)
	if _, err := empty.App(ctx); !errors.Is(err, store.ErrNotConfigured) {
		t.Fatalf("App() on empty DB = %v, want ErrNotConfigured", err)
	}

	const privateKey = "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEAexample-private-key\n-----END RSA PRIVATE KEY-----\n"
	creds := ghapp.AppCredentials{
		AppID:         12345,
		Slug:          "qa-control-center",
		HTMLURL:       "https://github.com/apps/qa-control-center",
		ClientID:      "Iv1.abcdef",
		ClientSecret:  "client-secret-value",
		WebhookSecret: "webhook-secret-value",
		PEM:           privateKey,
	}
	if err := ghapp.SaveCredentials(ctx, s, box, creds); err != nil {
		t.Fatalf("SaveCredentials() error = %v", err)
	}

	var row store.AppConfig
	if err := s.DB.WithContext(ctx).Where("id = 1").Take(&row).Error; err != nil {
		t.Fatalf("read saved AppConfig row: %v", err)
	}
	if row.AppID != creds.AppID || row.Slug != creds.Slug || row.HTMLURL != creds.HTMLURL || row.ClientID != creds.ClientID {
		t.Errorf("saved AppConfig public fields = %+v, want credentials %+v", row, creds)
	}
	for name, sealed := range map[string]string{
		"client_secret_enc":  row.ClientSecretEnc,
		"webhook_secret_enc": row.WebhookSecretEnc,
		"private_key_enc":    row.PrivateKeyEnc,
	} {
		if strings.Contains(sealed, privateKey) {
			t.Errorf("%s contains the PEM in clear text: %q", name, sealed)
		}
	}
	if row.PrivateKeyEnc == privateKey {
		t.Error("private_key_enc stored the PEM unencrypted")
	}
	if row.ClientSecretEnc == creds.ClientSecret {
		t.Error("client_secret_enc stored the client secret unencrypted")
	}
	if row.WebhookSecretEnc == creds.WebhookSecret {
		t.Error("webhook_secret_enc stored the webhook secret unencrypted")
	}

	app, err := ghapp.NewStoreAppProvider(s, box, apiURL).App(ctx)
	if err != nil {
		t.Fatalf("App() error = %v", err)
	}
	if app.Creds != creds {
		t.Errorf("App().Creds = %+v, want %+v", app.Creds, creds)
	}
	if app.APIURL != apiURL {
		t.Errorf("App().APIURL = %q, want %q", app.APIURL, apiURL)
	}
}

func TestNewStoreAppProviderCaches(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()
	box := newTestBox(t)

	first := ghapp.AppCredentials{
		AppID:        1,
		Slug:         "first",
		ClientSecret: "first-secret",
		PEM:          "first-pem",
	}
	if err := ghapp.SaveCredentials(ctx, s, box, first); err != nil {
		t.Fatalf("SaveCredentials(first): %v", err)
	}

	provider := ghapp.NewStoreAppProvider(s, box, "https://api.github.com")
	app, err := provider.App(ctx)
	if err != nil {
		t.Fatalf("App() first load: %v", err)
	}
	if app.Creds != first {
		t.Fatalf("App().Creds = %+v, want %+v", app.Creds, first)
	}

	second := ghapp.AppCredentials{
		AppID:        2,
		Slug:         "second",
		ClientSecret: "second-secret",
		PEM:          "second-pem",
	}
	if err := ghapp.SaveCredentials(ctx, s, box, second); err != nil {
		t.Fatalf("SaveCredentials(second): %v", err)
	}

	cached, err := provider.App(ctx)
	if err != nil {
		t.Fatalf("App() second load: %v", err)
	}
	if cached.Creds != first {
		t.Errorf("App() after DB change = %+v, want cached %+v", cached.Creds, first)
	}

	fresh, err := ghapp.NewStoreAppProvider(s, box, "https://api.github.com").App(ctx)
	if err != nil {
		t.Fatalf("App() from fresh provider: %v", err)
	}
	if fresh.Creds != second {
		t.Errorf("fresh provider App().Creds = %+v, want %+v", fresh.Creds, second)
	}
}

func TestInstallURL(t *testing.T) {
	a := &ghapp.App{Creds: ghapp.AppCredentials{HTMLURL: "https://github.com/apps/qa-control-center"}}
	if want := "https://github.com/apps/qa-control-center/installations/new"; a.InstallURL() != want {
		t.Errorf("InstallURL() = %q, want %q", a.InstallURL(), want)
	}
}

func TestInstallationClientUsesAPIURL(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	privateKey := string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))

	var mu sync.Mutex
	tokenRequests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens" {
			mu.Lock()
			tokenRequests++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"token":"ghs_installation","expires_at":%q}`,
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	a := &ghapp.App{
		Creds:  ghapp.AppCredentials{AppID: 12345, PEM: privateKey},
		APIURL: srv.URL,
	}
	client, err := a.InstallationClient(7)
	if err != nil {
		t.Fatalf("InstallationClient() error = %v", err)
	}
	if client == nil {
		t.Fatal("InstallationClient() = nil, want a go-github client")
	}
	if got := client.BaseURL(); got != srv.URL+"/" {
		t.Errorf("client.BaseURL() = %q, want %q", got, srv.URL+"/")
	}
	if got := client.UploadURL(); got != srv.URL+"/" {
		t.Errorf("client.UploadURL() = %q, want %q", got, srv.URL+"/")
	}

	// Triggering any API call makes ghinstallation request an installation token.
	resp, err := client.Client().Get(srv.URL + "/ping")
	if err != nil {
		t.Fatalf("request via installation client: %v", err)
	}
	defer resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if tokenRequests == 0 {
		t.Error("fake server did not receive POST /app/installations/7/access_tokens")
	}
}

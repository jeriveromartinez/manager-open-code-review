package jobs_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/jobs"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/syncer"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

type stubAppProvider struct {
	app *ghapp.App
}

func (p stubAppProvider) App(context.Context) (*ghapp.App, error) {
	return p.app, nil
}

func testPEM(t *testing.T) string {
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

// TestControlClientRunsSyncJob verifies the whole path from EnqueueSync through
// River to the real Syncer: the fake GitHub records the installation ID that
// reached its token endpoint.
func TestControlClientRunsSyncJob(t *testing.T) {
	s := testutil.NewStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan int64, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			raw := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				http.Error(w, "bad installation id", http.StatusBadRequest)
				return
			}
			received <- id
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"token":"ghs_test","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case r.URL.Path == "/installation/repositories":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"total_count":0,"repositories":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	sy := &syncer.Syncer{
		Store: s,
		Apps: stubAppProvider{app: &ghapp.App{
			Creds:  ghapp.AppCredentials{AppID: 12345, PEM: testPEM(t)},
			APIURL: srv.URL,
		}},
	}
	c, err := jobs.NewControlClient(s.Pool, sy, time.Hour)
	if err != nil {
		t.Fatalf("NewControlClient() error = %v", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := c.Stop(stopCtx); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	}()

	if err := s.UpsertInstallation(ctx, store.Installation{ID: 7, AccountLogin: "acme", AccountType: "Organization"}); err != nil {
		t.Fatalf("UpsertInstallation(7): %v", err)
	}

	if err := jobs.NewEnqueuer(c).EnqueueSync(ctx, 7); err != nil {
		t.Fatalf("EnqueueSync(7) error = %v", err)
	}

	select {
	case id := <-received:
		if id != 7 {
			t.Fatalf("installation ID received by fake GitHub = %d, want 7", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sync job did not reach fake GitHub within 5 seconds")
	}
}

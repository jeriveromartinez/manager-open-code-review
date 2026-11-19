package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	ghinstallation "github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v93/github"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// SaveCredentials encrypts the client secret, webhook secret and private key
// with box and writes them into the single AppConfig row.
func SaveCredentials(ctx context.Context, st *store.Store, box *crypto.Box, c AppCredentials) error {
	clientSecret, err := box.Seal([]byte(c.ClientSecret))
	if err != nil {
		return fmt.Errorf("encrypt client secret: %w", err)
	}
	webhookSecret, err := box.Seal([]byte(c.WebhookSecret))
	if err != nil {
		return fmt.Errorf("encrypt webhook secret: %w", err)
	}
	privateKey, err := box.Seal([]byte(c.PEM))
	if err != nil {
		return fmt.Errorf("encrypt private key: %w", err)
	}

	return st.SaveAppConfig(ctx, store.AppConfig{
		AppID:            c.AppID,
		Slug:             c.Slug,
		HTMLURL:          c.HTMLURL,
		ClientID:         c.ClientID,
		ClientSecretEnc:  clientSecret,
		WebhookSecretEnc: webhookSecret,
		PrivateKeyEnc:    privateKey,
	})
}

// App is a configured GitHub App: its credentials plus the API endpoint used
// for installation tokens and REST calls.
type App struct {
	Creds  AppCredentials
	APIURL string
}

// InstallationFromGitHub converts a go-github installation into a store row.
func InstallationFromGitHub(inst *gh.Installation) store.Installation {
	row := store.Installation{
		ID:           inst.GetID(),
		AccountLogin: inst.GetAccount().GetLogin(),
		AccountType:  inst.GetAccount().GetType(),
	}
	if inst.SuspendedAt != nil {
		suspendedAt := inst.SuspendedAt.Time
		row.SuspendedAt = &suspendedAt
	}
	return row
}

// AppClient returns a go-github client authenticated as the App itself (JWT).
// APIURL is honored both by ghinstallation (App endpoints) and by the REST
// client, which is required for GitHub Enterprise and test servers.
func (a *App) AppClient() (*gh.Client, error) {
	appTR, err := ghinstallation.NewAppsTransport(http.DefaultTransport, a.Creds.AppID, []byte(a.Creds.PEM))
	if err != nil {
		return nil, fmt.Errorf("create GitHub App transport: %w", err)
	}
	appTR.BaseURL = a.APIURL

	base := strings.TrimRight(a.APIURL, "/") + "/"
	client, err := gh.NewClient(
		gh.WithHTTPClient(&http.Client{Transport: appTR}),
		gh.WithURLs(&base, &base),
	)
	if err != nil {
		return nil, fmt.Errorf("create GitHub App client: %w", err)
	}
	return client, nil
}

// InstallationClient returns a go-github client authenticated as the given
// installation. APIURL is honored both by ghinstallation (token endpoint) and
// by the REST client, which is required for GitHub Enterprise and test servers.
func (a *App) InstallationClient(installationID int64) (*gh.Client, error) {
	appTR, err := ghinstallation.NewAppsTransport(http.DefaultTransport, a.Creds.AppID, []byte(a.Creds.PEM))
	if err != nil {
		return nil, fmt.Errorf("create GitHub App transport: %w", err)
	}
	appTR.BaseURL = a.APIURL

	tr := ghinstallation.NewFromAppsTransport(appTR, installationID)
	tr.BaseURL = a.APIURL

	base := strings.TrimRight(a.APIURL, "/") + "/"
	client, err := gh.NewClient(
		gh.WithHTTPClient(&http.Client{Transport: tr}),
		gh.WithURLs(&base, &base),
	)
	if err != nil {
		return nil, fmt.Errorf("create GitHub client: %w", err)
	}
	return client, nil
}

// InstallURL is the URL where an administrator installs this GitHub App.
func (a *App) InstallURL() string {
	return a.Creds.HTMLURL + "/installations/new"
}

// AppProvider exposes the configured App. It returns store.ErrNotConfigured
// when no credentials have been saved yet.
type AppProvider interface {
	App(ctx context.Context) (*App, error)
}

// SyncEnqueuer is the minimal interface needed to schedule a synchronization.
// It lives here so internal/github does not have to import internal/jobs.
type SyncEnqueuer interface {
	EnqueueSync(ctx context.Context, installationID int64) error
}

type storeAppProvider struct {
	st     *store.Store
	box    *crypto.Box
	apiURL string

	mu  sync.Mutex
	app *App
}

// NewStoreAppProvider returns a provider that decrypts the saved credentials on
// the first successful load and caches the resulting App afterwards.
func NewStoreAppProvider(st *store.Store, box *crypto.Box, apiURL string) AppProvider {
	return &storeAppProvider{st: st, box: box, apiURL: apiURL}
}

func (p *storeAppProvider) App(ctx context.Context) (*App, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.app != nil {
		return p.app, nil
	}

	cfg, err := p.st.LoadAppConfig(ctx)
	if err != nil {
		return nil, err
	}

	clientSecret, err := p.box.Open(cfg.ClientSecretEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypt client secret: %w", err)
	}
	webhookSecret, err := p.box.Open(cfg.WebhookSecretEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypt webhook secret: %w", err)
	}
	privateKey, err := p.box.Open(cfg.PrivateKeyEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypt private key: %w", err)
	}

	p.app = &App{
		Creds: AppCredentials{
			AppID:         cfg.AppID,
			Slug:          cfg.Slug,
			HTMLURL:       cfg.HTMLURL,
			ClientID:      cfg.ClientID,
			ClientSecret:  string(clientSecret),
			WebhookSecret: string(webhookSecret),
			PEM:           string(privateKey),
		},
		APIURL: p.apiURL,
	}
	return p.app, nil
}

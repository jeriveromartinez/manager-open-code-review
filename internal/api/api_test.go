package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/api"
	"github.com/jeriveromartinez/qa-control-center/internal/auth"
	"github.com/jeriveromartinez/qa-control-center/internal/config"
	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

type recordingJobs struct {
	mu  sync.Mutex
	ids []int64
}

func (j *recordingJobs) EnqueueSync(_ context.Context, id int64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.ids = append(j.ids, id)
	return nil
}

func (j *recordingJobs) snapshot() []int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]int64(nil), j.ids...)
}

type recordingPublisher struct {
	mu     sync.Mutex
	events []events.Event
}

func (p *recordingPublisher) Publish(_ context.Context, ev events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
	return nil
}

func (p *recordingPublisher) snapshot() []events.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]events.Event(nil), p.events...)
}

type apiFixture struct {
	t      *testing.T
	ctx    context.Context
	st     *store.Store
	box    *crypto.Box
	cfg    config.Config
	router http.Handler
	jobs   *recordingJobs
	pub    *recordingPublisher
	seq    int64
}

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()

	st := testutil.NewStore(t)
	box, err := crypto.NewBox(bytes.Repeat([]byte{0x22}, crypto.KeySize))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	cfg := config.Config{
		GitHubURL:       "https://github.example",
		GitHubAPIURL:    "https://api.github.example",
		PublicURL:       "https://qa.example",
		WebhooksEnabled: true,
	}
	jobs := &recordingJobs{}
	pub := &recordingPublisher{}
	provider := ghapp.NewStoreAppProvider(st, box, cfg.GitHubAPIURL)
	a := &auth.Auth{
		Store:     st,
		Apps:      provider,
		GitHubURL: cfg.GitHubURL,
		APIURL:    cfg.GitHubAPIURL,
		PublicURL: cfg.PublicURL,
		HTTP:      http.DefaultClient,
	}
	router := api.NewRouter(api.Deps{
		Cfg:      cfg,
		Store:    st,
		Box:      box,
		Apps:     provider,
		Auth:     a,
		Webhooks: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
		Broker:   events.NewBroker(st.Pool),
		Events:   pub,
		Jobs:     jobs,
		Static: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("spa"))
		}),
	})
	return &apiFixture{t: t, ctx: context.Background(), st: st, box: box, cfg: cfg, router: router, jobs: jobs, pub: pub}
}

func (f *apiFixture) nextID() int64 {
	f.seq++
	return f.seq
}

func (f *apiFixture) user(login string, admin bool) store.User {
	f.t.Helper()
	u := store.User{GitHubID: f.nextID(), Login: login, AvatarURL: "https://avatars.example/" + login, IsAdmin: admin}
	if err := f.st.DB.WithContext(f.ctx).Create(&u).Error; err != nil {
		f.t.Fatalf("seed user %q: %v", login, err)
	}
	return u
}

func (f *apiFixture) team(name string) store.Team {
	f.t.Helper()
	tm := store.Team{Name: name, Slug: strings.ToLower(name)}
	if err := f.st.DB.WithContext(f.ctx).Create(&tm).Error; err != nil {
		f.t.Fatalf("seed team %q: %v", name, err)
	}
	return tm
}

func (f *apiFixture) member(teamID, userID uint, role string) {
	f.t.Helper()
	m := store.TeamMember{TeamID: teamID, UserID: userID, Role: role}
	if err := f.st.DB.WithContext(f.ctx).Create(&m).Error; err != nil {
		f.t.Fatalf("seed team member: %v", err)
	}
}

func (f *apiFixture) installation(id int64) store.Installation {
	f.t.Helper()
	in := store.Installation{ID: id, AccountLogin: "acme", AccountType: "Organization"}
	if err := f.st.UpsertInstallation(f.ctx, in); err != nil {
		f.t.Fatalf("seed installation: %v", err)
	}
	return in
}

func (f *apiFixture) repo(id, installationID int64, fullName string, teamID *uint, enabled bool) store.Repository {
	f.t.Helper()
	r := store.Repository{
		ID: id, InstallationID: installationID, FullName: fullName,
		DefaultBranch: "main", Enabled: enabled, TeamID: teamID,
	}
	if err := f.st.UpsertRepository(f.ctx, r); err != nil {
		f.t.Fatalf("seed repository %q: %v", fullName, err)
	}
	return r
}

func (f *apiFixture) pr(repoID int64, number int, status string) store.PullRequest {
	f.t.Helper()
	pr := store.PullRequest{
		RepositoryID: repoID, Number: number,
		Title: fmt.Sprintf("PR %d", number), AuthorLogin: "octocat",
		HTMLURL: fmt.Sprintf("https://github.example/acme/pull/%d", number),
		State:   "open", QAStatus: status,
		UpdatedAt:       time.Now().UTC().Add(time.Duration(number) * time.Second),
		GitHubUpdatedAt: time.Now().UTC(),
	}
	if err := f.st.DB.WithContext(f.ctx).Create(&pr).Error; err != nil {
		f.t.Fatalf("seed pull request: %v", err)
	}
	return pr
}

func (f *apiFixture) session(user store.User) *http.Cookie {
	f.t.Helper()
	token, err := f.st.CreateSession(f.ctx, user.ID, time.Hour)
	if err != nil {
		f.t.Fatalf("create session: %v", err)
	}
	return &http.Cookie{Name: auth.SessionCookie, Value: token}
}

func (f *apiFixture) request(method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	f.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			f.t.Fatalf("encode request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func assertStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, want, rec.Body.String())
	}
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, dest any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dest); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
}

func assertKeys(t *testing.T, m map[string]any, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, ok := m[key]; !ok {
			t.Errorf("response is missing key %q: %v", key, m)
		}
	}
}

func TestHealthz(t *testing.T) {
	f := newAPIFixture(t)
	rec := f.request(http.MethodGet, "/healthz", nil, nil)
	assertStatus(t, rec, http.StatusOK)

	var body map[string]string
	decodeJSON(t, rec, &body)
	if body["status"] != "ok" {
		t.Errorf("healthz body = %v, want {\"status\":\"ok\"}", body)
	}
}

func TestUnauthenticated401(t *testing.T) {
	f := newAPIFixture(t)
	for _, path := range []string{"/api/me", "/api/prs", "/api/events"} {
		t.Run(path, func(t *testing.T) {
			rec := f.request(http.MethodGet, path, nil, nil)
			assertStatus(t, rec, http.StatusUnauthorized)
			var body map[string]string
			decodeJSON(t, rec, &body)
			if body["error"] != "unauthorized" {
				t.Errorf("body = %v, want {\"error\":\"unauthorized\"}", body)
			}
		})
	}
}

func TestDeveloperCannotPatchRepo(t *testing.T) {
	f := newAPIFixture(t)
	dev := f.user("dev", false)
	f.installation(991)
	f.repo(501, 991, "acme/app", nil, false)

	rec := f.request(http.MethodPatch, "/api/repos/501", map[string]any{"enabled": true}, f.session(dev))
	assertStatus(t, rec, http.StatusForbidden)

	var repo store.Repository
	if err := f.st.DB.WithContext(f.ctx).Take(&repo, 501).Error; err != nil {
		t.Fatalf("reload repository: %v", err)
	}
	if repo.Enabled {
		t.Error("developer was able to enable the repository")
	}
}

func TestLeadManagesOwnTeamOnly(t *testing.T) {
	f := newAPIFixture(t)
	lead := f.user("lead", false)
	target := f.user("target", false)
	teamOne := f.team("team-one")
	teamTwo := f.team("team-two")
	f.member(teamOne.ID, lead.ID, "lead")
	cookie := f.session(lead)

	ownPath := fmt.Sprintf("/api/teams/%d/members/%d", teamOne.ID, target.ID)
	rec := f.request(http.MethodPut, ownPath, map[string]string{"role": "qa"}, cookie)
	assertStatus(t, rec, http.StatusNoContent)
	if role, err := f.st.TeamRole(f.ctx, target.ID, teamOne.ID); err != nil || role != "qa" {
		t.Fatalf("role in own team = %q, err = %v, want qa", role, err)
	}

	otherPath := fmt.Sprintf("/api/teams/%d/members/%d", teamTwo.ID, target.ID)
	rec = f.request(http.MethodPut, otherPath, map[string]string{"role": "qa"}, cookie)
	assertStatus(t, rec, http.StatusForbidden)
	if role, err := f.st.TeamRole(f.ctx, target.ID, teamTwo.ID); err != nil || role != "" {
		t.Fatalf("role in other team = %q, err = %v, want none", role, err)
	}
}

func TestSetupAsksOnlyForTheAppName(t *testing.T) {
	f := newSecureFixture(t)

	rec := f.request(http.MethodPost, "/api/setup/manifest", map[string]any{}, nil)
	assertStatus(t, rec, http.StatusBadRequest)

	rec = f.request(http.MethodPost, "/api/setup/manifest", map[string]any{"name": "Acme QA"}, nil)
	assertStatus(t, rec, http.StatusOK)

	var body struct {
		ActionURL string `json:"action_url"`
		Manifest  struct {
			Name string `json:"name"`
		} `json:"manifest"`
	}
	decodeJSON(t, rec, &body)
	if body.Manifest.Name != "Acme QA" {
		t.Errorf("manifest name = %q, want Acme QA", body.Manifest.Name)
	}
	if body.ActionURL != "https://github.example/settings/apps/new" {
		t.Errorf("action_url = %q, want the personal account URL", body.ActionURL)
	}

	// El fixture ya trae public_url: el setup no debe reescribir la fila.
	stored, err := f.st.Settings(f.ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if stored["public_url"] != "" {
		t.Errorf("public_url = %q, want nothing persisted when the config already knows it", stored["public_url"])
	}
}

func TestSetupPersistsTheDetectedPublicURL(t *testing.T) {
	f := newSecureFixture(t)
	// El fixture fija una URL pública; se vacía para comprobar que el setup la
	// deduce de la petición, que es el caso de una instalación existente.
	f.deps.Cfg.PublicURL = ""
	f.rebuildRouter()

	req := httptest.NewRequest(http.MethodPost, "/api/setup/manifest",
		strings.NewReader(`{"name":"Acme QA"}`))
	req.Host = "qa.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	assertStatus(t, rec, http.StatusOK)

	var body struct {
		Manifest struct {
			URL          string   `json:"url"`
			RedirectURL  string   `json:"redirect_url"`
			CallbackURLs []string `json:"callback_urls"`
			SetupURL     string   `json:"setup_url"`
		} `json:"manifest"`
	}
	decodeJSON(t, rec, &body)
	if body.Manifest.URL != "https://qa.example.com" {
		t.Errorf("manifest url = %q, want https://qa.example.com", body.Manifest.URL)
	}
	if body.Manifest.RedirectURL != "https://qa.example.com/setup/callback" {
		t.Errorf("manifest redirect_url = %q, want the detected URL", body.Manifest.RedirectURL)
	}
	if len(body.Manifest.CallbackURLs) != 1 || body.Manifest.CallbackURLs[0] != "https://qa.example.com/auth/callback" {
		t.Errorf("manifest callback_urls = %v, want [https://qa.example.com/auth/callback]", body.Manifest.CallbackURLs)
	}
	if body.Manifest.SetupURL != "https://qa.example.com/setup/installed" {
		t.Errorf("manifest setup_url = %q, want the detected URL", body.Manifest.SetupURL)
	}

	stored, err := f.st.Settings(f.ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if stored["public_url"] != "https://qa.example.com" {
		t.Errorf("public_url = %q, want https://qa.example.com", stored["public_url"])
	}
}

func TestSetupTreatsAWhitespaceOnlyPublicURLAsUnset(t *testing.T) {
	f := newSecureFixture(t)
	f.deps.Cfg.PublicURL = "   "
	f.rebuildRouter()

	req := httptest.NewRequest(http.MethodPost, "/api/setup/manifest",
		strings.NewReader(`{"name":"Acme QA"}`))
	req.Host = "qa.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	assertStatus(t, rec, http.StatusOK)

	stored, err := f.st.Settings(f.ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if stored["public_url"] != "https://qa.example.com" {
		t.Errorf("public_url = %q, want the detected URL: a blank value is unset", stored["public_url"])
	}
}

func TestSetupWithoutAnyPublicURLIsRejected(t *testing.T) {
	f := newSecureFixture(t)
	f.deps.Cfg.PublicURL = ""
	f.rebuildRouter()

	req := httptest.NewRequest(http.MethodPost, "/api/setup/manifest",
		strings.NewReader(`{"name":"Acme QA"}`))
	req.Host = ""
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	assertStatus(t, rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "cannot determine the public URL") {
		t.Errorf("body = %q, want the public URL error", rec.Body.String())
	}
}

func TestSetupTokenIsEnforcedOnlyWhenConfigured(t *testing.T) {
	f := newSecureFixture(t)
	f.deps.SetupToken = "s3cret"
	f.rebuildRouter()

	rec := f.request(http.MethodPost, "/api/setup/manifest", map[string]any{"name": "Acme QA"}, nil)
	assertStatus(t, rec, http.StatusForbidden)
	rec = f.request(http.MethodPost, "/api/setup/manifest", map[string]any{"name": "Acme QA", "setup_token": "s3cret"}, nil)
	assertStatus(t, rec, http.StatusOK)
}

// TestSetupIsClosedOnceConfigured keeps the window shut: creating the App must
// stop being possible as soon as credentials exist, token or no token.
func TestSetupIsClosedOnceConfigured(t *testing.T) {
	f := newSecureFixture(t)

	err := ghapp.SaveCredentials(f.ctx, f.st, f.box, ghapp.AppCredentials{
		AppID: 1, Slug: "qa-control-center",
		HTMLURL:  "https://github.example/apps/qa-control-center",
		ClientID: "Iv1.setup", ClientSecret: "secret", WebhookSecret: "hook", PEM: "-----BEGIN RSA PRIVATE KEY-----",
	})
	if err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}

	rec := f.request(http.MethodPost, "/api/setup/manifest", map[string]any{"name": "Acme QA"}, nil)
	assertStatus(t, rec, http.StatusConflict)

	rec = f.request(http.MethodGet, "/setup/callback?code=abc", nil, nil)
	assertStatus(t, rec, http.StatusConflict)
}

func TestSetupStatus(t *testing.T) {
	f := newAPIFixture(t)

	rec := f.request(http.MethodGet, "/api/setup/status", nil, nil)
	assertStatus(t, rec, http.StatusOK)
	var status struct {
		Configured    bool   `json:"configured"`
		InstallURL    string `json:"install_url"`
		TokenRequired bool   `json:"token_required"`
	}
	decodeJSON(t, rec, &status)
	if status.Configured || status.InstallURL != "" || status.TokenRequired {
		t.Errorf("initial setup status = %+v, want false/\"\"/false", status)
	}

	if err := ghapp.SaveCredentials(f.ctx, f.st, f.box, ghapp.AppCredentials{
		AppID: 1, Slug: "qa-control-center",
		HTMLURL:  "https://github.example/apps/qa-control-center",
		ClientID: "Iv1.setup", ClientSecret: "secret", WebhookSecret: "hook", PEM: "pem",
	}); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}

	rec = f.request(http.MethodGet, "/api/setup/status", nil, nil)
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &status)
	if !status.Configured {
		t.Error("setup status reports configured=false after credentials were saved")
	}
	if want := "https://github.example/apps/qa-control-center/installations/new"; status.InstallURL != want {
		t.Errorf("install_url = %q, want %q", status.InstallURL, want)
	}
}

func TestSetupStatusReportsWhetherTheTokenIsRequired(t *testing.T) {
	f := newSecureFixture(t)
	f.deps.SetupToken = "s3cret"
	f.rebuildRouter()

	rec := f.request(http.MethodGet, "/api/setup/status", nil, nil)
	assertStatus(t, rec, http.StatusOK)
	var status struct {
		TokenRequired bool `json:"token_required"`
	}
	decodeJSON(t, rec, &status)
	if !status.TokenRequired {
		t.Error("token_required = false, want true when the operator configured a setup token")
	}
}

func TestSetupInstalledEnqueuesSync(t *testing.T) {
	f := newAPIFixture(t)
	rec := f.request(http.MethodGet, "/setup/installed?installation_id=4242", nil, nil)
	assertStatus(t, rec, http.StatusFound)
	if got := rec.Header().Get("Location"); got != "/" {
		t.Errorf("Location = %q, want /", got)
	}
	if ids := f.jobs.snapshot(); len(ids) != 1 || ids[0] != 4242 {
		t.Errorf("EnqueueSync calls = %v, want [4242]", ids)
	}
}

func TestSetupInstalledRequestPendingApproval(t *testing.T) {
	f := newAPIFixture(t)
	rec := f.request(http.MethodGet, "/setup/installed?setup_action=request", nil, nil)
	assertStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "pendiente de aprobación") ||
		!strings.Contains(body, "administrador de la organización") ||
		!strings.Contains(body, "aprobada") {
		t.Errorf("body = %q, want pending approval explanation in Spanish", body)
	}
	if !strings.Contains(body, `href="/"`) || !strings.Contains(body, "Volver al panel") {
		t.Errorf("body = %q, want a link back to the panel", body)
	}
	if ids := f.jobs.snapshot(); len(ids) != 0 {
		t.Errorf("EnqueueSync calls = %v, want none", ids)
	}
}

func TestSetupInstalledUpdateEnqueuesSync(t *testing.T) {
	f := newAPIFixture(t)
	rec := f.request(http.MethodGet, "/setup/installed?setup_action=update&installation_id=4242", nil, nil)
	assertStatus(t, rec, http.StatusFound)
	if got := rec.Header().Get("Location"); got != "/" {
		t.Errorf("Location = %q, want /", got)
	}
	if ids := f.jobs.snapshot(); len(ids) != 1 || ids[0] != 4242 {
		t.Errorf("EnqueueSync calls = %v, want [4242]", ids)
	}
}

func TestSetupInstalledWithoutInstallationID(t *testing.T) {
	f := newAPIFixture(t)
	rec := f.request(http.MethodGet, "/setup/installed", nil, nil)
	assertStatus(t, rec, http.StatusBadRequest)
	if ids := f.jobs.snapshot(); len(ids) != 0 {
		t.Errorf("EnqueueSync calls = %v, want none", ids)
	}
}

func TestPRsEndpointFiltersByTeam(t *testing.T) {
	f := newAPIFixture(t)
	inst := f.installation(1009)
	teamOne := f.team("team-one")
	teamTwo := f.team("team-two")

	repoA := f.repo(5701, inst.ID, "acme/a", &teamOne.ID, true)
	repoB := f.repo(5702, inst.ID, "acme/b", &teamTwo.ID, true)
	repoC := f.repo(5703, inst.ID, "acme/c", &teamOne.ID, false)

	f.pr(repoA.ID, 1, store.StatusPending)
	f.pr(repoB.ID, 1, store.StatusChangesRequested)
	f.pr(repoC.ID, 1, store.StatusPending)

	developer := f.user("dev-one", false)
	f.member(teamOne.ID, developer.ID, "developer")
	admin := f.user("admin", true)
	outsider := f.user("outsider", false)

	type prDTO struct {
		Repo        string `json:"repo"`
		Validations struct {
			Passed int `json:"passed"`
			Total  int `json:"total"`
		} `json:"validations"`
		UpdatedAt string `json:"updated_at"`
	}
	reposOf := func(rec *httptest.ResponseRecorder) []string {
		var views []prDTO
		decodeJSON(t, rec, &views)
		repos := make([]string, 0, len(views))
		for _, v := range views {
			repos = append(repos, v.Repo)
		}
		return repos
	}
	contains := func(got []string, want string) bool {
		for _, s := range got {
			if s == want {
				return true
			}
		}
		return false
	}

	rec := f.request(http.MethodGet, "/api/prs", nil, f.session(developer))
	assertStatus(t, rec, http.StatusOK)
	got := reposOf(rec)
	if len(got) != 1 || got[0] != "acme/a" {
		t.Errorf("developer sees %v, want [acme/a]", got)
	}

	var raw []map[string]any
	decodeJSON(t, f.request(http.MethodGet, "/api/prs", nil, f.session(developer)), &raw)
	if len(raw) != 1 {
		t.Fatalf("raw PR response length = %d, want 1", len(raw))
	}
	assertKeys(t, raw[0], "id", "repo", "number", "title", "author_login", "html_url",
		"qa_status", "group", "validations", "updated_at")
	validations, ok := raw[0]["validations"].(map[string]any)
	if !ok || validations["passed"] != float64(0) || validations["total"] != float64(0) {
		t.Errorf("validations = %v, want 0/0", raw[0]["validations"])
	}
	updatedAt, ok := raw[0]["updated_at"].(string)
	if !ok {
		t.Fatalf("updated_at = %v, want a string", raw[0]["updated_at"])
	}
	if _, err := time.Parse(time.RFC3339, updatedAt); err != nil {
		t.Errorf("updated_at %q is not RFC3339: %v", updatedAt, err)
	}

	rec = f.request(http.MethodGet, "/api/prs", nil, f.session(admin))
	assertStatus(t, rec, http.StatusOK)
	got = reposOf(rec)
	if len(got) != 2 || !contains(got, "acme/a") || !contains(got, "acme/b") {
		t.Errorf("admin sees %v, want [acme/a acme/b]", got)
	}

	rec = f.request(http.MethodGet, "/api/prs?group=blocked", nil, f.session(admin))
	assertStatus(t, rec, http.StatusOK)
	got = reposOf(rec)
	if len(got) != 1 || got[0] != "acme/b" {
		t.Errorf("admin filtered by blocked sees %v, want [acme/b]", got)
	}

	rec = f.request(http.MethodGet, "/api/prs", nil, f.session(outsider))
	assertStatus(t, rec, http.StatusOK)
	got = reposOf(rec)
	if len(got) != 0 {
		t.Errorf("outsider sees %v, want nothing", got)
	}
}

func TestPRsEndpointRendersStoredValidations(t *testing.T) {
	f := newAPIFixture(t)
	inst := f.installation(1016)
	team := f.team("ci-team")
	repo := f.repo(6201, inst.ID, "acme/ci", &team.ID, true)
	pr := f.pr(repo.ID, 1, store.StatusPending)
	admin := f.user("admin", true)

	if err := f.st.DB.WithContext(f.ctx).Model(&store.PullRequest{}).
		Where("id = ?", pr.ID).Update("head_sha", "sha-api").Error; err != nil {
		t.Fatalf("set head sha: %v", err)
	}
	if _, err := f.st.SetPRValidations(f.ctx, pr.ID, "sha-api", store.PRValidations{Passed: 9, Total: 9}); err != nil {
		t.Fatalf("SetPRValidations: %v", err)
	}

	rec := f.request(http.MethodGet, "/api/prs", nil, f.session(admin))
	assertStatus(t, rec, http.StatusOK)

	var views []map[string]any
	decodeJSON(t, rec, &views)
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	validations, ok := views[0]["validations"].(map[string]any)
	if !ok {
		t.Fatalf("validations = %v, want an object", views[0]["validations"])
	}
	if validations["passed"] != float64(9) || validations["total"] != float64(9) {
		t.Errorf("validations = %v, want 9/9", validations)
	}

	rec = f.request(http.MethodGet, fmt.Sprintf("/api/prs/%d", pr.ID), nil, f.session(admin))
	assertStatus(t, rec, http.StatusOK)
	var detail map[string]any
	decodeJSON(t, rec, &detail)
	validations, ok = detail["validations"].(map[string]any)
	if !ok || validations["passed"] != float64(9) || validations["total"] != float64(9) {
		t.Errorf("detail validations = %v, want 9/9", detail["validations"])
	}
}

func TestPatchRepoEnableEnqueuesSync(t *testing.T) {
	f := newAPIFixture(t)
	f.installation(991)
	f.repo(501, 991, "acme/backend-api", nil, false)
	admin := f.user("admin", true)

	rec := f.request(http.MethodPatch, "/api/repos/501", map[string]any{"enabled": true}, f.session(admin))
	assertStatus(t, rec, http.StatusNoContent)

	if ids := f.jobs.snapshot(); len(ids) != 1 || ids[0] != 991 {
		t.Errorf("EnqueueSync calls = %v, want [991]", ids)
	}
	var repo store.Repository
	if err := f.st.DB.WithContext(f.ctx).Take(&repo, 501).Error; err != nil {
		t.Fatalf("reload repository: %v", err)
	}
	if !repo.Enabled {
		t.Error("repository was not enabled")
	}

	var audits []store.AuditLog
	if err := f.st.DB.WithContext(f.ctx).Where("action = ?", "repo.updated").Find(&audits).Error; err != nil {
		t.Fatalf("query audit log: %v", err)
	}
	if len(audits) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(audits))
	}
	if audits[0].ActorUserID == nil || *audits[0].ActorUserID != admin.ID {
		t.Errorf("audit actor = %v, want %d", audits[0].ActorUserID, admin.ID)
	}
	if audits[0].TargetType != "repo" || audits[0].TargetID != "501" {
		t.Errorf("audit target = %s/%s, want repo/501", audits[0].TargetType, audits[0].TargetID)
	}

	published := f.pub.snapshot()
	if len(published) != 1 || published[0] != (events.Event{Type: "repo.updated", ID: "501"}) {
		t.Errorf("published events = %v, want [repo.updated 501]", published)
	}
}

func TestPatchRepoExplicitFields(t *testing.T) {
	f := newAPIFixture(t)
	team := f.team("team-one")
	f.installation(991)
	f.repo(501, 991, "acme/api", &team.ID, true)
	admin := f.user("admin", true)
	cookie := f.session(admin)

	rec := f.request(http.MethodPatch, "/api/repos/501", map[string]any{"team_id": nil}, cookie)
	assertStatus(t, rec, http.StatusNoContent)
	var repo store.Repository
	if err := f.st.DB.WithContext(f.ctx).Take(&repo, 501).Error; err != nil {
		t.Fatalf("reload repository: %v", err)
	}
	if repo.TeamID != nil {
		t.Errorf("team_id = %v, want nil after explicit null", *repo.TeamID)
	}
	if !repo.Enabled {
		t.Error("absent enabled key must not change the repository")
	}

	rec = f.request(http.MethodPatch, "/api/repos/501", map[string]any{"team_id": team.ID}, cookie)
	assertStatus(t, rec, http.StatusNoContent)
	if err := f.st.DB.WithContext(f.ctx).Take(&repo, 501).Error; err != nil {
		t.Fatalf("reload repository: %v", err)
	}
	if repo.TeamID == nil || *repo.TeamID != team.ID {
		t.Error("team_id was not reassigned")
	}
}

func TestMeIncludesTeams(t *testing.T) {
	f := newAPIFixture(t)
	lead := f.user("lead", false)
	team := f.team("team-one")
	f.member(team.ID, lead.ID, "lead")

	rec := f.request(http.MethodGet, "/api/me", nil, f.session(lead))
	assertStatus(t, rec, http.StatusOK)
	var me struct {
		ID        uint   `json:"id"`
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
		IsAdmin   bool   `json:"is_admin"`
		Teams     []struct {
			ID   uint   `json:"id"`
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"teams"`
	}
	decodeJSON(t, rec, &me)
	if me.ID != lead.ID || me.Login != "lead" || me.AvatarURL == "" || me.IsAdmin {
		t.Errorf("/api/me identity = %+v", me)
	}
	if len(me.Teams) != 1 || me.Teams[0].ID != team.ID || me.Teams[0].Name != "team-one" || me.Teams[0].Role != "lead" {
		t.Errorf("/api/me teams = %+v", me.Teams)
	}
}

func TestUsersAllowsNonAdminLead(t *testing.T) {
	f := newAPIFixture(t)
	lead := f.user("lead", false)
	team := f.team("team-one")
	f.member(team.ID, lead.ID, "lead")
	f.user("dev", false)

	rec := f.request(http.MethodGet, "/api/users", nil, f.session(lead))
	assertStatus(t, rec, http.StatusOK)

	var body []map[string]any
	decodeJSON(t, rec, &body)
	if len(body) != 2 {
		t.Fatalf("users = %v, want two", body)
	}
	// Any signed-in user may list users, but only minimal profile fields.
	if _, ok := body[0]["github_id"]; ok {
		t.Errorf("users response leaks github_id: %v", body[0])
	}
	if len(body[0]) != 4 {
		t.Errorf("users response has %d keys, want 4: %v", len(body[0]), body[0])
	}
	assertKeys(t, body[0], "id", "login", "avatar_url", "is_admin")
}

func TestReadOnlyEndpointsReturnSnakeCase(t *testing.T) {
	f := newAPIFixture(t)
	admin := f.user("admin", true)
	team := f.team("team-one")
	f.member(team.ID, admin.ID, "lead")
	inst := f.installation(991)
	f.repo(501, inst.ID, "acme/api", &team.ID, true)
	f.pr(501, 1, store.StatusPending)
	cookie := f.session(admin)

	t.Run("stats", func(t *testing.T) {
		rec := f.request(http.MethodGet, "/api/stats", nil, cookie)
		assertStatus(t, rec, http.StatusOK)
		var body map[string]any
		decodeJSON(t, rec, &body)
		assertKeys(t, body, "open", "awaiting_human", "in_evaluation", "with_problems")
		if body["open"] != float64(1) {
			t.Errorf("open = %v, want 1", body["open"])
		}
	})
	t.Run("repos", func(t *testing.T) {
		rec := f.request(http.MethodGet, "/api/repos", nil, cookie)
		assertStatus(t, rec, http.StatusOK)
		var body []map[string]any
		decodeJSON(t, rec, &body)
		if len(body) != 1 {
			t.Fatalf("repos = %v, want one", body)
		}
		assertKeys(t, body[0], "id", "installation_id", "full_name", "private", "default_branch", "enabled", "team_id")
	})
	t.Run("teams", func(t *testing.T) {
		rec := f.request(http.MethodGet, "/api/teams", nil, cookie)
		assertStatus(t, rec, http.StatusOK)
		var body []map[string]any
		decodeJSON(t, rec, &body)
		if len(body) != 1 {
			t.Fatalf("teams = %v, want one", body)
		}
		assertKeys(t, body[0], "id", "name", "slug", "members")
		members, ok := body[0]["members"].([]any)
		if !ok || len(members) != 1 {
			t.Fatalf("members = %v, want one", body[0]["members"])
		}
		assertKeys(t, members[0].(map[string]any), "user_id", "login", "avatar_url", "role")
	})
	t.Run("users", func(t *testing.T) {
		rec := f.request(http.MethodGet, "/api/users", nil, cookie)
		assertStatus(t, rec, http.StatusOK)
		var body []map[string]any
		decodeJSON(t, rec, &body)
		if len(body) != 1 {
			t.Fatalf("users = %v, want one", body)
		}
		assertKeys(t, body[0], "id", "login", "avatar_url", "is_admin")
	})
}

func TestInstallationsAdminOnly(t *testing.T) {
	f := newAPIFixture(t)
	admin := f.user("admin", true)
	dev := f.user("dev", false)

	suspended := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := f.st.UpsertInstallation(f.ctx, store.Installation{ID: 9001, AccountLogin: "Zulu", AccountType: "Organization"}); err != nil {
		t.Fatalf("seed installation Zulu: %v", err)
	}
	if err := f.st.UpsertInstallation(f.ctx, store.Installation{ID: 9002, AccountLogin: "acme", AccountType: "User", SuspendedAt: &suspended}); err != nil {
		t.Fatalf("seed installation acme: %v", err)
	}

	rec := f.request(http.MethodGet, "/api/installations", nil, f.session(admin))
	assertStatus(t, rec, http.StatusOK)

	var body []map[string]any
	decodeJSON(t, rec, &body)
	if len(body) != 2 {
		t.Fatalf("installations = %v, want 2", body)
	}
	assertKeys(t, body[0], "id", "account_login", "account_type", "suspended_at")
	if body[0]["account_login"] != "acme" || body[1]["account_login"] != "Zulu" {
		t.Fatalf("installation order = %v, want acme, Zulu", []any{body[0]["account_login"], body[1]["account_login"]})
	}
	wantSuspended := suspended.Format(time.RFC3339)
	if body[0]["suspended_at"] != wantSuspended {
		t.Errorf("acme suspended_at = %v, want %q", body[0]["suspended_at"], wantSuspended)
	}
	if body[1]["suspended_at"] != nil {
		t.Errorf("Zulu suspended_at = %v, want null", body[1]["suspended_at"])
	}

	rec = f.request(http.MethodGet, "/api/installations", nil, f.session(dev))
	assertStatus(t, rec, http.StatusForbidden)
}

func TestReposIncludeAccountLogin(t *testing.T) {
	f := newAPIFixture(t)
	admin := f.user("admin", true)
	if err := f.st.UpsertInstallation(f.ctx, store.Installation{ID: 9101, AccountLogin: "acme", AccountType: "Organization"}); err != nil {
		t.Fatalf("seed installation acme: %v", err)
	}
	if err := f.st.UpsertInstallation(f.ctx, store.Installation{ID: 9102, AccountLogin: "personal", AccountType: "User"}); err != nil {
		t.Fatalf("seed installation personal: %v", err)
	}
	f.repo(9201, 9101, "acme/app", nil, true)
	f.repo(9202, 9102, "personal/tool", nil, true)

	rec := f.request(http.MethodGet, "/api/repos", nil, f.session(admin))
	assertStatus(t, rec, http.StatusOK)

	var body []map[string]any
	decodeJSON(t, rec, &body)
	if len(body) != 2 {
		t.Fatalf("repos = %v, want 2", body)
	}
	byName := map[string]string{}
	for _, r := range body {
		assertKeys(t, r, "account_login")
		name, _ := r["full_name"].(string)
		login, _ := r["account_login"].(string)
		byName[name] = login
	}
	if byName["acme/app"] != "acme" {
		t.Errorf("acme/app account_login = %q, want acme", byName["acme/app"])
	}
	if byName["personal/tool"] != "personal" {
		t.Errorf("personal/tool account_login = %q, want personal", byName["personal/tool"])
	}
}

func TestReposAccountLoginTeamScoped(t *testing.T) {
	f := newAPIFixture(t)
	dev := f.user("dev", false)
	team1 := f.team("T1")
	team2 := f.team("T2")
	f.member(team1.ID, dev.ID, "developer")
	if err := f.st.UpsertInstallation(f.ctx, store.Installation{ID: 9301, AccountLogin: "acme", AccountType: "Organization"}); err != nil {
		t.Fatalf("seed installation acme: %v", err)
	}
	if err := f.st.UpsertInstallation(f.ctx, store.Installation{ID: 9302, AccountLogin: "other", AccountType: "Organization"}); err != nil {
		t.Fatalf("seed installation other: %v", err)
	}
	f.repo(9401, 9301, "acme/visible", &team1.ID, true)
	f.repo(9402, 9302, "other/hidden", &team2.ID, true)

	rec := f.request(http.MethodGet, "/api/repos", nil, f.session(dev))
	assertStatus(t, rec, http.StatusOK)

	var body []map[string]any
	decodeJSON(t, rec, &body)
	if len(body) != 1 {
		t.Fatalf("repos = %v, want 1", body)
	}
	assertKeys(t, body[0], "account_login")
	if body[0]["full_name"] != "acme/visible" || body[0]["account_login"] != "acme" {
		t.Errorf("visible repo = %v, want acme/visible with account_login acme", body[0])
	}
}

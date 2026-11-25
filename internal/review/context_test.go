package review

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestBuildBackgroundIncludesPRBodyAndCriteria(t *testing.T) {
	pr := store.PullRequest{
		Number:  7,
		HeadSHA: "head-sha",
		Body: "Adds a retry loop for <webhook> @team\n\n" +
			"## Acceptance criteria\n\n" +
			"- [ ] retries transient errors\n" +
			"- [x] audits every retry\n",
	}
	bg := buildBackground(pr, "", backgroundExtras{})

	for _, want := range []string{
		"## Descripción del PR (DATA)",
		"Procedencia: descripción del PR #7",
		"<untrusted>Adds a retry loop for &lt;webhook&gt; [at]team",
		"## Criterios de aceptación (DATA)",
		"Procedencia: extraídos de la descripción del PR.",
		"<untrusted>retries transient errors</untrusted>",
		"<untrusted>audits every retry</untrusted>",
	} {
		if !strings.Contains(bg, want) {
			t.Errorf("background missing %q:\n%s", want, bg)
		}
	}
	if strings.Contains(bg, "<webhook>") || strings.Contains(bg, "@team") {
		t.Errorf("background did not neutralize the PR body:\n%s", bg)
	}
}

func TestBuildBackgroundMissingBodyAndCriteria(t *testing.T) {
	bg := buildBackground(store.PullRequest{Number: 1, HeadSHA: "head"}, "", backgroundExtras{})

	for _, want := range []string{
		"Contexto ausente: el PR no tiene descripción.",
		"Contexto ausente: la descripción no declara criterios de aceptación.",
		"Contexto ausente: no se recopiló contexto del repositorio.",
	} {
		if !strings.Contains(bg, want) {
			t.Errorf("background missing %q:\n%s", want, bg)
		}
	}
}

func TestBuildBackgroundTruncatesLongBodyAndCriteria(t *testing.T) {
	long := strings.Repeat("x", maxPRBodyBytes+25)
	bg := buildBackground(store.PullRequest{Number: 2, HeadSHA: "head", Body: long}, "", backgroundExtras{})
	if !strings.Contains(bg, "[Descripción truncada a 8000 bytes de 8025.]") {
		t.Errorf("background did not record the body truncation:\n%s", bg)
	}
	if len(bg) > maxPRBodyBytes*3 {
		t.Errorf("background is %d bytes, want a bounded body", len(bg))
	}

	body := "## Acceptance criteria\n" + strings.Repeat("- [ ] criterion\n", maxCriteriaItems+5)
	bg = buildBackground(store.PullRequest{Number: 3, HeadSHA: "head", Body: body}, "", backgroundExtras{})
	if !strings.Contains(bg, "[Criterios recortados a") {
		t.Errorf("background did not record the criteria truncation:\n%s", bg)
	}
}

func TestExtractAcceptanceCriteria(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		want      []string
		truncated bool
	}{
		{
			name: "heading delimits the section",
			body: "## Acceptance criteria\n- [ ] a\n- [ ] b\n## Notes\n- [ ] ignored",
			want: []string{"a", "b"},
		},
		{
			name: "spanish heading with accent",
			body: "### Criterios de aceptación\n* [x] hecho",
			want: []string{"hecho"},
		},
		{
			name: "falls back to checkboxes without a heading",
			body: "Intro\n- [ ] one\n- [x] two",
			want: []string{"one", "two"},
		},
		{
			name:      "limits the number of items",
			body:      "## Acceptance criteria\n" + strings.Repeat("- [ ] item\n", maxCriteriaItems+5),
			want:      repeatItems("item", maxCriteriaItems),
			truncated: true,
		},
		{
			name:      "truncates an oversized item",
			body:      "## Acceptance criteria\n- [ ] " + strings.Repeat("x", maxCriteriaBytes+1),
			truncated: true,
		},
		{
			name: "empty body",
			body: " ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, truncated := extractAcceptanceCriteria(tt.body, maxCriteriaItems, maxCriteriaBytes)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("criteria = %v, want %v", got, tt.want)
			}
			if truncated != tt.truncated {
				t.Errorf("truncated = %v, want %v", truncated, tt.truncated)
			}
		})
	}
}

func TestBuildBackgroundLinkedIssues(t *testing.T) {
	pr := store.PullRequest{Number: 9, HeadSHA: "sha9"}
	extras := backgroundExtras{linked: linkedContext{
		Issues: []linkedIssue{
			{Number: 12, Title: "Add retries", Body: "## Acceptance criteria\n- [ ] handle 5xx\n- [ ] backoff"},
			{Number: 13, Err: "referencia inaccesible"},
			{Number: 5, Repo: "other/repo", Err: "referencia en otro repositorio; no se consulta"},
		},
		Omitted: 2,
	}}
	bg := buildBackground(pr, "", extras)

	for _, want := range []string{
		"## Criterios de aceptación enlazados (DATA)",
		"### Issue #12",
		"<untrusted>Add retries</untrusted>",
		"Procedencia: issue de GitHub consultado para el head <untrusted>sha9</untrusted>.",
		"<untrusted>handle 5xx</untrusted>",
		"<untrusted>backoff</untrusted>",
		"### Issue #13",
		"Contexto ausente: referencia inaccesible",
		"### Issue other/repo#5",
		"[2 referencias omitidas por el límite de 3.]",
	} {
		if !strings.Contains(bg, want) {
			t.Errorf("background missing %q:\n%s", want, bg)
		}
	}

	without := buildBackground(pr, "", backgroundExtras{})
	if !strings.Contains(without, "Sin referencias a issues enlazadas") {
		t.Errorf("background did not record the absence of linked issues:\n%s", without)
	}
}

func TestLinkedIssueNumbers(t *testing.T) {
	body := "Closes #12\n" +
		"Fixes acme/repo#13\n" +
		"See https://github.com/acme/repo/issues/14\n" +
		"```\nCloses #99\n```\n" +
		"Plain #15 is not linked"
	got, omitted := linkedIssueNumbers(body, "acme/repo", 10)
	want := []issueRef{
		{Owner: "acme", Repo: "repo", Number: 14},
		{Owner: "acme", Repo: "repo", Number: 12},
		{Owner: "acme", Repo: "repo", Number: 13},
	}
	if !reflect.DeepEqual(got, want) || omitted != 0 {
		t.Fatalf("linked refs = %v (omitted %d), want %v", got, omitted, want)
	}
	if limited, omitted := linkedIssueNumbers(body, "acme/repo", 2); len(limited) != 2 || omitted != 1 ||
		limited[0].Number != 14 || limited[1].Number != 12 {
		t.Errorf("limited refs = %v (omitted %d), want 14,12 with 1 omitted", limited, omitted)
	}
	if refs, omitted := linkedIssueNumbers("no references", "acme/repo", 3); refs != nil || omitted != 0 {
		t.Errorf("linked refs = %v (omitted %d), want nil", refs, omitted)
	}
	cross, _ := linkedIssueNumbers("Fixes other/repo#5", "acme/repo", 10)
	if len(cross) != 1 || cross[0].fullName() != "other/repo" {
		t.Errorf("cross-repo refs = %v, want other/repo#5", cross)
	}
}

func TestLoadLinkedIssuesWithoutGitHubRecordsInaccessible(t *testing.T) {
	got := loadLinkedIssues(context.Background(), nil, 1, "acme/repo", "Closes #3")
	if len(got.Issues) != 1 || got.Issues[0].Number != 3 || got.Issues[0].Err == "" {
		t.Fatalf("issues = %+v, want one inaccessible reference", got.Issues)
	}
	if empty := loadLinkedIssues(context.Background(), nil, 1, "acme/repo", "nothing"); len(empty.Issues) != 0 || empty.Omitted != 0 {
		t.Errorf("issues = %+v, want none", empty)
	}
	cross := loadLinkedIssues(context.Background(), nil, 1, "acme/repo", "Fixes other/repo#5")
	if len(cross.Issues) != 1 || cross.Issues[0].Repo != "other/repo" ||
		!strings.Contains(cross.Issues[0].Err, "otro repositorio") {
		t.Errorf("cross-repo issues = %+v, want a recorded skip", cross.Issues)
	}
}

func TestLoadLinkedIssuesFetchesAndRecordsPerIssueErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"token":"t","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		switch r.URL.Path {
		case "/repos/acme/repo/issues/12":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"number":12,"title":"Retries","body":"## Acceptance criteria\n- [ ] backoff"}`)
		case "/repos/acme/repo/issues/13":
			http.Error(w, "not found", http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app := &github.App{
		Creds:  github.AppCredentials{AppID: 12345, PEM: triggerTestPEM(t)},
		APIURL: srv.URL,
	}
	got := loadLinkedIssues(context.Background(), triggerTestApps{app: app}, 7, "acme/repo",
		"Closes #12\nFixes #13\nFixes other/repo#5")

	if len(got.Issues) != 3 {
		t.Fatalf("issues = %+v, want 3", got.Issues)
	}
	if got.Issues[0].Title != "Retries" || !strings.Contains(got.Issues[0].Body, "backoff") {
		t.Errorf("fetched issue = %+v, want the GitHub body", got.Issues[0])
	}
	if got.Issues[1].Err == "" {
		t.Errorf("per-issue fetch error not recorded: %+v", got.Issues[1])
	}
	if got.Issues[2].Repo != "other/repo" || got.Issues[2].Err == "" {
		t.Errorf("cross-repo reference was fetched or dropped: %+v", got.Issues[2])
	}
}

func TestBuildBackgroundRespectsContextBudget(t *testing.T) {
	pr := store.PullRequest{
		Number:  4,
		HeadSHA: "head",
		Body:    "## Acceptance criteria\n" + strings.Repeat("- [ ] item\n", 100),
	}
	extras := backgroundExtras{
		contextBytes:    512,
		contextBytesSet: true,
		linked: linkedContext{Issues: []linkedIssue{
			{Number: 1, Title: "one", Body: strings.Repeat("x", 5000)},
			{Number: 2, Title: "two", Body: strings.Repeat("y", 5000)},
			{Number: 3, Err: "referencia inaccesible"},
		}},
		repo: repositoryContext{Markdown: "contexto"},
	}
	bg := buildBackground(pr, "", extras)

	for _, want := range []string{
		"[Descripción truncada a 256 bytes de",
		"[Criterios recortados a",
		"[Descripción del issue truncada a 21 bytes.]",
	} {
		if !strings.Contains(bg, want) {
			t.Errorf("background missing %q:\n%s", want, bg)
		}
	}
	if len(bg) > extras.contextBytes+4000 {
		t.Errorf("background is %d bytes, want context below %d plus overhead", len(bg), extras.contextBytes)
	}
}

func TestBuildBackgroundBoundsMultibyteBodyInBytes(t *testing.T) {
	body := strings.Repeat("á", 3000) // 2 bytes per rune: 6000 bytes, 3000 runes.
	bg := buildBackground(store.PullRequest{Number: 6, HeadSHA: "head", Body: body}, "", backgroundExtras{
		contextBytes:    512,
		contextBytesSet: true,
	})

	if !strings.Contains(bg, "[Descripción truncada a 256 bytes de 6000.]") {
		t.Errorf("multibyte body not bounded in bytes:\n%s", bg)
	}
	if len(bg) > 512+4000 {
		t.Errorf("background is %d bytes, want the byte budget respected", len(bg))
	}
}

func TestBuildBackgroundDisabledByZeroBudget(t *testing.T) {
	pr := store.PullRequest{Number: 5, HeadSHA: "head", Body: "descripción"}
	bg := buildBackground(pr, "", backgroundExtras{contextBytesSet: true})

	if !strings.Contains(bg, "Contexto ausente: el presupuesto del run no deja margen para descripción ni criterios.") {
		t.Errorf("background did not record the disabled context:\n%s", bg)
	}
	if strings.Contains(bg, "## Descripción del PR") {
		t.Errorf("background sent the description despite the zero budget:\n%s", bg)
	}
}

func TestBackgroundContextBudget(t *testing.T) {
	tests := []struct {
		name        string
		ceiling     int
		tokenBudget int64
		want        int
	}{
		{name: "large ceiling is capped", ceiling: 270000, want: backgroundContextMaxBytes},
		{name: "minimum ceiling scales down", ceiling: 4096, want: 816},
		{name: "run budget below the floor disables context", ceiling: 270000, tokenBudget: 120, want: 0},
		{name: "run budget scales the section", ceiling: 270000, tokenBudget: 10000, want: 2000},
		{name: "no ceiling", ceiling: 0, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backgroundContextBudget(tt.ceiling, tt.tokenBudget); got != tt.want {
				t.Errorf("budget = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRepositoryContextBudget(t *testing.T) {
	tests := []struct {
		name        string
		ceiling     int
		tokenBudget int64
		want        int
	}{
		{name: "large ceiling is capped", ceiling: 270000, want: repoContextMaxBytes},
		{name: "minimum ceiling scales down", ceiling: 4096, want: 816},
		{name: "run budget below the floor disables context", ceiling: 270000, tokenBudget: 120, want: 0},
		{name: "run budget scales the excerpt", ceiling: 270000, tokenBudget: 10000, want: 2000},
		{name: "no ceiling", ceiling: 0, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := repositoryContextBudget(tt.ceiling, tt.tokenBudget); got != tt.want {
				t.Errorf("budget = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBuildRepositoryContextIncludesRelatedTests(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "svc.go", "package svc\n")
	writeTestFile(t, dir, "svc_test.go", "package svc\n\nfunc TestRetry(t *testing.T) {}\n")
	writeTestFile(t, dir, "other.go", "package svc\n")

	got := buildRepositoryContext(dir, "head-sha", []string{"svc.go", "other.go"}, nil, 4096)

	for _, want := range []string{
		"El checkout corresponde al SHA head <untrusted>head-sha</untrusted>",
		"### Test relacionado: <untrusted>svc_test.go</untrusted> (head <untrusted>head-sha</untrusted>)",
		"<untrusted>package svc",
		"func TestRetry(t *testing.T) {}",
	} {
		if !strings.Contains(got.Markdown, want) {
			t.Errorf("repository context missing %q:\n%s", want, got.Markdown)
		}
	}
	if strings.Contains(got.Markdown, "svc.go (head") {
		t.Errorf("repository context included a non-test file:\n%s", got.Markdown)
	}
}

func TestBuildRepositoryContextRecordsMissingAndTruncated(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "svc.go", "package svc\n")
	writeTestFile(t, dir, "svc_test.go", strings.Repeat("x", repoContextPerFileBytes*2))

	got := buildRepositoryContext(dir, "head", []string{"svc.go"}, nil, 4096)
	if !strings.Contains(got.Markdown, "[Recortado a") {
		t.Errorf("repository context did not record the truncation:\n%s", got.Markdown)
	}
	if len(got.Notes) == 0 {
		t.Error("repository context recorded no truncation note")
	}

	empty := buildRepositoryContext(dir, "head", nil, nil, 4096)
	if !strings.Contains(empty.Markdown, "Contexto ausente: el diff revisado no incluye archivos") {
		t.Errorf("empty diff not recorded:\n%s", empty.Markdown)
	}

	failed := buildRepositoryContext(dir, "head", nil, errors.New("git boom"), 4096)
	if !strings.Contains(failed.Markdown, "no se pudo listar el diff") ||
		!strings.Contains(failed.Markdown, "git boom") {
		t.Errorf("diff failure not distinguished from an empty diff:\n%s", failed.Markdown)
	}

	disabled := buildRepositoryContext(dir, "head", []string{"svc.go"}, nil, 0)
	if !strings.Contains(disabled.Markdown, "el presupuesto del run no deja margen") {
		t.Errorf("zero budget not recorded:\n%s", disabled.Markdown)
	}
}

func TestBuildRepositoryContextStaysInsideCheckout(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, "secret_test.go", "TOP SECRET")
	writeTestFile(t, dir, "svc.go", "package svc\n")

	got := buildRepositoryContext(dir, "head", []string{"../secret.go"}, nil, 4096)
	if strings.Contains(got.Markdown, "TOP SECRET") {
		t.Errorf("repository context escaped the checkout:\n%s", got.Markdown)
	}
	if !strings.Contains(got.Markdown, "Contexto ausente") {
		t.Errorf("repository context did not record the inaccessible test:\n%s", got.Markdown)
	}
}

func TestBuildRepositoryContextRejectsSymlinkedDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, outside, "secret_test.go", "TOP SECRET")
	writeTestFile(t, dir, "svc.go", "package svc\n")
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got := buildRepositoryContext(dir, "head", []string{"link/secret.go"}, nil, 4096)
	if strings.Contains(got.Markdown, "TOP SECRET") {
		t.Errorf("repository context followed a symlinked directory outside the checkout:\n%s", got.Markdown)
	}
	if !strings.Contains(got.Markdown, "Contexto ausente") {
		t.Errorf("repository context did not record the rejected symlink:\n%s", got.Markdown)
	}
}

func TestRelatedTestPath(t *testing.T) {
	tests := map[string]string{
		"pkg/svc.go":      "pkg/svc_test.go",
		"web/view.ts":     "web/view_test.ts",
		"pkg/svc_test.go": "",
		"Makefile":        "",
	}
	for in, want := range tests {
		if got := relatedTestPath(in); got != want {
			t.Errorf("relatedTestPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func repeatItems(value string, n int) []string {
	items := make([]string, n)
	for i := range items {
		items[i] = value
	}
	return items
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

package review

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runGit ejecuta git en dir y devuelve stdout recortado; falla el test si git falla.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newBareRepo crea un repo bare local con main, feature (hija de main) y orphan
// (historia no relacionada). Devuelve la ruta del bare y los SHA de las puntas.
func newBareRepo(t *testing.T) (bare, mainSHA, featureSHA, orphanSHA string) {
	t.Helper()

	root := t.TempDir()
	work := filepath.Join(root, "work")
	bare = filepath.Join(root, "bare.git")

	runGit(t, root, "init", "-q", "-b", "main", work)
	runGit(t, work, "config", "user.email", "test@example.com")
	runGit(t, work, "config", "user.name", "Test")

	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "file.txt")
	runGit(t, work, "commit", "-qm", "base")
	mainSHA = runGit(t, work, "rev-parse", "HEAD")

	runGit(t, work, "checkout", "-qb", "feature")
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "file.txt")
	runGit(t, work, "commit", "-qm", "feature")
	featureSHA = runGit(t, work, "rev-parse", "HEAD")

	// orphan no comparte historia con main: el merge-base nunca existe.
	runGit(t, work, "checkout", "-q", "--orphan", "orphan")
	if err := os.Remove(filepath.Join(work, "file.txt")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "orphan.txt"), []byte("orphan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-qm", "orphan")
	orphanSHA = runGit(t, work, "rev-parse", "HEAD")

	runGit(t, root, "clone", "-q", "--bare", work, bare)
	return bare, mainSHA, featureSHA, orphanSHA
}

func TestCloneFindsMergeBase(t *testing.T) {
	bare, mainSHA, featureSHA, _ := newBareRepo(t)
	dir := filepath.Join(t.TempDir(), "checkout")

	mergeBase, err := Clone(context.Background(), dir, "file://"+bare, "main", "refs/heads/feature", 20)
	if err != nil {
		t.Fatalf("Clone() error = %v", err)
	}
	if mergeBase != mainSHA {
		t.Errorf("mergeBase = %q, want main SHA %q", mergeBase, mainSHA)
	}

	head := runGit(t, dir, "rev-parse", "HEAD")
	if head != featureSHA {
		t.Errorf("HEAD = %q, want feature SHA %q", head, featureSHA)
	}

	// el checkout debe quedar detachado y sin el remote efímero.
	cmd := exec.Command("git", "symbolic-ref", "-q", "HEAD")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("HEAD should be detached, got branch %q", strings.TrimSpace(string(out)))
	}
	if remotes := runGit(t, dir, "remote"); remotes != "" {
		t.Errorf("remotes = %q, want none", remotes)
	}
}

func TestFetchHeadRefs(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "1111111111111111111111111111111111111111\t\tbranch 'main' of origin\n" +
		"2222222222222222222222222222222222222222\t\t'refs/pull/1/head' of origin\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "FETCH_HEAD"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := fetchHeadBase(dir)
	if err != nil || base != "1111111111111111111111111111111111111111" {
		t.Fatalf("fetchHeadBase() = %q, %v", base, err)
	}
	tip, err := fetchHeadTip(dir)
	if err != nil || tip != "2222222222222222222222222222222222222222" {
		t.Fatalf("fetchHeadTip() = %q, %v", tip, err)
	}
}

func TestCloneURLInjectsTokenOnlyForHTTP(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		repo    string
		token   string
		want    string
	}{
		{
			name:    "https",
			baseURL: "https://github.com",
			repo:    "o/r",
			token:   "t",
			want:    "https://x-access-token:t@github.com/o/r.git",
		},
		{
			name:    "http",
			baseURL: "http://git.example.com",
			repo:    "o/r",
			token:   "t",
			want:    "http://x-access-token:t@git.example.com/o/r.git",
		},
		{
			name:    "https con path base y barra final",
			baseURL: "https://git.example.com/scm/",
			repo:    "o/r.git",
			token:   "t",
			want:    "https://x-access-token:t@git.example.com/scm/o/r.git",
		},
		{
			name:    "file sin token",
			baseURL: "file:///tmp/x",
			repo:    "o/r",
			token:   "t",
			want:    "file:///tmp/x",
		},
		{
			name:    "ssh sin token",
			baseURL: "ssh://git@github.com/o/r",
			repo:    "o/r",
			token:   "t",
			want:    "ssh://git@github.com/o/r",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cloneURL(tt.baseURL, tt.repo, tt.token); got != tt.want {
				t.Errorf("cloneURL(%q, %q, %q) = %q, want %q", tt.baseURL, tt.repo, tt.token, got, tt.want)
			}
		})
	}
}

func TestCloneHonorsCanceledContext(t *testing.T) {
	bare, _, _, _ := newBareRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dir := filepath.Join(t.TempDir(), "canceled")
	if _, err := Clone(ctx, dir, "file://"+bare, "main", "refs/heads/feature", 20); err == nil {
		t.Error("Clone() con contexto cancelado: esperaba error")
	}
}

func TestCloneDepthCapFails(t *testing.T) {
	bare, _, _, orphanSHA := newBareRepo(t)

	t.Run("ref inexistente", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "missing")
		if _, err := Clone(context.Background(), dir, "file://"+bare, "main", "refs/heads/nope", 4); err == nil {
			t.Error("Clone() con head inexistente: esperaba error")
		}
	})

	t.Run("sin merge-base agota la profundidad", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "orphan")
		_, err := Clone(context.Background(), dir, "file://"+bare, "main", "refs/heads/orphan", 4)
		if err == nil {
			t.Fatalf("Clone() con head sin historia común (%s): esperaba error", orphanSHA)
		}
		if !strings.Contains(err.Error(), "merge-base") {
			t.Errorf("error = %q, want que mencione merge-base", err)
		}
	})
}

const (
	// transientFetchFailure is the wording git uses when the transport cannot
	// reach the remote, as opposed to a rejected credential or a missing ref.
	transientFetchFailure = "git fetch: exit status 128: fatal: unable to access " +
		"'https://github.com/acme/app.git/': Failed to connect to github.com port 443: Connection timed out"
	permanentFetchFailure = "git fetch: exit status 128: fatal: couldn't find remote ref refs/pull/1/head"
)

// stubFetch replaces fetchGit for the duration of the test, shrinks the retry
// backoff and returns a counter of the attempts it observed. fail receives the
// 1-based attempt number; returning nil delegates to the real fetch.
func stubFetch(t *testing.T, fail func(attempt int) error) *int {
	t.Helper()
	previousFetch, previousBase := fetchGit, fetchRetryBase
	t.Cleanup(func() { fetchGit, fetchRetryBase = previousFetch, previousBase })
	fetchRetryBase = time.Millisecond

	attempts := 0
	fetchGit = func(ctx context.Context, dir string, args ...string) (string, error) {
		attempts++
		if err := fail(attempts); err != nil {
			return "", err
		}
		return previousFetch(ctx, dir, args...)
	}
	return &attempts
}

func TestCloneRetriesTransientFetchFailure(t *testing.T) {
	bare, mainSHA, _, _ := newBareRepo(t)
	attempts := stubFetch(t, func(attempt int) error {
		if attempt == 1 {
			return errors.New(transientFetchFailure)
		}
		return nil
	})
	dir := filepath.Join(t.TempDir(), "checkout")

	mergeBase, err := Clone(context.Background(), dir, "file://"+bare, "main", "refs/heads/feature", 20)
	if err != nil {
		t.Fatalf("Clone() after a transient fetch failure = %v, want the retry to recover", err)
	}
	if mergeBase != mainSHA {
		t.Errorf("mergeBase = %q, want %q", mergeBase, mainSHA)
	}
	if *attempts < 2 {
		t.Errorf("fetch attempts = %d, want the first failure retried", *attempts)
	}
}

func TestCloneDoesNotRetryPermanentFetchFailure(t *testing.T) {
	bare, _, _, _ := newBareRepo(t)
	tests := []struct {
		name    string
		message string
	}{
		{"missing ref", permanentFetchFailure},
		{"tls certificate", "git fetch: exit status 128: fatal: unable to access 'https://github.com/acme/app.git/': SSL certificate problem: unable to get local issuer certificate"},
		{"handshake", "git fetch: exit status 128: gnutls_handshake() failed: The TLS connection was non-properly terminated."},
		{"corrupt pack", "git fetch: exit status 128: fatal: index-pack failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempts := stubFetch(t, func(int) error { return errors.New(tt.message) })
			dir := filepath.Join(t.TempDir(), "checkout")

			if _, err := Clone(context.Background(), dir, "file://"+bare, "main", "refs/heads/feature", 20); err == nil {
				t.Fatal("Clone() with a permanent fetch failure: want an error")
			}
			if *attempts != 1 {
				t.Errorf("fetch attempts = %d, want 1: a permanent failure is not retried", *attempts)
			}
		})
	}
}

func TestCloneExhaustedRetryReportsCause(t *testing.T) {
	bare, _, _, _ := newBareRepo(t)
	attempts := stubFetch(t, func(int) error { return errors.New(transientFetchFailure) })
	dir := filepath.Join(t.TempDir(), "checkout")

	_, err := Clone(context.Background(), dir, "file://"+bare, "main", "refs/heads/feature", 20)
	if err == nil {
		t.Fatal("Clone() with an unreachable remote: want an error")
	}
	if *attempts != fetchMaxAttempts {
		t.Errorf("fetch attempts = %d, want the cap %d", *attempts, fetchMaxAttempts)
	}
	if !strings.Contains(err.Error(), "Connection timed out") {
		t.Errorf("error = %q, want the transport cause preserved", err)
	}
	if !strings.Contains(err.Error(), "intentos") {
		t.Errorf("error = %q, want the number of attempts in the cause", err)
	}
}

func TestCloneRetryStopsWhenContextIsDone(t *testing.T) {
	bare, _, _, _ := newBareRepo(t)
	previousFetch, previousBase := fetchGit, fetchRetryBase
	t.Cleanup(func() { fetchGit, fetchRetryBase = previousFetch, previousBase })
	// A wait longer than the test would tolerate: it must be interrupted by the
	// context instead of sleeping past the run deadline.
	fetchRetryBase = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	fetchGit = func(context.Context, string, ...string) (string, error) {
		attempts++
		cancel()
		return "", errors.New(transientFetchFailure)
	}
	dir := filepath.Join(t.TempDir(), "checkout")

	started := time.Now()
	if _, err := Clone(ctx, dir, "file://"+bare, "main", "refs/heads/feature", 20); err == nil {
		t.Fatal("Clone() with a canceled context: want an error")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("Clone() took %v, want the retry to stop at the deadline", elapsed)
	}
	if attempts != 1 {
		t.Errorf("fetch attempts = %d, want 1: no retry after cancellation", attempts)
	}
}

func TestCloneRetrySkipsWaitBeyondDeadline(t *testing.T) {
	bare, _, _, _ := newBareRepo(t)
	previousFetch, previousBase := fetchGit, fetchRetryBase
	t.Cleanup(func() { fetchGit, fetchRetryBase = previousFetch, previousBase })
	// The backoff is far longer than the remaining budget: the retry must not
	// start a wait it cannot finish.
	fetchRetryBase = time.Hour

	attempts := 0
	fetchGit = func(context.Context, string, ...string) (string, error) {
		attempts++
		return "", errors.New(transientFetchFailure)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir := filepath.Join(t.TempDir(), "checkout")

	started := time.Now()
	if _, err := Clone(ctx, dir, "file://"+bare, "main", "refs/heads/feature", 20); err == nil {
		t.Fatal("Clone() with an unreachable remote: want an error")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("Clone() took %v, want no wait that exceeds the deadline", elapsed)
	}
	if attempts != 1 {
		t.Errorf("fetch attempts = %d, want 1: the backoff does not fit the deadline", attempts)
	}
}

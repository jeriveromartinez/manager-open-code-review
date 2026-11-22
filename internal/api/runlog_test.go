package api_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestReviewRunLogsScopedAndPaginated(t *testing.T) {
	f := newSecureFixture(t)
	inst := f.installation(1101)
	team := f.team("log-team")
	repo := f.repo(7101, inst.ID, "acme/logs", &team.ID, true)
	pr := f.pr(repo.ID, 11, store.StatusPending)
	dev := f.user("logdev", false)
	f.member(team.ID, dev.ID, "developer")
	outsider := f.user("logoutsider", false)

	profile, err := f.st.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: "log-profile", Provider: "openai", Model: "gpt-4o-mini", APIKeyEnc: "sealed", Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}
	run, err := f.st.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, ProfileID: profile.ID, HeadSHA: "head-sha", Status: store.RunRunning,
	})
	if err != nil {
		t.Fatalf("CreateReviewRun: %v", err)
	}

	now := time.Now().UTC()
	entries := []store.ReviewRunLog{
		{RunID: run.ID, Seq: 1, TS: now.Add(-2 * time.Second), Source: "worker", Phase: "claim", Level: "info", Line: "run reclamado"},
		{RunID: run.ID, Seq: 2, TS: now.Add(-time.Second), Source: "ocr", Invocation: "main", Phase: "ocr", Level: "info", Line: "fase: agrupando"},
		{RunID: run.ID, Seq: 3, TS: now, Source: "worker", Phase: "finish", Level: "warn", Line: "run inconcluso"},
	}
	if err := f.st.AppendReviewRunLog(f.ctx, entries); err != nil {
		t.Fatalf("AppendReviewRunLog: %v", err)
	}

	path := fmt.Sprintf("/api/review-runs/%d/logs", run.ID)
	type logEntry struct {
		Seq        int64  `json:"seq"`
		TS         string `json:"ts"`
		Source     string `json:"source"`
		Invocation string `json:"invocation"`
		Level      string `json:"level"`
		Phase      string `json:"phase"`
		Line       string `json:"line"`
	}
	type logsPage struct {
		RunID     uint       `json:"run_id"`
		Entries   []logEntry `json:"entries"`
		NextAfter int64      `json:"next_after"`
		HasMore   bool       `json:"has_more"`
	}

	rec := f.request(http.MethodGet, path, nil, f.session(dev))
	assertStatus(t, rec, http.StatusOK)
	var page logsPage
	decodeJSON(t, rec, &page)
	if page.RunID != run.ID || len(page.Entries) != 3 {
		t.Fatalf("page = %+v, want three entries of run %d", page, run.ID)
	}
	if page.Entries[1].Source != "ocr" || page.Entries[1].Invocation != "main" || page.Entries[1].Line != "fase: agrupando" {
		t.Errorf("second entry = %+v, want the OCR line", page.Entries[1])
	}
	if page.NextAfter != 3 || page.HasMore {
		t.Errorf("page cursor = %d/%v, want 3/false", page.NextAfter, page.HasMore)
	}

	rec = f.request(http.MethodGet, path+"?after=1&limit=1", nil, f.session(dev))
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &page)
	if len(page.Entries) != 1 || page.Entries[0].Seq != 2 || page.NextAfter != 2 || !page.HasMore {
		t.Fatalf("paged = %+v, want seq 2 and has_more", page)
	}

	rec = f.request(http.MethodGet, path, nil, f.session(outsider))
	assertStatus(t, rec, http.StatusNotFound)

	rec = f.request(http.MethodGet, "/api/review-runs/999999/logs", nil, f.session(dev))
	assertStatus(t, rec, http.StatusNotFound)

	rec = f.request(http.MethodGet, path+"?after=-1", nil, f.session(dev))
	assertStatus(t, rec, http.StatusBadRequest)
}

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestReviewRunLogsAppendListAndPrune(t *testing.T) {
	f := newFixture(t)
	installationID := f.nextID()
	f.installation(installationID)
	f.repo(installationID, installationID, "acme/app", nil, true)
	pr := f.openPR(installationID, 31, store.StatusPending, false)
	run := f.run(pr, store.RunRunning)

	now := time.Now().UTC()
	entries := []store.ReviewRunLog{
		{RunID: run.ID, Seq: 1, TS: now.Add(-2 * time.Hour), Source: "worker", Level: "info", Phase: "cloning", Line: "primer paso"},
		{RunID: run.ID, Seq: 2, TS: now.Add(-time.Hour), Source: "ocr", Invocation: "main", Level: "info", Line: "ocr trabajando"},
		{RunID: run.ID, Seq: 3, TS: now, Source: "ocr", Invocation: "chunk 1/2", Level: "warn", Line: "trozo avisando"},
	}
	if err := f.s.AppendReviewRunLog(f.ctx, entries); err != nil {
		t.Fatalf("AppendReviewRunLog() error = %v", err)
	}
	// A retried flush must not fail or duplicate rows.
	if err := f.s.AppendReviewRunLog(f.ctx, entries[:1]); err != nil {
		t.Fatalf("AppendReviewRunLog(duplicate) error = %v", err)
	}

	logs, err := f.s.ReviewRunLogs(f.ctx, run.ID, 0, 2)
	if err != nil {
		t.Fatalf("ReviewRunLogs() error = %v", err)
	}
	if len(logs) != 2 || logs[0].Seq != 1 || logs[1].Seq != 2 {
		t.Fatalf("first page = %+v, want seq 1 and 2", logs)
	}
	logs, err = f.s.ReviewRunLogs(f.ctx, run.ID, 1, 10)
	if err != nil {
		t.Fatalf("ReviewRunLogs(after) error = %v", err)
	}
	if len(logs) != 2 || logs[0].Seq != 2 || logs[1].Seq != 3 {
		t.Fatalf("page after seq 1 = %+v, want seq 2 and 3", logs)
	}
	if logs[1].Invocation != "chunk 1/2" || logs[1].Level != "warn" {
		t.Errorf("third line = %+v, want chunk invocation and warn level", logs[1])
	}

	prID, err := f.s.ReviewRunPRID(f.ctx, run.ID)
	if err != nil {
		t.Fatalf("ReviewRunPRID() error = %v", err)
	}
	if prID != pr.ID {
		t.Errorf("ReviewRunPRID() = %d, want %d", prID, pr.ID)
	}
	if _, err := f.s.ReviewRunPRID(f.ctx, 999999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ReviewRunPRID(missing) error = %v, want ErrNotFound", err)
	}

	pruned, err := f.s.PruneReviewRunLogs(f.ctx, now.Add(-90*time.Minute))
	if err != nil {
		t.Fatalf("PruneReviewRunLogs() error = %v", err)
	}
	if pruned != 1 {
		t.Errorf("PruneReviewRunLogs() removed %d rows, want 1", pruned)
	}
	logs, err = f.s.ReviewRunLogs(f.ctx, run.ID, 0, 10)
	if err != nil {
		t.Fatalf("ReviewRunLogs(after prune) error = %v", err)
	}
	if len(logs) != 2 || logs[0].Seq != 2 {
		t.Errorf("logs after prune = %+v, want seq 2 and 3", logs)
	}
}

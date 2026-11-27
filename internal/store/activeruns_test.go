package store_test

import (
	"errors"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestSetReviewRunPhaseOnlyOnRunning(t *testing.T) {
	f := newFixture(t)
	inst := f.nextID()
	f.installation(inst)
	f.repo(inst, inst, "acme/app", nil, true)
	pr := f.openPR(inst, 21, store.StatusPending, false)
	running := f.run(pr, store.RunRunning)

	if err := f.s.SetReviewRunPhase(f.ctx, running.ID, "", store.RunPhaseCloning); err != nil {
		t.Fatalf("SetReviewRunPhase() error = %v", err)
	}
	got, err := f.s.ReviewRun(f.ctx, running.ID)
	if err != nil {
		t.Fatalf("ReviewRun() error = %v", err)
	}
	if got.Phase != store.RunPhaseCloning || got.PhaseStartedAt == nil {
		t.Fatalf("run phase = %q/%v, want cloning with a start", got.Phase, got.PhaseStartedAt)
	}

	if err := f.s.RequeueReviewRun(f.ctx, running.ID, "", "capacity", "sin hueco"); err != nil {
		t.Fatalf("RequeueReviewRun() error = %v", err)
	}
	got, _ = f.s.ReviewRun(f.ctx, running.ID)
	if got.Phase != "" || got.PhaseStartedAt != nil {
		t.Errorf("requeued run kept its phase: %q/%v", got.Phase, got.PhaseStartedAt)
	}

	queuedPR := f.openPR(inst, 22, store.StatusPending, false)
	queued := f.run(queuedPR, store.RunQueued)
	if err := f.s.SetReviewRunPhase(f.ctx, queued.ID, "", store.RunPhaseReviewing); !errors.Is(err, store.ErrRunNotActive) {
		t.Errorf("SetReviewRunPhase(queued) error = %v, want ErrRunNotActive", err)
	}

	if _, _, err := f.s.ClaimReviewRun(f.ctx, running.ID); err != nil {
		t.Fatalf("ClaimReviewRun() error = %v", err)
	}
	if err := f.s.SetReviewRunPhase(f.ctx, running.ID, "", store.RunPhaseFinishing); err != nil {
		t.Fatalf("SetReviewRunPhase(finishing) error = %v", err)
	}
	if err := f.s.CompleteReviewRun(f.ctx, running.ID, "", store.RunCompletion{Status: store.RunSucceeded}); err != nil {
		t.Fatalf("CompleteReviewRun() error = %v", err)
	}
	got, _ = f.s.ReviewRun(f.ctx, running.ID)
	if got.Phase != "" || got.PhaseStartedAt != nil {
		t.Errorf("finished run kept its phase: %q/%v", got.Phase, got.PhaseStartedAt)
	}
	if err := f.s.SetReviewRunPhase(f.ctx, running.ID, "", store.RunPhasePreparing); !errors.Is(err, store.ErrRunNotActive) {
		t.Errorf("SetReviewRunPhase(finished) error = %v, want ErrRunNotActive", err)
	}
}

func TestListActiveReviewRunsScopesAndOrders(t *testing.T) {
	f := newFixture(t)
	instA := f.nextID()
	f.installation(instA)
	instB := f.nextID()
	f.installation(instB)
	teamA := f.team("alpha")
	teamB := f.team("beta")
	repoA := f.repo(instA, instA, "acme/a", &teamA.ID, true)
	repoB := f.repo(instB, instB, "acme/b", &teamB.ID, true)
	devA := f.user("deva", false)
	f.member(teamA, devA, "developer")
	admin := f.user("root", true)

	prA := f.openPR(repoA.ID, 1, store.StatusPending, false)
	prB := f.openPR(repoB.ID, 2, store.StatusPending, false)
	queued := f.run(prA, store.RunQueued)
	running := f.run(prB, store.RunRunning)
	if err := f.s.SetReviewRunPhase(f.ctx, running.ID, "", store.RunPhaseReviewing); err != nil {
		t.Fatalf("SetReviewRunPhase() error = %v", err)
	}

	// A closed PR must not keep an active run in the list.
	closedPR := f.openPR(repoA.ID, 3, store.StatusPending, false)
	f.run(closedPR, store.RunQueued)
	if err := f.s.DB.WithContext(f.ctx).Exec("UPDATE pull_requests SET state = 'closed' WHERE id = ?", closedPR.ID).Error; err != nil {
		t.Fatalf("close pull request: %v", err)
	}

	runs, err := f.s.ListActiveReviewRuns(f.ctx, devA)
	if err != nil {
		t.Fatalf("ListActiveReviewRuns(dev) error = %v", err)
	}
	if len(runs) != 1 || runs[0].ID != queued.ID {
		t.Fatalf("dev runs = %+v, want only %d", runs, queued.ID)
	}
	if runs[0].Repo != "acme/a" || runs[0].PRNumber != 1 || runs[0].Team != teamA.Name {
		t.Errorf("dev run = %+v, want repo acme/a, PR 1, team alpha", runs[0])
	}

	runs, err = f.s.ListActiveReviewRuns(f.ctx, admin)
	if err != nil {
		t.Fatalf("ListActiveReviewRuns(admin) error = %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("admin runs = %+v, want the queued and running runs", runs)
	}
	if runs[0].ID != running.ID || runs[0].Phase != store.RunPhaseReviewing || runs[0].PhaseStartedAt == nil {
		t.Errorf("first admin run = %+v, want the running run first with its phase", runs[0])
	}
	if runs[1].ID != queued.ID {
		t.Errorf("second admin run = %+v, want the queued run", runs[1])
	}

	if err := f.s.CompleteReviewRun(f.ctx, running.ID, "", store.RunCompletion{Status: store.RunSucceeded}); err != nil {
		t.Fatalf("CompleteReviewRun() error = %v", err)
	}
	runs, _ = f.s.ListActiveReviewRuns(f.ctx, admin)
	if len(runs) != 1 || runs[0].ID != queued.ID {
		t.Errorf("runs after completion = %+v, want only the queued run", runs)
	}
}

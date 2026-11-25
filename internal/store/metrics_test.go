package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

func TestQueueStatsCountsRiverStates(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()

	insert := func(state, kind string, scheduledAt time.Time, finalized bool) {
		t.Helper()
		var finalizedAt any
		if finalized {
			finalizedAt = time.Now()
		}
		if _, err := s.Pool.Exec(ctx, `
			INSERT INTO river_job (state, kind, args, max_attempts, priority, queue, attempted_at, scheduled_at, finalized_at)
			VALUES ($1, $2, '{}'::jsonb, 25, 1, 'review', now(), $3, $4)`,
			state, kind, scheduledAt, finalizedAt); err != nil {
			t.Fatalf("insert %s job: %v", state, err)
		}
	}

	insert("available", "stats_available_a", time.Now().Add(-time.Hour), false)
	insert("available", "stats_available_b", time.Now(), false)
	insert("running", "stats_running", time.Now(), false)
	insert("scheduled", "stats_scheduled", time.Now(), false)
	insert("discarded", "stats_discarded", time.Now(), true)

	stats, err := s.QueueStats(ctx)
	if err != nil {
		t.Fatalf("QueueStats: %v", err)
	}
	if stats.Available != 2 {
		t.Errorf("available = %d, want 2", stats.Available)
	}
	if stats.Running != 1 {
		t.Errorf("running = %d, want 1", stats.Running)
	}
	if stats.Scheduled != 1 {
		t.Errorf("scheduled = %d, want 1", stats.Scheduled)
	}
	if stats.Discarded != 1 {
		t.Errorf("discarded = %d, want 1", stats.Discarded)
	}
	// The oldest available job was queued an hour ago, which is what tells an
	// idle worker apart from a stalled one.
	if stats.OldestAvailableSeconds < 3500 {
		t.Errorf("oldest available = %ds, want about an hour", stats.OldestAvailableSeconds)
	}
}

func TestQueueStatsWithoutJobs(t *testing.T) {
	s := testutil.NewStore(t)
	stats, err := s.QueueStats(context.Background())
	if err != nil {
		t.Fatalf("QueueStats: %v", err)
	}
	if stats != (store.QueueStats{}) {
		t.Errorf("stats = %+v, want zero value", stats)
	}
}

// TestStageStatsObservesEachStageSeparately covers #154: a running sync job
// must not hide an old publish backlog, and running review phases are reported
// by their own age and heartbeat.
func TestStageStatsObservesEachStageSeparately(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()

	insertJob := func(queue, kind, state string, scheduledAt time.Time) {
		t.Helper()
		var finalizedAt any
		if state == "discarded" {
			finalizedAt = time.Now()
		}
		if _, err := s.Pool.Exec(ctx, `
			INSERT INTO river_job (state, kind, args, max_attempts, priority, queue, attempted_at, scheduled_at, finalized_at)
			VALUES ($1, $2, '{}'::jsonb, 25, 1, $3, now(), $4, $5)`,
			state, kind, queue, scheduledAt, finalizedAt); err != nil {
			t.Fatalf("insert %s/%s job: %v", queue, state, err)
		}
	}

	// An active sync on the control queue is exactly what used to mask a
	// stopped publish queue in the aggregate report.
	insertJob("control", "sync_installation", "running", time.Now())
	insertJob("review", "review_run", "available", time.Now().Add(-2*time.Hour))
	insertJob("review", "review_run", "running", time.Now())
	insertJob("review", "review_run", "retryable", time.Now())
	insertJob("publish", "publish_review", "available", time.Now().Add(-3*time.Hour))
	insertJob("publish", "publish_review", "available", time.Now().Add(-2*time.Hour))
	insertJob("publish", "publish_review", "available", time.Now().Add(-time.Hour))
	insertJob("publish", "publish_review", "retryable", time.Now())
	insertJob("publish", "publish_review", "discarded", time.Now())

	team := store.Team{Name: "stages", Slug: "stages"}
	if err := s.DB.WithContext(ctx).Create(&team).Error; err != nil {
		t.Fatalf("create team: %v", err)
	}
	installation := store.Installation{ID: 9911, AccountLogin: "stages", AccountType: "Organization"}
	if err := s.DB.WithContext(ctx).Create(&installation).Error; err != nil {
		t.Fatalf("create installation: %v", err)
	}
	teamID := team.ID
	repo := store.Repository{
		ID: 9912, InstallationID: installation.ID, FullName: "stages/repo",
		DefaultBranch: "main", Enabled: true, TeamID: &teamID,
	}
	if err := s.DB.WithContext(ctx).Create(&repo).Error; err != nil {
		t.Fatalf("create repository: %v", err)
	}

	insertRun := func(number int, phase string, started, heartbeat time.Time, logAt *time.Time) {
		t.Helper()
		pr := store.PullRequest{
			RepositoryID: repo.ID, Number: number, Title: "stage", HeadSHA: "sha",
			BaseRef: "main", State: "open", QAStatus: store.StatusPending,
			GitHubUpdatedAt: time.Now(),
		}
		if err := s.DB.WithContext(ctx).Create(&pr).Error; err != nil {
			t.Fatalf("create pull request: %v", err)
		}
		run, err := s.CreateReviewRun(ctx, store.ReviewRun{
			PullRequestID: pr.ID, HeadSHA: "sha", Status: store.RunRunning,
			Phase: phase, PhaseStartedAt: &started, StartedAt: &started, HeartbeatAt: &heartbeat,
		})
		if err != nil {
			t.Fatalf("create running run: %v", err)
		}
		if logAt != nil {
			if _, err := s.Pool.Exec(ctx, `
				INSERT INTO review_run_logs (run_id, seq, ts, source, phase, line)
				VALUES ($1, 1, $2, 'worker', $3, 'still working')`,
				run.ID, *logAt, phase); err != nil {
				t.Fatalf("insert run log: %v", err)
			}
		}
	}
	// The clone run logged this second: a real zero age must survive even
	// though its phase started an hour ago.
	cloneLog := time.Now()
	insertRun(1, store.RunPhaseCloning, time.Now().Add(-time.Hour), time.Now().Add(-time.Second), &cloneLog)
	// This run keeps heartbeating but writes no log: its progress is the phase
	// start, which is what a hung phase looks like.
	insertRun(2, store.RunPhaseReviewing, time.Now().Add(-2*time.Hour), time.Now().Add(-10*time.Minute), nil)
	// An older log than the phase start must not hide the newer phase start:
	// progress is the newest activity, not the newest log.
	oldLog := time.Now().Add(-2 * time.Hour)
	insertRun(3, store.RunPhaseVerifying, time.Now().Add(-10*time.Minute), time.Now().Add(-time.Minute), &oldLog)

	stages, err := s.StageStats(ctx)
	if err != nil {
		t.Fatalf("StageStats: %v", err)
	}
	byName := map[string]store.StageStats{}
	for _, stage := range stages {
		byName[stage.Name] = stage
	}
	if len(byName) != 5 {
		t.Fatalf("stages = %+v, want queue, clone, review, verify and publish", stages)
	}

	queue := byName[store.StageQueue]
	if queue.Available != 1 || queue.Running != 1 || queue.Retryable != 1 {
		t.Errorf("queue stage = %+v, want 1 available, 1 running and 1 retryable", queue)
	}
	if queue.OldestPendingSeconds < 7100 {
		t.Errorf("queue oldest pending = %ds, want about 2h", queue.OldestPendingSeconds)
	}

	if publish := byName[store.StagePublish]; publish.Available != 3 || publish.Running != 0 ||
		publish.Retryable != 1 || publish.Discarded != 1 {
		t.Errorf("publish stage = %+v, want 3 available, no running, 1 retryable and 1 discarded", publish)
	} else if publish.OldestPendingSeconds < 10700 {
		t.Errorf("publish oldest pending = %ds, want about 3h", publish.OldestPendingSeconds)
	}

	clone := byName[store.StageClone]
	if clone.Running != 1 || clone.OldestPendingSeconds < 3500 || clone.LastProgressSeconds > 5 {
		t.Errorf("clone stage = %+v, want 1 run started an hour ago but logging this second", clone)
	}
	review := byName[store.StageReview]
	if review.Running != 1 || review.OldestPendingSeconds < 7100 || review.LastProgressSeconds < 7100 {
		t.Errorf("review stage = %+v, want 1 run with no log for two hours despite its heartbeat", review)
	}
	verify := byName[store.StageVerify]
	if verify.Running != 1 || verify.LastProgressSeconds < 500 || verify.LastProgressSeconds > 1200 {
		t.Errorf("verify stage = %+v, want progress from the 10m-old phase start, not the 2h-old log", verify)
	}
}

func TestRunStatsSinceAggregatesFinishedRuns(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()

	team := store.Team{Name: "metrics", Slug: "metrics"}
	if err := s.DB.WithContext(ctx).Create(&team).Error; err != nil {
		t.Fatalf("create team: %v", err)
	}
	installation := store.Installation{ID: 9901, AccountLogin: "metrics", AccountType: "Organization"}
	if err := s.DB.WithContext(ctx).Create(&installation).Error; err != nil {
		t.Fatalf("create installation: %v", err)
	}
	teamID := team.ID
	repo := store.Repository{
		ID: 9902, InstallationID: installation.ID, FullName: "metrics/repo",
		DefaultBranch: "main", Enabled: true, TeamID: &teamID,
	}
	if err := s.DB.WithContext(ctx).Create(&repo).Error; err != nil {
		t.Fatalf("create repository: %v", err)
	}
	pr := store.PullRequest{
		RepositoryID: repo.ID, Number: 1, Title: "metrics", HeadSHA: "sha",
		BaseRef: "main", State: "open", QAStatus: store.StatusPending,
		GitHubUpdatedAt: time.Now(),
	}
	if err := s.DB.WithContext(ctx).Create(&pr).Error; err != nil {
		t.Fatalf("create pull request: %v", err)
	}
	profile, err := s.CreateLLMProfile(ctx, store.LLMProfile{
		Name: "metrics", Provider: "openai", Model: "gpt-4o-mini", APIKeyEnc: "sealed", Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}

	finished := time.Now().Add(-time.Minute)
	insertRun := func(status string, started time.Time, done *time.Time) {
		t.Helper()
		run, err := s.CreateReviewRun(ctx, store.ReviewRun{
			PullRequestID: pr.ID, ProfileID: profile.ID, HeadSHA: "sha", Status: status,
			CloneTokenEnc: "sealed", StartedAt: &started, FinishedAt: done,
		})
		if err != nil {
			t.Fatalf("CreateReviewRun(%s): %v", status, err)
		}
		if run.ID == 0 {
			t.Fatal("run id was not assigned")
		}
	}

	insertRun(store.RunSucceeded, finished.Add(-4*time.Minute), &finished)
	insertRun(store.RunSucceeded, finished.Add(-2*time.Minute), &finished)
	insertRun(store.RunFailed, finished.Add(-30*time.Second), &finished)
	insertRun(store.RunInconclusive, finished.Add(-10*time.Second), &finished)
	// Outside the window and unfinished runs must not count.
	old := finished.Add(-48 * time.Hour)
	insertRun(store.RunSucceeded, old, &old)
	insertRun(store.RunRunning, finished, nil)

	stats, err := s.RunStatsSince(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("RunStatsSince: %v", err)
	}
	if stats.Succeeded != 2 {
		t.Errorf("succeeded = %d, want 2", stats.Succeeded)
	}
	if stats.Failed != 1 {
		t.Errorf("failed = %d, want 1", stats.Failed)
	}
	if stats.Inconclusive != 1 {
		t.Errorf("inconclusive = %d, want 1", stats.Inconclusive)
	}
	if stats.DurationP95Seconds != 240 {
		t.Errorf("p95 = %ds, want 240s", stats.DurationP95Seconds)
	}
}

func TestRunStatsSinceWithoutRuns(t *testing.T) {
	s := testutil.NewStore(t)
	stats, err := s.RunStatsSince(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("RunStatsSince: %v", err)
	}
	if stats != (store.RunStats{}) {
		t.Errorf("stats = %+v, want zero value", stats)
	}
}

func TestPingReportsDatabaseState(t *testing.T) {
	s := testutil.NewStore(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

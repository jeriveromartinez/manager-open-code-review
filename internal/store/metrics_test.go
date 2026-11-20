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

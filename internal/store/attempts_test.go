package store_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"gorm.io/datatypes"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestProfileConfigRevisionTracksDestination(t *testing.T) {
	base := store.LLMProfile{ID: 1, Provider: "openai", Model: "gpt-a"}
	revision := store.ProfileConfigRevision(base)
	if revision == "" {
		t.Fatal("ProfileConfigRevision() is empty")
	}
	if got := store.ProfileConfigRevision(base); got != revision {
		t.Errorf("revision is not stable: %q != %q", got, revision)
	}

	renamed := base
	renamed.Name = "other-name"
	if got := store.ProfileConfigRevision(renamed); got != revision {
		t.Error("revision changed with the display name")
	}

	for name, mutate := range map[string]func(*store.LLMProfile){
		"provider": func(p *store.LLMProfile) { p.Provider = "anthropic" },
		"model":    func(p *store.LLMProfile) { p.Model = "gpt-b" },
		"key":      func(p *store.LLMProfile) { p.APIKeyEnc = "sealed-next" },
		"base_url": func(p *store.LLMProfile) { url := "https://proxy.example/v1"; p.BaseURL = &url },
	} {
		changed := base
		mutate(&changed)
		if store.ProfileConfigRevision(changed) == revision {
			t.Errorf("revision did not change with %s", name)
		}
	}
}

func TestUpdateLLMProfileInvalidatesHealthOnTargetChange(t *testing.T) {
	f := newFixture(t)
	profile := f.llmProfile("health-profile", true)
	if err := f.s.SetLLMHealth(f.ctx, profile.ID, store.HealthOK, ""); err != nil {
		t.Fatalf("SetLLMHealth: %v", err)
	}

	// A cosmetic change keeps the verdict.
	current, err := f.s.LLMProfile(f.ctx, profile.ID)
	if err != nil {
		t.Fatalf("LLMProfile: %v", err)
	}
	current.Name = "renamed"
	if err := f.s.UpdateLLMProfile(f.ctx, current); err != nil {
		t.Fatalf("UpdateLLMProfile(name): %v", err)
	}
	if health := loadHealth(t, f.s, profile.ID); health.HealthStatus != store.HealthOK || health.HealthCheckedAt == nil {
		t.Fatalf("health after a rename = %+v, want the previous verdict kept", health)
	}

	for name, mutate := range map[string]func(*store.LLMProfile){
		"model":    func(p *store.LLMProfile) { p.Model = "claude-next" },
		"provider": func(p *store.LLMProfile) { p.Provider = "openai" },
		"key":      func(p *store.LLMProfile) { p.APIKeyEnc = "sealed-rotated" },
		"base_url": func(p *store.LLMProfile) { url := "https://proxy.example/v1"; p.BaseURL = &url },
	} {
		t.Run(name, func(t *testing.T) {
			loaded, err := f.s.LLMProfile(f.ctx, profile.ID)
			if err != nil {
				t.Fatalf("LLMProfile: %v", err)
			}
			if err := f.s.SetLLMHealth(f.ctx, profile.ID, store.HealthOK, ""); err != nil {
				t.Fatalf("SetLLMHealth: %v", err)
			}
			mutate(&loaded)
			if err := f.s.UpdateLLMProfile(f.ctx, loaded); err != nil {
				t.Fatalf("UpdateLLMProfile: %v", err)
			}
			health := loadHealth(t, f.s, profile.ID)
			if health.HealthStatus != store.HealthUnknown || health.HealthCheckedAt != nil || health.HealthError != "" {
				t.Fatalf("health after changing %s = %+v, want unknown/invalidated", name, health)
			}
			// Restore a clean baseline for the next subtest.
			restored, err := f.s.LLMProfile(f.ctx, profile.ID)
			if err != nil {
				t.Fatalf("LLMProfile: %v", err)
			}
			restored.Provider = "anthropic"
			restored.Model = "claude-test"
			restored.BaseURL = nil
			if err := f.s.UpdateLLMProfile(f.ctx, restored); err != nil {
				t.Fatalf("restore profile: %v", err)
			}
		})
	}
}

func loadHealth(t *testing.T, s *store.Store, id uint) store.LLMProfile {
	t.Helper()
	profile, err := s.LLMProfile(context.Background(), id)
	if err != nil {
		t.Fatalf("LLMProfile(%d): %v", id, err)
	}
	return profile
}

func TestRecordLLMHealthAtRejectsStaleRevision(t *testing.T) {
	f := newFixture(t)
	profile := f.llmProfile("stale-health", true)
	revision := store.ProfileConfigRevision(profile)

	profile.Model = "claude-next"
	if err := f.s.UpdateLLMProfile(f.ctx, profile); err != nil {
		t.Fatalf("UpdateLLMProfile: %v", err)
	}

	err := f.s.RecordLLMHealthAt(f.ctx, profile.ID, revision, store.HealthOK, "", nil)
	if !errors.Is(err, store.ErrStaleHealthCheck) {
		t.Fatalf("RecordLLMHealthAt(stale) error = %v, want ErrStaleHealthCheck", err)
	}
	if health := loadHealth(t, f.s, profile.ID); health.HealthStatus != store.HealthUnknown {
		t.Fatalf("health_status = %q, want the stale verdict discarded", health.HealthStatus)
	}
	requireCount(t, f.s.DB.WithContext(f.ctx), "llm_health_checks", 0)

	current, err := f.s.LLMProfile(f.ctx, profile.ID)
	if err != nil {
		t.Fatalf("LLMProfile: %v", err)
	}
	if err := f.s.RecordLLMHealthAt(f.ctx, profile.ID, store.ProfileConfigRevision(current), store.HealthOK, "", nil); err != nil {
		t.Fatalf("RecordLLMHealthAt(current): %v", err)
	}
	if health := loadHealth(t, f.s, profile.ID); health.HealthStatus != store.HealthOK {
		t.Fatalf("health_status = %q, want %q", health.HealthStatus, store.HealthOK)
	}
}

func TestFallbackProfileIDsAreOrderedAndAuthorized(t *testing.T) {
	f := newFixture(t)
	team := f.team("qa")
	primary := f.llmProfile("primary", true)
	first := f.llmProfile("first-backup", true)
	second := f.llmProfile("second-backup", true)

	raw := datatypes.JSON([]byte(`[
		` + uintJSON(second.ID) + `,
		` + uintJSON(first.ID) + `,
		` + uintJSON(second.ID) + `,
		0
	]`))
	saved, err := f.s.SaveQAPolicy(f.ctx, store.QAPolicy{
		TeamID: team.ID, LLMProfileID: primary.ID, FallbackProfileIDs: raw,
	})
	if err != nil {
		t.Fatalf("SaveQAPolicy: %v", err)
	}
	want := []uint{second.ID, first.ID}
	if got := store.FallbackProfileIDs(saved); !reflect.DeepEqual(got, want) {
		t.Fatalf("FallbackProfileIDs = %v, want %v", got, want)
	}
	loaded, err := f.s.QAPolicyForTeam(f.ctx, team.ID)
	if err != nil {
		t.Fatalf("QAPolicyForTeam: %v", err)
	}
	if got := store.FallbackProfileIDs(loaded); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored fallback ids = %v, want %v", got, want)
	}

	// A profile authorized as a fallback is still referenced: deleting it would
	// silently drop the backup.
	if err := f.s.DeleteLLMProfile(f.ctx, first.ID); !errors.Is(err, store.ErrProfileInUse) {
		t.Fatalf("DeleteLLMProfile(fallback) error = %v, want ErrProfileInUse", err)
	}
}

func TestReviewAttemptsAttributeExecutionProfileAndUsage(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	enqueued := f.llmProfile("enqueued", true)
	executed := f.llmProfile("executed-fallback", true)
	repo := f.repo(installID, installID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)

	run, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, ProfileID: enqueued.ID, HeadSHA: pr.HeadSHA,
	})
	if err != nil {
		t.Fatalf("CreateReviewRun: %v", err)
	}
	claimed, ok, err := f.s.ClaimReviewRun(f.ctx, run.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimReviewRun = %v/%v", ok, err)
	}

	first, err := f.s.StartReviewAttempt(f.ctx, run.ID, "", executed, "rev-1")
	if err != nil {
		t.Fatalf("StartReviewAttempt(first): %v", err)
	}
	current, err := f.s.ReviewRun(f.ctx, run.ID)
	if err != nil {
		t.Fatalf("ReviewRun: %v", err)
	}
	if current.EnqueuedProfileID != enqueued.ID || current.ProfileID != executed.ID {
		t.Fatalf("run profiles = enqueued %d/executed %d, want %d/%d",
			current.EnqueuedProfileID, current.ProfileID, enqueued.ID, executed.ID)
	}
	if first.Seq != 1 || first.Provider != executed.Provider || first.Model != executed.Model ||
		first.ConfigRevision != "rev-1" {
		t.Fatalf("first attempt = %+v, want the executed destination and revision", first)
	}
	if err := f.s.FinishReviewAttempt(f.ctx, first.ID, store.AttemptFailed, "llm_error", "partial", &store.RunUsage{
		Input: 80, Output: 20, Total: 100,
	}, true); err != nil {
		t.Fatalf("FinishReviewAttempt(first): %v", err)
	}

	second, err := f.s.StartReviewAttempt(f.ctx, run.ID, "", executed, "rev-2")
	if err != nil {
		t.Fatalf("StartReviewAttempt(second): %v", err)
	}
	if second.Seq != 2 || second.ID == first.ID {
		t.Fatalf("second attempt = %+v, want sequence 2", second)
	}
	if err := f.s.FinishReviewAttempt(f.ctx, second.ID, store.AttemptSucceeded, "", "", &store.RunUsage{
		Input: 40, Output: 10, Total: 50,
	}, true); err != nil {
		t.Fatalf("FinishReviewAttempt(second): %v", err)
	}

	if err := f.s.CompleteReviewRun(f.ctx, claimed.ID, "", store.RunCompletion{
		Status: store.RunSucceeded, TokensKnown: true, Tokens: store.RunUsage{Total: 150},
	}); err != nil {
		t.Fatalf("CompleteReviewRun: %v", err)
	}

	stats, err := f.s.ProfileTokenUsage(f.ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("ProfileTokenUsage: %v", err)
	}
	byID := map[uint]store.ProfileTokenStats{}
	for _, entry := range stats {
		byID[entry.ProfileID] = entry
	}
	if entry := byID[enqueued.ID]; entry.HasData() {
		t.Errorf("enqueued profile usage = %+v, want none", entry)
	}
	executedStats := byID[executed.ID]
	if !executedStats.HasData() || executedStats.Runs != 1 || executedStats.Total != 150 {
		t.Fatalf("executed profile usage = %+v, want one run and 150 tokens", executedStats)
	}
}

func TestCompleteReviewRunClosesOpenAttempt(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	profile := f.llmProfile("open-attempt", true)
	repo := f.repo(installID, installID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)
	run, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, ProfileID: profile.ID, HeadSHA: pr.HeadSHA,
	})
	if err != nil {
		t.Fatalf("CreateReviewRun: %v", err)
	}
	if _, ok, err := f.s.ClaimReviewRun(f.ctx, run.ID); err != nil || !ok {
		t.Fatalf("ClaimReviewRun = %v/%v", ok, err)
	}
	attempt, err := f.s.StartReviewAttempt(f.ctx, run.ID, "", profile, "rev")
	if err != nil {
		t.Fatalf("StartReviewAttempt: %v", err)
	}
	if err := f.s.CompleteReviewRun(f.ctx, run.ID, "", store.RunCompletion{Status: store.RunFailed}); err != nil {
		t.Fatalf("CompleteReviewRun: %v", err)
	}

	var got store.ReviewAttempt
	if err := f.s.DB.WithContext(f.ctx).First(&got, attempt.ID).Error; err != nil {
		t.Fatalf("load attempt: %v", err)
	}
	if got.Status != store.AttemptFailed || got.ErrorCode != "interrupted" || got.FinishedAt == nil {
		t.Fatalf("attempt = %+v, want it closed as interrupted", got)
	}
}

func TestProfileTokenUsageKeepsRunTokensWhenAttemptsAreUnknown(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	profile := f.llmProfile("unknown-attempt", true)
	repo := f.repo(installID, installID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)
	run, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, ProfileID: profile.ID, HeadSHA: pr.HeadSHA,
	})
	if err != nil {
		t.Fatalf("CreateReviewRun: %v", err)
	}
	if _, ok, err := f.s.ClaimReviewRun(f.ctx, run.ID); err != nil || !ok {
		t.Fatalf("ClaimReviewRun = %v/%v", ok, err)
	}
	attempt, err := f.s.StartReviewAttempt(f.ctx, run.ID, "", profile, "rev")
	if err != nil {
		t.Fatalf("StartReviewAttempt: %v", err)
	}
	if err := f.s.FinishReviewAttempt(f.ctx, attempt.ID, store.AttemptFailed,
		"interrupted", "", nil, false); err != nil {
		t.Fatalf("FinishReviewAttempt: %v", err)
	}
	if err := f.s.CompleteReviewRun(f.ctx, run.ID, "", store.RunCompletion{
		Status: store.RunFailed, TokensKnown: true, Tokens: store.RunUsage{Total: 77},
	}); err != nil {
		t.Fatalf("CompleteReviewRun: %v", err)
	}

	stats, err := f.s.ProfileTokenUsage(f.ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("ProfileTokenUsage: %v", err)
	}
	for _, entry := range stats {
		if entry.ProfileID != profile.ID {
			continue
		}
		if !entry.HasData() || entry.Total != 77 || entry.Runs != 1 {
			t.Fatalf("usage = %+v, want the run's known 77 tokens counted once", entry)
		}
		return
	}
	t.Fatalf("profile %d missing from usage", profile.ID)
}

func uintJSON(id uint) string {
	return fmt.Sprintf("%d", id)
}

func TestStaleLLMProfilesIncludesAuthorizedFallbacks(t *testing.T) {
	f := newFixture(t)
	team := f.team("qa")
	primary := f.llmProfile("primary-stale", true)
	backup := f.llmProfile("backup-stale", true)
	unreferenced := f.llmProfile("unreferenced-stale", true)

	fallbacks, err := store.NormalizeFallbackProfileIDs([]uint{backup.ID})
	if err != nil {
		t.Fatalf("normalize fallback ids: %v", err)
	}
	if _, err := f.s.SaveQAPolicy(f.ctx, store.QAPolicy{
		TeamID: team.ID, LLMProfileID: primary.ID, FallbackProfileIDs: fallbacks,
	}); err != nil {
		t.Fatalf("SaveQAPolicy: %v", err)
	}

	profiles, err := f.s.StaleLLMProfiles(f.ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("StaleLLMProfiles: %v", err)
	}
	got := map[uint]bool{}
	for _, profile := range profiles {
		got[profile.ID] = true
	}
	if !got[primary.ID] || !got[backup.ID] {
		t.Fatalf("stale profiles = %v, want the primary and the fallback", got)
	}
	if got[unreferenced.ID] {
		t.Errorf("stale profiles include the unreferenced profile")
	}
}

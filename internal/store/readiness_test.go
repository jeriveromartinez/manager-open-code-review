package store_test

import (
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const readinessStaleAfter = time.Hour

func createReadinessProfile(t *testing.T, f *fixture, name string, enabled bool, status string, checkedAt *time.Time) store.LLMProfile {
	t.Helper()
	profile := store.LLMProfile{
		Name:            name,
		Provider:        "anthropic",
		Model:           "claude-sonnet",
		Mode:            "api_key",
		Enabled:         enabled,
		HealthStatus:    status,
		HealthCheckedAt: checkedAt,
	}
	if err := f.s.DB.WithContext(f.ctx).Create(&profile).Error; err != nil {
		t.Fatalf("create llm profile %q: %v", name, err)
	}
	return profile
}

// seedReadyEngine leaves every link complete: an enabled repo with a team, a
// team with a policy and a recently checked healthy profile.
func seedReadyEngine(t *testing.T, f *fixture) store.Team {
	t.Helper()
	team := f.team("readiness")
	checkedAt := time.Now().UTC()
	profile := createReadinessProfile(t, f, "ready-profile", true, store.HealthOK, &checkedAt)
	if err := f.s.DB.WithContext(f.ctx).Create(&store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID,
	}).Error; err != nil {
		t.Fatalf("create qa policy: %v", err)
	}
	installationID := f.nextID()
	f.installation(installationID)
	f.repo(f.nextID(), installationID, "acme/ready", &team.ID, true)
	return team
}

func TestReadinessReportAllComplete(t *testing.T) {
	f := newFixture(t)
	seedReadyEngine(t, f)

	report, err := f.s.ReadinessReport(f.ctx, readinessStaleAfter)
	if err != nil {
		t.Fatalf("ReadinessReport: %v", err)
	}
	if report.ReposWithoutTeam != 0 || report.TeamsWithoutPolicy != 0 || report.UnhealthyProfiles != 0 {
		t.Errorf("report = %+v, want every count zero", report)
	}
	if report.NoTeams || report.NoProfiles {
		t.Errorf("report = %+v, want teams and profiles present", report)
	}
}

func TestReadinessReportEmptyInstance(t *testing.T) {
	f := newFixture(t)

	report, err := f.s.ReadinessReport(f.ctx, readinessStaleAfter)
	if err != nil {
		t.Fatalf("ReadinessReport: %v", err)
	}
	if !report.NoTeams || !report.NoProfiles {
		t.Errorf("report = %+v, want NoTeams and NoProfiles", report)
	}
	if report.ReposWithoutTeam != 0 || report.TeamsWithoutPolicy != 0 || report.UnhealthyProfiles != 0 {
		t.Errorf("report = %+v, want zero counts on an empty instance", report)
	}
}

func TestReadinessReportReposWithoutTeam(t *testing.T) {
	f := newFixture(t)
	seedReadyEngine(t, f)
	installationID := f.nextID()
	f.installation(installationID)
	f.repo(f.nextID(), installationID, "acme/orphan", nil, true)

	report, err := f.s.ReadinessReport(f.ctx, readinessStaleAfter)
	if err != nil {
		t.Fatalf("ReadinessReport: %v", err)
	}
	if report.ReposWithoutTeam != 1 {
		t.Errorf("ReposWithoutTeam = %d, want 1", report.ReposWithoutTeam)
	}
	if report.TeamsWithoutPolicy != 0 || report.UnhealthyProfiles != 0 {
		t.Errorf("report = %+v, want only the repo gap", report)
	}
}

func TestReadinessReportTeamsWithoutPolicy(t *testing.T) {
	f := newFixture(t)
	seedReadyEngine(t, f)
	f.team("policy-less")

	report, err := f.s.ReadinessReport(f.ctx, readinessStaleAfter)
	if err != nil {
		t.Fatalf("ReadinessReport: %v", err)
	}
	if report.TeamsWithoutPolicy != 1 {
		t.Errorf("TeamsWithoutPolicy = %d, want 1", report.TeamsWithoutPolicy)
	}
	if report.ReposWithoutTeam != 0 || report.UnhealthyProfiles != 0 {
		t.Errorf("report = %+v, want only the policy gap", report)
	}
}

func TestReadinessReportUnhealthyProfiles(t *testing.T) {
	f := newFixture(t)
	seedReadyEngine(t, f)

	now := time.Now().UTC()
	recentError := now.Add(-time.Minute)
	staleOK := now.Add(-2 * time.Hour)
	createReadinessProfile(t, f, "recent-error", true, store.HealthError, &recentError)
	createReadinessProfile(t, f, "never-checked", true, store.HealthUnknown, nil)
	createReadinessProfile(t, f, "stale-ok", true, store.HealthOK, &staleOK)
	createReadinessProfile(t, f, "disabled-error", false, store.HealthError, &recentError)
	createReadinessProfile(t, f, "fresh-ok", true, store.HealthOK, &recentError)

	report, err := f.s.ReadinessReport(f.ctx, readinessStaleAfter)
	if err != nil {
		t.Fatalf("ReadinessReport: %v", err)
	}
	if report.UnhealthyProfiles != 3 {
		t.Errorf("UnhealthyProfiles = %d, want 3 (error, never checked, stale)", report.UnhealthyProfiles)
	}
	if report.ReposWithoutTeam != 0 || report.TeamsWithoutPolicy != 0 {
		t.Errorf("report = %+v, want only the profile gap", report)
	}
}

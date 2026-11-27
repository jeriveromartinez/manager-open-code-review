package store_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func (f *fixture) profile(name string) store.LLMProfile {
	f.t.Helper()

	profile := store.LLMProfile{
		Name: name, Provider: "anthropic", Model: "claude-test", Mode: "api_key",
		APIKeyEnc: "sealed", Concurrency: 1, Enabled: true, HealthStatus: store.HealthUnknown,
	}
	if err := f.s.DB.WithContext(f.ctx).Create(&profile).Error; err != nil {
		f.t.Fatalf("create profile %q: %v", name, err)
	}
	return profile
}

func (f *fixture) teamPolicy(team store.Team, profile store.LLMProfile) store.QAPolicy {
	f.t.Helper()

	policy, err := f.s.SaveQAPolicy(f.ctx, store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID,
		BlockingThreshold: "major", MaxFilesPerPass: 40, MaxDiffLinesPerPass: 3000,
		MaxPasses: 4, ReviewEffort: "low", ExtraInstructions: "team rules",
		ExcludeGlobs: []byte(`["vendor/**"]`),
	})
	if err != nil {
		f.t.Fatalf("SaveQAPolicy: %v", err)
	}
	return policy
}

func stringPtr(v string) *string { return &v }
func boolPtr(v bool) *bool       { return &v }

func TestResolvePolicyAppliesCriticalGlobsOverride(t *testing.T) {
	f := newFixture(t)
	installationID := f.nextID()
	f.installation(installationID)
	team := f.team("critical-areas")
	profile := f.profile("primary")
	teamPolicy, err := f.s.SaveQAPolicy(f.ctx, store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID,
		BlockingThreshold: "major", MaxFilesPerPass: 40, MaxDiffLinesPerPass: 3000,
		MaxPasses: 4, ReviewEffort: "low",
		CriticalGlobs: []byte(`["internal/store/**"]`),
	})
	if err != nil {
		t.Fatalf("SaveQAPolicy: %v", err)
	}
	teamID := team.ID
	repo := f.repo(201, installationID, "acme/payments", &teamID, true)

	if _, err := f.s.SaveRepositoryPolicy(f.ctx, store.RepositoryPolicy{
		RepositoryID:  repo.ID,
		CriticalGlobs: []byte(`["internal/payments/**"]`),
	}); err != nil {
		t.Fatalf("SaveRepositoryPolicy: %v", err)
	}

	resolved, err := f.s.ResolveReviewPolicy(f.ctx, repo.ID)
	if err != nil {
		t.Fatalf("ResolveReviewPolicy: %v", err)
	}
	if string(resolved.Effective.CriticalGlobs) != `["internal/payments/**"]` {
		t.Errorf("effective critical globs = %s, want the repository override", resolved.Effective.CriticalGlobs)
	}
	if resolved.Provenance["critical_globs"] != store.PolicySourceRepo {
		t.Errorf("critical_globs provenance = %q, want repo", resolved.Provenance["critical_globs"])
	}
	if resolved.Revision == store.PolicyRevision(teamPolicy, false) {
		t.Error("revision did not change with the critical globs override")
	}

	if _, err := f.s.SaveRepositoryPolicy(f.ctx, store.RepositoryPolicy{
		RepositoryID:  repo.ID,
		CriticalGlobs: []byte(`not-json`),
	}); !errors.Is(err, store.ErrInvalidPolicy) {
		t.Errorf("invalid critical globs error = %v, want ErrInvalidPolicy", err)
	}
}

func TestResolvePolicyAppliesDifferentOverridesPerRepository(t *testing.T) {
	f := newFixture(t)
	installationID := f.nextID()
	f.installation(installationID)
	team := f.team("multi-stack")
	profile := f.profile("primary")
	f.teamPolicy(team, profile)
	teamID := team.ID
	apiRepo := f.repo(101, installationID, "acme/api", &teamID, true)
	webRepo := f.repo(102, installationID, "acme/web", &teamID, true)

	threshold := "blocker"
	if _, err := f.s.SaveRepositoryPolicy(f.ctx, store.RepositoryPolicy{
		RepositoryID:      apiRepo.ID,
		BlockingThreshold: &threshold, MaxFilesPerPass: intPtr(10),
	}); err != nil {
		t.Fatalf("SaveRepositoryPolicy(api): %v", err)
	}
	checks, err := store.NormalizeRequiredChecks([]store.RequiredCheck{{Name: "build", Origin: "app:7"}})
	if err != nil {
		t.Fatalf("NormalizeRequiredChecks: %v", err)
	}
	if _, err := f.s.SaveRepositoryPolicy(f.ctx, store.RepositoryPolicy{
		RepositoryID: webRepo.ID,
		ReviewEffort: stringPtr("high"), RequireGreenCI: boolPtr(true), RequiredChecks: checks,
	}); err != nil {
		t.Fatalf("SaveRepositoryPolicy(web): %v", err)
	}

	api, err := f.s.ResolveReviewPolicy(f.ctx, apiRepo.ID)
	if err != nil {
		t.Fatalf("ResolveReviewPolicy(api): %v", err)
	}
	web, err := f.s.ResolveReviewPolicy(f.ctx, webRepo.ID)
	if err != nil {
		t.Fatalf("ResolveReviewPolicy(web): %v", err)
	}

	if api.Effective.BlockingThreshold != "blocker" || api.Effective.MaxFilesPerPass != 10 {
		t.Errorf("api effective = threshold %q / files %d, want blocker / 10",
			api.Effective.BlockingThreshold, api.Effective.MaxFilesPerPass)
	}
	// Fields the API repo did not override stay at the team values.
	if api.Effective.ReviewEffort != "low" || api.Effective.MaxPasses != 4 {
		t.Errorf("api inherited fields = effort %q / passes %d, want low / 4",
			api.Effective.ReviewEffort, api.Effective.MaxPasses)
	}
	if web.Effective.ReviewEffort != "high" || !web.Effective.RequireGreenCI {
		t.Errorf("web effective = effort %q / CI %v, want high / true",
			web.Effective.ReviewEffort, web.Effective.RequireGreenCI)
	}
	if web.Effective.BlockingThreshold != "major" || web.Effective.MaxFilesPerPass != 40 {
		t.Errorf("web inherited fields = threshold %q / files %d, want major / 40",
			web.Effective.BlockingThreshold, web.Effective.MaxFilesPerPass)
	}
	if api.Revision == web.Revision {
		t.Errorf("revisions are equal (%q), want one per effective policy", api.Revision)
	}
	if api.Provenance["blocking_threshold"] != store.PolicySourceRepo ||
		api.Provenance["review_effort"] != store.PolicySourceTeam {
		t.Errorf("api provenance = %v, want the threshold from the repo and effort from the team",
			api.Provenance)
	}
	// A nil JSON column is an absent override, not an explicit empty list.
	if string(api.Effective.ExcludeGlobs) != `["vendor/**"]` ||
		api.Provenance["exclude_globs"] != store.PolicySourceTeam {
		t.Errorf("api globs = %s (source %q), want the inherited team globs",
			api.Effective.ExcludeGlobs, api.Provenance["exclude_globs"])
	}
	if web.Provenance["required_checks"] != store.PolicySourceRepo ||
		web.Provenance["max_files"] != store.PolicySourceTeam {
		t.Errorf("web provenance = %v, want checks from the repo and files from the team",
			web.Provenance)
	}
}

func TestDeleteRepositoryPolicyReturnsToInheritance(t *testing.T) {
	f := newFixture(t)
	installationID := f.nextID()
	f.installation(installationID)
	team := f.team("inherit")
	profile := f.profile("primary")
	teamPolicy := f.teamPolicy(team, profile)
	teamID := team.ID
	repo := f.repo(103, installationID, "acme/inherit", &teamID, true)

	if _, err := f.s.SaveRepositoryPolicy(f.ctx, store.RepositoryPolicy{
		RepositoryID: repo.ID, BlockingThreshold: stringPtr("nit"),
	}); err != nil {
		t.Fatalf("SaveRepositoryPolicy: %v", err)
	}
	overridden, err := f.s.ResolveReviewPolicy(f.ctx, repo.ID)
	if err != nil {
		t.Fatalf("ResolveReviewPolicy(overridden): %v", err)
	}
	if overridden.Override == nil || overridden.Effective.BlockingThreshold != "nit" {
		t.Fatalf("resolved = %+v, want an enforced override", overridden)
	}

	if err := f.s.DeleteRepositoryPolicy(f.ctx, repo.ID); err != nil {
		t.Fatalf("DeleteRepositoryPolicy: %v", err)
	}
	if _, err := f.s.RepositoryPolicyFor(f.ctx, repo.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("RepositoryPolicyFor after delete = %v, want ErrNotFound", err)
	}
	inherited, err := f.s.ResolveReviewPolicy(f.ctx, repo.ID)
	if err != nil {
		t.Fatalf("ResolveReviewPolicy(inherited): %v", err)
	}
	if inherited.Override != nil {
		t.Fatalf("override = %+v, want nil after clearing it", inherited.Override)
	}
	if !reflect.DeepEqual(inherited.Effective, teamPolicy) {
		t.Errorf("effective = %+v, want the team policy %+v", inherited.Effective, teamPolicy)
	}
	if inherited.Revision != store.PolicyRevision(teamPolicy, false) {
		t.Errorf("revision = %q, want the team revision", inherited.Revision)
	}
	for field, source := range inherited.Provenance {
		if field == "review_context_ceiling" {
			continue
		}
		if source != store.PolicySourceTeam {
			t.Errorf("provenance[%q] = %q, want team after clearing the override", field, source)
		}
	}
}

func TestObservationModeResolvesOverridesWithoutEnforcingThem(t *testing.T) {
	f := newFixture(t)
	installationID := f.nextID()
	f.installation(installationID)
	team := f.team("observe")
	profile := f.profile("primary")
	f.teamPolicy(team, profile)
	teamID := team.ID
	repo := f.repo(104, installationID, "acme/observe", &teamID, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)

	if _, err := f.s.SaveRepositoryPolicy(f.ctx, store.RepositoryPolicy{
		RepositoryID: repo.ID, ObservationMode: true,
		BlockingThreshold: stringPtr("blocker"), RequireGreenCI: boolPtr(true),
	}); err != nil {
		t.Fatalf("SaveRepositoryPolicy: %v", err)
	}

	resolved, err := f.s.ResolveReviewPolicy(f.ctx, repo.ID)
	if err != nil {
		t.Fatalf("ResolveReviewPolicy: %v", err)
	}
	if resolved.Effective.BlockingThreshold != "blocker" || !resolved.Effective.RequireGreenCI {
		t.Errorf("effective = %+v, want the observed overrides", resolved.Effective)
	}
	if resolved.Enforced.BlockingThreshold != "major" || resolved.Enforced.RequireGreenCI {
		t.Errorf("enforced = %+v, want the team policy untouched", resolved.Enforced)
	}
	if !resolved.Observation {
		t.Error("Observation = false, want true")
	}

	cfg, err := f.s.ReviewConfigForPR(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("ReviewConfigForPR: %v", err)
	}
	if cfg.Policy.BlockingThreshold != "major" || cfg.Policy.RequireGreenCI {
		t.Errorf("enforced policy = %+v, want the team policy while observing", cfg.Policy)
	}
	if cfg.EffectivePolicy.BlockingThreshold != "blocker" || !cfg.EffectivePolicy.RequireGreenCI {
		t.Errorf("effective policy = %+v, want the resolved override", cfg.EffectivePolicy)
	}
	if !cfg.PolicyObservation || cfg.PolicyRevision != resolved.Revision {
		t.Errorf("config observation/revision = %v/%q, want true/%q",
			cfg.PolicyObservation, cfg.PolicyRevision, resolved.Revision)
	}
}

func TestSaveRepositoryPolicyRejectsInvalidOverride(t *testing.T) {
	f := newFixture(t)
	installationID := f.nextID()
	f.installation(installationID)
	team := f.team("invalid")
	teamID := team.ID
	repo := f.repo(105, installationID, "acme/invalid", &teamID, true)

	bad := "critical"
	_, err := f.s.SaveRepositoryPolicy(f.ctx, store.RepositoryPolicy{
		RepositoryID: repo.ID, BlockingThreshold: &bad,
	})
	if !errors.Is(err, store.ErrInvalidPolicy) {
		t.Fatalf("SaveRepositoryPolicy error = %v, want ErrInvalidPolicy", err)
	}

	// The team PUT does not trim enum whitespace, so the override must not
	// accept a value the team policy would reject.
	padded := " low "
	if _, err := f.s.SaveRepositoryPolicy(f.ctx, store.RepositoryPolicy{
		RepositoryID: repo.ID, ReviewEffort: &padded,
	}); !errors.Is(err, store.ErrInvalidPolicy) {
		t.Fatalf("padded effort error = %v, want ErrInvalidPolicy", err)
	}
}

func TestSaveRepositoryPolicyAcceptsTeamDefaults(t *testing.T) {
	f := newFixture(t)
	installationID := f.nextID()
	f.installation(installationID)
	team := f.team("defaults")
	teamID := team.ID
	repo := f.repo(106, installationID, "acme/defaults", &teamID, true)

	empty, zero64 := "", int64(0)
	zero := 0
	saved, err := f.s.SaveRepositoryPolicy(f.ctx, store.RepositoryPolicy{
		RepositoryID:      repo.ID,
		BlockingThreshold: &empty,
		ReviewEffort:      &empty,
		AutoTriageMode:    &empty,
		VerifyMaxFindings: &zero,
		VerifyMaxFiles:    &zero,
		VerifyTokenShare:  &zero,
		VerifyTimeShare:   &zero,
		VerifyTokenMax:    &zero64,
	})
	if err != nil {
		t.Fatalf("SaveRepositoryPolicy: %v", err)
	}
	// Empty and zero values are normalized like SaveQAPolicy, so the override
	// cannot become a zero-valued policy the engine reads as weaker.
	if saved.BlockingThreshold == nil || *saved.BlockingThreshold != "major" {
		t.Errorf("blocking threshold = %v, want major", saved.BlockingThreshold)
	}
	if saved.ReviewEffort == nil || *saved.ReviewEffort != store.ReviewEffortLow {
		t.Errorf("review effort = %v, want low", saved.ReviewEffort)
	}
	if saved.AutoTriageMode == nil || *saved.AutoTriageMode != store.AutoTriageOff {
		t.Errorf("auto triage = %v, want off", saved.AutoTriageMode)
	}
	if saved.VerifyMaxFindings == nil || *saved.VerifyMaxFindings != store.DefaultVerifyMaxFindings {
		t.Errorf("verify findings = %v, want %d", saved.VerifyMaxFindings, store.DefaultVerifyMaxFindings)
	}
	if saved.VerifyMaxFiles == nil || *saved.VerifyMaxFiles != store.DefaultVerifyMaxFiles {
		t.Errorf("verify files = %v, want %d", saved.VerifyMaxFiles, store.DefaultVerifyMaxFiles)
	}
	if saved.VerifyTokenShare == nil || *saved.VerifyTokenShare != store.DefaultVerifyTokenShare {
		t.Errorf("verify token share = %v, want %d", saved.VerifyTokenShare, store.DefaultVerifyTokenShare)
	}
	if saved.VerifyTimeShare == nil || *saved.VerifyTimeShare != store.DefaultVerifyTimeShare {
		t.Errorf("verify time share = %v, want %d", saved.VerifyTimeShare, store.DefaultVerifyTimeShare)
	}
	if saved.VerifyTokenMax == nil || *saved.VerifyTokenMax != store.DefaultVerifyTokenMax {
		t.Errorf("verify token max = %v, want %d", saved.VerifyTokenMax, store.DefaultVerifyTokenMax)
	}
}

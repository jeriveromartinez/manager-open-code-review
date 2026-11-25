package store_test

import (
	"encoding/json"
	"testing"

	"gorm.io/datatypes"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestClassifyOrigin(t *testing.T) {
	cases := []struct {
		base, head, want string
	}{
		{"acme/repo", "acme/repo", store.OriginInternal},
		{"acme/repo", "ACME/repo", store.OriginInternal},
		{"acme/repo", "ext/repo", store.OriginCrossRepo},
		{"acme/repo", "", store.OriginUnknown},
		{"", "ext/repo", store.OriginUnknown},
	}
	for _, tc := range cases {
		if got := store.ClassifyOrigin(tc.base, tc.head); got != tc.want {
			t.Errorf("ClassifyOrigin(%q, %q) = %q, want %q", tc.base, tc.head, got, tc.want)
		}
	}
}

// A pull request from a branch of the base repository is internal even when
// that repository is itself a fork, because the identities match.
func TestDecideExternalReviewInternal(t *testing.T) {
	pr := store.PullRequest{
		Origin: store.OriginInternal, IsFork: true, AuthorAssociation: "NONE",
	}
	if decision := store.DecideExternalReview(pr, store.QAPolicy{}, store.ExternalUsage{}); !decision.Allow {
		t.Fatalf("internal pull request denied: %+v", decision)
	}
}

func TestDecideExternalReview(t *testing.T) {
	external := store.PullRequest{
		Origin: store.OriginCrossRepo, IsFork: true,
		AuthorAssociation: "CONTRIBUTOR", Additions: 10, Deletions: 5,
	}
	enabled := store.QAPolicy{
		AllowExternalReviews:        true,
		ExternalTrustedAssociations: datatypes.JSON([]byte(`["CONTRIBUTOR"]`)),
		MaxExternalDiffLines:        100,
		MaxExternalRunsPerDay:       5,
		ExternalDailyTokenBudget:    1000,
	}

	if decision := store.DecideExternalReview(external, enabled, store.ExternalUsage{}); !decision.Allow {
		t.Fatalf("authorized contribution denied: %+v", decision)
	}

	cases := []struct {
		name   string
		pr     store.PullRequest
		policy store.QAPolicy
		usage  store.ExternalUsage
		cause  string
	}{
		{
			name:   "unknown origin",
			pr:     store.PullRequest{Origin: store.OriginUnknown, IsFork: true},
			policy: enabled,
			cause:  store.ReviewRequestCauseForkOriginUnknown,
		},
		{
			name:   "external reviews disabled",
			pr:     external,
			policy: store.QAPolicy{},
			cause:  store.ReviewRequestCauseFork,
		},
		{
			name: "untrusted author",
			pr:   external,
			policy: store.QAPolicy{
				AllowExternalReviews:        true,
				ExternalTrustedAssociations: datatypes.JSON([]byte(`["MEMBER"]`)),
			},
			cause: store.ReviewRequestCauseForkUnauthorized,
		},
		{
			name:   "over the size limit",
			pr:     external,
			policy: enabled,
			cause:  store.ReviewRequestCauseForkTooLarge,
		},
		{
			name:   "over the daily run limit",
			pr:     external,
			policy: enabled,
			usage:  store.ExternalUsage{RunsToday: 5},
			cause:  store.ReviewRequestCauseForkRateLimited,
		},
		{
			name:   "over the daily token budget",
			pr:     external,
			policy: enabled,
			usage:  store.ExternalUsage{TokensToday: 1000},
			cause:  store.ReviewRequestCauseForkBudget,
		},
	}
	// The size case needs a contribution above the enabled limit.
	for i := range cases {
		if cases[i].name == "over the size limit" {
			cases[i].pr.Additions = 101
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := store.DecideExternalReview(tc.pr, tc.policy, tc.usage)
			if decision.Allow {
				t.Fatalf("decision = %+v, want a denial", decision)
			}
			if decision.Cause != tc.cause {
				t.Errorf("cause = %q, want %q", decision.Cause, tc.cause)
			}
			if decision.Reason == "" {
				t.Error("denial has no reason")
			}
		})
	}
}

func TestQAPolicyExternalLimitsRoundTrip(t *testing.T) {
	f := newFixture(t)
	team := f.team("external")
	profile := f.llmProfile("external-profile", true)

	saved, err := f.s.SaveQAPolicy(f.ctx, store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID,
		AllowExternalReviews:        true,
		ExternalTrustedAssociations: datatypes.JSON([]byte(`["OWNER","MEMBER"]`)),
		MaxExternalDiffLines:        500,
		MaxExternalRunsPerDay:       3,
		ExternalDailyTokenBudget:    9000,
	})
	if err != nil {
		t.Fatalf("SaveQAPolicy: %v", err)
	}
	if !saved.AllowExternalReviews || saved.MaxExternalDiffLines != 500 ||
		saved.MaxExternalRunsPerDay != 3 || saved.ExternalDailyTokenBudget != 9000 {
		t.Fatalf("saved external limits = %+v", saved)
	}
	var associations []string
	if err := json.Unmarshal(saved.ExternalTrustedAssociations, &associations); err != nil {
		t.Fatalf("decode associations: %v", err)
	}
	if len(associations) != 2 || associations[0] != "OWNER" || associations[1] != "MEMBER" {
		t.Fatalf("associations = %v", associations)
	}

	defaults, err := f.s.SaveQAPolicy(f.ctx, store.QAPolicy{TeamID: team.ID, LLMProfileID: profile.ID})
	if err != nil {
		t.Fatalf("SaveQAPolicy(defaults): %v", err)
	}
	if defaults.MaxExternalDiffLines != store.DefaultExternalDiffLines ||
		defaults.MaxExternalRunsPerDay != store.DefaultExternalRunsPerDay ||
		defaults.ExternalDailyTokenBudget != 0 || defaults.AllowExternalReviews {
		t.Fatalf("defaults = %+v", defaults)
	}
	var defaultAssociations []string
	if err := json.Unmarshal(defaults.ExternalTrustedAssociations, &defaultAssociations); err != nil {
		t.Fatalf("decode default associations: %v", err)
	}
	if len(defaultAssociations) != len(store.DefaultExternalAssociations) {
		t.Fatalf("default associations = %v", defaultAssociations)
	}
}

func TestDecideExternalReviewDefaults(t *testing.T) {
	pr := store.PullRequest{
		Origin: store.OriginCrossRepo, IsFork: true,
		AuthorAssociation: "OWNER", Additions: 2999, Deletions: 1,
	}
	policy := store.QAPolicy{AllowExternalReviews: true}
	if decision := store.DecideExternalReview(pr, policy, store.ExternalUsage{}); !decision.Allow {
		t.Fatalf("default limits denied a contribution on the boundary: %+v", decision)
	}
	pr.Additions = 3001
	if decision := store.DecideExternalReview(pr, policy, store.ExternalUsage{}); decision.Allow ||
		decision.Cause != store.ReviewRequestCauseForkTooLarge {
		t.Fatalf("default size limit not enforced: %+v", decision)
	}
}

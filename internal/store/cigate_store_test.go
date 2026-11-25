package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

func seedCIPullRequest(t *testing.T, st *store.Store, number int, headSHA string) store.PullRequest {
	t.Helper()
	ctx := context.Background()
	inst := store.Installation{ID: 1, AccountLogin: "acme", AccountType: "Organization"}
	if err := st.UpsertInstallation(ctx, inst); err != nil {
		t.Fatalf("upsert installation: %v", err)
	}
	repo := store.Repository{ID: 10, InstallationID: inst.ID, FullName: "acme/repo", Enabled: true}
	if err := st.UpsertRepository(ctx, repo); err != nil {
		t.Fatalf("upsert repository: %v", err)
	}
	pr := store.PullRequest{
		RepositoryID: repo.ID, Number: number, Title: "PR", HeadSHA: headSHA,
		BaseRef: "main", State: "open", GitHubUpdatedAt: time.Now().UTC(),
	}
	if err := st.DB.WithContext(ctx).Create(&pr).Error; err != nil {
		t.Fatalf("create pull request: %v", err)
	}
	return pr
}

func TestValidationObservationIsTiedToTheHead(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	pr := seedCIPullRequest(t, st, 1, "sha-1")
	required, err := store.NormalizeRequiredChecks([]store.RequiredCheck{{Name: "build", Origin: "app:7"}})
	if err != nil {
		t.Fatalf("NormalizeRequiredChecks: %v", err)
	}

	observation := store.ValidationObservation{
		Result: store.ObservationOK,
		Checks: []store.CheckObservation{{Name: "build", Origin: "app:7", State: store.CheckPassed}},
	}
	stamp := store.ObservationStamp{At: time.Now().UTC(), Source: store.ObservationSourcePolling}
	if _, err := st.SetPRValidationObservation(ctx, pr.ID, pr.HeadSHA,
		store.PRValidations{Passed: 1, Total: 1}, observation, stamp); err != nil {
		t.Fatalf("SetPRValidationObservation: %v", err)
	}
	verdict, err := st.CIVerdictFor(ctx, pr.ID, pr.HeadSHA, required)
	if err != nil {
		t.Fatalf("CIVerdictFor: %v", err)
	}
	if !verdict.Green() {
		t.Fatalf("verdict = %+v, want green", verdict)
	}
	changed, err := st.SetPRValidationObservation(ctx, pr.ID, pr.HeadSHA,
		store.PRValidations{Passed: 1, Total: 1}, observation, stamp)
	if err != nil {
		t.Fatalf("SetPRValidationObservation again: %v", err)
	}
	if changed {
		t.Error("repeating the same observation reported a change")
	}

	// A new head clears both the counters and the named observation.
	if _, _, err := st.SyncPullRequest(ctx, store.PullRequest{
		RepositoryID: pr.RepositoryID, Number: pr.Number, Title: pr.Title,
		HeadSHA: "sha-2", BaseRef: "main", State: "open",
		GitHubUpdatedAt: pr.GitHubUpdatedAt.Add(time.Minute),
	}); err != nil {
		t.Fatalf("SyncPullRequest: %v", err)
	}
	verdict, err = st.CIVerdictFor(ctx, pr.ID, "sha-2", required)
	if err != nil {
		t.Fatalf("CIVerdictFor(new head): %v", err)
	}
	if verdict.Green() || verdict.State != store.CIUnknown {
		t.Fatalf("verdict of a new head = %+v, want unknown", verdict)
	}
	if stored, err := st.PRValidationObservation(ctx, pr.ID, "sha-1"); err != nil || stored != nil {
		t.Fatalf("observation of the old head = %+v, err = %v; want none", stored, err)
	}
	var row store.PullRequest
	if err := st.DB.WithContext(ctx).Take(&row, pr.ID).Error; err != nil {
		t.Fatalf("load pull request: %v", err)
	}
	if !row.ValidationsObservedAt.IsZero() || row.ValidationsSource != "" {
		t.Errorf("stamp of the new head = %v/%q, want none", row.ValidationsObservedAt, row.ValidationsSource)
	}

	// A late write for the previous head must not label the new one.
	changed, err = st.SetPRValidationObservation(ctx, pr.ID, "sha-1",
		store.PRValidations{Passed: 1, Total: 1}, observation, stamp)
	if err != nil {
		t.Fatalf("SetPRValidationObservation(old head): %v", err)
	}
	if changed {
		t.Error("a write for the previous head changed the new head")
	}
}

func TestLookupFailureClearsThePreviousVerdict(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	pr := seedCIPullRequest(t, st, 1, "sha-1")
	observation := store.ValidationObservation{
		Result: store.ObservationOK,
		Checks: []store.CheckObservation{{Name: "build", State: store.CheckPassed}},
	}
	stamp := store.ObservationStamp{At: time.Now().UTC(), Source: store.ObservationSourcePolling}
	if _, err := st.SetPRValidationObservation(ctx, pr.ID, pr.HeadSHA,
		store.PRValidations{Passed: 1, Total: 1}, observation, stamp); err != nil {
		t.Fatalf("SetPRValidationObservation: %v", err)
	}
	if _, err := st.SetPRValidationLookupFailure(ctx, pr.ID, pr.HeadSHA,
		store.ObservationStamp{At: stamp.At.Add(time.Second), Source: store.ObservationSourcePolling}); err != nil {
		t.Fatalf("SetPRValidationLookupFailure: %v", err)
	}

	if got, err := st.PRValidationsFor(ctx, pr.ID, pr.HeadSHA); err != nil || got.Total != 0 {
		t.Fatalf("counters after a failed lookup = %+v, err = %v; want zero", got, err)
	}
	verdict, err := st.CIVerdictFor(ctx, pr.ID, pr.HeadSHA, nil)
	if err != nil {
		t.Fatalf("CIVerdictFor: %v", err)
	}
	if verdict.Green() || verdict.State != store.CIUnknown {
		t.Fatalf("verdict after a failed lookup = %+v, want unknown", verdict)
	}
}

func TestStaleValidationObservationDoesNotOverwriteANewerOne(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	pr := seedCIPullRequest(t, st, 1, "sha-1")
	required, err := store.NormalizeRequiredChecks([]store.RequiredCheck{{Name: "build", Origin: "app:7"}})
	if err != nil {
		t.Fatalf("NormalizeRequiredChecks: %v", err)
	}
	newer := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	older := newer.Add(-time.Minute)

	green := store.ValidationObservation{
		Result: store.ObservationOK,
		Checks: []store.CheckObservation{{Name: "build", Origin: "app:7", State: store.CheckPassed}},
	}
	red := store.ValidationObservation{
		Result: store.ObservationOK,
		Checks: []store.CheckObservation{{Name: "build", Origin: "app:7", State: store.CheckFailed}},
	}
	if _, err := st.SetPRValidationObservation(ctx, pr.ID, pr.HeadSHA,
		store.PRValidations{Passed: 1, Total: 1}, green,
		store.ObservationStamp{At: newer, Source: store.ObservationSourcePolling}); err != nil {
		t.Fatalf("SetPRValidationObservation(green): %v", err)
	}

	// A read that started before the stored one and finishes later must be
	// discarded instead of turning the head red.
	changed, err := st.SetPRValidationObservation(ctx, pr.ID, pr.HeadSHA,
		store.PRValidations{Failed: 1, Total: 1}, red,
		store.ObservationStamp{At: older, Source: store.ObservationSourceWebhook})
	if err != nil {
		t.Fatalf("SetPRValidationObservation(late red): %v", err)
	}
	if changed {
		t.Error("a stale observation reported a change")
	}
	verdict, err := st.CIVerdictFor(ctx, pr.ID, pr.HeadSHA, required)
	if err != nil {
		t.Fatalf("CIVerdictFor: %v", err)
	}
	if !verdict.Green() {
		t.Fatalf("verdict after a late red = %+v, want the newer green", verdict)
	}

	var row store.PullRequest
	if err := st.DB.WithContext(ctx).Take(&row, pr.ID).Error; err != nil {
		t.Fatalf("load pull request: %v", err)
	}
	if !row.ValidationsObservedAt.Equal(newer) {
		t.Errorf("observed at = %v, want %v", row.ValidationsObservedAt, newer)
	}
	if row.ValidationsSource != store.ObservationSourcePolling {
		t.Errorf("observation source = %q, want %q", row.ValidationsSource, store.ObservationSourcePolling)
	}

	// A newer reading does replace the stored one.
	changed, err = st.SetPRValidationObservation(ctx, pr.ID, pr.HeadSHA,
		store.PRValidations{Failed: 1, Total: 1}, red,
		store.ObservationStamp{At: newer.Add(time.Minute), Source: store.ObservationSourceWebhook})
	if err != nil {
		t.Fatalf("SetPRValidationObservation(newer red): %v", err)
	}
	if !changed {
		t.Error("a newer observation did not report a change")
	}
	verdict, err = st.CIVerdictFor(ctx, pr.ID, pr.HeadSHA, required)
	if err != nil {
		t.Fatalf("CIVerdictFor: %v", err)
	}
	if verdict.State != store.CIFailed {
		t.Fatalf("verdict after a newer red = %+v, want failed", verdict)
	}
}

func TestStaleLookupFailureDoesNotClearANewerObservation(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	pr := seedCIPullRequest(t, st, 1, "sha-1")
	newer := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	observation := store.ValidationObservation{
		Result: store.ObservationOK,
		Checks: []store.CheckObservation{{Name: "build", State: store.CheckPassed}},
	}
	if _, err := st.SetPRValidationObservation(ctx, pr.ID, pr.HeadSHA,
		store.PRValidations{Passed: 1, Total: 1}, observation,
		store.ObservationStamp{At: newer, Source: store.ObservationSourceWebhook}); err != nil {
		t.Fatalf("SetPRValidationObservation: %v", err)
	}
	if _, err := st.SetPRValidationLookupFailure(ctx, pr.ID, pr.HeadSHA,
		store.ObservationStamp{At: newer.Add(-time.Minute), Source: store.ObservationSourcePolling}); err != nil {
		t.Fatalf("SetPRValidationLookupFailure(late): %v", err)
	}

	if got, err := st.PRValidationsFor(ctx, pr.ID, pr.HeadSHA); err != nil || got.Total != 1 {
		t.Fatalf("counters after a late lookup failure = %+v, err = %v; want the newer observation", got, err)
	}
	verdict, err := st.CIVerdictFor(ctx, pr.ID, pr.HeadSHA, nil)
	if err != nil {
		t.Fatalf("CIVerdictFor: %v", err)
	}
	if !verdict.Green() {
		t.Fatalf("verdict after a late lookup failure = %+v, want green", verdict)
	}
}

func TestOpenPRsForHeadSHAReturnsEveryPullRequest(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	first := seedCIPullRequest(t, st, 1, "sha-shared")
	second := seedCIPullRequest(t, st, 2, "sha-shared")
	other := seedCIPullRequest(t, st, 3, "sha-other")

	prs, err := st.OpenPRsForHeadSHA(ctx, first.RepositoryID, "sha-shared")
	if err != nil {
		t.Fatalf("OpenPRsForHeadSHA: %v", err)
	}
	if len(prs) != 2 || prs[0].ID != first.ID || prs[1].ID != second.ID {
		t.Fatalf("pull requests at the shared SHA = %+v, want %d and %d", prs, first.ID, second.ID)
	}
	for _, pr := range prs {
		if pr.ID == other.ID {
			t.Error("a pull request of another SHA was returned")
		}
	}
}

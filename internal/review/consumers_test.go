package review

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v93/github"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

type consumerCall struct {
	request consumerResolveRequest
}

type fakeConsumerResolver struct {
	calls       []consumerCall
	resolutions map[string]consumerResolution
	errs        map[string]error
}

func (f *fakeConsumerResolver) Resolve(_ context.Context, req consumerResolveRequest) (consumerResolution, error) {
	f.calls = append(f.calls, consumerCall{request: req})
	key := req.Repository + "@" + req.Ref
	if err := f.errs[key]; err != nil {
		return consumerResolution{}, err
	}
	return f.resolutions[key], nil
}

func digestByRef(values map[string]string) func(context.Context, string, string, int64) (string, error) {
	return func(_ context.Context, ref, _ string, _ int64) (string, error) {
		value, ok := values[ref]
		if !ok {
			return "", errors.New("missing digest")
		}
		return value, nil
	}
}

func consumerPolicy(t *testing.T, refs ...store.ConsumerRef) store.QAPolicy {
	t.Helper()
	encoded, err := store.NormalizeConsumerAllowList(refs)
	if err != nil {
		t.Fatalf("NormalizeConsumerAllowList() error = %v", err)
	}
	return store.QAPolicy{
		ConsumerAllowList:  encoded,
		ConsumerMaxLookups: store.DefaultConsumerMaxLookups,
		ConsumerMaxBytes:   store.DefaultConsumerMaxBytes,
	}
}

func consumerEvaluationFixture(policy store.QAPolicy, resolver consumerResolver, digests map[string]string) consumerEvaluation {
	return consumerEvaluation{
		BaseRef:         "base-sha",
		HeadRef:         "head-sha",
		Files:           []changedFile{{Path: "api/openapi.yaml", Kind: changedModified}},
		Policy:          policy,
		ProducerPrivate: false,
		Remaining:       time.Minute,
		Resolver:        resolver,
		ContractDigest:  digestByRef(digests),
	}
}

func TestEvaluateConsumersReportsPinnedSHAForAffectedConsumer(t *testing.T) {
	policy := consumerPolicy(t, store.ConsumerRef{
		Repository: "acme/consumer", Contract: "api/openapi.yaml", Ref: "v1.2.0",
		Visibility: store.ConsumerVisibilityPublic, Essential: true,
	})
	resolver := &fakeConsumerResolver{resolutions: map[string]consumerResolution{
		"acme/consumer@v1.2.0": {
			PinnedSHA: "consumer-sha", ContractSHA: "blob-sha", Digest: "base-digest",
		},
	}}
	got := evaluateConsumers(context.Background(), consumerEvaluationFixture(policy, resolver,
		map[string]string{"base-sha": "base-digest", "head-sha": "head-digest"}))

	if got.Configured != 1 || got.Checked != 1 {
		t.Fatalf("configured/checked = %d/%d, want 1/1", got.Configured, got.Checked)
	}
	if got.Status != store.ConsumersComplete {
		t.Fatalf("status = %q, want complete (errors: %v)", got.Status, got.Errors)
	}
	if len(got.Consumers) != 1 {
		t.Fatalf("consumers = %+v", got.Consumers)
	}
	consumer := got.Consumers[0]
	if consumer.Status != store.ConsumerStatusAffected || consumer.Impact != store.ConsumerImpactVerified {
		t.Errorf("consumer = %+v, want affected/verified", consumer)
	}
	if consumer.SHA != "consumer-sha" || consumer.ContractSHA != "blob-sha" || consumer.ProducerDigest != "head-digest" {
		t.Errorf("pinned evidence = %+v", consumer)
	}
	if consumer.Redacted {
		t.Errorf("public consumer was redacted")
	}
	if len(resolver.calls) != 1 || resolver.calls[0].request.MaxBytes != policy.ConsumerMaxBytes {
		t.Errorf("lookup calls = %+v, want one bounded call", resolver.calls)
	}
	if !containsString(got.Sources, store.ConsumerSourcePolicy) || !containsString(got.Sources, "github") {
		t.Errorf("sources = %v, want policy and github", got.Sources)
	}
}

func TestEvaluateConsumersUnauthorizedEssentialIsUnknown(t *testing.T) {
	policy := consumerPolicy(t, store.ConsumerRef{
		Repository: "acme/consumer", Contract: "api/openapi.yaml", Ref: "v1.2.0",
		Visibility: store.ConsumerVisibilityPublic, Essential: true,
	})
	resolver := &fakeConsumerResolver{errs: map[string]error{
		"acme/consumer@v1.2.0": errConsumerUnauthorized,
	}}
	got := evaluateConsumers(context.Background(), consumerEvaluationFixture(policy, resolver, nil))

	if got.Status != store.ConsumersUnknown {
		t.Fatalf("status = %q, want unknown", got.Status)
	}
	if !got.RequiresHuman {
		t.Error("an essential unauthorized consumer did not require a human")
	}
	consumer := got.Consumers[0]
	if consumer.Status != store.ConsumerStatusUnknown || consumer.Reason != store.ConsumerReasonUnauthorized {
		t.Errorf("consumer = %+v, want unknown/unauthorized", consumer)
	}
	if consumer.SHA != "" || consumer.Digest != "" || consumer.ProducerDigest != "" {
		t.Errorf("unavailable consumer leaked evidence: %+v", consumer)
	}
}

func TestEvaluateConsumersUnavailablePrivateConsumerIsRedacted(t *testing.T) {
	policy := consumerPolicy(t, store.ConsumerRef{
		Repository: "acme/private-consumer", Contract: "internal/api.yaml", Ref: "v1.2.0",
		Essential: true,
	})
	resolver := &fakeConsumerResolver{errs: map[string]error{
		"acme/private-consumer@v1.2.0": errConsumerUnavailable,
	}}
	evaluation := consumerEvaluationFixture(policy, resolver, nil)
	evaluation.Files = []changedFile{{Path: "internal/api.yaml", Kind: changedModified}}
	got := evaluateConsumers(context.Background(), evaluation)

	consumer := got.Consumers[0]
	if !consumer.Redacted {
		t.Fatalf("private consumer on a public repository was not redacted: %+v", consumer)
	}
	if got.Status != store.ConsumersUnknown || !got.RequiresHuman {
		t.Errorf("aggregate = %+v, want unknown and requires_human", got)
	}

	verdict := applyConsumerImpact(store.ArchitectureVerdict{Requested: true, Consumers: got})
	published := strings.Join(append(append([]string{}, verdict.Risks...), verdict.Uncertainties...), "\n") +
		"\n" + consumerDimensionContent(got)
	for _, leaked := range []string{"acme/private-consumer", "internal/api.yaml", "v1.2.0"} {
		if strings.Contains(published, leaked) {
			t.Errorf("verdict text leaked %q: %s", leaked, published)
		}
	}
	if !strings.Contains(published, "consumidor privado") {
		t.Errorf("verdict text does not explain the redacted consumer: %s", published)
	}
	if !containsString(verdict.Uncertainties,
		"Un consumidor esencial no se pudo comprobar; la decisión requiere criterio humano.") {
		t.Errorf("verdict does not ask for a human decision: %v", verdict.Uncertainties)
	}
}

func TestEvaluateConsumersEnforcesLookupBudget(t *testing.T) {
	policy := consumerPolicy(t,
		store.ConsumerRef{Repository: "acme/a", Contract: "api/openapi.yaml", Ref: "v1", Visibility: store.ConsumerVisibilityPublic},
		store.ConsumerRef{Repository: "acme/b", Contract: "api/openapi.yaml", Ref: "v1", Visibility: store.ConsumerVisibilityPublic},
		store.ConsumerRef{Repository: "acme/c", Contract: "api/openapi.yaml", Ref: "v1", Visibility: store.ConsumerVisibilityPublic},
	)
	policy.ConsumerMaxLookups = 1
	resolver := &fakeConsumerResolver{
		resolutions: map[string]consumerResolution{
			"acme/a@v1": {PinnedSHA: "sha-a", Digest: "head-digest"},
		},
	}
	got := evaluateConsumers(context.Background(), consumerEvaluationFixture(policy, resolver,
		map[string]string{"base-sha": "base-digest", "head-sha": "head-digest"}))

	if len(resolver.calls) != 1 {
		t.Fatalf("resolver calls = %d, want the configured budget of 1", len(resolver.calls))
	}
	if got.Checked != 1 || got.Status != store.ConsumersPartial {
		t.Errorf("checked/status = %d/%q, want 1/partial", got.Checked, got.Status)
	}
	skipped := 0
	for _, consumer := range got.Consumers {
		if consumer.Status == store.ConsumerStatusSkipped && consumer.Reason == store.ConsumerReasonLookupBudget {
			skipped++
		}
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2: %+v", skipped, got.Consumers)
	}
}

func TestEvaluateConsumersSkippedEssentialNeverPasses(t *testing.T) {
	policy := consumerPolicy(t,
		store.ConsumerRef{Repository: "acme/a", Contract: "api/openapi.yaml", Ref: "v1", Essential: true},
		store.ConsumerRef{Repository: "acme/b", Contract: "api/openapi.yaml", Ref: "v1", Essential: true},
	)
	policy.ConsumerMaxLookups = 1
	resolver := &fakeConsumerResolver{
		resolutions: map[string]consumerResolution{"acme/a@v1": {PinnedSHA: "sha-a", Digest: "head-digest"}},
	}
	got := evaluateConsumers(context.Background(), consumerEvaluationFixture(policy, resolver,
		map[string]string{"base-sha": "base-digest", "head-sha": "head-digest"}))
	if got.Status != store.ConsumersUnknown || !got.RequiresHuman {
		t.Fatalf("aggregate = %+v, want unknown and requires_human", got)
	}
}

func TestEvaluateConsumersSkipsUnchangedContract(t *testing.T) {
	policy := consumerPolicy(t, store.ConsumerRef{
		Repository: "acme/consumer", Contract: "api/other.yaml", Ref: "v1",
	})
	resolver := &fakeConsumerResolver{}
	got := evaluateConsumers(context.Background(), consumerEvaluationFixture(policy, resolver, nil))

	if len(resolver.calls) != 0 {
		t.Fatalf("resolver was called for an unchanged contract: %+v", resolver.calls)
	}
	if got.Status != store.ConsumersComplete || got.Checked != 0 {
		t.Errorf("aggregate = %+v, want complete without lookups", got)
	}
	if got.Consumers[0].Status != store.ConsumerStatusOutOfScope {
		t.Errorf("consumer = %+v, want out_of_scope", got.Consumers[0])
	}
	if consumerLookupRelevant(got) {
		t.Error("an all out-of-scope lookup requested an architectural verdict")
	}
}

func TestEvaluateConsumersCompatibleExtensionIsNotReportedAsRisk(t *testing.T) {
	policy := consumerPolicy(t, store.ConsumerRef{
		Repository: "acme/consumer", Contract: "api/openapi.yaml", Ref: "v2",
		Visibility: store.ConsumerVisibilityPublic, Essential: true,
	})
	resolver := &fakeConsumerResolver{resolutions: map[string]consumerResolution{
		"acme/consumer@v2": {PinnedSHA: "consumer-sha", ContractSHA: "blob-sha", Digest: "head-digest"},
	}}
	got := evaluateConsumers(context.Background(), consumerEvaluationFixture(policy, resolver,
		map[string]string{"base-sha": "base-digest", "head-sha": "head-digest"}))

	if got.Status != store.ConsumersComplete || got.RequiresHuman {
		t.Fatalf("aggregate = %+v, want complete without a human request", got)
	}
	consumer := got.Consumers[0]
	if consumer.Status != store.ConsumerStatusCompatible || consumer.Impact != store.ConsumerImpactNone ||
		consumer.Reason != store.ConsumerReasonOnHead {
		t.Fatalf("consumer = %+v, want compatible on head", consumer)
	}
	if consumer.SHA != "consumer-sha" || consumer.ContractSHA != "blob-sha" {
		t.Errorf("pinned evidence = %+v", consumer)
	}
	verdict := applyConsumerImpact(store.ArchitectureVerdict{Requested: true, Consumers: got})
	for _, item := range verdict.Risks {
		if strings.Contains(item, "coexistencia de versiones") {
			t.Errorf("a compatible extension was reported as a risk: %v", verdict.Risks)
		}
	}
}

func TestEvaluateConsumersDifferentVersionLeavesUncertainty(t *testing.T) {
	policy := consumerPolicy(t, store.ConsumerRef{
		Repository: "acme/consumer", Contract: "api/openapi.yaml", Ref: "v1",
		Visibility: store.ConsumerVisibilityPublic,
	})
	resolver := &fakeConsumerResolver{
		resolutions: map[string]consumerResolution{
			"acme/consumer@v1": {PinnedSHA: "consumer-sha", Digest: "another-version"},
		},
	}
	got := evaluateConsumers(context.Background(), consumerEvaluationFixture(policy, resolver,
		map[string]string{"base-sha": "base-digest", "head-sha": "head-digest"}))
	if got.Status != store.ConsumersPartial {
		t.Fatalf("status = %q, want partial", got.Status)
	}
	if got.Consumers[0].Reason != store.ConsumerReasonDifferentVersion {
		t.Errorf("reason = %q, want different version", got.Consumers[0].Reason)
	}
	verdict := applyConsumerImpact(store.ArchitectureVerdict{Requested: true, Consumers: got})
	if !containsString(verdict.Uncertainties, "No hay evidencia suficiente sobre acme/consumer (api/openapi.yaml) (versión distinta sin evidencia de compatibilidad).") {
		t.Errorf("uncertainties = %v", verdict.Uncertainties)
	}
}

func TestApplyConsumerImpactRedactsAffectedPrivateConsumer(t *testing.T) {
	consumers := store.ArchitectureConsumers{
		Status: store.ConsumersComplete, Configured: 1, Checked: 1,
		Consumers: []store.ArchitectureConsumer{{
			Repository: "acme/private-consumer", Contract: "internal/api.yaml",
			Ref: "v1.2.0", SHA: "0123456789abcdef",
			Status: store.ConsumerStatusAffected, Impact: store.ConsumerImpactVerified,
			Private: true, Redacted: true,
		}},
	}
	verdict := applyConsumerImpact(store.ArchitectureVerdict{Requested: true, Consumers: consumers})
	published := strings.Join(append(append([]string{}, verdict.Risks...), verdict.Uncertainties...), "\n") +
		"\n" + consumerDimensionContent(consumers)
	for _, leaked := range []string{"acme/private-consumer", "internal/api.yaml", "v1.2.0", "0123456789abcdef"} {
		if strings.Contains(published, leaked) {
			t.Errorf("verdict text leaked %q: %s", leaked, published)
		}
	}
	if !strings.Contains(published, "consumidor privado") {
		t.Errorf("verdict text does not explain the redacted consumer: %s", published)
	}
}

func TestApplyConsumerImpactCarriesDimensionAndRisk(t *testing.T) {
	policy := consumerPolicy(t, store.ConsumerRef{
		Repository: "acme/consumer", Contract: "api/openapi.yaml", Ref: "v1",
		Visibility: store.ConsumerVisibilityPublic, Essential: true,
	})
	resolver := &fakeConsumerResolver{
		resolutions: map[string]consumerResolution{
			"acme/consumer@v1": {PinnedSHA: "consumer-sha", Digest: "base-digest"},
		},
	}
	consumers := evaluateConsumers(context.Background(), consumerEvaluationFixture(policy, resolver,
		map[string]string{"base-sha": "base-digest", "head-sha": "head-digest"}))
	verdict := applyConsumerImpact(store.ArchitectureVerdict{
		Requested: true, Consumers: consumers,
		Risks: []string{"existing risk"},
	})

	found := false
	for _, dimension := range verdict.Dimensions {
		if dimension.Name == consumerDimensionName {
			found = true
			if !strings.Contains(dimension.Content, "acme/consumer") {
				t.Errorf("dimension content = %q", dimension.Content)
			}
		}
	}
	if !found {
		t.Fatalf("dimensions = %+v, want %q", verdict.Dimensions, consumerDimensionName)
	}
	if !containsString(verdict.Risks, "existing risk") {
		t.Errorf("pre-existing risks were dropped: %v", verdict.Risks)
	}
	risk := ""
	for _, item := range verdict.Risks {
		if strings.Contains(item, "coexistencia de versiones") {
			risk = item
		}
	}
	if !strings.Contains(risk, "acme/consumer") || !strings.Contains(risk, "api/openapi.yaml") {
		t.Errorf("risk = %q, want the consumer impact", risk)
	}
	if len(verdict.Uncertainties) != 0 {
		t.Errorf("ambiguous uncertainties = %v", verdict.Uncertainties)
	}
}

func TestEvaluateConsumersIsReproducible(t *testing.T) {
	policy := consumerPolicy(t, store.ConsumerRef{
		Repository: "acme/consumer", Contract: "api/openapi.yaml", Ref: "v1",
		Visibility: store.ConsumerVisibilityPublic,
	})
	newResolver := func() *fakeConsumerResolver {
		return &fakeConsumerResolver{resolutions: map[string]consumerResolution{
			"acme/consumer@v1": {PinnedSHA: "consumer-sha", ContractSHA: "blob-sha", Digest: "base-digest"},
		}}
	}
	digests := map[string]string{"base-sha": "base-digest", "head-sha": "head-digest"}
	first := evaluateConsumers(context.Background(), consumerEvaluationFixture(policy, newResolver(), digests))
	second := evaluateConsumers(context.Background(), consumerEvaluationFixture(policy, newResolver(), digests))
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Errorf("consumer evidence is not reproducible:\n%s\n%s", firstJSON, secondJSON)
	}
}

func TestMergeConsumersPolicyWinsAndBaselineParses(t *testing.T) {
	policy := []configuredConsumer{{
		Ref:    store.ConsumerRef{Repository: "acme/consumer", Contract: "api/openapi.yaml", Ref: "policy"},
		Source: store.ConsumerSourcePolicy,
	}}
	baseline := []configuredConsumer{
		{Ref: store.ConsumerRef{Repository: "acme/consumer", Contract: "api/openapi.yaml", Ref: "baseline"}, Source: store.ConsumerSourceBaseline},
		{Ref: store.ConsumerRef{Repository: "acme/other", Contract: "api/openapi.yaml", Ref: "baseline"}, Source: store.ConsumerSourceBaseline},
	}
	merged := mergeConsumers(append(policy, baseline...))
	if len(merged) != 2 {
		t.Fatalf("merged = %+v, want two entries", merged)
	}
	for _, entry := range merged {
		if entry.Ref.Repository == "acme/consumer" && entry.Ref.Ref != "policy" {
			t.Errorf("policy did not win: %+v", entry)
		}
	}

	refs, err := parseConsumerBaseline([]byte(`{"consumers":[{"repository":"acme/x","contract":"api/openapi.yaml","ref":"v1"}]}`))
	if err != nil || len(refs) != 1 || refs[0].Repository != "acme/x" {
		t.Errorf("parseConsumerBaseline() = %+v, %v", refs, err)
	}
	refs, err = parseConsumerBaseline([]byte(`[{"repository":"acme/y","contract":"api/openapi.yaml","ref":"v1"}]`))
	if err != nil || len(refs) != 1 || refs[0].Repository != "acme/y" {
		t.Errorf("parseConsumerBaseline(array) = %+v, %v", refs, err)
	}
}

func TestEvaluateConsumersRenameKeepsOldContractInScope(t *testing.T) {
	policy := consumerPolicy(t, store.ConsumerRef{
		Repository: "acme/consumer", Contract: "api/openapi.yaml", Ref: "v1",
		Visibility: store.ConsumerVisibilityPublic,
	})
	resolver := &fakeConsumerResolver{resolutions: map[string]consumerResolution{
		"acme/consumer@v1": {PinnedSHA: "consumer-sha", Digest: "base-digest"},
	}}
	evaluation := consumerEvaluationFixture(policy, resolver,
		map[string]string{"base-sha": "base-digest"})
	evaluation.Files = []changedFile{{
		Path: "api/openapi-v2.yaml", OldPath: "api/openapi.yaml", Kind: changedModified, Renamed: true,
	}}
	got := evaluateConsumers(context.Background(), evaluation)

	if got.Checked != 1 || len(resolver.calls) != 1 {
		t.Fatalf("old contract path was out of scope: checked=%d calls=%d", got.Checked, len(resolver.calls))
	}
	if got.Consumers[0].Status != store.ConsumerStatusAffected {
		t.Errorf("consumer = %+v, want affected", got.Consumers[0])
	}
}

func TestEvaluateConsumersModeOnlyChangeIsNotCompatible(t *testing.T) {
	policy := consumerPolicy(t, store.ConsumerRef{
		Repository: "acme/consumer", Contract: "api/openapi.yaml", Ref: "v1",
		Visibility: store.ConsumerVisibilityPublic,
	})
	resolver := &fakeConsumerResolver{resolutions: map[string]consumerResolution{
		"acme/consumer@v1": {PinnedSHA: "consumer-sha", Digest: "another-version"},
	}}
	got := evaluateConsumers(context.Background(), consumerEvaluationFixture(policy, resolver,
		map[string]string{"base-sha": "same-digest", "head-sha": "same-digest"}))
	consumer := got.Consumers[0]
	if consumer.Status == store.ConsumerStatusCompatible {
		t.Fatalf("consumer = %+v, want unknown without a digest match", consumer)
	}
	if consumer.Reason != store.ConsumerReasonDifferentVersion || got.Status != store.ConsumersPartial {
		t.Errorf("consumer/reason/aggregate = %+v/%q/%q", consumer, consumer.Reason, got.Status)
	}
}

func TestEvaluateConsumersOversizedContractIsSkipped(t *testing.T) {
	policy := consumerPolicy(t, store.ConsumerRef{
		Repository: "acme/consumer", Contract: "api/openapi.yaml", Ref: "v1", Essential: true,
	})
	policy.ConsumerMaxBytes = store.ConsumerMaxBytesMin
	resolver := &fakeConsumerResolver{errs: map[string]error{
		"acme/consumer@v1": errConsumerTooLarge,
	}}
	got := evaluateConsumers(context.Background(), consumerEvaluationFixture(policy, resolver, nil))
	consumer := got.Consumers[0]
	if consumer.Status != store.ConsumerStatusSkipped || consumer.Reason != store.ConsumerReasonTooLarge {
		t.Errorf("consumer = %+v, want skipped/too_large", consumer)
	}
	if got.Status != store.ConsumersUnknown || !got.RequiresHuman {
		t.Errorf("aggregate = %+v, want unknown and requires_human", got)
	}
	if len(resolver.calls) != 1 || resolver.calls[0].request.MaxBytes != store.ConsumerMaxBytesMin {
		t.Errorf("calls = %+v, want the policy byte cap", resolver.calls)
	}
}

func TestMaskRedactedConsumersPersistsNoPrivateValues(t *testing.T) {
	verdict := store.ArchitectureVerdict{
		Requested: true,
		Consumers: store.ArchitectureConsumers{
			Status: store.ConsumersComplete, Configured: 2, Checked: 2,
			Consumers: []store.ArchitectureConsumer{
				{
					Repository: "acme/private-consumer", Contract: "internal/api.yaml",
					Path: "vendor/api.yaml", Ref: "v1.2.0", SHA: "private-sha",
					ContractSHA: "private-blob", Version: "1.2.0",
					Digest: "private-digest", ProducerDigest: "producer-digest",
					Status: store.ConsumerStatusAffected, Impact: store.ConsumerImpactVerified,
					Private: true, Redacted: true,
				},
				{
					Repository: "acme/public-consumer", Contract: "api/openapi.yaml",
					Ref: "v1", SHA: "public-sha", Status: store.ConsumerStatusCompatible,
				},
			},
		},
	}
	masked := store.MaskRedactedConsumers(verdict)
	raw, err := json.Marshal(masked)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{
		"acme/private-consumer", "internal/api.yaml", "vendor/api.yaml",
		"v1.2.0", "private-sha", "private-blob", "private-digest", "1.2.0",
	} {
		if strings.Contains(string(raw), leaked) {
			t.Errorf("serialized verdict leaked %q: %s", leaked, raw)
		}
	}
	if !strings.Contains(string(raw), "acme/public-consumer") || !strings.Contains(string(raw), "public-sha") {
		t.Errorf("public consumer was masked: %s", raw)
	}
}

func TestResolveConsumerImpactWithoutDiffIsUnknownAndMasked(t *testing.T) {
	policy := consumerPolicy(t, store.ConsumerRef{
		Repository: "acme/private-consumer", Contract: "internal/api.yaml", Ref: "v1",
		Essential: true,
	})
	got := resolveConsumerImpact(context.Background(), consumerTriggerEvaluation{
		Policy: policy,
		// No changed-file resolver: the control process cannot prove anything.
	})

	if got.Status != store.ConsumersUnknown || !got.RequiresHuman {
		t.Fatalf("aggregate = %+v, want unknown and requires_human", got)
	}
	if len(got.Consumers) != 1 {
		t.Fatalf("consumers = %+v", got.Consumers)
	}
	consumer := got.Consumers[0]
	if !consumer.Redacted || consumer.Status != store.ConsumerStatusUnknown ||
		consumer.Reason != store.ConsumerReasonUnavailable {
		t.Errorf("consumer = %+v, want a redacted unavailable unknown", consumer)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"acme/private-consumer", "internal/api.yaml", "v1"} {
		if strings.Contains(string(raw), leaked) {
			t.Errorf("persisted snapshot leaked %q: %s", leaked, raw)
		}
	}
}

func TestResolveConsumerImpactNotConfiguredSkipsLookup(t *testing.T) {
	got := resolveConsumerImpact(context.Background(), consumerTriggerEvaluation{
		Policy: store.QAPolicy{},
	})
	if got.Status != store.ConsumersNotConfigured || got.Configured != 0 {
		t.Fatalf("aggregate = %+v, want not_configured", got)
	}
}

func TestUnavailableConsumerImpactNeverPasses(t *testing.T) {
	policy := consumerPolicy(t, store.ConsumerRef{
		Repository: "acme/consumer", Contract: "api/openapi.yaml", Ref: "v1",
		Visibility: store.ConsumerVisibilityPublic, Essential: true,
	})
	got := unavailableConsumerImpact(context.Background(), t.TempDir(), "base-sha", policy, false)
	if got.Status != store.ConsumersUnknown || !got.RequiresHuman {
		t.Fatalf("aggregate = %+v, want unknown and requires_human", got)
	}
	if len(got.Consumers) != 1 || got.Consumers[0].Status != store.ConsumerStatusUnknown ||
		got.Consumers[0].Reason != store.ConsumerReasonUnavailable {
		t.Errorf("consumer = %+v, want an explicit unavailable entry", got.Consumers)
	}
}

func TestConsumerFileContentRejectsUnfaithfulBody(t *testing.T) {
	size := 2 << 20
	empty := &gh.RepositoryContent{
		Size: &size, Encoding: gh.String("base64"), Content: gh.String(""),
	}
	if _, err := consumerFileContent(empty); !errors.Is(err, errConsumerDigest) {
		t.Errorf("oversized empty body error = %v, want errConsumerDigest", err)
	}
	unsupported := 10
	encoding := "gzip"
	content := "payload"
	file := &gh.RepositoryContent{Size: &unsupported, Encoding: &encoding, Content: &content}
	if _, err := consumerFileContent(file); !errors.Is(err, errConsumerDigest) {
		t.Errorf("unsupported encoding error = %v, want errConsumerDigest", err)
	}
	plain := "YWJj"
	base64 := "base64"
	size = 3
	ok := &gh.RepositoryContent{Size: &size, Encoding: &base64, Content: &plain}
	if got, err := consumerFileContent(ok); err != nil || got != "abc" {
		t.Errorf("base64 content = %q, %v", got, err)
	}
}

package review

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
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

// fakeConsumerReader is the read-only GitHub repositories API the resolver uses.
// The counters prove the visibility probe is cached and that a failed probe
// never attempts the contract reads.
type fakeConsumerReader struct {
	repository     *gh.Repository
	repositoryResp *gh.Response
	repositoryErr  error
	sha            string
	shaResp        *gh.Response
	shaErr         error
	file           *gh.RepositoryContent
	contentsResp   *gh.Response
	contentsErr    error

	getCalls      int
	shaCalls      int
	contentsCalls int
}

func (f *fakeConsumerReader) Get(context.Context, string, string) (*gh.Repository, *gh.Response, error) {
	f.getCalls++
	return f.repository, f.repositoryResp, f.repositoryErr
}

func (f *fakeConsumerReader) GetCommitSHA1(context.Context, string, string, string, string) (string, *gh.Response, error) {
	f.shaCalls++
	return f.sha, f.shaResp, f.shaErr
}

func (f *fakeConsumerReader) GetContents(context.Context, string, string, string,
	*gh.RepositoryContentGetOptions) (*gh.RepositoryContent, []*gh.RepositoryContent, *gh.Response, error) {
	f.contentsCalls++
	return f.file, nil, f.contentsResp, f.contentsErr
}

func ghResponse(status int) *gh.Response {
	return &gh.Response{Response: &http.Response{StatusCode: status}}
}

func consumerContractFile(content string) *gh.RepositoryContent {
	size := len(content)
	encoding := "base64"
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	return &gh.RepositoryContent{Size: &size, Encoding: &encoding, Content: &encoded}
}

func fakeVisibilityResolver(reader consumerRepositoryReader) *githubConsumerResolver {
	return newGitHubConsumerResolverWithReaders(
		func(context.Context, int64) (consumerRepositoryReader, error) { return reader, nil }, 42)
}

func TestGitHubConsumerResolverProbesPublicRepository(t *testing.T) {
	reader := &fakeConsumerReader{
		repository: &gh.Repository{Visibility: gh.String("public")},
		sha:        "consumer-sha",
		file:       consumerContractFile("contract"),
	}
	resolved, err := fakeVisibilityResolver(reader).Resolve(context.Background(), consumerResolveRequest{
		Repository: "acme/consumer", Ref: "v1", Path: "api/openapi.yaml",
		// A public repository must be read even if the configuration declared it
		// private and left it redacted.
		ConfiguredPrivate: true,
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if !resolved.VisibilityKnown || resolved.Private {
		t.Errorf("visibility = known:%t private:%t, want known public", resolved.VisibilityKnown, resolved.Private)
	}
	if resolved.PinnedSHA != "consumer-sha" || resolved.Digest == "" {
		t.Errorf("resolution = %+v, want the contract of the public repository", resolved)
	}
	if reader.getCalls != 1 || reader.shaCalls != 1 || reader.contentsCalls != 1 {
		t.Errorf("calls = get:%d sha:%d contents:%d, want one each", reader.getCalls, reader.shaCalls, reader.contentsCalls)
	}
}

func TestGitHubConsumerResolverReadsAuthorizedPrivateRepository(t *testing.T) {
	reader := &fakeConsumerReader{
		repository: &gh.Repository{Visibility: gh.String("private"), Private: gh.Bool(true)},
		sha:        "consumer-sha",
		file:       consumerContractFile("contract"),
	}
	resolved, err := fakeVisibilityResolver(reader).Resolve(context.Background(), consumerResolveRequest{
		Repository: "acme/private-consumer", Ref: "v1", Path: "internal/api.yaml",
		// The probe wins over a configured public visibility: the repository is
		// private even though the configuration said otherwise.
		ConfiguredPrivate: false,
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if !resolved.VisibilityKnown || !resolved.Private {
		t.Errorf("visibility = known:%t private:%t, want known private", resolved.VisibilityKnown, resolved.Private)
	}
	if resolved.PinnedSHA != "consumer-sha" {
		t.Errorf("resolution = %+v, want the authorized read", resolved)
	}
}

func TestGitHubConsumerResolverProbeFailureIsNeverCompatible(t *testing.T) {
	cases := []struct {
		name              string
		status            int
		configuredPrivate bool
		want              error
	}{
		{name: "private forbidden", status: http.StatusForbidden, configuredPrivate: true, want: errConsumerUnauthorized},
		{name: "private not visible", status: http.StatusNotFound, configuredPrivate: true, want: errConsumerUnauthorized},
		{name: "public not found", status: http.StatusNotFound, configuredPrivate: false, want: errConsumerUnavailable},
		{name: "transient error", status: http.StatusInternalServerError, configuredPrivate: true, want: errConsumerUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &fakeConsumerReader{
				repositoryResp: ghResponse(tc.status),
				repositoryErr:  errors.New("probe failed"),
				sha:            "should-not-be-read",
				file:           consumerContractFile("contract"),
			}
			_, err := fakeVisibilityResolver(reader).Resolve(context.Background(), consumerResolveRequest{
				Repository: "acme/consumer", Ref: "v1", Path: "api/openapi.yaml",
				ConfiguredPrivate: tc.configuredPrivate,
			})
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
			if reader.shaCalls != 0 || reader.contentsCalls != 0 {
				t.Errorf("a failed probe read the contract: sha:%d contents:%d", reader.shaCalls, reader.contentsCalls)
			}
		})
	}
}

func TestGitHubConsumerResolverCachesVisibilityProbe(t *testing.T) {
	newReader := func() *fakeConsumerReader {
		return &fakeConsumerReader{
			repository: &gh.Repository{Visibility: gh.String("public")},
			sha:        "consumer-sha",
			file:       consumerContractFile("contract"),
		}
	}
	first, second := newReader(), newReader()
	resolver := newGitHubConsumerResolverWithReaders(
		func(_ context.Context, installationID int64) (consumerRepositoryReader, error) {
			switch installationID {
			case 1:
				return first, nil
			case 2:
				return second, nil
			}
			return nil, errors.New("unexpected installation")
		}, 42)
	read := func(repository, ref string, installationID int64) {
		t.Helper()
		if _, err := resolver.Resolve(context.Background(), consumerResolveRequest{
			Repository: repository, Ref: ref, Path: "api/openapi.yaml", InstallationID: installationID,
		}); err != nil {
			t.Fatalf("Resolve(%s@%s, installation %d) error = %v", repository, ref, installationID, err)
		}
	}
	read("acme/one", "v1", 1)
	read("acme/one", "v2", 1) // same repository and installation: cached
	read("acme/two", "v1", 1) // second repository: its own probe
	read("acme/one", "v1", 2) // second installation: its own probe

	if first.getCalls != 2 {
		t.Errorf("installation 1 probe calls = %d, want one per repository", first.getCalls)
	}
	if second.getCalls != 1 {
		t.Errorf("installation 2 probe calls = %d, want its own probe", second.getCalls)
	}
	if first.shaCalls != 3 || first.contentsCalls != 3 {
		t.Errorf("installation 1 reads = sha:%d contents:%d, want three each", first.shaCalls, first.contentsCalls)
	}
	if second.shaCalls != 1 || second.contentsCalls != 1 {
		t.Errorf("installation 2 reads = sha:%d contents:%d, want one each", second.shaCalls, second.contentsCalls)
	}
}

func TestEvaluateConsumersProbeErrorKeepsConfiguredVisibility(t *testing.T) {
	policy := consumerPolicy(t, store.ConsumerRef{
		Repository: "acme/private-consumer", Contract: "internal/api.yaml", Ref: "v1",
		Essential: true,
	})
	reader := &fakeConsumerReader{
		repositoryResp: ghResponse(http.StatusInternalServerError),
		repositoryErr:  errors.New("probe failed"),
	}
	evaluation := consumerEvaluationFixture(policy, fakeVisibilityResolver(reader), nil)
	evaluation.Files = []changedFile{{Path: "internal/api.yaml", Kind: changedModified}}
	got := evaluateConsumers(context.Background(), evaluation)

	consumer := got.Consumers[0]
	if consumer.Status != store.ConsumerStatusUnknown || consumer.Reason != store.ConsumerReasonUnavailable {
		t.Errorf("consumer = %+v, want unknown/unavailable", consumer)
	}
	if !consumer.Redacted {
		t.Errorf("probe failure lost the configured private fallback: %+v", consumer)
	}
	if got.Status != store.ConsumersUnknown || !got.RequiresHuman {
		t.Errorf("aggregate = %+v, want unknown and requires_human", got)
	}
	raw, err := json.Marshal(store.MaskRedactedConsumerImpact(got))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "acme/private-consumer") {
		t.Errorf("persisted snapshot leaked the private repository: %s", raw)
	}
}

func TestEvaluateConsumersProbedPrivateIsRedactedWhenConfiguredPublic(t *testing.T) {
	policy := consumerPolicy(t, store.ConsumerRef{
		Repository: "acme/hidden-consumer", Contract: "api/openapi.yaml", Ref: "v1",
		Visibility: store.ConsumerVisibilityPublic, Essential: true,
	})
	reader := &fakeConsumerReader{
		repository: &gh.Repository{Visibility: gh.String("private"), Private: gh.Bool(true)},
		sha:        "consumer-sha",
		file:       consumerContractFile("contract"),
	}
	reader.file.SHA = gh.String("contract-sha")
	sum := sha256.Sum256([]byte("contract"))
	baseDigest := hex.EncodeToString(sum[:])
	got := evaluateConsumers(context.Background(), consumerEvaluationFixture(policy,
		fakeVisibilityResolver(reader), map[string]string{"base-sha": baseDigest, "head-sha": "head-digest"}))

	consumer := got.Consumers[0]
	if consumer.Status != store.ConsumerStatusAffected || consumer.Impact != store.ConsumerImpactVerified {
		t.Fatalf("consumer = %+v, want affected/verified from the authorized read", consumer)
	}
	if !consumer.Private || !consumer.Redacted {
		t.Errorf("probed private consumer was not redacted: %+v", consumer)
	}
	verdict := applyConsumerImpact(store.ArchitectureVerdict{Requested: true, Consumers: got})
	verdict = store.MaskRedactedConsumers(verdict)
	raw, err := json.Marshal(verdict)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{
		"acme/hidden-consumer", "api/openapi.yaml", "consumer-sha", "contract-sha", baseDigest,
	} {
		if strings.Contains(string(raw), leaked) {
			t.Errorf("persisted verdict leaked %q: %s", leaked, raw)
		}
	}
}

func TestEvaluateConsumersProbedPrivateReadFailureIsMasked(t *testing.T) {
	policy := consumerPolicy(t, store.ConsumerRef{
		Repository: "acme/hidden-consumer", Contract: "api/openapi.yaml", Ref: "v1",
		Visibility: store.ConsumerVisibilityPublic, Essential: true,
	})
	reader := &fakeConsumerReader{
		repository: &gh.Repository{Visibility: gh.String("private"), Private: gh.Bool(true)},
		shaResp:    ghResponse(http.StatusNotFound),
		shaErr:     errors.New("not found"),
	}
	got := evaluateConsumers(context.Background(), consumerEvaluationFixture(policy,
		fakeVisibilityResolver(reader), map[string]string{"base-sha": "base-digest"}))

	consumer := got.Consumers[0]
	if consumer.Status != store.ConsumerStatusUnknown || consumer.Reason != store.ConsumerReasonUnavailable {
		t.Fatalf("consumer = %+v, want unknown/unavailable", consumer)
	}
	if !consumer.Private || !consumer.Redacted {
		t.Errorf("a failed read discarded the private probe: %+v", consumer)
	}
	raw, err := json.Marshal(store.MaskRedactedConsumerImpact(got))
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"acme/hidden-consumer", "api/openapi.yaml", `"v1"`} {
		if strings.Contains(string(raw), leaked) {
			t.Errorf("persisted snapshot leaked %q: %s", leaked, raw)
		}
	}
}

func TestEvaluateConsumersProbedPublicIsUnredactedWhenConfiguredPrivate(t *testing.T) {
	policy := consumerPolicy(t, store.ConsumerRef{
		Repository: "acme/public-consumer", Contract: "api/openapi.yaml", Ref: "v1",
		Essential: true,
	})
	reader := &fakeConsumerReader{
		repository: &gh.Repository{Visibility: gh.String("public")},
		sha:        "consumer-sha",
		file:       consumerContractFile("contract"),
	}
	sum := sha256.Sum256([]byte("contract"))
	baseDigest := hex.EncodeToString(sum[:])
	evaluation := consumerEvaluationFixture(policy, fakeVisibilityResolver(reader),
		map[string]string{"base-sha": baseDigest, "head-sha": "head-digest"})
	got := evaluateConsumers(context.Background(), evaluation)

	consumer := got.Consumers[0]
	if consumer.Status != store.ConsumerStatusAffected || consumer.Impact != store.ConsumerImpactVerified {
		t.Fatalf("consumer = %+v, want affected/verified", consumer)
	}
	if consumer.Private || consumer.Redacted {
		t.Errorf("a probed public consumer stayed private: %+v", consumer)
	}
	verdict := applyConsumerImpact(store.ArchitectureVerdict{Requested: true, Consumers: got})
	if !strings.Contains(consumerDimensionContent(got), "acme/public-consumer") {
		t.Errorf("public consumer was not named in the evidence: %+v", verdict.Dimensions)
	}
}

func TestGitHubConsumerResolverRateLimitIsUnavailable(t *testing.T) {
	rateLimit := &gh.RateLimitError{Response: &http.Response{StatusCode: http.StatusForbidden}}
	reader := &fakeConsumerReader{
		repositoryResp: ghResponse(http.StatusForbidden),
		repositoryErr:  rateLimit,
	}
	_, err := fakeVisibilityResolver(reader).Resolve(context.Background(), consumerResolveRequest{
		Repository: "acme/consumer", Ref: "v1", Path: "api/openapi.yaml", ConfiguredPrivate: true,
	})
	if !errors.Is(err, errConsumerUnavailable) {
		t.Errorf("rate-limited probe error = %v, want unavailable", err)
	}
	if reader.shaCalls != 0 {
		t.Errorf("rate-limited probe read the contract %d times", reader.shaCalls)
	}
}

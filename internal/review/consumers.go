package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v93/github"

	appgithub "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	// consumerDimensionName is the verdict section that carries the consumer
	// impact. It is added only when at least one consumer is configured.
	consumerDimensionName = "Impacto en consumidores"

	// maxConsumerBaselineBytes bounds the optional baseline file. A baseline is
	// a declaration of consumers, not a place to ship data.
	maxConsumerBaselineBytes = 64 << 10

	// consumerLookupTimeout bounds one remote read. The aggregate lookup is also
	// capped so a slow consumer cannot delay the review indefinitely.
	consumerLookupTimeout = 10 * time.Second
	consumerTotalTimeMax  = 30 * time.Second
)

var (
	errConsumerUnauthorized = errors.New("consumer unauthorized")
	errConsumerUnavailable  = errors.New("consumer unavailable")
	errConsumerTooLarge     = errors.New("consumer contract too large")
	errConsumerDigest       = errors.New("consumer contract digest unavailable")
)

// configuredConsumer is one allow-list entry with the source that declared it.
type configuredConsumer struct {
	Ref    store.ConsumerRef
	Source string
}

// consumerResolveRequest is everything a resolver needs to read one pinned
// consumer contract. Only allow-listed entries ever become a request.
type consumerResolveRequest struct {
	Repository     string
	Ref            string
	Path           string
	InstallationID int64
	MaxBytes       int64
	// ConfiguredPrivate is the visibility the policy declared. It is only a
	// fallback: the resolver probes GitHub when it can.
	ConfiguredPrivate bool
}

// consumerResolution is what the resolver could prove. No file content leaves
// the resolver: only the pinned commit and a digest of the consumer contract.
type consumerResolution struct {
	PinnedSHA   string
	ContractSHA string
	Digest      string
	// Private and VisibilityKnown carry the probed visibility. When Known is
	// false the caller keeps the configured visibility.
	Private         bool
	VisibilityKnown bool
}

// consumerRepositoryReader is the read-only slice of the GitHub repositories
// API the consumer resolver needs. *gh.RepositoriesService implements it.
type consumerRepositoryReader interface {
	Get(ctx context.Context, owner, repo string) (*gh.Repository, *gh.Response, error)
	GetCommitSHA1(ctx context.Context, owner, repo, ref, lastSHA string) (string, *gh.Response, error)
	GetContents(ctx context.Context, owner, repo, path string,
		opts *gh.RepositoryContentGetOptions) (*gh.RepositoryContent, []*gh.RepositoryContent, *gh.Response, error)
}

// consumerResolver reads one consumer contract at its pinned ref. The review
// pipeline only ever passes allow-listed entries to it.
type consumerResolver interface {
	Resolve(ctx context.Context, req consumerResolveRequest) (consumerResolution, error)
}

// consumerLookupBudget bounds how many consumers are contacted and how many
// bytes each contract may have.
type consumerLookupBudget struct {
	MaxLookups int
	MaxBytes   int64
	PerLookup  time.Duration
}

// consumerEvaluation is the input of the consumer layer. ContractDigest is
// injectable so the comparison is testable without a checkout.
type consumerEvaluation struct {
	Dir             string
	BaseRef         string
	HeadRef         string
	Files           []changedFile
	Policy          store.QAPolicy
	ProducerPrivate bool
	Remaining       time.Duration
	Resolver        consumerResolver
	ContractDigest  func(ctx context.Context, ref, path string, maxBytes int64) (string, error)
	// BaselineRead overrides the local git read of the optional baseline. The
	// control process uses the GitHub App because it has no checkout.
	BaselineRead func(ctx context.Context, ref, path string, maxBytes int64) ([]byte, error)
	// Configured, Sources and ConfigErrors let the control process resolve the
	// allow-list and baseline once and reuse it for the evaluation.
	Configured   []configuredConsumer
	Sources      []string
	ConfigErrors []string
	configReady  bool
}

// evaluateConsumers resolves the authorized consumers whose declared contract
// changed in this comparison. Every outcome is evidence: it never returns an
// error and never changes the review result. An essential consumer that cannot
// be proved leaves the aggregate unknown and asks for a human decision.
func evaluateConsumers(ctx context.Context, opts consumerEvaluation) store.ArchitectureConsumers {
	result := store.ArchitectureConsumers{Status: store.ConsumersNotConfigured}

	// The aggregate lookup window is a share of what the run has left and has
	// its own hard cap, so a slow consumer cannot extend the review.
	total := consumerTotalTimeMax
	if opts.Remaining > 0 && opts.Remaining < total {
		total = opts.Remaining
	}
	if total <= 0 {
		total = consumerTotalTimeMax
	}
	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	configured, sources, configErrors := opts.Configured, opts.Sources, opts.ConfigErrors
	if !opts.configReady {
		readFile := opts.BaselineRead
		if readFile == nil {
			dir := opts.Dir
			readFile = func(ctx context.Context, ref, path string, maxBytes int64) ([]byte, error) {
				return gitFileBytes(ctx, dir, ref, path, maxBytes)
			}
		}
		configured, sources, configErrors = consumerConfig(ctx, opts.BaseRef, opts.Policy, readFile)
	}
	result.Configured = len(configured)
	result.Sources = sources
	result.Errors = append(result.Errors, configErrors...)
	if len(configured) == 0 {
		return result
	}

	budget := consumerBudgetFor(opts.Policy, opts.Remaining)
	digest := opts.ContractDigest
	if digest == nil {
		dir := opts.Dir
		digest = func(ctx context.Context, ref, path string, maxBytes int64) (string, error) {
			return gitContractDigest(ctx, dir, ref, path, maxBytes)
		}
	}
	resolver := opts.Resolver
	if resolver == nil {
		resolver = unavailableConsumerResolver{}
	}
	digests := make(map[string]consumerDigests)

	lookups := 0
	for _, entry := range configured {
		consumer := newArchitectureConsumer(entry, opts.ProducerPrivate)
		if entry.Ref.Essential {
			result.Essential++
		}
		if !consumerContractChanged(entry.Ref.Contract, opts.Files) {
			consumer.Status = store.ConsumerStatusOutOfScope
			consumer.Impact = store.ConsumerImpactNone
			consumer.Reason = store.ConsumerReasonContractUnchanged
			result.Consumers = append(result.Consumers, consumer)
			continue
		}
		if lookups >= budget.MaxLookups {
			consumer.Status = store.ConsumerStatusSkipped
			consumer.Impact = store.ConsumerImpactUnknown
			consumer.Reason = store.ConsumerReasonLookupBudget
			if entry.Ref.Essential {
				result.RequiresHuman = true
			}
			result.Consumers = append(result.Consumers, consumer)
			continue
		}
		lookups++
		result.Checked++

		producer := digests[entry.Ref.Contract]
		if !producer.known {
			baseDigest, headDigest := consumerProducerDigests(ctx, opts.Dir, opts.BaseRef, opts.HeadRef, entry.Ref.Contract, budget.MaxBytes, digest)
			producer = consumerDigests{known: true, Base: baseDigest, Head: headDigest}
			digests[entry.Ref.Contract] = producer
		}

		lookupCtx, cancel := context.WithTimeout(ctx, budget.PerLookup)
		resolved, err := resolver.Resolve(lookupCtx, consumerResolveRequest{
			Repository:        entry.Ref.Repository,
			Ref:               entry.Ref.Ref,
			Path:              entry.Ref.Path,
			InstallationID:    entry.Ref.InstallationID,
			MaxBytes:          budget.MaxBytes,
			ConfiguredPrivate: store.ConsumerIsPrivate(entry.Ref),
		})
		timedOut := lookupCtx.Err() != nil
		cancel()

		// The probe is authoritative even when the read fails, so a repository
		// GitHub reported private is masked before the reason switch.
		if resolved.VisibilityKnown {
			consumer.Private = resolved.Private
			consumer.Redacted = resolved.Private && !opts.ProducerPrivate
		}
		switch {
		case err == nil:
			consumer.SHA = resolved.PinnedSHA
			consumer.ContractSHA = resolved.ContractSHA
			consumer.Digest = resolved.Digest
			consumer.Version = entry.Ref.Version
			classifyConsumerImpact(&consumer, producer)
		case errors.Is(err, errConsumerUnauthorized):
			consumer.Status = store.ConsumerStatusUnknown
			consumer.Impact = store.ConsumerImpactUnknown
			consumer.Reason = store.ConsumerReasonUnauthorized
		case errors.Is(err, errConsumerTooLarge):
			consumer.Status = store.ConsumerStatusSkipped
			consumer.Impact = store.ConsumerImpactUnknown
			consumer.Reason = store.ConsumerReasonTooLarge
		case errors.Is(err, errConsumerUnavailable):
			consumer.Status = store.ConsumerStatusUnknown
			consumer.Impact = store.ConsumerImpactUnknown
			consumer.Reason = store.ConsumerReasonUnavailable
		case timedOut || errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			consumer.Status = store.ConsumerStatusUnknown
			consumer.Impact = store.ConsumerImpactUnknown
			consumer.Reason = store.ConsumerReasonTimeout
		case errors.Is(err, errConsumerDigest):
			consumer.Status = store.ConsumerStatusUnknown
			consumer.Impact = store.ConsumerImpactUnknown
			consumer.Reason = store.ConsumerReasonNoDigest
		default:
			consumer.Status = store.ConsumerStatusUnknown
			consumer.Impact = store.ConsumerImpactUnknown
			consumer.Reason = store.ConsumerReasonLookupFailed
		}
		if entry.Ref.Essential && (consumer.Status == store.ConsumerStatusUnknown ||
			consumer.Status == store.ConsumerStatusSkipped) {
			result.RequiresHuman = true
		}
		result.Consumers = append(result.Consumers, consumer)
	}

	if result.Checked > 0 {
		result.Sources = appendUnique(result.Sources, "github")
	}
	result.Status = consumersAggregateStatus(result)
	return result
}

// consumerDigests are the producer-side versions of the contract. An empty
// digest means the file did not exist at that ref.
type consumerDigests struct {
	known bool
	Base  string
	Head  string
}

func newArchitectureConsumer(entry configuredConsumer, producerPrivate bool) store.ArchitectureConsumer {
	consumer := store.ArchitectureConsumer{
		Repository: entry.Ref.Repository,
		Contract:   entry.Ref.Contract,
		Path:       entry.Ref.Path,
		Ref:        entry.Ref.Ref,
		Essential:  entry.Ref.Essential,
		Source:     entry.Source,
		Private:    store.ConsumerIsPrivate(entry.Ref),
	}
	// The PR must not reveal another repository's details when visibility is not
	// equivalent. A private producer already has the same audience.
	consumer.Redacted = consumer.Private && !producerPrivate
	return consumer
}

// classifyConsumerImpact compares the consumer's contract digest with the
// producer's base and head. It is deliberately conservative: a digest that
// matches neither side is missing evidence, not compatibility.
func classifyConsumerImpact(consumer *store.ArchitectureConsumer, producer consumerDigests) {
	switch {
	case strings.TrimSpace(consumer.Digest) == "":
		consumer.Status = store.ConsumerStatusUnknown
		consumer.Impact = store.ConsumerImpactUnknown
		consumer.Reason = store.ConsumerReasonNoDigest
	case producer.Head != "" && consumer.Digest == producer.Head:
		consumer.Status = store.ConsumerStatusCompatible
		consumer.Impact = store.ConsumerImpactNone
		consumer.Reason = store.ConsumerReasonOnHead
		consumer.ProducerDigest = producer.Head
	case producer.Base != "" && producer.Base == producer.Head &&
		(consumer.Digest == producer.Head || consumer.Digest == producer.Base):
		consumer.Status = store.ConsumerStatusCompatible
		consumer.Impact = store.ConsumerImpactNone
		consumer.Reason = store.ConsumerReasonContractUnchanged
		consumer.ProducerDigest = producer.Head
	case producer.Base != "" && consumer.Digest == producer.Base:
		consumer.Status = store.ConsumerStatusAffected
		consumer.Impact = store.ConsumerImpactVerified
		consumer.Reason = store.ConsumerReasonOnBase
		consumer.ProducerDigest = producer.Head
	default:
		consumer.Status = store.ConsumerStatusUnknown
		consumer.Impact = store.ConsumerImpactUnknown
		consumer.Reason = store.ConsumerReasonDifferentVersion
		consumer.ProducerDigest = producer.Head
	}
}

// consumersAggregateStatus never reports complete when an essential consumer
// could not be proved: that case is unknown and requires a person.
func consumersAggregateStatus(result store.ArchitectureConsumers) string {
	if result.Configured == 0 {
		return store.ConsumersNotConfigured
	}
	if result.RequiresHuman {
		return store.ConsumersUnknown
	}
	incomplete := len(result.Errors) > 0
	for _, consumer := range result.Consumers {
		if consumer.Status == store.ConsumerStatusUnknown || consumer.Status == store.ConsumerStatusSkipped {
			incomplete = true
		}
	}
	if incomplete {
		return store.ConsumersPartial
	}
	return store.ConsumersComplete
}

// consumerLookupRelevant reports whether the layer did any work worth an
// architectural verdict: a configured consumer outside the unchanged set, or a
// configuration problem that left the scope unknown.
func consumerLookupRelevant(result store.ArchitectureConsumers) bool {
	if result.Status == "" || result.Status == store.ConsumersNotConfigured {
		return false
	}
	if len(result.Errors) > 0 {
		return true
	}
	for _, consumer := range result.Consumers {
		if consumer.Status != store.ConsumerStatusOutOfScope {
			return true
		}
	}
	return false
}

// consumerContractChanged reports whether the declared contract is part of the
// reviewed range. Consumers for unchanged contracts are not contacted.
func consumerContractChanged(contract string, files []changedFile) bool {
	contract = normalizeCoveragePath(contract)
	if contract == "" {
		return false
	}
	for _, file := range files {
		if normalizeCoveragePath(file.Path) == contract || normalizeCoveragePath(file.OldPath) == contract {
			return true
		}
	}
	return false
}

func consumerBudgetFor(policy store.QAPolicy, remaining time.Duration) consumerLookupBudget {
	lookups := policy.ConsumerMaxLookups
	if lookups <= 0 {
		lookups = store.DefaultConsumerMaxLookups
	}
	if lookups > store.ConsumerMaxLookupsMax {
		lookups = store.ConsumerMaxLookupsMax
	}
	maxBytes := policy.ConsumerMaxBytes
	if maxBytes <= 0 {
		maxBytes = store.DefaultConsumerMaxBytes
	}
	if maxBytes > store.ConsumerMaxBytesMax {
		maxBytes = store.ConsumerMaxBytesMax
	}
	perLookup := consumerLookupTimeout
	if remaining > 0 && remaining < consumerTotalTimeMax {
		perLookup = remaining
	}
	if perLookup > consumerTotalTimeMax {
		perLookup = consumerTotalTimeMax
	}
	if perLookup <= 0 {
		perLookup = consumerLookupTimeout
	}
	return consumerLookupBudget{MaxLookups: lookups, MaxBytes: maxBytes, PerLookup: perLookup}
}

// consumerConfig merges the policy allow-list with the optional baseline file.
// The policy wins on a duplicate; the baseline is read from the reviewed base
// so a pull request cannot change the consumers it is checked against.
func consumerConfig(ctx context.Context, baseRef string, policy store.QAPolicy,
	readFile func(context.Context, string, string, int64) ([]byte, error)) ([]configuredConsumer, []string, []string) {
	var configured []configuredConsumer
	var sources []string
	var problems []string

	policyConsumers, err := store.ParseConsumerAllowList(policy.ConsumerAllowList)
	if err != nil {
		problems = append(problems, "no se pudo leer la lista de consumidores de la política")
	} else if policyConsumers, err = normalizedConsumerRefs(policyConsumers); err != nil {
		problems = append(problems, "la lista de consumidores de la política no es válida")
		policyConsumers = nil
	}
	for _, ref := range policyConsumers {
		configured = append(configured, configuredConsumer{Ref: ref, Source: store.ConsumerSourcePolicy})
	}
	if len(policyConsumers) > 0 {
		sources = append(sources, store.ConsumerSourcePolicy)
	}

	baselinePath := strings.TrimSpace(policy.ConsumerBaselinePath)
	if baselinePath != "" {
		if !store.ValidConsumerBaselinePath(baselinePath) {
			problems = append(problems, "la ruta del baseline de consumidores no es válida")
		} else if strings.TrimSpace(baseRef) == "" {
			problems = append(problems, "no hay base para leer el baseline de consumidores")
		} else {
			raw, err := readFile(ctx, baseRef, baselinePath, maxConsumerBaselineBytes)
			if err != nil {
				problems = append(problems, fmt.Sprintf("no se pudo leer el baseline %s", baselinePath))
			} else if refs, err := parseConsumerBaseline(raw); err != nil {
				problems = append(problems, fmt.Sprintf("el baseline %s no es válido", baselinePath))
			} else if refs, err = normalizedConsumerRefs(refs); err != nil {
				problems = append(problems, fmt.Sprintf("el baseline %s no es válido", baselinePath))
			} else {
				sources = append(sources, store.ConsumerSourceBaseline+":"+baselinePath)
				for _, ref := range refs {
					configured = append(configured, configuredConsumer{Ref: ref, Source: store.ConsumerSourceBaseline})
				}
			}
		}
	}

	merged := mergeConsumers(configured)
	return merged, sources, problems
}

// normalizedConsumerRefs applies the same validation to the baseline as the
// policy, so an unexpected file cannot escape the checkout or inject a wildcard
// path.
func normalizedConsumerRefs(refs []store.ConsumerRef) ([]store.ConsumerRef, error) {
	encoded, err := store.NormalizeConsumerAllowList(refs)
	if err != nil {
		return nil, err
	}
	return store.ParseConsumerAllowList(encoded)
}

// mergeConsumers deduplicates by repository and contract. The policy entry is
// first and therefore wins; the output order is stable for tests and evidence.
func mergeConsumers(configured []configuredConsumer) []configuredConsumer {
	seen := make(map[string]bool, len(configured))
	merged := make([]configuredConsumer, 0, len(configured))
	for _, entry := range configured {
		key := entry.Ref.Repository + "\x00" + entry.Ref.Contract
		if seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, entry)
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].Ref.Repository != merged[j].Ref.Repository {
			return merged[i].Ref.Repository < merged[j].Ref.Repository
		}
		if merged[i].Ref.Contract != merged[j].Ref.Contract {
			return merged[i].Ref.Contract < merged[j].Ref.Contract
		}
		return merged[i].Ref.Ref < merged[j].Ref.Ref
	})
	return merged
}

func parseConsumerBaseline(raw []byte) ([]store.ConsumerRef, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var refs []store.ConsumerRef
	if err := json.Unmarshal(trimmed, &refs); err == nil {
		return refs, nil
	}
	var document struct {
		Consumers []store.ConsumerRef `json:"consumers"`
	}
	if err := json.Unmarshal(trimmed, &document); err != nil {
		return nil, err
	}
	return document.Consumers, nil
}

// consumerProducerDigests reads the producer contract at the reviewed base and
// head. A missing file is an empty digest, not an error: a new or deleted
// contract is a valid comparison input.
func consumerProducerDigests(ctx context.Context, dir, baseRef, headRef, contract string,
	maxBytes int64, digest func(context.Context, string, string, int64) (string, error)) (string, string) {
	var base, head string
	if strings.TrimSpace(baseRef) != "" {
		value, err := digest(ctx, baseRef, contract, maxBytes)
		if err == nil {
			base = value
		}
	}
	if strings.TrimSpace(headRef) != "" {
		value, err := digest(ctx, headRef, contract, maxBytes)
		if err == nil {
			head = value
		}
	}
	return base, head
}

// gitContractDigest hashes a file at a ref without checking it out. The size is
// read first so an oversized contract is never loaded into memory.
func gitContractDigest(ctx context.Context, dir, ref, path string, maxBytes int64) (string, error) {
	if maxBytes <= 0 {
		maxBytes = store.DefaultConsumerMaxBytes
	}
	obj := ref + ":" + path
	sizeText, err := git(ctx, dir, "cat-file size", "cat-file", "-s", obj)
	if err != nil {
		return "", err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(sizeText), 10, 64)
	if err != nil || size < 0 {
		return "", fmt.Errorf("tamaño de %s no válido", path)
	}
	if size > maxBytes {
		return "", errConsumerTooLarge
	}
	raw, err := gitFileBytes(ctx, dir, ref, path, maxBytes)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// gitFileBytes reads one file from a ref and enforces the byte cap after the
// read as a second guard for a size that changed between ls-tree and show.
func gitFileBytes(ctx context.Context, dir, ref, path string, maxBytes int64) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "show", ref+":"+path)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			return nil, fmt.Errorf("git show %s: %w", path, err)
		}
		return nil, fmt.Errorf("git show %s: %w: %s", path, err, detail)
	}
	if maxBytes > 0 && int64(stdout.Len()) > maxBytes {
		return nil, errConsumerTooLarge
	}
	return stdout.Bytes(), nil
}

// applyConsumerImpact copies the consumer evidence into the verdict sections the
// publisher and the panel render. Risks and uncertainties only carry text that
// is safe to publish: a redacted consumer is never named.
func applyConsumerImpact(verdict store.ArchitectureVerdict) store.ArchitectureVerdict {
	consumers := verdict.Consumers
	if consumers.Status == "" || consumers.Status == store.ConsumersNotConfigured {
		return verdict
	}
	// Consumer evidence is independent of the OCR analysis. A verdict without a
	// requested analysis still publishes it when the lookup did work; an
	// all-out-of-scope lookup without a requested analysis stays silent.
	if !verdict.Requested && !consumerLookupRelevant(consumers) {
		return verdict
	}
	verdict.Dimensions = replaceConsumerDimension(verdict.Dimensions, consumerDimensionContent(consumers))

	for _, consumer := range consumers.Consumers {
		label := consumerEvidenceLabel(consumer)
		switch {
		case consumer.Status == store.ConsumerStatusAffected && consumer.Impact == store.ConsumerImpactVerified:
			if consumer.Redacted {
				verdict.Risks = appendUnique(verdict.Risks,
					"Un consumidor privado sigue fijado en el contrato anterior durante la coexistencia de versiones.")
			} else {
				verdict.Risks = appendUnique(verdict.Risks,
					fmt.Sprintf("%s sigue fijado en el contrato anterior de %s durante la coexistencia de versiones.",
						label, consumer.Contract))
			}
		case consumer.Status == store.ConsumerStatusUnknown:
			verdict.Uncertainties = appendUnique(verdict.Uncertainties,
				fmt.Sprintf("No hay evidencia suficiente sobre %s (%s).", label, consumerReasonLabel(consumer.Reason)))
		case consumer.Status == store.ConsumerStatusSkipped:
			verdict.Uncertainties = appendUnique(verdict.Uncertainties,
				fmt.Sprintf("%s quedó sin consultar (%s).", label, consumerReasonLabel(consumer.Reason)))
		}
	}
	if consumers.RequiresHuman {
		verdict.Uncertainties = appendUnique(verdict.Uncertainties,
			"Un consumidor esencial no se pudo comprobar; la decisión requiere criterio humano.")
	}
	outOfScope := 0
	for _, consumer := range consumers.Consumers {
		if consumer.Status == store.ConsumerStatusOutOfScope {
			outOfScope++
		}
	}
	if outOfScope > 0 {
		verdict.Uncertainties = appendUnique(verdict.Uncertainties,
			fmt.Sprintf("%d consumidor(es) quedaron fuera del alcance porque su contrato no cambió.", outOfScope))
	}
	for _, problem := range consumers.Errors {
		verdict.Uncertainties = appendUnique(verdict.Uncertainties, problem+".")
	}
	return verdict
}

func replaceConsumerDimension(dimensions []store.ArchitectureDimension, content string) []store.ArchitectureDimension {
	if strings.TrimSpace(content) == "" {
		return dimensions
	}
	replaced := false
	for i := range dimensions {
		if dimensions[i].Name == consumerDimensionName {
			dimensions[i].Content = content
			replaced = true
		}
	}
	if !replaced {
		dimensions = append(dimensions, store.ArchitectureDimension{
			Name: consumerDimensionName, Content: content,
		})
	}
	return dimensions
}

func consumerDimensionContent(consumers store.ArchitectureConsumers) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Estado: %s · %d configurado(s) · %d consultado(s)", consumers.Status, consumers.Configured, consumers.Checked)
	if consumers.Essential > 0 {
		fmt.Fprintf(&b, " · %d esencial(es)", consumers.Essential)
	}
	b.WriteString("\n")
	for _, consumer := range consumers.Consumers {
		label := consumerEvidenceLabel(consumer)
		line := fmt.Sprintf("- %s: %s", label, consumer.Status)
		if consumer.Impact != "" {
			line += fmt.Sprintf(" (impacto %s)", consumer.Impact)
		}
		if consumer.SHA != "" && !consumer.Redacted {
			line += " @ " + consumer.SHA
		}
		if consumer.Reason != "" {
			line += " — " + consumerReasonLabel(consumer.Reason)
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimSpace(b.String())
}

// consumerEvidenceLabel names a consumer only when its visibility allows it.
// The contract path stays out of a redacted label too.
func consumerEvidenceLabel(consumer store.ArchitectureConsumer) string {
	if consumer.Redacted {
		return "consumidor privado"
	}
	label := consumer.Repository
	if consumer.Contract != "" {
		label += " (" + consumer.Contract + ")"
	}
	return label
}

func consumerReasonLabel(reason string) string {
	switch reason {
	case store.ConsumerReasonUnauthorized:
		return "sin autorización de la App"
	case store.ConsumerReasonUnavailable:
		return "no disponible"
	case store.ConsumerReasonTimeout:
		return "tiempo de consulta agotado"
	case store.ConsumerReasonLookupFailed:
		return "falló la consulta"
	case store.ConsumerReasonLookupBudget:
		return "presupuesto de consultas agotado"
	case store.ConsumerReasonTooLarge:
		return "contrato demasiado grande"
	case store.ConsumerReasonNoDigest:
		return "sin digest del contrato"
	case store.ConsumerReasonContractUnchanged:
		return "contrato sin cambios"
	case store.ConsumerReasonDifferentVersion:
		return "versión distinta sin evidencia de compatibilidad"
	case store.ConsumerReasonOnHead:
		return "ya referencia la cabeza"
	case store.ConsumerReasonOnBase:
		return "sigue en la versión base"
	default:
		if reason == "" {
			return "sin evidencia"
		}
		return reason
	}
}

// unavailableConsumerResolver is used when no GitHub access is configured. It
// records the consumer as unavailable instead of silently passing it.
type unavailableConsumerResolver struct{}

func (unavailableConsumerResolver) Resolve(context.Context, consumerResolveRequest) (consumerResolution, error) {
	return consumerResolution{}, errConsumerUnavailable
}

// githubConsumerResolver reads a consumer contract through the App installation
// that covers it. The allow-list is the only source of requests, so the App is
// never used to browse repositories the team did not configure. Before reading,
// it probes the repository visibility; the probe is cached per run because a
// resolver instance lives for one control-process resolution.
type githubConsumerResolver struct {
	ownerInstallation int64
	readers           func(ctx context.Context, installationID int64) (consumerRepositoryReader, error)

	mu      sync.Mutex
	clients map[int64]consumerRepositoryReader
	probes  map[string]consumerVisibilityProbe
}

// consumerVisibilityProbe is the cached answer of GET /repos/{owner}/{repo}.
// status is 0 when the request succeeded; err is a transport or decoding error.
type consumerVisibilityProbe struct {
	private bool
	status  int
	err     error
}

func newGitHubConsumerResolver(apps appgithub.AppProvider, ownerInstallation int64) consumerResolver {
	if apps == nil {
		return unavailableConsumerResolver{}
	}
	readers := func(ctx context.Context, installationID int64) (consumerRepositoryReader, error) {
		app, err := apps.App(ctx)
		if err != nil {
			return nil, err
		}
		client, err := app.InstallationClient(installationID)
		if err != nil {
			return nil, err
		}
		return client.Repositories, nil
	}
	return newGitHubConsumerResolverWithReaders(readers, ownerInstallation)
}

func newGitHubConsumerResolverWithReaders(
	readers func(context.Context, int64) (consumerRepositoryReader, error), ownerInstallation int64) *githubConsumerResolver {
	return &githubConsumerResolver{
		ownerInstallation: ownerInstallation,
		readers:           readers,
		clients:           make(map[int64]consumerRepositoryReader),
		probes:            make(map[string]consumerVisibilityProbe),
	}
}

func (r *githubConsumerResolver) Resolve(ctx context.Context, req consumerResolveRequest) (consumerResolution, error) {
	owner, name, ok := splitRepository(req.Repository)
	if !ok {
		return consumerResolution{}, errConsumerUnavailable
	}
	installationID := req.InstallationID
	if installationID == 0 {
		installationID = r.ownerInstallation
	}
	client, err := r.client(ctx, installationID)
	if err != nil {
		return consumerResolution{}, err
	}

	private, err := r.probeVisibility(ctx, client, installationID, owner, name, req.ConfiguredPrivate)
	if err != nil {
		return consumerResolution{}, err
	}
	// A late read error must not discard what the probe proved: the caller uses
	// it to mask the consumer even when the contract could not be read.
	resolved := consumerResolution{Private: private, VisibilityKnown: true}

	sha, resp, err := client.GetCommitSHA1(ctx, owner, name, req.Ref, "")
	if err != nil {
		return resolved, consumerLookupError(resp, err)
	}
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return resolved, errConsumerUnavailable
	}

	file, _, resp, err := client.GetContents(ctx, owner, name, req.Path,
		&gh.RepositoryContentGetOptions{Ref: req.Ref})
	if err != nil {
		return resolved, consumerLookupError(resp, err)
	}
	if file == nil {
		return resolved, errConsumerUnavailable
	}
	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = store.DefaultConsumerMaxBytes
	}
	if file.GetSize() > int(maxBytes) {
		return resolved, errConsumerTooLarge
	}
	content, err := consumerFileContent(file)
	if err != nil {
		return resolved, err
	}
	sum := sha256.Sum256([]byte(content))
	resolved.PinnedSHA = sha
	resolved.ContractSHA = strings.TrimSpace(file.GetSHA())
	resolved.Digest = hex.EncodeToString(sum[:])
	return resolved, nil
}

// probeVisibility asks GitHub whether the consumer repository is public. A
// private repository whose read succeeds is authorized: the App could see it.
// A forbidden, missing or failed answer is an explicit unknown, never a reason
// to trust the configured visibility for compatibility.
func (r *githubConsumerResolver) probeVisibility(ctx context.Context, client consumerRepositoryReader,
	installationID int64, owner, name string, configuredPrivate bool) (bool, error) {
	// Authorization is installation-specific: a repository hidden from one
	// installation may be visible to another, so the cache is keyed by both.
	key := strconv.FormatInt(installationID, 10) + "\x00" + strings.ToLower(owner+"/"+name)
	r.mu.Lock()
	probe, ok := r.probes[key]
	r.mu.Unlock()
	if !ok {
		repository, resp, err := client.Get(ctx, owner, name)
		probe = consumerVisibilityProbe{status: probeStatusCode(resp, err), err: err}
		if err == nil {
			if repository == nil {
				probe.err = errConsumerUnavailable
			} else {
				probe.private = repositoryIsPrivate(repository)
			}
		}
		// A canceled lookup says nothing about the repository; a later consumer
		// of the same run may still ask.
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			r.mu.Lock()
			r.probes[key] = probe
			r.mu.Unlock()
		}
	}
	switch {
	case errors.Is(probe.err, context.DeadlineExceeded) || errors.Is(probe.err, context.Canceled):
		return false, probe.err
	case isRateLimitError(probe.err):
		// A rate-limited 403 says nothing about authorization.
		return false, errConsumerUnavailable
	case probe.status == http.StatusUnauthorized || probe.status == http.StatusForbidden:
		return false, errConsumerUnauthorized
	case probe.status == http.StatusNotFound:
		// GitHub answers 404 for a private repository the installation cannot
		// see, so a configured private consumer is explicitly unauthorized.
		if configuredPrivate {
			return false, errConsumerUnauthorized
		}
		return false, errConsumerUnavailable
	case probe.err != nil:
		return false, errConsumerUnavailable
	}
	return probe.private, nil
}

func isRateLimitError(err error) bool {
	var rateLimit *gh.RateLimitError
	if errors.As(err, &rateLimit) {
		return true
	}
	var abuse *gh.AbuseRateLimitError
	return errors.As(err, &abuse)
}

func repositoryIsPrivate(repository *gh.Repository) bool {
	if visibility := strings.ToLower(strings.TrimSpace(repository.GetVisibility())); visibility != "" {
		return visibility != store.ConsumerVisibilityPublic
	}
	return repository.GetPrivate()
}

func probeStatusCode(resp *gh.Response, err error) int {
	if resp != nil {
		return resp.StatusCode
	}
	var response *gh.ErrorResponse
	if errors.As(err, &response) && response.Response != nil {
		return response.Response.StatusCode
	}
	return 0
}

func (r *githubConsumerResolver) client(ctx context.Context, installationID int64) (consumerRepositoryReader, error) {
	if installationID <= 0 {
		return nil, errConsumerUnauthorized
	}
	r.mu.Lock()
	client := r.clients[installationID]
	r.mu.Unlock()
	if client != nil {
		return client, nil
	}
	if r.readers == nil {
		return nil, errConsumerUnavailable
	}
	client, err := r.readers(ctx, installationID)
	if err != nil || client == nil {
		return nil, errConsumerUnavailable
	}
	r.mu.Lock()
	r.clients[installationID] = client
	r.mu.Unlock()
	return client, nil
}

// consumerFileContent rejects a body that cannot be hashed faithfully. GitHub
// returns an empty body for files over its contents API limit even when base64
// is declared; hashing that would produce the empty digest and could falsely
// match an empty producer contract.
func consumerFileContent(file *gh.RepositoryContent) (string, error) {
	if file == nil {
		return "", errConsumerUnavailable
	}
	size := file.GetSize()
	encoding := strings.ToLower(strings.TrimSpace(file.GetEncoding()))
	if size > 0 && encoding != "" && encoding != "base64" && encoding != "none" {
		return "", errConsumerDigest
	}
	content, err := file.GetContent()
	if err != nil {
		return "", errConsumerDigest
	}
	if size > 0 && strings.TrimSpace(content) == "" {
		return "", errConsumerDigest
	}
	return content, nil
}

func consumerLookupError(resp *gh.Response, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	if resp != nil {
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return errConsumerUnauthorized
		case http.StatusNotFound:
			return errConsumerUnavailable
		}
	}
	var response *gh.ErrorResponse
	if errors.As(err, &response) && response.Response != nil {
		switch response.Response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return errConsumerUnauthorized
		case http.StatusNotFound:
			return errConsumerUnavailable
		}
	}
	return errConsumerUnavailable
}

// changedFilesResolver is implemented by github.Compare. The control process
// uses it only when the policy configures consumers, so runs without them keep
// their existing Compare call count.
type changedFilesResolver interface {
	ComparisonFiles(ctx context.Context, installationID int64, repoFullName, base, head string) (appgithub.Comparison, error)
}

// consumerTriggerEvaluation is what the control process needs to resolve the
// authorized consumers before queueing a run. It has the App credentials and
// no checkout, so it reads the producer contracts through the same App.
type consumerTriggerEvaluation struct {
	Apps            appgithub.AppProvider
	Comparator      any
	InstallationID  int64
	Repository      string
	// Base is the recorded base tip (or the branch name when no tip is known),
	// pinned so the changed-file diff belongs to the comparison being queued.
	Base            string
	HeadSHA         string
	MergeBase       string
	Policy          store.QAPolicy
	ProducerPrivate bool
}

// resolveConsumerImpact resolves the allow-listed consumers in the control
// process, where the GitHub App credentials live (#139 leaves the worker
// without them). It never fails enqueue: an unavailable diff or lookup becomes
// an explicit unknown entry so a configured consumer can never pass silently.
// The aggregate is masked before it is persisted on the run.
func resolveConsumerImpact(ctx context.Context, opts consumerTriggerEvaluation) store.ArchitectureConsumers {
	if !consumerPolicyConfigured(opts.Policy) {
		return store.ArchitectureConsumers{Status: store.ConsumersNotConfigured}
	}
	// The trigger calls this while holding the pull request lock, so the whole
	// lookup has the same hard window as the evaluator itself.
	ctx, cancel := context.WithTimeout(ctx, consumerTotalTimeMax)
	defer cancel()
	reader := newGitHubContractReader(opts.Apps, opts.InstallationID, opts.Repository)
	differ, ok := opts.Comparator.(changedFilesResolver)
	if !ok {
		// Without a diff every contract is unknown. Do not let the evaluator see
		// an empty file list, which would mark configured consumers out of scope.
		configured, sources, problems := consumerConfig(ctx, opts.MergeBase, opts.Policy, reader.Read)
		return unavailableConsumerAggregate(configured, sources, problems, opts.ProducerPrivate,
			"el proceso de control no pudo determinar el diff de consumidores")
	}
	comparison, err := differ.ComparisonFiles(ctx, opts.InstallationID, opts.Repository, opts.Base, opts.HeadSHA)
	if err != nil {
		configured, sources, problems := consumerConfig(ctx, opts.MergeBase, opts.Policy, reader.Read)
		return unavailableConsumerAggregate(configured, sources, problems, opts.ProducerPrivate,
			"el proceso de control no pudo determinar el diff de consumidores")
	}
	if comparison.Truncated {
		configured, sources, problems := consumerConfig(ctx, comparison.MergeBase, opts.Policy, reader.Read)
		return unavailableConsumerAggregate(configured, sources, problems, opts.ProducerPrivate,
			"el diff es demasiado grande para comprobar consumidores con certeza")
	}
	baseRef := comparison.MergeBase
	if baseRef == "" {
		baseRef = opts.MergeBase
	}
	configured, sources, problems := consumerConfig(ctx, baseRef, opts.Policy, reader.Read)
	if len(configured) == 0 {
		if len(problems) == 0 {
			return store.ArchitectureConsumers{Status: store.ConsumersNotConfigured, Sources: sources}
		}
		return unavailableConsumerAggregate(configured, sources, problems, opts.ProducerPrivate, "")
	}
	files := make([]changedFile, 0, len(comparison.Files))
	for _, file := range comparison.Files {
		files = append(files, changedFile{Path: file.Filename, OldPath: file.PreviousFilename, Kind: changedModified})
	}
	return store.MaskRedactedConsumerImpact(evaluateConsumers(ctx, consumerEvaluation{
		BaseRef:         baseRef,
		HeadRef:         opts.HeadSHA,
		Files:           files,
		Policy:          opts.Policy,
		ProducerPrivate: opts.ProducerPrivate,
		Remaining:       consumerTotalTimeMax,
		Resolver:        newGitHubConsumerResolver(opts.Apps, opts.InstallationID),
		ContractDigest:  reader.Digest,
		Configured:      configured,
		Sources:         sources,
		ConfigErrors:    problems,
		configReady:     true,
	}))
}

// consumerPolicyConfigured is the cheap gate that keeps runs without
// consumers from paying for a Compare call.
func consumerPolicyConfigured(policy store.QAPolicy) bool {
	if strings.TrimSpace(policy.ConsumerBaselinePath) != "" {
		return true
	}
	refs, err := store.ParseConsumerAllowList(policy.ConsumerAllowList)
	return err != nil || len(refs) > 0
}

// unavailableConsumerAggregate records every configured consumer as unknown.
// It is the safe result whenever the evidence that would prove a contract
// unchanged or compatible is missing.
func unavailableConsumerAggregate(configured []configuredConsumer, sources, problems []string,
	producerPrivate bool, reason string) store.ArchitectureConsumers {
	result := store.ArchitectureConsumers{
		Configured: len(configured), Sources: sources, Errors: append([]string(nil), problems...),
	}
	if reason != "" {
		result.Errors = append(result.Errors, reason)
	}
	if len(configured) == 0 {
		result.Status = store.ConsumersUnknown
		return result
	}
	for _, entry := range configured {
		consumer := newArchitectureConsumer(entry, producerPrivate)
		consumer.Status = store.ConsumerStatusUnknown
		consumer.Impact = store.ConsumerImpactUnknown
		consumer.Reason = store.ConsumerReasonUnavailable
		if entry.Ref.Essential {
			result.Essential++
			result.RequiresHuman = true
		}
		result.Consumers = append(result.Consumers, consumer)
	}
	result.Status = consumersAggregateStatus(result)
	return store.MaskRedactedConsumerImpact(result)
}

// unavailableConsumerImpact is the worker-side fallback for a run without a
// control-process snapshot (old runs or a failed enqueue step). It reads the
// local baseline and declares every configured consumer unavailable.
func unavailableConsumerImpact(ctx context.Context, dir, baseRef string, policy store.QAPolicy,
	producerPrivate bool) store.ArchitectureConsumers {
	readFile := func(ctx context.Context, ref, path string, maxBytes int64) ([]byte, error) {
		return gitFileBytes(ctx, dir, ref, path, maxBytes)
	}
	configured, sources, problems := consumerConfig(ctx, baseRef, policy, readFile)
	if len(configured) == 0 && len(problems) == 0 {
		return store.ArchitectureConsumers{Status: store.ConsumersNotConfigured}
	}
	return unavailableConsumerAggregate(configured, sources, problems, producerPrivate,
		"el proceso de control no dejó evidencia de consumidores para esta revisión")
}

// githubContractReader reads the producer's contract through the App so the
// control process can hash it at the merge base and at the head.
type githubContractReader struct {
	apps           appgithub.AppProvider
	installationID int64
	repository     string

	mu     sync.Mutex
	client *gh.Client
}

func newGitHubContractReader(apps appgithub.AppProvider, installationID int64, repository string) *githubContractReader {
	return &githubContractReader{apps: apps, installationID: installationID, repository: repository}
}

func (r *githubContractReader) file(ctx context.Context, ref, path string, maxBytes int64) (*gh.RepositoryContent, error) {
	owner, name, ok := splitRepository(r.repository)
	if !ok {
		return nil, errConsumerUnavailable
	}
	if maxBytes <= 0 {
		maxBytes = store.DefaultConsumerMaxBytes
	}
	r.mu.Lock()
	client := r.client
	r.mu.Unlock()
	if client == nil {
		if r.apps == nil || r.installationID <= 0 {
			return nil, errConsumerUnauthorized
		}
		app, err := r.apps.App(ctx)
		if err != nil {
			return nil, errConsumerUnavailable
		}
		client, err = app.InstallationClient(r.installationID)
		if err != nil {
			return nil, errConsumerUnavailable
		}
		r.mu.Lock()
		r.client = client
		r.mu.Unlock()
	}
	file, _, resp, err := client.Repositories.GetContents(ctx, owner, name, path,
		&gh.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		return nil, consumerLookupError(resp, err)
	}
	if file == nil {
		return nil, errConsumerUnavailable
	}
	if file.GetSize() > int(maxBytes) {
		return nil, errConsumerTooLarge
	}
	return file, nil
}

func (r *githubContractReader) Digest(ctx context.Context, ref, path string, maxBytes int64) (string, error) {
	file, err := r.file(ctx, ref, path, maxBytes)
	if err != nil {
		return "", err
	}
	content, err := consumerFileContent(file)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:]), nil
}

func (r *githubContractReader) Read(ctx context.Context, ref, path string, maxBytes int64) ([]byte, error) {
	file, err := r.file(ctx, ref, path, maxBytes)
	if err != nil {
		return nil, err
	}
	content, err := consumerFileContent(file)
	if err != nil {
		return nil, err
	}
	return []byte(content), nil
}

func splitRepository(fullName string) (string, string, bool) {
	parts := strings.Split(fullName, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

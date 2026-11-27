package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	criterionSourceIssue  = "issue"
	criterionSourcePRBody = "pr_body"

	criterionEvaluatorOCR           = "ocr"
	criterionEvaluatorDeterministic = "deterministic"
	criterionEvaluatorUnavailable   = "none"

	// maxCriterionEvidence bounds the citations one verdict may carry. A verdict
	// with more ranges than the reader can use is noise, not more proof.
	maxCriterionEvidence = 8
	// maxCriterionDigestBytes bounds the file prefix hashed to detect whether a
	// cited evidence file still describes the current checkout. A prefix is
	// enough to invalidate the common push; the range check below catches a
	// shift past the prefix.
	maxCriterionDigestBytes = 1 << 20
	// maxCriteriaEvidencePaths bounds the files the OCR evaluator scans.
	maxCriteriaEvidencePaths = 40
	// criteriaEvaluationTimeout bounds the focused criteria pass without
	// consuming the run's remaining budget pointlessly.
	criteriaEvaluationTimeout = 90 * time.Second
)

// CriterionSource identifies where a requirement was quoted from and at which
// version of that source, so a changed issue can be told apart from the same
// requirement seen again.
type CriterionSource struct {
	Kind    string `json:"kind"`
	Repo    string `json:"repo,omitempty"`
	Number  int    `json:"number,omitempty"`
	URL     string `json:"url,omitempty"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// Requirement is one applicable criterion of the linked issue or of the PR
// description. Fingerprint covers its text alone; Version covers the source it
// came from. Contradicts names the other requirement that cannot be true at the
// same time.
type Requirement struct {
	ID           string          `json:"id"`
	Text         string          `json:"text"`
	Fingerprint  string          `json:"fingerprint"`
	Source       CriterionSource `json:"source"`
	Link         string          `json:"link,omitempty"`
	Inaccessible bool            `json:"inaccessible,omitempty"`
	Uncertainty  string          `json:"uncertainty,omitempty"`
	Contradicts  []string        `json:"contradicts,omitempty"`
}

// RequirementSet is the criteria extracted from one PR description and its
// linked issues plus the reasons the extraction itself is uncertain.
type RequirementSet struct {
	Requirements  []Requirement `json:"requirements,omitempty"`
	Uncertainties []string      `json:"uncertainties,omitempty"`
}

// RequirementAssessment is one evaluator answer before it is bound to the
// store. Evidence is validated against the checkout before it is persisted.
type RequirementAssessment struct {
	CriterionID string                    `json:"criterion_id"`
	Status      string                    `json:"status"`
	Evidence    []store.CriterionEvidence `json:"evidence,omitempty"`
	Observed    string                    `json:"observed"`
	Required    string                    `json:"required"`
	Reason      string                    `json:"reason,omitempty"`
	Uncertainty string                    `json:"uncertainty,omitempty"`
}

// CriterionEvaluationInput is what one focused evaluation receives. It is
// deliberately data-only: the evaluator reads files from WorkDir and never
// mutates review state.
type CriterionEvaluationInput struct {
	PullRequest  store.PullRequest
	HeadSHA      string
	BaseSHA      string
	WorkDir      string
	Requirements []Requirement
	Paths        []string
	External     bool
	MaxTokens    int
	TokenBudget  int64
	Timeout      time.Duration
}

// CriterionEvaluation is the evaluator's answer plus the spend it caused.
type CriterionEvaluation struct {
	Assessments map[string]RequirementAssessment
	Usage       tokenUsage
	UsageKnown  bool
	SessionID   string
	Model       string
}

// CriterionEvaluator answers whether each requirement is met at the head. The
// native implementation runs a focused OCR scan; tests inject a fake.
type CriterionEvaluator interface {
	Evaluate(ctx context.Context, input CriterionEvaluationInput) (CriterionEvaluation, error)
}

// ExtractRequirements turns the linked context of a run into stable criteria.
// Checkboxes under an acceptance-criteria heading are the source; a linked
// issue without them contributes its description as an explicit uncertainty
// instead of silently contributing nothing. An inaccessible reference becomes
// a criterion that can only be "no verificable".
func ExtractRequirements(pr store.PullRequest, repoFullName string, linked linkedContext) RequirementSet {
	var set RequirementSet
	seen := make(map[string]bool)
	add := func(req Requirement) {
		key := normalizeCriterionText(req.Text)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		set.Requirements = append(set.Requirements, req)
	}

	for _, issue := range linked.Issues {
		repo := issue.Repo
		if repo == "" {
			repo = repoFullName
		}
		source := CriterionSource{
			Kind: criterionSourceIssue,
			Repo: repo,
			// The label stays meaningful for a cross-repository reference that
			// was never read.
			Number: issue.Number,
			Title:  issue.Title,
		}
		if issue.Err != "" {
			source.Version = "inaccesible"
			req := Requirement{
				ID:           criterionID("issue:"+source.Repo, "inaccesible:"+fmt.Sprint(issue.Number)),
				Text:         fmt.Sprintf("Referencia inaccesible: %s#%d", boundedText(source.Repo, backgroundRefBytes), issue.Number),
				Fingerprint:  criterionFingerprint("inaccesible:" + fmt.Sprint(issue.Number)),
				Source:       source,
				Link:         issueLink(source.Repo, issue.Number),
				Inaccessible: true,
				Uncertainty:  "fuente inaccesible: " + issue.Err,
			}
			add(req)
			set.Uncertainties = append(set.Uncertainties, req.Text+": "+req.Uncertainty)
			continue
		}
		source.Version = criterionVersion(issue.Title, issue.Body)
		criteria, truncated := extractAcceptanceCriteria(issue.Body, maxCriteriaItems, maxCriteriaBytes)
		if truncated {
			set.Uncertainties = append(set.Uncertainties,
				fmt.Sprintf("criterios de %s#%d recortados", boundedText(repo, backgroundRefBytes), issue.Number))
		}
		if len(criteria) == 0 {
			set.Uncertainties = append(set.Uncertainties,
				fmt.Sprintf("%s#%d no declara criterios de aceptación", boundedText(repo, backgroundRefBytes), issue.Number))
			continue
		}
		for _, text := range criteria {
			add(Requirement{
				ID:          criterionID("issue:"+source.Repo, text),
				Text:        text,
				Fingerprint: criterionFingerprint(text),
				Source:      source,
				Link:        issueLink(source.Repo, issue.Number),
			})
		}
	}

	bodySource := CriterionSource{
		Kind:    criterionSourcePRBody,
		Repo:    repoFullName,
		Number:  pr.Number,
		URL:     pr.HTMLURL,
		Title:   pr.Title,
		Version: criterionVersion(pr.Title, pr.Body),
	}
	bodyCriteria, truncated := extractAcceptanceCriteria(pr.Body, maxCriteriaItems, maxCriteriaBytes)
	if truncated {
		set.Uncertainties = append(set.Uncertainties, "criterios de la descripción recortados")
	}
	for _, text := range bodyCriteria {
		add(Requirement{
			ID:          criterionID("pr:"+repoFullName, text),
			Text:        text,
			Fingerprint: criterionFingerprint(text),
			Source:      bodySource,
			Link:        pr.HTMLURL,
		})
	}

	set.Requirements = detectCriterionContradictions(set.Requirements)
	return set
}

func issueLink(repo string, number int) string {
	if repo == "" || number <= 0 {
		return ""
	}
	return "https://github.com/" + repo + "/issues/" + fmt.Sprint(number)
}

// criterionID identifies a criterion inside one source. The same text in
// another source is another criterion, because its quote and version differ.
func criterionID(sourceKey, text string) string {
	sum := sha256.Sum256([]byte(sourceKey + "\n" + normalizeCriterionText(text)))
	return hex.EncodeToString(sum[:8])
}

// criterionFingerprint identifies the text alone, independent of its source.
func criterionFingerprint(text string) string {
	sum := sha256.Sum256([]byte(normalizeCriterionText(text)))
	return hex.EncodeToString(sum[:8])
}

// criterionVersion fingerprints the source at extraction time. A push to the
// issue or an edit of the PR description yields a different version.
func criterionVersion(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// normalizeCriterionText makes two spellings of the same requirement collide
// and different requirements not. Punctuation and case do not change a
// criterion; wording does.
func normalizeCriterionText(text string) string {
	replacer := strings.NewReplacer(
		".", " ", ",", " ", ";", " ", ":", " ", "!", " ", "?", " ",
		"(", " ", ")", " ", "[", " ", "]", " ", "\"", " ", "'", " ",
		"`", " ", "*", " ", "_", " ",
	)
	normalized := strings.ToLower(replacer.Replace(text))
	return strings.Join(strings.Fields(normalized), " ")
}

var criterionNegationPrefixes = []string{
	"no se deben ", "no se debe ", "no deben ", "no debe ",
	"no se pueden ", "no se puede ", "no pueden ", "no puede ",
	"no ", "nunca ", "jamas ", "sin ",
	"must not ", "should not ", "do not ", "never ", "without ",
}

var criterionModalPrefixes = []string{
	"deben ", "debe ", "must ", "should ", "shall ", "has to ", "have to ",
}

// stripCriterionNegation reduces a criterion to its proposition and whether it
// was stated negatively. Removing the modal as well lets "debe reintentar" and
// "no debe reintentar" collide, which is how a contradiction is detected.
func stripCriterionNegation(normalized string) (string, bool) {
	negated := false
	for _, prefix := range criterionNegationPrefixes {
		if strings.HasPrefix(normalized, prefix) {
			normalized = strings.TrimSpace(strings.TrimPrefix(normalized, prefix))
			negated = true
			break
		}
	}
	for _, prefix := range criterionModalPrefixes {
		if strings.HasPrefix(normalized, prefix) {
			normalized = strings.TrimSpace(strings.TrimPrefix(normalized, prefix))
			break
		}
	}
	return normalized, negated
}

// detectCriterionContradictions flags pairs whose only difference is negation.
// Two criteria the change cannot satisfy at once are shown as uncertain instead
// of letting the evaluator pick one and hide the conflict.
func detectCriterionContradictions(requirements []Requirement) []Requirement {
	for i := range requirements {
		if requirements[i].Inaccessible {
			continue
		}
		left, leftNegated := stripCriterionNegation(normalizeCriterionText(requirements[i].Text))
		if left == "" {
			continue
		}
		for j := i + 1; j < len(requirements); j++ {
			if requirements[j].Inaccessible {
				continue
			}
			right, rightNegated := stripCriterionNegation(normalizeCriterionText(requirements[j].Text))
			if right == "" || left != right || leftNegated == rightNegated {
				continue
			}
			a, b := requirements[i].ID, requirements[j].ID
			requirements[i].Contradicts = appendUnique(requirements[i].Contradicts, b)
			requirements[j].Contradicts = appendUnique(requirements[j].Contradicts, a)
			requirements[i].Uncertainty = contradictionText(requirements[i].Contradicts)
			requirements[j].Uncertainty = contradictionText(requirements[j].Contradicts)
		}
	}
	return requirements
}

func contradictionText(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return "requisitos contradictorios: " + strings.Join(ids, ", ")
}

// requirementEvaluationPlan splits a criterion set into the ones that can keep
// their validated answer, the ones a deterministic rule settles, and the ones
// that need the evaluator at this head.
type requirementEvaluationPlan struct {
	Evaluate    []Requirement
	Carried     map[string]store.CriterionAssessment
	Precomputed map[string]store.CriterionAssessment
}

// planRequirementEvaluation reuses an answer only when the criterion text, the
// source version and the cited evidence still describe this checkout. A push
// that touches an evidence file, or a test in a package the push changed,
// forces a fresh answer for that criterion (#232).
func planRequirementEvaluation(requirements []Requirement, previous []store.CriterionAssessment,
	headSHA, workDir string, changedPaths []string) requirementEvaluationPlan {
	plan := requirementEvaluationPlan{
		Carried:     make(map[string]store.CriterionAssessment),
		Precomputed: make(map[string]store.CriterionAssessment),
	}
	previousByID := make(map[string]store.CriterionAssessment, len(previous))
	for _, assessment := range previous {
		previousByID[assessment.CriterionID] = assessment
	}
	now := time.Now().UTC()
	for _, req := range requirements {
		if req.Inaccessible || req.Uncertainty != "" {
			plan.Precomputed[req.ID] = deterministicCriterionAssessment(req, now)
			continue
		}
		if prev, ok := previousByID[req.ID]; ok && criterionCarryable(req, prev, headSHA, workDir, changedPaths) {
			plan.Carried[req.ID] = prev
			continue
		}
		plan.Evaluate = append(plan.Evaluate, req)
	}
	return plan
}

func deterministicCriterionAssessment(req Requirement, now time.Time) store.CriterionAssessment {
	uncertainty := req.Uncertainty
	if uncertainty == "" && req.Inaccessible {
		uncertainty = "fuente inaccesible"
	}
	return store.CriterionAssessment{
		CriterionID: req.ID,
		Fingerprint: req.Fingerprint,
		Status:      store.CriterionUnverifiable,
		Evidence:    store.EncodeCriterionEvidence(nil),
		Observed:    "No se pudo leer el requisito en su fuente.",
		Required:    req.Text,
		Reason:      uncertainty,
		Uncertainty: uncertainty,
		Evaluator:   criterionEvaluatorDeterministic,
		EvaluatedAt: now,
	}
}

// criterionCarryable answers whether the previous verdict still applies at the
// current head. Evidence digests are the primary check; the changed-path check
// catches a source change that makes a cited test stale even when the test file
// itself did not move.
func criterionCarryable(req Requirement, prev store.CriterionAssessment, headSHA, workDir string, changedPaths []string) bool {
	if prev.Fingerprint != req.Fingerprint || prev.SourceVersion != req.Source.Version {
		return false
	}
	if !store.ValidCriterionStatus(prev.Status) {
		return false
	}
	evidence := store.DecodeCriterionEvidence(prev.Evidence)
	if len(evidence) == 0 {
		// A conclusion never stands without a citation. "No verificable" and "no
		// aplicable" are conclusions about the source, not about the code, so they
		// may be carried.
		return prev.Status == store.CriterionUnverifiable || prev.Status == store.CriterionNotApplicable
	}
	headMoved := !strings.EqualFold(prev.HeadSHA, headSHA)
	for _, item := range evidence {
		if item.Path == "" || item.Digest == "" || item.Digest != criterionFileDigest(workDir, item.Path) {
			return false
		}
		if headMoved {
			for _, path := range changedPaths {
				if sameCriterionPath(path, item.Path) {
					return false
				}
				if item.Test && sameCriterionDirectory(path, item.Path) {
					return false
				}
			}
		}
	}
	return true
}

func sameCriterionPath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

func sameCriterionDirectory(a, b string) bool {
	return filepath.Clean(filepath.Dir(a)) == filepath.Clean(filepath.Dir(b))
}

// criterionFileDigest hashes the readable content of a path inside the
// checkout. A missing or unreadable file yields "", which invalidates any
// evidence that cited it.
func criterionFileDigest(workDir, path string) string {
	digest, _, _ := criterionFileState(workDir, path)
	return digest
}

// criterionFileState returns the digest and the line count of a cited file.
// lines is -1 when the file is larger than the hashed prefix, so an unknown
// line count never rejects evidence the evaluator could read.
func criterionFileState(workDir, path string) (digest string, lines int, ok bool) {
	content, truncated, err := readRelatedFile(workDir, path, maxCriterionDigestBytes)
	if err != nil {
		return "", 0, false
	}
	sum := sha256.Sum256([]byte(content))
	lines = strings.Count(content, "\n") + 1
	if truncated {
		lines = -1
	}
	return hex.EncodeToString(sum[:8]), lines, true
}

// BindCriterionEvidence drops citations the checkout cannot confirm at this
// head and fingerprints the ones it keeps, so the next plan can detect a moved
// file without trusting the evaluator.
func BindCriterionEvidence(workDir string, evidence []store.CriterionEvidence) []store.CriterionEvidence {
	bound := make([]store.CriterionEvidence, 0, len(evidence))
	for _, item := range evidence {
		item.Path = strings.TrimSpace(item.Path)
		if item.Path == "" {
			continue
		}
		digest, lines, ok := criterionFileState(workDir, item.Path)
		if !ok || digest == "" {
			continue
		}
		if item.StartLine < 0 {
			item.StartLine = 0
		}
		if item.EndLine < item.StartLine {
			item.EndLine = item.StartLine
		}
		if lines >= 0 && item.StartLine > lines {
			// The citation points past the current file: it cannot support the
			// verdict at this head.
			continue
		}
		if lines >= 0 && item.EndLine > lines {
			item.EndLine = lines
		}
		item.Digest = digest
		bound = append(bound, item)
		if len(bound) >= maxCriterionEvidence {
			break
		}
	}
	return bound
}

// BuildCriterionAssessments merges the carried, deterministic and evaluated
// answers into one complete set, in the source order. Every requirement gets an
// answer: an evaluator that stayed silent makes the criterion "no verificable",
// never a silent success. A model answer of "cumplido" or "incumplido" without
// evidence that exists at the head is downgraded for the same reason.
func BuildCriterionAssessments(requirements []Requirement, plan requirementEvaluationPlan,
	answers map[string]RequirementAssessment, workDir, headSHA, baseRef string,
	prID, runID uint, evaluator string, now time.Time) []store.CriterionAssessment {
	assessments := make([]store.CriterionAssessment, 0, len(requirements))
	for _, req := range requirements {
		var assessment store.CriterionAssessment
		switch {
		case plan.Carried[req.ID].CriterionID != "":
			assessment = plan.Carried[req.ID]
		case plan.Precomputed[req.ID].CriterionID != "":
			assessment = plan.Precomputed[req.ID]
		default:
			assessment = assessmentFromAnswer(req, answers[req.ID], workDir, evaluator, now)
		}
		if req.Uncertainty != "" {
			assessment.Status = store.CriterionUnverifiable
			assessment.Uncertainty = req.Uncertainty
			if assessment.Reason == "" {
				assessment.Reason = req.Uncertainty
			}
		}
		assessment.PullRequestID = prID
		assessment.ReviewRunID = runID
		assessment.HeadSHA = headSHA
		assessment.BaseRef = baseRef
		// A carried row is a copy of the previous current row. It is inserted as
		// a new row while the old one is invalidated in the same transaction, so
		// the database identity and the timestamps must not travel with it.
		assessment.ID = 0
		assessment.CreatedAt = time.Time{}
		assessment.UpdatedAt = time.Time{}
		assessment.InvalidatedAt = nil
		assessment.InvalidatedReason = ""
		assessment.CriterionID = req.ID
		assessment.Fingerprint = req.Fingerprint
		assessment.Text = req.Text
		assessment.SourceKind = req.Source.Kind
		assessment.SourceRepo = req.Source.Repo
		assessment.SourceNumber = req.Source.Number
		assessment.SourceURL = req.Link
		assessment.SourceTitle = req.Source.Title
		assessment.SourceVersion = req.Source.Version
		if assessment.Required == "" {
			assessment.Required = req.Text
		}
		if assessment.EvaluatedAt.IsZero() {
			assessment.EvaluatedAt = now
		}
		assessments = append(assessments, assessment)
	}
	return assessments
}

func assessmentFromAnswer(req Requirement, answer RequirementAssessment, workDir, evaluator string, now time.Time) store.CriterionAssessment {
	status := answer.Status
	if !store.ValidCriterionStatus(status) {
		status = store.CriterionUnverifiable
		if answer.Reason == "" {
			answer.Reason = "estado del evaluador desconocido"
		}
	}
	evidence := BindCriterionEvidence(workDir, answer.Evidence)
	if (status == store.CriterionSatisfied || status == store.CriterionUnsatisfied) && len(evidence) == 0 {
		status = store.CriterionUnverifiable
		if answer.Reason == "" {
			answer.Reason = "el evaluador no citó evidencia válida en el head"
		}
	}
	observed := strings.TrimSpace(answer.Observed)
	uncertainty := answer.Uncertainty
	if observed == "" && (status == store.CriterionSatisfied || status == store.CriterionUnsatisfied) {
		uncertainty = "el evaluador no describió el comportamiento observado"
	}
	model := evaluator
	if model == "" {
		model = criterionEvaluatorOCR
	}
	return store.CriterionAssessment{
		Status:      status,
		Evidence:    store.EncodeCriterionEvidence(evidence),
		Observed:    observed,
		Required:    strings.TrimSpace(answer.Required),
		Reason:      strings.TrimSpace(answer.Reason),
		Uncertainty: uncertainty,
		Evaluator:   model,
		EvaluatedAt: now,
	}
}

// RequirementMetrics is the observation counter #232 asks for: how many valid
// non-compliances were surfaced and how many criteria were escalated without a
// reason to. It feeds the run log; it never changes the check.
type RequirementMetrics struct {
	Satisfied             int
	Unsatisfied           int
	Unverifiable          int
	NotApplicable         int
	ReEvaluated           int
	Carried               int
	ValidNonCompliance    int
	UnnecessaryEscalation int
}

// MeasureRequirements computes the counters of one evaluation.
func MeasureRequirements(assessments []store.CriterionAssessment, carried, evaluated int) RequirementMetrics {
	metrics := RequirementMetrics{ReEvaluated: evaluated, Carried: carried}
	for _, assessment := range assessments {
		switch assessment.Status {
		case store.CriterionSatisfied:
			metrics.Satisfied++
		case store.CriterionUnsatisfied:
			metrics.Unsatisfied++
			if len(store.DecodeCriterionEvidence(assessment.Evidence)) > 0 {
				metrics.ValidNonCompliance++
			}
		case store.CriterionUnverifiable:
			metrics.Unverifiable++
			if len(store.DecodeCriterionEvidence(assessment.Evidence)) > 0 {
				// The evaluator found evidence but still refused to decide. In
				// observation this is the signal of an unnecessary escalation.
				metrics.UnnecessaryEscalation++
			}
		case store.CriterionNotApplicable:
			metrics.NotApplicable++
		}
	}
	return metrics
}

// criteriaEvidencePaths lists the changed files the criteria pass may cite, plus
// the sibling test file that already exists at the head. A path OCR cannot take
// (a comma is a separator in its flags) is skipped instead of being sent to the
// wrong file.
func criteriaEvidencePaths(workDir string, changedPaths []string) []string {
	paths := make([]string, 0, maxCriteriaEvidencePaths)
	seen := make(map[string]bool, maxCriteriaEvidencePaths)
	add := func(path string) {
		if path == "" || len(paths) >= maxCriteriaEvidencePaths {
			return
		}
		if strings.Contains(path, ",") || seen[path] {
			return
		}
		if !regularCheckoutFile(workDir, path) {
			return
		}
		seen[path] = true
		paths = append(paths, path)
	}
	for _, path := range changedPaths {
		add(path)
		if test := relatedTestPath(path); test != "" {
			add(test)
		}
	}
	return paths
}

func regularCheckoutFile(workDir, path string) bool {
	_, _, err := readRelatedFile(workDir, path, 1)
	return err == nil
}

// evaluateRequirements is the run integration point. It is deliberately
// best-effort: a failed or unavailable evaluator records explicit "no
// verificable" verdicts and never fails the review. Publishing reads only rows
// stored for the current head, so an evaluation that never completed is not
// presented as a confirmed result (#227).
func (r *Runner) evaluateRequirements(ctx context.Context, run store.ReviewRun, cfg store.ReviewConfig,
	profile store.LLMProfile, apiKey []byte, workDir, headSHA, baseSHA string,
	changedPaths []string, out *runOutcome, log *runLogSink) {
	pr := cfg.PR
	linked := linkedContext{}
	if len(run.LinkedIssues) > 0 {
		if err := json.Unmarshal(run.LinkedIssues, &linked); err != nil {
			log.Log(ctx, "worker", "", "warn", logPhasePrepare,
				fmt.Sprintf("criterios: no se pudo leer el contexto de issues enlazados: %v", err))
		}
	}
	set := ExtractRequirements(pr, cfg.Repo.FullName, linked)
	if len(set.Requirements) == 0 {
		return
	}
	for _, note := range set.Uncertainties {
		log.Log(ctx, "worker", "criteria", "warn", logPhaseOCR, "criterios: "+note)
	}

	previous, err := r.Store.CurrentCriterionAssessments(ctx, pr.ID, "")
	if err != nil {
		log.Log(ctx, "worker", "criteria", "warn", logPhaseOCR,
			fmt.Sprintf("criterios: no se pudo leer la evaluación previa: %v", err))
	}
	plan := planRequirementEvaluation(set.Requirements, previous, headSHA, workDir, changedPaths)

	answers := make(map[string]RequirementAssessment)
	evaluatorName := criterionEvaluatorUnavailable
	if len(plan.Evaluate) > 0 {
		paths := criteriaEvidencePaths(workDir, changedPaths)
		if len(paths) == 0 {
			log.Log(ctx, "worker", "criteria", "warn", logPhaseOCR,
				"criterios: el diff no expone archivos donde citar evidencia")
		} else {
			timeout := criteriaEvaluationTimeout
			if deadline, ok := ctx.Deadline(); ok {
				if left := time.Until(deadline); left < timeout {
					timeout = left
				}
			}
			budget := int64(0)
			skipReason := ""
			if timeout <= 0 {
				skipReason = "no queda tiempo del run"
			}
			if skipReason == "" && cfg.Policy.ReviewTokenBudget > 0 {
				remaining := cfg.Policy.ReviewTokenBudget - int64(out.tokens.Total)
				if remaining <= 0 {
					skipReason = "presupuesto de tokens agotado"
				} else {
					budget = remaining
				}
			}
			if skipReason != "" {
				log.Log(ctx, "worker", "criteria", "warn", logPhaseOCR, fmt.Sprintf(
					"criterios: evaluación omitida (%s); %d criterio(s) quedan como no verificables",
					skipReason, len(plan.Evaluate)))
			} else {
				evaluator := r.CriteriaEvaluator
				if evaluator == nil {
					evaluator = &NativeCriteriaEvaluator{
						Bin:     r.Cfg.OCRBin,
						Profile: profile,
						APIKey:  string(apiKey),
						OnLog:   log.OCRLogger(ctx, "criteria"),
					}
				}
				evaluation, evalErr := evaluator.Evaluate(ctx, CriterionEvaluationInput{
					PullRequest:  pr,
					HeadSHA:      headSHA,
					BaseSHA:      baseSHA,
					WorkDir:      workDir,
					Requirements: plan.Evaluate,
					Paths:        paths,
					External:     pr.External(),
					MaxTokens:    r.Cfg.ContextCeiling,
					TokenBudget:  budget,
					Timeout:      timeout,
				})
				if evalErr != nil {
					log.Log(ctx, "worker", "criteria", "warn", logPhaseOCR,
						fmt.Sprintf("criterios: la evaluación falló; quedan como no verificables: %v", evalErr))
				} else {
					answers = evaluation.Assessments
					evaluatorName = criterionEvaluatorOCR
					if evaluation.Model != "" {
						evaluatorName = criterionEvaluatorOCR + ":" + evaluation.Model
					}
					if evaluation.UsageKnown {
						out.tokens.add(evaluation.Usage)
						out.tokensKnown = true
					}
				}
			}
		}
	}

	now := time.Now().UTC()
	assessments := BuildCriterionAssessments(set.Requirements, plan, answers, workDir,
		headSHA, pr.BaseRef, pr.ID, run.ID, evaluatorName, now)
	if err := r.Store.SaveCriterionAssessments(ctx, assessments); err != nil {
		log.Log(ctx, "worker", "criteria", "warn", logPhaseOCR,
			fmt.Sprintf("criterios: no se pudo guardar la evaluación: %v", err))
		return
	}
	metrics := MeasureRequirements(assessments, len(plan.Carried), len(plan.Evaluate))
	log.Log(ctx, "worker", "criteria", "info", logPhaseOCR, fmt.Sprintf(
		"criterios: %d cumplidos, %d incumplidos, %d no verificables, %d no aplicables; "+
			"%d re-evaluados, %d reutilizados, %d incumplimientos válidos, %d escalaciones innecesarias",
		metrics.Satisfied, metrics.Unsatisfied, metrics.Unverifiable, metrics.NotApplicable,
		metrics.ReEvaluated, metrics.Carried, metrics.ValidNonCompliance, metrics.UnnecessaryEscalation))
}

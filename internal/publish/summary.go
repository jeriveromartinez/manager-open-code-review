package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const summaryMarker = "<!-- qa-control-center:summary -->"

type reviewCoverage struct {
	ReviewedFiles    int            `json:"reviewed_files"`
	SkippedFiles     int            `json:"skipped_files"`
	NotRequiredFiles int            `json:"not_required_files"`
	Files            []coverageFile `json:"files"`
}

// coverageFile mirrors the per-file coverage the review worker stores.
type coverageFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Method string `json:"method"`
	Reason string `json:"reason"`
	Human  bool   `json:"human"`
}

// pendingFiles returns the changed files the review still owes. They are not
// policy exclusions and were not covered by any pass.
func (c reviewCoverage) pendingFiles() []coverageFile {
	var pending []coverageFile
	for _, file := range c.Files {
		if file.Status == "pending" {
			pending = append(pending, file)
		}
	}
	return pending
}

// humanFiles returns the changed files no automated pass can analyse, so the
// summary names them instead of letting them pass silently.
func (c reviewCoverage) humanFiles() []coverageFile {
	var human []coverageFile
	for _, file := range c.Files {
		if file.Human {
			human = append(human, file)
		}
	}
	return human
}

func decodeReviewCoverage(raw []byte) reviewCoverage {
	var coverage reviewCoverage
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &coverage)
	}
	return coverage
}

// pendingCoverageLine renders the pending files as one line for the check run
// output. Empty when every required file was covered.
func (p *publishContext) pendingCoverageLine() string {
	pending := decodeReviewCoverage(p.run.Coverage).pendingFiles()
	if len(pending) == 0 {
		return ""
	}
	parts := make([]string, 0, len(pending))
	for _, file := range pending {
		if file.Reason != "" {
			parts = append(parts, fmt.Sprintf("%s (%s)", p.sanitize(file.Path), p.sanitize(file.Reason)))
			continue
		}
		parts = append(parts, p.sanitize(file.Path))
	}
	return "Archivos pendientes: " + strings.Join(parts, ", ")
}

// publishSummary creates the single summary comment of the pull request or
// edits the one carrying the marker. It never posts a second summary.
func (p *publishContext) publishSummary(ctx context.Context, findings []store.Finding, resolvedSincePrevious int) error {
	body := p.buildSummary(ctx, findings, resolvedSincePrevious)

	var existingID int64
	commentsPath := p.issueCommentsPath()
	for page := 1; existingID == 0; page++ {
		var comments []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
		}
		path := fmt.Sprintf("%s?per_page=100&page=%d", commentsPath, page)
		if err := p.request(ctx, "GET", path, nil, &comments); err != nil {
			return fmt.Errorf("list issue comments: %w", err)
		}
		for _, c := range comments {
			if strings.Contains(c.Body, summaryMarker) {
				existingID = c.ID
				break
			}
		}
		if len(comments) < 100 {
			break
		}
	}

	var commentID int64
	if existingID != 0 {
		commentID = existingID
		if err := p.request(ctx, "PATCH", p.issueCommentPath(commentID),
			map[string]string{"body": body}, nil); err != nil {
			return fmt.Errorf("edit summary comment %d: %w", commentID, err)
		}
	} else {
		var created struct {
			ID int64 `json:"id"`
		}
		if err := p.request(ctx, "POST", p.issueCommentsPath(),
			map[string]string{"body": body}, &created); err != nil {
			return fmt.Errorf("create summary comment: %w", err)
		}
		if created.ID == 0 {
			return fmt.Errorf("create summary comment: GitHub returned no comment id")
		}
		commentID = created.ID
	}

	url := p.w.PublicURL
	if url != "" {
		url = strings.TrimRight(url, "/") + "/prs/" + strconv.FormatUint(uint64(p.pr.ID), 10)
	}
	_, err := p.w.Store.UpsertArtifact(ctx, store.GitHubArtifact{
		PullRequestID: p.pr.ID,
		Kind:          store.ArtifactSummary,
		HeadSHA:       p.run.HeadSHA,
		ExternalID:    strconv.FormatInt(commentID, 10),
		URL:           url,
	})
	if err != nil {
		return fmt.Errorf("save summary artifact: %w", err)
	}
	return nil
}

func (p *publishContext) buildSummary(ctx context.Context, findings []store.Finding, resolvedSincePrevious int) string {
	var b strings.Builder
	b.WriteString(summaryMarker)
	b.WriteString("\n")

	automatedInconclusive := p.run.Status == store.RunInconclusive || p.run.Status == store.RunFailed
	if automatedInconclusive {
		// The technical detail (phase, parameter, stderr) belongs to the panel
		// and the run logs. Publishing it here would overwrite a confirmed
		// summary with error text, so the comment states only that QA could not
		// conclude.
		b.WriteString("\n> ⚠️ **QA no pudo concluir la revisión automática.** Revisa el panel para la fase y el detalle técnico.\n")
	}

	fmt.Fprintf(&b, "\n### Revisión #%d · %s\n\n", p.runNumber, shortSHA(p.run.HeadSHA))

	// The human outcome is a different result from the automated one and is
	// rendered as its own section, so the comment never claims the engine
	// completed the coverage.
	if automatedInconclusive {
		if human := p.humanVerdictBlock(ctx); human != "" {
			b.WriteString("\n")
			b.WriteString(human)
			b.WriteString("\n")
		}
	}

	counts := map[string]int{"blocker": 0, "major": 0, "minor": 0, "nit": 0}
	// Severity counts describe the current open findings of the PR.
	for _, f := range p.findingSnapshot {
		if f.Status != store.FindingOpen || f.Advisory {
			continue
		}
		if _, ok := counts[f.Severity]; ok {
			counts[f.Severity]++
		}
	}
	b.WriteString("**Severidades:** ")
	b.WriteString(fmt.Sprintf("blocker %d · major %d · minor %d · nit %d",
		counts["blocker"], counts["major"], counts["minor"], counts["nit"]))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "**Resueltos desde la revisión anterior:** %d\n\n", resolvedSincePrevious)

	b.WriteString(p.buildVerificationBlock())
	b.WriteString(p.buildCriteriaBlock(ctx))

	coverage := decodeReviewCoverage(p.run.Coverage)
	fmt.Fprintf(&b, "**Cobertura:** %d/%d (revisados/omitidos)", coverage.ReviewedFiles, coverage.SkippedFiles)
	if coverage.NotRequiredFiles > 0 {
		fmt.Fprintf(&b, " · %d no requeridos (eliminados, binarios o submódulos)", coverage.NotRequiredFiles)
	}
	b.WriteString("\n\n")
	if pending := coverage.pendingFiles(); len(pending) > 0 {
		fmt.Fprintf(&b, "**Archivos pendientes de revisión (%d):**\n", len(pending))
		for _, file := range pending {
			reason := file.Reason
			if reason == "" {
				reason = "sin cobertura"
			}
			fmt.Fprintf(&b, "\n- `%s` — %s\n", p.sanitize(file.Path), p.sanitize(reason))
		}
		b.WriteString("\n")
	}
	if human := coverage.humanFiles(); len(human) > 0 {
		fmt.Fprintf(&b, "**Revisión humana requerida (%d):**\n", len(human))
		for _, file := range human {
			reason := file.Reason
			if reason == "" {
				reason = "sin análisis automático"
			}
			fmt.Fprintf(&b, "\n- `%s` — %s\n", p.sanitize(file.Path), p.sanitize(reason))
		}
		b.WriteString("\n")
	}

	b.WriteString(p.sanitize(p.run.Summary))
	b.WriteString("\n")

	// When the advisory design finding carries the verdict's items, the block
	// only renders the surrounding evidence so the summary does not repeat
	// the same risks and conditions twice.
	b.WriteString(p.buildArchitectureBlock(p.hasAdvisoryFindings()))

	if missing := p.findingsWithoutInlineComment(findings); len(missing) > 0 {
		b.WriteString(fmt.Sprintf("\n**Hallazgos sin comentario inline (%d):**\n", len(missing)))
		for _, f := range missing {
			severity := f.Severity
			if severity == "" {
				severity = "sin severidad"
			}
			if f.Advisory {
				severity += " · observación"
			}
			fmt.Fprintf(&b, "\n- **%s** · `%s` — %s\n",
				p.sanitize(severity), p.findingLocation(f), p.sanitize(f.Title))
			if body := p.sanitize(f.Body); body != "" {
				b.WriteString("  " + strings.ReplaceAll(body, "\n", "\n  ") + "\n")
			}
		}
	}

	if link := p.panelLink(); link != "" {
		fmt.Fprintf(&b, "\n[Ver en el panel](%s)\n", link)
	}
	return b.String()
}

// buildArchitectureBlock renders the observational architectural verdict of
// the comparison plus the trusted baseline, the deterministic operational
// evidence and the consumer impact. It is evidence only: no check, status or
// inline decision reads it. The baseline, the operational dimensions and the
// consumer evidence run independently of the OCR analysis, so they are shown
// even when #157's activation rule did not request the analysis; a run with
// none of them renders nothing. When the advisory finding already carries the
// verdict's items, only the surrounding evidence is rendered so the same risks
// and conditions are not repeated.
func (p *publishContext) buildArchitectureBlock(itemsRendered bool) string {
	verdict := store.DecodeArchitectureVerdict(p.run.ArchitectureAnalysis)
	requested := verdict.Requested && verdict.Status != "" && verdict.Status != store.ArchitectureNotRequested
	ops := verdict.OpsEvidence
	opsShown := ops != nil && ops.HasContent()
	consumerBlock := renderConsumerBlock(verdict.Consumers, p.sanitize)
	// The trusted baseline, the operational dimensions and the consumer evidence
	// run even when the OCR report was not requested, so they are shown on their
	// own.
	if !requested && !opsShown && consumerBlock == "" && verdict.Baseline == nil {
		return ""
	}
	var b strings.Builder
	if requested {
		fmt.Fprintf(&b, "\n**Análisis arquitectónico:** %s · %s\n",
			p.sanitize(architectureStatusLabel(verdict.Status)), p.sanitize(architectureModeLabel(verdict.Mode)))
		if len(verdict.Signals) > 0 {
			signals := make([]string, 0, len(verdict.Signals))
			for _, signal := range verdict.Signals {
				signals = append(signals, "`"+p.sanitize(signal)+"`")
			}
			fmt.Fprintf(&b, "\n**Señales:** %s\n", strings.Join(signals, ", "))
		}
		if included, excluded := len(verdict.Scope.Included), len(verdict.Scope.Excluded); included+excluded > 0 {
			fmt.Fprintf(&b, "\n**Alcance:** %d archivo(s) incluido(s), %d fuera del análisis", included, excluded)
			if len(verdict.Scope.OutOfScope) > 0 {
				fmt.Fprintf(&b, " (%s)", p.sanitize(strings.Join(verdict.Scope.OutOfScope, ", ")))
			}
			b.WriteString("\n")
		}
		if len(verdict.Dimensions) > 0 {
			names := make([]string, 0, len(verdict.Dimensions))
			for _, dimension := range verdict.Dimensions {
				name := p.sanitize(dimension.Name)
				if dimension.Stale {
					name += " (pendiente de revalidación)"
				}
				names = append(names, name)
			}
			fmt.Fprintf(&b, "\n**Dimensiones evaluadas:** %s\n", strings.Join(names, " · "))
		}
	}
	writeArchitectureBaseline(&b, verdict.Baseline, p.sanitize)
	stale := verdict.Status == store.ArchitectureStale
	if stale {
		fmt.Fprintf(&b, "\n> Evidencia obsoleta: un push, un cambio de base o un cambio de reglas invalidó el dictamen. "+
			"Las dimensiones afectadas no afirman su resultado anterior hasta que el análisis las vuelva a evidenciar; "+
			"por eso no se publican sus conclusiones.\n")
	}
	if verdict.CarriedFrom != "" {
		fmt.Fprintf(&b, "\n_Evidencia conservada desde %s._\n", p.sanitize(shortSHA(verdict.CarriedFrom)))
	}
	// Stale evidence publishes no conclusions; an advisory finding that already
	// carries the verdict's items owns them, so the block does not repeat them.
	switch {
	case requested && !stale && !itemsRendered:
		writeArchitectureItems(&b, "Comportamiento antes/después", verdict.BehaviorChanges, p.sanitize)
		writeArchitectureItems(&b, "Riesgos y compatibilidad", verdict.Risks, p.sanitize)
		writeArchitectureItems(&b, "Alternativas", verdict.Alternatives, p.sanitize)
		writeArchitectureItems(&b, "Condiciones de aceptación", verdict.AcceptanceConditions, p.sanitize)
		writeArchitectureItems(&b, "Incertidumbres", verdict.Uncertainties, p.sanitize)
	case !requested && !stale:
		// Without a requested analysis these sections hold only consumer impact.
		writeArchitectureItems(&b, "Riesgos y compatibilidad", verdict.Risks, p.sanitize)
		writeArchitectureItems(&b, "Incertidumbres", verdict.Uncertainties, p.sanitize)
	}
	b.WriteString(consumerBlock)
	if requested && !stale && !itemsRendered {
		if recommendation := strings.TrimSpace(verdict.Recommendation); recommendation != "" {
			fmt.Fprintf(&b, "\n**Recomendación original:** %s\n", p.sanitize(recommendation))
		}
	}
	if verdict.Evidence.SchemaVersion != "" || len(verdict.Evidence.CitedObservationIDs) > 0 {
		fmt.Fprintf(&b, "\n_Evidencia: %s · %d cita(s)_\n",
			p.sanitize(verdict.Evidence.SchemaVersion), len(verdict.Evidence.CitedObservationIDs))
	}
	if len(verdict.Errors) > 0 && p.run.Status == store.RunSucceeded {
		// An incomplete run keeps its technical detail in the panel and logs; a
		// human-verdict comment must not carry error text into the conversation.
		fmt.Fprintf(&b, "\n> Análisis incompleto: %s\n", p.sanitize(strings.Join(verdict.Errors, "; ")))
	}
	if opsShown {
		b.WriteString(p.buildOpsEvidenceBlock())
	}
	b.WriteString("\n> Modo observación: el dictamen no cambia el check, el estado de QA ni los comentarios inline.\n")
	return b.String()
}

// buildOpsEvidenceBlock renders the per-dimension operational verdict: what the
// change touches, what evidence it carries and what is still missing. The head
// SHA ties the evidence to the change that was actually reviewed.
func (p *publishContext) buildOpsEvidenceBlock() string {
	ops := store.DecodeArchitectureVerdict(p.run.ArchitectureAnalysis).OpsEvidence
	if ops == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n**Evidencia operativa:** %s · modo observación · `%s`\n",
		opsStatusLabel(ops.Status), shortSHA(p.run.HeadSHA))
	if errorText := strings.TrimSpace(ops.Error); errorText != "" && p.run.Status == store.RunSucceeded {
		fmt.Fprintf(&b, "\n> Evidencia operativa incompleta: %s\n", p.sanitize(errorText))
	}
	for _, dimension := range ops.Dimensions {
		if !dimension.Applies {
			continue
		}
		fmt.Fprintf(&b, "\n- **%s:** %s", opsDimensionLabel(dimension.Name), opsStatusLabel(dimension.Status))
		if dimension.Reason != "" {
			fmt.Fprintf(&b, " — %s", p.sanitize(dimension.Reason))
		}
		if len(dimension.Missing) > 0 {
			fmt.Fprintf(&b, " · falta: %s", p.sanitize(strings.Join(dimension.Missing, ", ")))
		}
		b.WriteString("\n")
		if len(dimension.Evidence) > 0 {
			fmt.Fprintf(&b, "  Evidencia: %s\n", p.sanitize(strings.Join(dimension.Evidence, " · ")))
		}
	}
	return b.String()
}

// renderConsumerBlock shows the consumer impact the review could prove. A
// redacted consumer never exposes its repository, path or pinned SHA: the
// published pull request may have a wider audience than that repository.
// Redacted entries are masked before the verdict is persisted, so the panel has
// no hidden detail to fall back to; that gap is deliberate until a live
// visibility check exists.
func renderConsumerBlock(consumers store.ArchitectureConsumers, sanitize func(string) string) string {
	if consumers.Status == "" || consumers.Status == store.ConsumersNotConfigured || consumers.Configured == 0 {
		return ""
	}
	var b strings.Builder
	counts := fmt.Sprintf("%d configurado(s) · %d consultado(s)", consumers.Configured, consumers.Checked)
	if consumers.RequiresHuman {
		counts += " · requiere criterio humano"
	}
	fmt.Fprintf(&b, "\n**Consumidores del contrato:** %s · %s\n",
		sanitize(consumerStatusLabel(consumers.Status)), counts)

	redacted := 0
	for _, consumer := range consumers.Consumers {
		if consumer.Redacted {
			redacted++
			fmt.Fprintf(&b, "\n- consumidor privado — %s\n", sanitize(consumerStatusLabel(consumer.Status)))
			continue
		}
		detail := fmt.Sprintf("**%s** · `%s`", sanitize(consumer.Repository), sanitize(consumer.Contract))
		if consumer.Ref != "" {
			detail += " @ `" + sanitize(consumer.Ref) + "`"
		}
		if consumer.SHA != "" {
			detail += " (`" + shortSHA(consumer.SHA) + "`)"
		}
		detail += " — " + sanitize(consumerStatusLabel(consumer.Status))
		if consumer.Impact != "" {
			detail += " · impacto " + sanitize(consumerImpactLabel(consumer.Impact))
		}
		fmt.Fprintf(&b, "\n- %s\n", detail)
	}
	if redacted > 0 {
		fmt.Fprintf(&b, "\n_%d consumidor(es) privado(s): el detalle no se publica por falta de visibilidad equivalente._\n", redacted)
	}
	return b.String()
}

func opsStatusLabel(status string) string {
	switch status {
	case store.OpsStatusSatisfied:
		return "satisfecha"
	case store.OpsStatusUnknown:
		return "desconocida"
	case store.OpsStatusRisk:
		return "riesgo"
	case store.OpsStatusNotApplicable:
		return "no aplica"
	default:
		return status
	}
}

func consumerStatusLabel(status string) string {
	switch status {
	case store.ConsumersComplete:
		return "completo"
	case store.ConsumersPartial:
		return "parcial"
	case store.ConsumersUnknown:
		return "desconocido"
	case store.ConsumerStatusAffected:
		return "afectado"
	case store.ConsumerStatusCompatible:
		return "compatible"
	case store.ConsumerStatusOutOfScope:
		return "fuera de alcance"
	case store.ConsumerStatusSkipped:
		return "omitido"
	default:
		return status
	}
}

func opsDimensionLabel(name string) string {
	switch name {
	case store.OpsDimensionSchema:
		return "Migraciones y datos"
	case store.OpsDimensionTopology:
		return "Despliegue y topología"
	case store.OpsDimensionPermissions:
		return "Permisos"
	case store.OpsDimensionLoad:
		return "Carga y operación"
	default:
		return name
	}
}

func consumerImpactLabel(impact string) string {
	switch impact {
	case store.ConsumerImpactVerified:
		return "comprobado"
	case store.ConsumerImpactNone:
		return "sin impacto"
	case store.ConsumerImpactUnknown:
		return "desconocido"
	default:
		return impact
	}
}

func writeArchitectureItems(b *strings.Builder, label string, items []string, sanitize func(string) string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n**%s:**\n", label)
	for _, item := range items {
		fmt.Fprintf(b, "- %s\n", sanitize(item))
	}
}

func writeArchitectureBaseline(b *strings.Builder, baseline *store.ArchitectureBaseline, sanitize func(string) string) {
	if baseline == nil {
		return
	}
	fmt.Fprintf(b, "\n**Línea base de arquitectura:** %s", sanitize(architectureBaselineStatusLabel(baseline.Status)))
	if baseline.RulesPath != "" {
		fmt.Fprintf(b, " · `%s`", sanitize(baseline.RulesPath))
	}
	if baseline.ChangedInPR {
		b.WriteString(" · la regla cambió en el PR; se evaluó la versión de la base")
	}
	b.WriteString("\n")
	for _, check := range baseline.Checks {
		fmt.Fprintf(b, "- %s `%s`: `%s` → `%s` en `%s` (import `%s`)",
			sanitize(architectureDependencyStatusLabel(check.Status)), sanitize(check.RuleID), sanitize(check.FromModule),
			sanitize(check.ToModule), sanitize(check.File), sanitize(check.Import))
		if check.ExceptionID != "" {
			fmt.Fprintf(b, " [%s]", sanitize(check.ExceptionID))
		}
		b.WriteString("\n")
	}
	if len(baseline.Errors) > 0 {
		fmt.Fprintf(b, "> Línea base: %s\n", sanitize(strings.Join(baseline.Errors, "; ")))
	}
}

func architectureBaselineStatusLabel(status string) string {
	switch status {
	case store.ArchitectureBaselinePass:
		return "correcta"
	case store.ArchitectureBaselineViolation:
		return "violación"
	case store.ArchitectureBaselineUnknown:
		return "desconocida"
	default:
		return status
	}
}

func architectureDependencyStatusLabel(status string) string {
	switch status {
	case store.ArchitectureDependencyViolation:
		return "violación"
	case store.ArchitectureDependencyException:
		return "excepción autorizada"
	default:
		return status
	}
}

func architectureStatusLabel(status string) string {
	switch status {
	case store.ArchitectureComplete:
		return "completo"
	case store.ArchitecturePartial:
		return "parcial"
	case store.ArchitectureInconclusive:
		return "inconcluso"
	case store.ArchitectureViolation:
		return "violación de la línea base"
	case store.ArchitectureStale:
		return "obsoleto"
	default:
		return status
	}
}

func architectureModeLabel(mode string) string {
	if mode == store.ArchitectureAnalysisObservation {
		return "modo observación"
	}
	return mode
}

// architectureCheckLine is the one-line summary appended to the check output.
// It never changes the conclusion.
func (p *publishContext) architectureCheckLine() string {
	verdict := store.DecodeArchitectureVerdict(p.run.ArchitectureAnalysis)
	hasAnalysis := verdict.Requested && verdict.Status != "" && verdict.Status != store.ArchitectureNotRequested
	ops := verdict.OpsEvidence
	opsShown := ops != nil && ops.HasContent()
	hasConsumers := verdict.Consumers.Status != "" && verdict.Consumers.Status != store.ConsumersNotConfigured
	if !hasAnalysis && !opsShown && verdict.Baseline == nil && !hasConsumers {
		return ""
	}
	line := ""
	if hasAnalysis {
		line = fmt.Sprintf("Análisis arquitectónico (observación): %s",
			architectureStatusLabel(verdict.Status))
		if len(verdict.Signals) > 0 {
			line += " · señales: " + strings.Join(verdict.Signals, ", ")
		}
		if len(verdict.Dimensions) > 0 {
			line += fmt.Sprintf(" · %d dimensión(es)", len(verdict.Dimensions))
		}
	}
	if opsShown {
		if line != "" {
			line += " · "
		}
		line += "Evidencia operativa (observación): " + opsStatusLabel(ops.Status)
	}
	if verdict.Baseline != nil {
		switch {
		case line == "":
			line = "Línea base de arquitectura: " + architectureBaselineStatusLabel(verdict.Baseline.Status)
		default:
			line += " · línea base: " + architectureBaselineStatusLabel(verdict.Baseline.Status)
		}
		if verdict.Baseline.RulesPath != "" {
			line += " · `" + verdict.Baseline.RulesPath + "`"
		}
	}
	if hasConsumers {
		switch {
		case line == "":
			line = "Consumidores del contrato (observación): "
		case hasAnalysis:
			line += " · "
		default:
			line += " · consumidores: "
		}
		line += consumerStatusLabel(verdict.Consumers.Status)
	}
	return line
}

// verificationApplied reports whether the second OCR pass changed findings.
// It ran in this run when the session is recorded, and its triage can also be
// reused from a previous run on the same head, where no new scan runs. Only
// then are inline comments limited to the confirmed ones; a dry run or a
// skipped verification keeps today's behaviour.
func (p *publishContext) verificationApplied() bool {
	return p.policy.AutoTriageMode == store.AutoTriageApply &&
		(p.run.VerifySessionID != "" || p.hasVerifierTriage())
}

func (p *publishContext) hasAdvisoryFindings() bool {
	for _, f := range p.findingSnapshot {
		if f.Advisory && f.Status == store.FindingOpen {
			return true
		}
	}
	return false
}

// inlineAllowed is the single predicate the summary and the inline review share:
// once the verifier's decisions are applied, only a finding confirmed with
// evidence that still matches this head may be published inline.
func (p *publishContext) inlineAllowed(f store.Finding) bool {
	return !p.verificationApplied() || p.findingVerdict(f) == "confirm"
}

func (p *publishContext) hasVerifierTriage() bool {
	for _, f := range p.findingSnapshot {
		if f.TriageSource == store.TriageSourceVerifier {
			return true
		}
	}
	return false
}

// buildVerificationBlock renders the curated summary of the second OCR pass.
// With auto-triage off since before the run and no stored verifier decision,
// the summary stays exactly as it was.
func (p *publishContext) buildVerificationBlock() string {
	if p.policy.AutoTriageMode == store.AutoTriageOff &&
		p.run.VerifySessionID == "" && !p.hasVerifierTriage() {
		return ""
	}

	var validated, uncertain []store.Finding
	dismissed := 0
	for _, f := range p.findingSnapshot {
		if f.TriageSource == store.TriageSourceHuman || f.Advisory {
			continue
		}
		switch p.findingVerdict(f) {
		case "confirm":
			if f.Status == store.FindingOpen {
				validated = append(validated, f)
			}
		case "refute":
			// Only a withdrawal the policy applied counts as dismissed; a
			// refutation it degraded (blocking severity, auto-withdraw off or
			// evidence missing) is still the human's call.
			switch f.Status {
			case store.FindingWithdrawn:
				dismissed++
			case store.FindingOpen:
				uncertain = append(uncertain, f)
			}
		default:
			// Uncertainty is the human's call even when the row still looks
			// withdrawn: new evidence may have invalidated that dismissal.
			uncertain = append(uncertain, f)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "**Verificación automática:** %d validados · %d para tu criterio · %d descartados\n",
		len(validated), len(uncertain), dismissed)
	if p.policy.AutoTriageMode == store.AutoTriageDryRun {
		b.WriteString("\n_(simulación: no se cambió el estado de ningún hallazgo)_\n")
	}
	if len(validated) > 0 {
		b.WriteString("\n**Validado por la verificación:**\n")
		for _, f := range validated {
			b.WriteString(p.findingBullet(f))
		}
	}
	if len(uncertain) > 0 {
		fmt.Fprintf(&b, "\n<details><summary>Necesita tu criterio (%d)</summary>\n\n", len(uncertain))
		for _, f := range uncertain {
			b.WriteString(p.findingBullet(f))
		}
		b.WriteString("\n</details>\n")
	}
	if dismissed > 0 {
		fmt.Fprintf(&b, "\n%d descartados automáticamente", dismissed)
		if link := p.panelLink(); link != "" {
			fmt.Fprintf(&b, " · [ver en el panel](%s)", link)
		}
		b.WriteString("\n")
	}
	if p.policy.AutoTriageMode != store.AutoTriageOff &&
		p.run.VerifySessionID == "" && !p.hasVerifierTriage() {
		b.WriteString("\n> La verificación automática no se ejecutó; estos hallazgos no están confirmados.\n")
	}
	b.WriteString("\n")
	return b.String()
}

// findingVerdict is the verifier's answer for a finding. The stored record is
// authoritative because it is rewritten every run, including the uncertain
// answer that degrades a stale or invalid confirmation; a verdict retained from
// another commit is not trusted at all.
func (p *publishContext) findingVerdict(f store.Finding) string {
	v, trusted := p.verificationAtHead(f)
	if !trusted {
		return ""
	}
	return v.Verdict
}

func (p *publishContext) findingBullet(f store.Finding) string {
	severity := f.Severity
	if severity == "" {
		severity = "sin severidad"
	}
	bullet := fmt.Sprintf("- **%s** · `%s` — %s\n",
		p.sanitize(severity), p.findingLocation(f), p.sanitize(f.Title))
	if reason := strings.TrimSpace(p.findingReason(f)); reason != "" {
		bullet += fmt.Sprintf("  _%s_\n", p.sanitize(reason))
	}
	return bullet
}

// findingReason explains why the verifier's answer could not be used, so the
// summary does not leave an uncertainty without its cause.
func (p *publishContext) findingReason(f store.Finding) string {
	if v, trusted := p.verificationAtHead(f); trusted {
		return v.Reason
	}
	if v := parseVerification(f); v.Verdict != "" {
		return "la verificación corresponde a otro commit"
	}
	return ""
}

func (p *publishContext) panelLink() string {
	if p.w.PublicURL == "" {
		return ""
	}
	return strings.TrimRight(p.w.PublicURL, "/") + "/prs/" + strconv.FormatUint(uint64(p.pr.ID), 10)
}

// findingsWithoutInlineComment selects the open findings that have no inline
// comment yet. It re-checks both conditions even though the caller supplies a
// snapshot, so a retried publish can never list an already published finding.
func (p *publishContext) findingsWithoutInlineComment(findings []store.Finding) []store.Finding {
	missing := make([]store.Finding, 0)
	for _, f := range findings {
		if f.Status != store.FindingOpen || f.GithubCommentID != 0 {
			continue
		}
		// Advisory evidence is never posted inline, so the summary is its one
		// GitHub surface regardless of the verifier's inline gate.
		if !f.Advisory && !p.inlineAllowed(f) {
			continue
		}
		missing = append(missing, f)
	}
	sort.SliceStable(missing, func(i, j int) bool {
		if a, b := severityRank[missing[i].Severity], severityRank[missing[j].Severity]; a != b {
			return a > b
		}
		if missing[i].Path != missing[j].Path {
			return missing[i].Path < missing[j].Path
		}
		if missing[i].StartLine != missing[j].StartLine {
			return missing[i].StartLine < missing[j].StartLine
		}
		return missing[i].Title < missing[j].Title
	})
	return missing
}

// findingLocation renders every citation of the finding. A design finding that
// spans files lists all of them; a single path keeps the historical
// path:start-end form. Citations from the base side are marked as previous
// code because their line numbers belong to the removed version.
func (p *publishContext) findingLocation(f store.Finding) string {
	if citations := store.DecodeFindingCitations(f.Citations); len(citations) > 0 {
		return p.citationLocations(citations)
	}
	return p.pathRange(f.Path, f.StartLine, f.EndLine)
}

func (p *publishContext) citationLocations(citations []store.FindingCitation) string {
	parts := make([]string, 0, len(citations))
	for _, citation := range citations {
		location := p.pathRange(citation.Path, citation.StartLine, citation.EndLine)
		if citation.Side == store.FindingCitationBase {
			location += " (código anterior)"
		}
		parts = append(parts, location)
	}
	return strings.Join(parts, ", ")
}

func (p *publishContext) pathRange(rawPath string, start, end int) string {
	path := p.sanitize(rawPath)
	if path == "" {
		return "sin ruta"
	}
	if start <= 0 {
		return path
	}
	if end < start {
		end = start
	}
	if end == start {
		return fmt.Sprintf("%s:%d", path, start)
	}
	return fmt.Sprintf("%s:%d-%d", path, start, end)
}

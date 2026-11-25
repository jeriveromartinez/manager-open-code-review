package store

import "testing"

func TestOpsEvidenceSurfacesRisksAndUncertainties(t *testing.T) {
	ops := OpsEvidence{
		Status: OpsStatusUnknown,
		Dimensions: []OpsDimension{
			{
				Name:    OpsDimensionSchema,
				Applies: true,
				Status:  OpsStatusRisk,
				Reason:  "destructive migration migrations/1_drop.sql",
			},
			{
				Name:    OpsDimensionTopology,
				Applies: true,
				Status:  OpsStatusUnknown,
				Reason:  "topology change without evidence",
				Missing: []string{"rollback/restore", "observability/alerts"},
			},
			{Name: OpsDimensionLoad, Applies: false, Status: OpsStatusNotApplicable},
		},
		Controls: OpsControls{Observation: true, WritesDecision: false},
	}

	if !ops.HasApplicable() {
		t.Fatal("HasApplicable = false, want true")
	}
	risks := ops.RiskItems()
	if len(risks) != 1 || risks[0] != "ops/schema: destructive migration migrations/1_drop.sql" {
		t.Errorf("risks = %v", risks)
	}
	uncertainties := ops.UncertaintyItems()
	if len(uncertainties) != 1 || uncertainties[0] !=
		"ops/topology: topology change without evidence (missing: rollback/restore, observability/alerts)" {
		t.Errorf("uncertainties = %v", uncertainties)
	}

	empty := OpsEvidence{Dimensions: []OpsDimension{{Name: OpsDimensionSchema, Status: OpsStatusNotApplicable}}}
	if empty.HasApplicable() {
		t.Error("HasApplicable = true for a not-applicable dimension")
	}
	if empty.HasContent() {
		t.Error("HasContent = true for an empty verdict")
	}
}

func TestOpsEvidenceErrorIsAnInventoryUncertainty(t *testing.T) {
	ops := OpsEvidence{
		Status:   OpsStatusUnknown,
		Error:    "diff inventory failed: boom",
		Controls: OpsControls{Observation: true, WritesDecision: false},
	}
	if !ops.HasContent() {
		t.Error("HasContent = false for an inventory error")
	}
	items := ops.UncertaintyItems()
	if len(items) != 1 || items[0] != "ops/inventory: diff inventory failed: boom" {
		t.Errorf("uncertainties = %v, want the inventory error", items)
	}
}

func TestArchitectureVerdictRoundTripsOpsEvidence(t *testing.T) {
	ops := OpsEvidence{
		Status: OpsStatusSatisfied,
		Dimensions: []OpsDimension{
			{Name: OpsDimensionSchema, Applies: true, Status: OpsStatusSatisfied, Evidence: []string{"artifact: migration x"}},
		},
		Controls: OpsControls{Observation: true, WritesDecision: false},
	}
	raw := ArchitectureVerdictJSON(ArchitectureVerdict{
		Status:      ArchitectureComplete,
		Requested:   true,
		Mode:        ArchitectureAnalysisObservation,
		OpsEvidence: &ops,
	})
	decoded := DecodeArchitectureVerdict(raw)
	if decoded.OpsEvidence == nil {
		t.Fatal("ops evidence was not decoded")
	}
	if decoded.OpsEvidence.Status != OpsStatusSatisfied || !decoded.OpsEvidence.Controls.Observation ||
		decoded.OpsEvidence.Controls.WritesDecision {
		t.Errorf("decoded ops evidence = %+v", decoded.OpsEvidence)
	}
	if decoded.Status != ArchitectureComplete {
		t.Errorf("architecture status = %q", decoded.Status)
	}
}

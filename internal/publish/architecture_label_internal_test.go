package publish

import (
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestArchitectureStatusLabelCoversStale(t *testing.T) {
	cases := map[string]string{
		store.ArchitectureComplete:     "completo",
		store.ArchitecturePartial:      "parcial",
		store.ArchitectureInconclusive: "inconcluso",
		store.ArchitectureStale:        "obsoleto",
	}
	for status, want := range cases {
		if got := architectureStatusLabel(status); got != want {
			t.Errorf("architectureStatusLabel(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestBuildArchitectureBlockRendersBaselineStaleAndCarriedOnce(t *testing.T) {
	p := &publishContext{redactor: NewRedactor()}
	p.run.ArchitectureAnalysis = store.ArchitectureVerdictJSON(store.ArchitectureVerdict{
		Status:      store.ArchitectureStale,
		Requested:   true,
		Mode:        store.ArchitectureAnalysisObservation,
		CarriedFrom: "abcdef1234567890",
		Baseline: &store.ArchitectureBaseline{
			Status:    store.ArchitectureBaselinePass,
			RulesPath: ".architecture/rules.yaml",
		},
		Dimensions: []store.ArchitectureDimension{{Name: "Boundaries", Content: "old", Stale: true}},
	})

	block := p.buildArchitectureBlock(false)
	if strings.Count(block, "Línea base de arquitectura") != 1 {
		t.Errorf("baseline rendered more than once:\n%s", block)
	}
	for _, wanted := range []string{"Evidencia obsoleta", "Evidencia conservada desde", "pendiente de revalidación", "abcdef1"} {
		if !strings.Contains(block, wanted) {
			t.Errorf("summary lacks %q:\n%s", wanted, block)
		}
	}
}

func TestBuildArchitectureBlockRendersCarriedConclusionsWhenCurrent(t *testing.T) {
	p := &publishContext{redactor: NewRedactor()}
	p.run.ArchitectureAnalysis = store.ArchitectureVerdictJSON(store.ArchitectureVerdict{
		Status:      store.ArchitectureComplete,
		Requested:   true,
		Mode:        store.ArchitectureAnalysisObservation,
		CarriedFrom: "abcdef1234567890",
		Risks:       []string{"risk claim"},
		Baseline:    &store.ArchitectureBaseline{Status: store.ArchitectureBaselinePass},
	})

	block := p.buildArchitectureBlock(false)
	if !strings.Contains(block, "Riesgos y compatibilidad") || !strings.Contains(block, "risk claim") {
		t.Errorf("valid carried conclusions were not published:\n%s", block)
	}
	if !strings.Contains(block, "Evidencia conservada desde") || strings.Contains(block, "Evidencia obsoleta") {
		t.Errorf("carried notice is wrong:\n%s", block)
	}
	if strings.Count(block, "Línea base de arquitectura") != 1 {
		t.Errorf("baseline rendered more than once:\n%s", block)
	}
}

func TestBuildArchitectureBlockOmitsConclusionsWhenStale(t *testing.T) {
	p := &publishContext{redactor: NewRedactor()}
	p.run.ArchitectureAnalysis = store.ArchitectureVerdictJSON(store.ArchitectureVerdict{
		Status:               store.ArchitectureStale,
		Requested:            true,
		Mode:                 store.ArchitectureAnalysisObservation,
		Dimensions:           []store.ArchitectureDimension{{Name: "Boundaries", Content: "old", Stale: true}},
		BehaviorChanges:      []string{"behavior claim"},
		Risks:                []string{"risk claim"},
		Alternatives:         []string{"alternative claim"},
		AcceptanceConditions: []string{"acceptance claim"},
		Uncertainties:        []string{"uncertainty claim"},
		Recommendation:       "original recommendation",
	})

	block := p.buildArchitectureBlock(false)
	for _, unwanted := range []string{
		"Comportamiento antes/después", "Riesgos y compatibilidad", "Alternativas",
		"Condiciones de aceptación", "Incertidumbres", "Recomendación original",
		"risk claim", "alternative claim", "acceptance claim",
	} {
		if strings.Contains(block, unwanted) {
			t.Errorf("stale summary published %q:\n%s", unwanted, block)
		}
	}
	if !strings.Contains(block, "Evidencia obsoleta") || !strings.Contains(block, "pendiente de revalidación") {
		t.Errorf("stale summary lacks its warning:\n%s", block)
	}
}

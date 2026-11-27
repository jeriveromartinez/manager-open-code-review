package review

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// planFixture commits a base tree and a changed head that exercises every
// critical signal: persistence, a public contract, concurrency, a policy glob
// and a cross-file dependency. The critical paths sort after the ordinary ones
// on purpose, so the comparison below can show what the plan changes.
type planFixture struct {
	dir     string
	baseSHA string
	headSHA string
}

func newPlanFixture(t *testing.T) planFixture {
	t.Helper()
	dir := newChunkRepo(t)
	base := map[string]string{
		"go.mod":                "module example.com/app\n\ngo 1.22\n",
		"a.go":                  "package app\n",
		"b.go":                  "package app\n",
		"c.go":                  "package app\n",
		"d.go":                  "package app\n",
		"docs/readme.md":        "# doc\n",
		"zz/store/order.go":     "package store\n\nfunc Order() {}\n",
		"zz/store/db/db.go":     "package db\n\nfunc Save() {}\n",
		"zapi/openapi.yaml":     "openapi: 3.0.0\npaths: {}\n",
		"zworker.go":            "package app\n\nfunc Run() {}\n",
		"zcritical/checkout.go": "package critical\n\nfunc Pay() {}\n",
		"zcmd/main.go":          "package main\n\nimport \"example.com/app/zz/store\"\n\nfunc main() { store.Order() }\n",
	}
	baseSHA := commitChunkFiles(t, dir, "base", base)

	head := map[string]string{
		"go.mod":                base["go.mod"],
		"a.go":                  "package app\n\nvar A = 1\n",
		"b.go":                  "package app\n\nvar B = 1\n",
		"c.go":                  "package app\n\nvar C = 1\n",
		"d.go":                  "package app\n\nvar D = 1\n",
		"docs/readme.md":        "# doc\n\nmore\n",
		"zz/store/order.go":     "package store\n\nfunc Order() {}\n\nfunc Persist() {}\n",
		"zz/store/db/db.go":     "package db\n\nfunc Save() {}\n\nfunc SaveAll() {}\n",
		"zapi/openapi.yaml":     "openapi: 3.0.0\npaths:\n  /orders: {}\n",
		"zworker.go":            "package app\n\nfunc Run() {\n\tgo func() {}()\n}\n",
		"zcritical/checkout.go": "package critical\n\nfunc Pay() {}\n\nfunc Refund() {}\n",
		"zcmd/main.go":          "package main\n\nimport \"example.com/app/zz/store\"\n\nfunc main() {\n\tstore.Order()\n\tstore.Persist()\n}\n",
	}
	headSHA := commitChunkFiles(t, dir, "head", head)
	return planFixture{dir: dir, baseSHA: baseSHA, headSHA: headSHA}
}

func (f planFixture) build(t *testing.T, criticalGlobs []string) reviewPlan {
	t.Helper()
	changed, err := gitChangedFiles(context.Background(), f.dir, f.baseSHA, f.headSHA)
	if err != nil {
		t.Fatalf("gitChangedFiles: %v", err)
	}
	contracts := store.ArchitectureContracts{Contracts: []store.ArchitectureContract{{Path: "zapi/openapi.yaml"}}}
	return buildReviewPlan(context.Background(), planInputs{
		Dir:           f.dir,
		BaseSHA:       f.baseSHA,
		HeadSHA:       f.headSHA,
		Changed:       changed,
		CriticalGlobs: criticalGlobs,
		Contracts:     contracts,
		ContextBytes:  4096,
		ReviewTimeout: 10 * time.Minute,
		VerifyReserve: 2 * time.Minute,
	})
}

func planUnitWithPath(plan reviewPlan, file string) (planUnit, bool) {
	for _, unit := range plan.Units {
		if planContainsString(unit.Paths, file) {
			return unit, true
		}
	}
	return planUnit{}, false
}

func TestBuildReviewPlanPrioritisesCriticalEvidence(t *testing.T) {
	f := newPlanFixture(t)
	plan := f.build(t, []string{"zcritical/**"})

	if plan.CriticalTotal == 0 {
		t.Fatalf("plan = %+v, want at least one critical unit", plan)
	}
	if plan.VerifyReserveSeconds != 120 || plan.PersistReserveSeconds != 5 {
		t.Errorf("reserves = verify %d / persist %d, want 120 / 5",
			plan.VerifyReserveSeconds, plan.PersistReserveSeconds)
	}

	seenNormal := false
	for _, unit := range plan.Units {
		if unit.Priority == planPriorityNormal {
			seenNormal = true
			continue
		}
		if seenNormal && unit.Priority == planPriorityCritical {
			t.Errorf("critical unit %v ordered after a normal unit", unit.Paths)
		}
		if unit.TimeLimitSeconds <= 0 {
			t.Errorf("unit %v has no time limit", unit.Paths)
		}
	}

	storeUnit, ok := planUnitWithPath(plan, "zz/store/order.go")
	if !ok {
		t.Fatal("plan has no unit for zz/store/order.go")
	}
	if storeUnit.Priority != planPriorityCritical ||
		!planContainsString(storeUnit.Evidence, "persistence:data_layer") {
		t.Errorf("store unit = %+v, want critical with persistence evidence", storeUnit)
	}
	if len(storeUnit.Ranges) == 0 || storeUnit.Ranges[0].Start < 1 {
		t.Errorf("store unit ranges = %+v, want a new-side range", storeUnit.Ranges)
	}

	contractUnit, _ := planUnitWithPath(plan, "zapi/openapi.yaml")
	if !planContainsString(contractUnit.Evidence, "public_contract:changed") {
		t.Errorf("contract unit evidence = %v, want public_contract:changed", contractUnit.Evidence)
	}

	workerUnit, _ := planUnitWithPath(plan, "zworker.go")
	if !planContainsString(workerUnit.Evidence, "concurrency:go func") {
		t.Errorf("worker unit evidence = %v, want concurrency:go func", workerUnit.Evidence)
	}

	policyUnit, _ := planUnitWithPath(plan, "zcritical/checkout.go")
	if !planContainsString(policyUnit.Evidence, "policy:zcritical/**") {
		t.Errorf("policy unit evidence = %v, want policy:zcritical/**", policyUnit.Evidence)
	}

	// A changed file that imports another changed file shares its unit and the
	// edge is recorded, so related work is not split across the budget.
	mainUnit, _ := planUnitWithPath(plan, "zcmd/main.go")
	if !planContainsString(mainUnit.Paths, "zz/store/order.go") {
		t.Errorf("main unit paths = %v, want the imported store file grouped", mainUnit.Paths)
	}
	if !planContainsString(mainUnit.Dependencies, "zcmd/main.go -> zz/store/order.go") {
		t.Errorf("main unit dependencies = %v, want the store edge", mainUnit.Dependencies)
	}
	if !planContainsString(mainUnit.Evidence, "dependency:zcmd/main.go -> zz/store/order.go") {
		t.Errorf("main unit evidence = %v, want the dependency evidence", mainUnit.Evidence)
	}
}

func TestReviewPlanOrderPathsPutsCriticalUnitsFirst(t *testing.T) {
	plan := reviewPlan{Units: []planUnit{
		{Paths: []string{"b.go"}, Priority: planPriorityNormal, Status: planStatusPending},
		{Paths: []string{"z.go", "a.go"}, Priority: planPriorityCritical, Status: planStatusPending},
	}}
	order := plan.orderPaths([]string{"a.go", "b.go", "z.go", "unknown.go"})
	// The critical unit goes first and its related paths stay together; paths
	// inside the unit keep a deterministic order.
	want := []string{"a.go", "z.go", "b.go", "unknown.go"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("orderPaths = %v, want %v", order, want)
	}
}

func TestApplyCoverageMarksReviewedReusedAndPending(t *testing.T) {
	plan := reviewPlan{Units: []planUnit{
		{Paths: []string{"a.go", "b.go"}, Priority: planPriorityCritical, Status: planStatusPending},
		{Paths: []string{"c.go"}, Priority: planPriorityNormal, Status: planStatusPending},
		{Paths: []string{"d.go"}, Priority: planPriorityNormal, Status: planStatusSkipped, Reason: changedDeleted},
	}}
	files := []coverageFile{
		{Path: "a.go", Status: coverageReviewed},
		{Path: "b.go", Status: coveragePending, Reason: "token_budget"},
		{Path: "c.go", Status: coverageReviewed},
		{Path: "d.go", Status: changedDeleted},
	}
	plan.applyCoverage(files, map[string]bool{"c.go": true})

	if plan.CriticalPending != 1 {
		t.Errorf("critical pending = %d, want 1", plan.CriticalPending)
	}
	if plan.Units[0].Status != planStatusPending || plan.Units[0].Reason != "token_budget" {
		t.Errorf("critical unit = %+v, want pending/token_budget", plan.Units[0])
	}
	if plan.Units[1].Status != planStatusReused {
		t.Errorf("normal unit = %+v, want reused", plan.Units[1])
	}
	if plan.Units[2].Status != planStatusSkipped || plan.Units[2].Reason != changedDeleted {
		t.Errorf("skipped unit = %+v, want unchanged", plan.Units[2])
	}

	// Once every critical path is covered the unit stops blocking the gate.
	plan.applyCoverage([]coverageFile{
		{Path: "a.go", Status: coverageReviewed},
		{Path: "b.go", Status: coverageReviewedByParts},
	}, nil)
	if plan.CriticalPending != 0 || plan.Units[0].Status != planStatusReviewed {
		t.Errorf("covered critical unit = %+v, pending %d; want reviewed/0", plan.Units[0], plan.CriticalPending)
	}
}

func TestApplyPlanGateBlocksConclusiveResult(t *testing.T) {
	pending := &reviewPlan{CriticalPending: 1}
	if status, code := applyPlanGate(store.RunSucceeded, "", pending); status != store.RunInconclusive || code != "coverage_incomplete" {
		t.Errorf("gate = %s/%s, want inconclusive/coverage_incomplete", status, code)
	}
	if status, code := applyPlanGate(store.RunInconclusive, "coverage_incomplete", pending); status != store.RunInconclusive {
		t.Errorf("existing inconclusive became %s/%s", status, code)
	}
	if status, _ := applyPlanGate(store.RunFailed, "config", pending); status != store.RunFailed {
		t.Errorf("failed run became %s, want failed", status)
	}
	if status, code := applyPlanGate(store.RunSucceeded, "", &reviewPlan{}); status != store.RunSucceeded || code != "" {
		t.Errorf("gate with no critical gap = %s/%s, want succeeded", status, code)
	}
	if status, code := applyPlanGate(store.RunSucceeded, "", nil); status != store.RunSucceeded || code != "" {
		t.Errorf("gate without a plan = %s/%s, want succeeded", status, code)
	}
}

// TestPlanOrderingImprovesCriticalCoverageWithinBudget is the local comparison
// required by #231. The same changed set, budget and cost per file are given to
// the current (inventory) order and to the planned order; the planned order
// must cover more critical files, detect more seeded severe defects and spend
// the same budget.
func TestPlanOrderingImprovesCriticalCoverageWithinBudget(t *testing.T) {
	f := newPlanFixture(t)
	plan := f.build(t, []string{"zcritical/**"})

	changed, err := gitChangedFiles(context.Background(), f.dir, f.baseSHA, f.headSHA)
	if err != nil {
		t.Fatalf("gitChangedFiles: %v", err)
	}
	current := make([]string, 0, len(changed))
	for _, entry := range changed {
		current = append(current, entry.Path)
	}
	planned := plan.orderPaths(current)

	severe := map[string]bool{
		"zz/store/order.go": true, // persistence
		"zapi/openapi.yaml": true, // public contract
		"zworker.go":        true, // concurrency
	}
	const costPerFile = 10
	const filesAllowed = 5
	budget := costPerFile * filesAllowed

	measure := func(order []string) (critical, total, severeFound, cost int) {
		for _, file := range order {
			if cost+costPerFile > budget {
				break
			}
			cost += costPerFile
			total++
			if severe[file] {
				severeFound++
			}
			if unit, ok := planUnitWithPath(plan, file); ok && unit.Priority == planPriorityCritical {
				critical++
			}
		}
		return critical, total, severeFound, cost
	}

	plannedCritical, plannedTotal, plannedSevere, plannedCost := measure(planned)
	currentCritical, currentTotal, currentSevere, currentCost := measure(current)

	if plannedCritical <= currentCritical {
		t.Errorf("critical covered = planned %d / current %d, want the plan to improve", plannedCritical, currentCritical)
	}
	if plannedSevere <= currentSevere {
		t.Errorf("severe defects detected = planned %d / current %d, want the plan to improve", plannedSevere, currentSevere)
	}
	if plannedTotal != currentTotal || plannedCost != currentCost {
		t.Errorf("budget use = planned %d files/%d tokens, current %d files/%d tokens; want equal",
			plannedTotal, plannedCost, currentTotal, currentCost)
	}
	if plannedCost > budget {
		t.Errorf("planned cost = %d, want within %d", plannedCost, budget)
	}
}

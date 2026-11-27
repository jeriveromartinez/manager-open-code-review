package store

import (
	"testing"

	"gorm.io/datatypes"
)

func obs(checks ...CheckObservation) *ValidationObservation {
	return &ValidationObservation{Result: ObservationOK, Checks: checks}
}

func TestEvaluateCIRequiresNamedChecks(t *testing.T) {
	required := []RequiredCheck{{Name: "build", Origin: "app:7"}, {Name: "ci/test", Origin: "status"}}
	observation := obs(
		CheckObservation{Name: "build", Origin: "app:7", State: CheckPassed},
		CheckObservation{Name: "ci/test", Origin: "status", State: CheckPassed},
		CheckObservation{Name: "optional", Origin: "app:9", State: CheckFailed},
	)

	if got := EvaluateCI(observation, PRValidations{}, required); !got.Green() {
		t.Fatalf("EvaluateCI() = %+v, want green: only the required checks matter", got)
	}
}

func TestEvaluateCISelectionByNameAndOrigin(t *testing.T) {
	observation := obs(CheckObservation{Name: "build", Origin: "app:7", State: CheckPassed})

	tests := []struct {
		name     string
		required []RequiredCheck
		want     string
	}{
		{
			name:     "matching name and origin",
			required: []RequiredCheck{{Name: "build", Origin: "app:7"}},
			want:     CIGreen,
		},
		{
			name:     "name from another origin is absent",
			required: []RequiredCheck{{Name: "build", Origin: "app:8"}},
			want:     CIAbsent,
		},
		{
			name:     "other name is absent",
			required: []RequiredCheck{{Name: "test"}},
			want:     CIAbsent,
		},
		{
			name:     "origin may be omitted",
			required: []RequiredCheck{{Name: "build"}},
			want:     CIGreen,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateCI(observation, PRValidations{}, tt.required)
			if got.State != tt.want {
				t.Fatalf("EvaluateCI() state = %q, want %q (%+v)", got.State, tt.want, got)
			}
			if tt.want == CIAbsent && len(got.Missing) == 0 {
				t.Error("Missing is empty, want the absent check")
			}
		})
	}
}

func TestEvaluateCINeverTreatsAbsentOrPendingAsGreen(t *testing.T) {
	required := []RequiredCheck{{Name: "build"}}

	if got := EvaluateCI(obs(), PRValidations{}, required); got.Green() || got.State != CIAbsent {
		t.Fatalf("absent required check = %+v, want absent and not green", got)
	}
	if got := EvaluateCI(obs(CheckObservation{Name: "build", State: CheckPending}), PRValidations{}, required); got.Green() || got.State != CIPending {
		t.Fatalf("pending required check = %+v, want pending and not green", got)
	}
	// A configured required check cannot be proven from legacy counters.
	if got := EvaluateCI(nil, PRValidations{Passed: 3, Total: 3}, required); got.Green() || got.State != CIUnknown {
		t.Fatalf("legacy counters with required checks = %+v, want unknown and not green", got)
	}
}

func TestEvaluateCILookupFailureIsUnknown(t *testing.T) {
	failed := &ValidationObservation{Result: ObservationLookupFailed}
	if got := EvaluateCI(failed, PRValidations{Passed: 4, Total: 4}, nil); got.Green() || got.State != CIUnknown {
		t.Fatalf("lookup failure = %+v, want unknown and not green", got)
	}
	if got := EvaluateCI(failed, PRValidations{Passed: 4, Total: 4}, []RequiredCheck{{Name: "build"}}); got.Green() {
		t.Fatal("lookup failure with required checks is green, want not green")
	}
}

func TestEvaluateCIWithoutRequiredChecks(t *testing.T) {
	tests := []struct {
		name string
		obs  *ValidationObservation
		want string
	}{
		{name: "all passed", obs: obs(CheckObservation{Name: "a", State: CheckPassed}), want: CIGreen},
		{name: "one failed", obs: obs(CheckObservation{Name: "a", State: CheckFailed}), want: CIFailed},
		{name: "one pending", obs: obs(CheckObservation{Name: "a", State: CheckPending}), want: CIPending},
		{name: "no checks to verify", obs: obs(), want: CIGreen},
		{name: "no observation", obs: nil, want: CIUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EvaluateCI(tt.obs, PRValidations{}, nil); got.State != tt.want {
				t.Fatalf("EvaluateCI() = %+v, want %q", got, tt.want)
			}
		})
	}
}

func TestEvaluateCILegacyCounters(t *testing.T) {
	tests := []struct {
		name     string
		counters PRValidations
		want     string
	}{
		{name: "green", counters: PRValidations{Passed: 3, Total: 3}, want: CIGreen},
		{name: "failed", counters: PRValidations{Passed: 2, Failed: 1, Total: 3}, want: CIFailed},
		{name: "pending", counters: PRValidations{Passed: 1, Total: 3}, want: CIPending},
		{name: "never read", counters: PRValidations{}, want: CIUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EvaluateCI(nil, tt.counters, nil); got.State != tt.want {
				t.Fatalf("EvaluateCI() = %+v, want %q", got, tt.want)
			}
		})
	}
}

func TestEvaluateConfiguredCIRejectsUnreadablePolicy(t *testing.T) {
	raw := datatypes.JSON([]byte(`[{"name":"build","origin":"bogus"}]`))
	got := EvaluateConfiguredCI(obs(CheckObservation{Name: "build", State: CheckPassed}), PRValidations{}, raw)
	if got.Green() || got.State != CIUnknown {
		t.Fatalf("unreadable policy = %+v, want unknown and not green", got)
	}
}

func TestValidCheckOrigin(t *testing.T) {
	tests := []struct {
		origin string
		want   bool
	}{
		{"", true},
		{"status", true},
		{"app:7", true},
		{"app:0", false},
		{"app:-1", false},
		{"app:", false},
		{"app:abc", false},
		{"app:7x", false},
		{"bogus", false},
	}
	for _, tc := range tests {
		if got := validCheckOrigin(tc.origin); got != tc.want {
			t.Errorf("validCheckOrigin(%q) = %v, want %v", tc.origin, got, tc.want)
		}
	}
}

func TestParseRequiredChecksDropsInvalidEntries(t *testing.T) {
	raw := datatypes.JSON([]byte(`[
		{"name":"build","origin":"app:7"},
		{"name":"ci/test","origin":"status"},
		{"name":"  "},
		{"name":"x","origin":"bogus"}
	]`))
	checks := ParseRequiredChecks(raw)
	if len(checks) != 2 || checks[0].Name != "build" || checks[1].Origin != "status" {
		t.Fatalf("ParseRequiredChecks() = %+v, want the two valid entries", checks)
	}
	if got := ParseRequiredChecks(datatypes.JSON([]byte(`{`))); got != nil {
		t.Fatalf("malformed policy = %+v, want nil", got)
	}
}

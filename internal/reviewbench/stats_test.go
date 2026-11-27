package reviewbench

import (
	"math"
	"testing"
)

func almostEqual(a, b, tolerance float64) bool {
	return math.Abs(a-b) <= tolerance
}

func TestNewProportionWilson(t *testing.T) {
	p := NewProportion(8, 10)
	if p.Estimate == nil || p.CI95 == nil {
		t.Fatal("expected a defined estimate and interval")
	}
	if !almostEqual(*p.Estimate, 0.8, 1e-12) {
		t.Errorf("estimate = %v, want 0.8", *p.Estimate)
	}
	if !almostEqual(p.CI95.Low, 0.4902, 1e-4) || !almostEqual(p.CI95.High, 0.9433, 1e-4) {
		t.Errorf("CI95 = %+v, want [0.4902, 0.9433]", *p.CI95)
	}
}

func TestNewProportionEmptyDenominatorIsUndefined(t *testing.T) {
	p := NewProportion(0, 0)
	if p.Estimate != nil || p.CI95 != nil {
		t.Fatalf("empty proportion = %+v, want nil estimate and interval", p)
	}
}

func TestClopperPearson(t *testing.T) {
	interval, ok := ClopperPearson(10, 12)
	if !ok {
		t.Fatal("expected an interval")
	}
	// Exact 95% interval for 10/12 is approximately [0.5159, 0.9791].
	if !almostEqual(interval.Low, 0.5159, 2e-3) || !almostEqual(interval.High, 0.9791, 2e-3) {
		t.Errorf("interval = %+v, want approximately [0.5159, 0.9791]", interval)
	}
	// The bounds are defined by the exact tail equations.
	if !almostEqual(binomialTailAtLeast(10, 12, interval.Low), 0.025, 1e-6) {
		t.Errorf("lower bound tail = %v, want 0.025", binomialTailAtLeast(10, 12, interval.Low))
	}
	if !almostEqual(binomialTailAtMost(10, 12, interval.High), 0.025, 1e-6) {
		t.Errorf("upper bound tail = %v, want 0.025", binomialTailAtMost(10, 12, interval.High))
	}
	if _, ok := ClopperPearson(0, 0); ok {
		t.Fatal("zero denominator must not produce an interval")
	}
}

func TestNearestRank(t *testing.T) {
	samples := []int64{40, 10, 30, 20}
	tests := []struct {
		percentile float64
		want       int64
	}{
		{0.50, 20},
		{0.95, 40},
		{0.01, 10},
		{1.00, 40},
	}
	for _, test := range tests {
		if got := NearestRank(samples, test.percentile); got != test.want {
			t.Errorf("NearestRank(%v) = %d, want %d", test.percentile, got, test.want)
		}
	}
	if got := NearestRank(nil, 0.95); got != 0 {
		t.Errorf("NearestRank(empty) = %d, want 0", got)
	}
}

func TestExactMcNemar(t *testing.T) {
	// b=10, c=2: 2*P(X<=2) with X~Binomial(12, 1/2) = 158/4096.
	if got := ExactMcNemar(10, 2); !almostEqual(got, 0.03857421875, 1e-12) {
		t.Errorf("ExactMcNemar(10,2) = %v, want 0.03857421875", got)
	}
	if got := ExactMcNemar(0, 0); got != 1 {
		t.Errorf("ExactMcNemar(0,0) = %v, want 1", got)
	}
	// Symmetry in the discordant counts.
	if a, b := ExactMcNemar(3, 9), ExactMcNemar(9, 3); a != b {
		t.Errorf("McNemar is not symmetric: %v vs %v", a, b)
	}
}

func TestComparePaired(t *testing.T) {
	// A succeeds only in 10 cases, B only in 2, 8 both, n=20.
	a := make([]bool, 20)
	b := make([]bool, 20)
	for i := 0; i < 8; i++ {
		a[i], b[i] = true, true
	}
	for i := 8; i < 18; i++ {
		a[i] = true
	}
	for i := 18; i < 20; i++ {
		b[i] = true
	}
	result, err := ComparePaired(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if result.N != 20 || result.Both != 8 || result.ATrueBFalse != 10 || result.AFalseBTrue != 2 {
		t.Errorf("paired counts = %+v", result)
	}
	if !almostEqual(result.Difference, 0.4, 1e-12) {
		t.Errorf("difference = %v, want 0.4", result.Difference)
	}
	if !almostEqual(result.McNemarP, 0.03857421875, 1e-12) {
		t.Errorf("McNemar p = %v, want 0.03857421875", result.McNemarP)
	}
	if result.DifferenceCI95 == nil {
		t.Fatal("expected a difference interval")
	}
	// 10/12 has an exact interval of about [0.5159, 0.9791]; scaled by 12/20
	// and mapped through 2p-1 the difference interval is about [0.019, 0.575].
	if !almostEqual(result.DifferenceCI95.Low, 0.0190, 2e-3) || !almostEqual(result.DifferenceCI95.High, 0.5750, 2e-3) {
		t.Errorf("difference CI = %+v", *result.DifferenceCI95)
	}
}

func TestComparePairedRejectsMismatchedLengths(t *testing.T) {
	if _, err := ComparePaired([]bool{true}, []bool{}); err == nil {
		t.Fatal("expected an error for mismatched lengths")
	}
}

func TestComparePairedNoDiscordance(t *testing.T) {
	result, err := ComparePaired([]bool{true, false, true}, []bool{true, false, true})
	if err != nil {
		t.Fatal(err)
	}
	if result.McNemarP != 1 || result.Difference != 0 || result.DifferenceCI95 != nil {
		t.Errorf("identical outcomes = %+v, want p=1, difference=0 and no interval", result)
	}
}

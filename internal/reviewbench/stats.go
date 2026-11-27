package reviewbench

import (
	"errors"
	"math"
	"sort"
)

// z95 is the two-sided 95% normal quantile used by the Wilson interval.
const z95 = 1.959963984540054

// Interval is a closed confidence interval.
type Interval struct {
	Low  float64 `json:"low"`
	High float64 `json:"high"`
}

// Proportion reports a binary outcome with its denominator and a 95% Wilson
// score interval. Estimate and CI95 are nil when Total is zero, so an empty
// denominator is never presented as a measured value.
type Proportion struct {
	Successes int       `json:"successes"`
	Total     int       `json:"total"`
	Estimate  *float64  `json:"estimate,omitempty"`
	CI95      *Interval `json:"ci95,omitempty"`
}

// NewProportion computes the Wilson 95% interval. Wilson is used instead of the
// Wald interval because it keeps the coverage close to nominal for small
// samples and never leaves [0,1], which matters for the per-class denominators
// of this benchmark.
func NewProportion(successes, total int) Proportion {
	p := Proportion{Successes: successes, Total: total}
	if total <= 0 {
		return p
	}
	n := float64(total)
	estimate := float64(successes) / n
	z2 := z95 * z95
	denominator := 1 + z2/n
	center := (estimate + z2/(2*n)) / denominator
	half := z95 * math.Sqrt(estimate*(1-estimate)/n+z2/(4*n*n)) / denominator
	p.Estimate = &estimate
	p.CI95 = &Interval{Low: math.Max(0, center-half), High: math.Min(1, center+half)}
	return p
}

// NearestRank returns the percentile (0<p<=1) using the nearest-rank method:
// sort the sample, take the element at position ceil(p*n), one-based. It is
// used for p50/p95 latency because it needs no interpolation and is stable for
// the small sample sizes of a pilot.
func NearestRank(samples []int64, percentile float64) int64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]int64(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := int(math.Ceil(percentile * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// LatencyStats summarizes a latency sample with nearest-rank percentiles.
type LatencyStats struct {
	Samples int   `json:"samples"`
	P50MS   int64 `json:"p50_ms"`
	P95MS   int64 `json:"p95_ms"`
}

// Latency computes nearest-rank p50 and p95 over non-negative samples.
func Latency(samples []int64) LatencyStats {
	return LatencyStats{Samples: len(samples), P50MS: NearestRank(samples, 0.50), P95MS: NearestRank(samples, 0.95)}
}

// ExactMcNemar returns the two-sided exact McNemar p-value for b and c
// discordant pairs: 2*P(X<=min(b,c)) with X~Binomial(b+c, 1/2), capped at 1.
// No continuity correction or chi-square approximation is used.
func ExactMcNemar(b, c int) float64 {
	n := b + c
	if n == 0 {
		return 1
	}
	k := b
	if c < k {
		k = c
	}
	p := 0.0
	for i := 0; i <= k; i++ {
		p += binomialPMF(i, n, 0.5)
	}
	p *= 2
	if p > 1 {
		p = 1
	}
	return p
}

// PairedBinary is the exact McNemar comparison of two aligned binary outcomes.
// ATrueBFalse and AFalseBTrue are the discordant pairs; Difference is
// (ATrueBFalse-AFalseBTrue)/N.
type PairedBinary struct {
	N              int        `json:"n"`
	Both           int        `json:"both"`
	Neither        int        `json:"neither"`
	ATrueBFalse    int        `json:"a_true_b_false"`
	AFalseBTrue    int        `json:"a_false_b_true"`
	A              Proportion `json:"a"`
	B              Proportion `json:"b"`
	Difference     float64    `json:"difference"`
	DifferenceCI95 *Interval  `json:"difference_ci95,omitempty"`
	McNemarP       float64    `json:"mcnemar_p"`
}

// ComparePaired runs the exact McNemar test on two equal-length outcome slices.
// The difference interval is derived from the Clopper-Pearson interval of the
// discordant proportion b/(b+c): scaling 2p-1 by (b+c)/n yields an interval for
// (b-c)/n. It is reported only when there is at least one discordant pair.
func ComparePaired(a, b []bool) (PairedBinary, error) {
	if len(a) != len(b) {
		return PairedBinary{}, errors.New("paired outcomes differ in length")
	}
	result := PairedBinary{N: len(a)}
	successesA, successesB := 0, 0
	for i := range a {
		switch {
		case a[i] && b[i]:
			result.Both++
		case !a[i] && !b[i]:
			result.Neither++
		case a[i]:
			result.ATrueBFalse++
		case b[i]:
			result.AFalseBTrue++
		}
		if a[i] {
			successesA++
		}
		if b[i] {
			successesB++
		}
	}
	result.A = NewProportion(successesA, result.N)
	result.B = NewProportion(successesB, result.N)
	if result.N == 0 {
		return result, nil
	}
	result.Difference = float64(result.ATrueBFalse-result.AFalseBTrue) / float64(result.N)
	if discordant := result.ATrueBFalse + result.AFalseBTrue; discordant > 0 {
		if interval, ok := ClopperPearson(result.ATrueBFalse, discordant); ok {
			scale := float64(discordant) / float64(result.N)
			low := math.Max(-1, scale*(2*interval.Low-1))
			high := math.Min(1, scale*(2*interval.High-1))
			result.DifferenceCI95 = &Interval{Low: low, High: high}
		}
	}
	result.McNemarP = ExactMcNemar(result.ATrueBFalse, result.AFalseBTrue)
	return result, nil
}

// ClopperPearson returns the exact two-sided 95% interval for a binomial
// proportion. It is used for the discordant proportion of the paired test.
func ClopperPearson(successes, total int) (Interval, bool) {
	if total <= 0 || successes < 0 || successes > total {
		return Interval{}, false
	}
	alpha := 0.025
	low := 0.0
	if successes > 0 {
		low = solveBinomialLower(successes, total, alpha)
	}
	high := 1.0
	if successes < total {
		high = solveBinomialUpper(successes, total, alpha)
	}
	return Interval{Low: low, High: high}, true
}

// binomialPMF evaluates the binomial probability mass with a log-gamma
// formulation so the tails stay stable for the sample sizes used here.
func binomialPMF(k, n int, p float64) float64 {
	if k < 0 || k > n {
		return 0
	}
	switch {
	case p <= 0:
		if k == 0 {
			return 1
		}
		return 0
	case p >= 1:
		if k == n {
			return 1
		}
		return 0
	}
	logPMF := lgamma(float64(n+1)) - lgamma(float64(k+1)) - lgamma(float64(n-k+1)) +
		float64(k)*math.Log(p) + float64(n-k)*math.Log1p(-p)
	return math.Exp(logPMF)
}

func lgamma(x float64) float64 {
	value, _ := math.Lgamma(x)
	return value
}

func binomialTailAtLeast(k, n int, p float64) float64 {
	tail := 0.0
	for i := k; i <= n; i++ {
		tail += binomialPMF(i, n, p)
	}
	return math.Min(1, tail)
}

func binomialTailAtMost(k, n int, p float64) float64 {
	tail := 0.0
	for i := 0; i <= k; i++ {
		tail += binomialPMF(i, n, p)
	}
	return math.Min(1, tail)
}

// solveBinomialLower finds the p where P(X>=k) = target. The upper tail is
// monotone increasing in p, so a bisection converges to the Clopper-Pearson
// lower bound.
func solveBinomialLower(k, n int, target float64) float64 {
	low, high := 0.0, 1.0
	for i := 0; i < 80; i++ {
		mid := (low + high) / 2
		if binomialTailAtLeast(k, n, mid) < target {
			low = mid
		} else {
			high = mid
		}
	}
	return (low + high) / 2
}

// solveBinomialUpper finds the p where P(X<=k) = target. The lower tail is
// monotone decreasing in p.
func solveBinomialUpper(k, n int, target float64) float64 {
	low, high := 0.0, 1.0
	for i := 0; i < 80; i++ {
		mid := (low + high) / 2
		if binomialTailAtMost(k, n, mid) > target {
			low = mid
		} else {
			high = mid
		}
	}
	return (low + high) / 2
}

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package routingbench

import (
	"math"
	"math/rand/v2"
	"sort"
)

// zFor returns the two-sided normal quantile for a confidence level such as
// 0.95. It uses the Acklam approximation of the inverse normal CDF, accurate to
// about 1e-9, so no table of levels limits what a benchmark can ask for.
func zFor(confidence float64) float64 {
	if confidence <= 0 || confidence >= 1 {
		confidence = 0.95
	}
	return invNormal(1 - (1-confidence)/2)
}

func invNormal(p float64) float64 {
	a := [...]float64{-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02, 1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00}
	b := [...]float64{-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02, 6.680131188771972e+01, -1.328068155288572e+01}
	c := [...]float64{-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00, -2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00}
	d := [...]float64{7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00, 3.754408661907416e+00}
	const low, high = 0.02425, 1 - 0.02425
	switch {
	case p < low:
		q := math.Sqrt(-2 * math.Log(p))
		return (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) / ((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	case p > high:
		q := math.Sqrt(-2 * math.Log(1-p))
		return -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) / ((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
	q := p - 0.5
	r := q * q
	return (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q / (((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
}

// Interval is a confidence interval.
type Interval struct{ Lo, Hi float64 }

// Contains reports whether v lies within the interval.
func (i Interval) Contains(v float64) bool { return v >= i.Lo && v <= i.Hi }

// Wilson returns the Wilson score interval for k successes in n trials at the
// given confidence. It stays inside [0, 1] and behaves at k = 0 and k = n,
// which a plain normal interval does not; with n = 0 it is the whole range.
func Wilson(k, n int, confidence float64) Interval {
	if n <= 0 {
		return Interval{0, 1}
	}
	z := zFor(confidence)
	nf, p := float64(n), float64(k)/float64(n)
	den := 1 + z*z/nf
	centre := (p + z*z/(2*nf)) / den
	half := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / den
	return Interval{math.Max(0, centre-half), math.Min(1, centre+half)}
}

// NewcombeDiff returns the interval for the difference p1 - p2 of two
// proportions from independent samples (k1 of n1, k2 of n2), by Newcombe's
// hybrid score method, which is built from the two Wilson intervals. It is the
// interval to use when comparing two separate groups, such as how often the
// dearer model wins on calls routed to it against calls routed away from it.
func NewcombeDiff(k1, n1, k2, n2 int, confidence float64) (diff float64, ci Interval) {
	if n1 <= 0 || n2 <= 0 {
		return 0, Interval{-1, 1}
	}
	p1, p2 := float64(k1)/float64(n1), float64(k2)/float64(n2)
	w1, w2 := Wilson(k1, n1, confidence), Wilson(k2, n2, confidence)
	d := p1 - p2
	lo := d - math.Sqrt((p1-w1.Lo)*(p1-w1.Lo)+(w2.Hi-p2)*(w2.Hi-p2))
	hi := d + math.Sqrt((w1.Hi-p1)*(w1.Hi-p1)+(p2-w2.Lo)*(p2-w2.Lo))
	return d, Interval{lo, hi}
}

// bootstrap draws resamples of n indices with replacement, using a generator
// seeded from seed so a given input always yields the same interval, and
// returns the percentile interval of stat over the resamples.
func bootstrap(n, resamples int, confidence float64, seed uint64, stat func(idx []int) (float64, bool)) Interval {
	if n == 0 || resamples <= 0 {
		return Interval{math.NaN(), math.NaN()}
	}
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	idx := make([]int, n)
	vals := make([]float64, 0, resamples)
	for range resamples {
		for i := range idx {
			idx[i] = rng.IntN(n)
		}
		if v, ok := stat(idx); ok {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		return Interval{math.NaN(), math.NaN()}
	}
	sort.Float64s(vals)
	alpha := (1 - confidence) / 2
	return Interval{percentile(vals, alpha), percentile(vals, 1-alpha)}
}

// percentile returns the q-quantile of sorted values by linear interpolation.
func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

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
	"testing"
)

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.4f, want %.4f (±%.4f)", name, got, want, tol)
	}
}

func TestZFor(t *testing.T) {
	near(t, "z(0.95)", zFor(0.95), 1.959964, 1e-5)
	near(t, "z(0.90)", zFor(0.90), 1.644854, 1e-5)
	near(t, "z(0.99)", zFor(0.99), 2.575829, 1e-5)
	near(t, "an invalid level falls back to 0.95", zFor(1.5), 1.959964, 1e-5)
}

// Reference values for the Wilson score interval at 95%.
func TestWilsonKnownValues(t *testing.T) {
	for _, tc := range []struct {
		k, n   int
		lo, hi float64
	}{
		{5, 10, 0.2366, 0.7634},
		{0, 10, 0.0000, 0.2775},
		{10, 10, 0.7225, 1.0000},
		{81, 263, 0.2553, 0.3662}, // Newcombe 1998, the first of his worked proportions
	} {
		got := Wilson(tc.k, tc.n, 0.95)
		near(t, "lo", got.Lo, tc.lo, 5e-4)
		near(t, "hi", got.Hi, tc.hi, 5e-4)
	}
	if got := Wilson(0, 0, 0.95); got.Lo != 0 || got.Hi != 1 {
		t.Errorf("no trials = %+v, want the whole range", got)
	}
}

// Newcombe 1998, Table II, the hybrid score interval (method 10) for 56/70
// against 48/80: difference 0.2, interval 0.0524 to 0.3339.
func TestNewcombeDiffKnownValue(t *testing.T) {
	d, ci := NewcombeDiff(56, 70, 48, 80, 0.95)
	near(t, "difference", d, 0.2, 1e-9)
	near(t, "lo", ci.Lo, 0.0524, 1e-3)
	near(t, "hi", ci.Hi, 0.3339, 1e-3)
	if _, ci := NewcombeDiff(1, 0, 1, 1, 0.95); ci.Lo != -1 || ci.Hi != 1 {
		t.Errorf("an empty group = %+v, want the whole range", ci)
	}
}

// The interval should cover the true difference about as often as it claims.
func TestNewcombeCoverage(t *testing.T) {
	const trials = 3000
	p1, p2, n := 0.6, 0.45, 60
	rng := seededRand(7)
	covered := 0
	for range trials {
		k1, k2 := 0, 0
		for range n {
			if rng.Float64() < p1 {
				k1++
			}
			if rng.Float64() < p2 {
				k2++
			}
		}
		if _, ci := NewcombeDiff(k1, n, k2, n, 0.95); ci.Contains(p1 - p2) {
			covered++
		}
	}
	if rate := float64(covered) / trials; rate < 0.93 || rate > 0.975 {
		t.Errorf("coverage = %.3f, want about 0.95", rate)
	}
}

func TestBootstrapIsSeededAndCoversTheMean(t *testing.T) {
	vals := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	mean := func(idx []int) (float64, bool) {
		s := 0.0
		for _, i := range idx {
			s += vals[i]
		}
		return s / float64(len(idx)), true
	}
	a := bootstrap(len(vals), 500, 0.95, 42, mean)
	b := bootstrap(len(vals), 500, 0.95, 42, mean)
	c := bootstrap(len(vals), 500, 0.95, 43, mean)
	if a != b {
		t.Errorf("same seed gave %+v and %+v", a, b)
	}
	if a == c {
		t.Error("a different seed gave the same interval")
	}
	if !a.Contains(5.5) || a.Hi-a.Lo > 5 {
		t.Errorf("interval %+v should surround the mean 5.5 and be narrower than the data", a)
	}
	if got := bootstrap(0, 500, 0.95, 1, mean); !math.IsNaN(got.Lo) {
		t.Errorf("no data = %+v, want NaN", got)
	}
}

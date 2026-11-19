package bandwidth

import (
	"math"
	"testing"
)

func TestWeight(t *testing.T) {
	tests := []struct {
		name              string
		seeders, leechers int
		want              float64
	}{
		{"aucun leecher", 10, 0, 0},
		{"essaim vide", 0, 0, 0},
		{"0 seeder", 0, 3, 3},           // 3×3/3
		{"1 leecher 0 seeder", 0, 1, 1}, // 1×1/1
		{"3 seeders 1 leecher", 3, 1, 0.25},
		{"1 seeder 3 leechers", 1, 3, 2.25},   // 9/4
		{"10 seeders 10 leechers", 10, 10, 5}, // 100/20
		{"négatifs ramenés à 0", -4, -2, 0},
		{"seeders négatifs", -4, 2, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Weight(tc.seeders, tc.leechers); math.Abs(got-tc.want) > 1e-12 {
				t.Fatalf("Weight(%d,%d) = %v, veut %v", tc.seeders, tc.leechers, got, tc.want)
			}
		})
	}
}

func TestFixedWeightExactForIntegers(t *testing.T) {
	if got := fixedWeight(0, 3); got != 3*weightScale {
		t.Fatalf("fixedWeight(0,3) = %d", got)
	}
	if got := fixedWeight(5, 0); got != 0 {
		t.Fatalf("sans leecher = %d", got)
	}
	// valeurs extrêmes : pas de panique ni de débordement
	_ = fixedWeight(math.MaxInt, math.MaxInt)
}

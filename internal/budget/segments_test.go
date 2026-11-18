package budget

import (
	"math/rand"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// fp and ip are declared in admit_test.go.

// row builds an observation at t0+min minutes.
func row(id int64, min float64, u float64, resets *int64) Obs {
	return Obs{ID: id, Pool: "p", Utilization: fp(u), ResetsAt: resets, Source: "test",
		ObservedAt: t0.Add(time.Duration(min * float64(time.Minute)))}
}

func ids(segs [][]Obs) [][]int64 {
	var out [][]int64
	for _, s := range segs {
		var r []int64
		for _, o := range s {
			r = append(r, o.ID)
		}
		out = append(out, r)
	}
	return out
}

func eqIDs(a, b [][]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

func TestBudgetSegmentsSawtooth(t *testing.T) {
	r0 := t0.Add(2 * time.Hour).Unix()
	tests := []struct {
		name string
		rows []Obs
		want [][]int64
	}{
		{"empty", nil, nil},
		{"rise only", []Obs{row(1, 0, .1, nil), row(2, 5, .2, nil), row(3, 10, .3, nil)}, [][]int64{{1, 2, 3}}},
		{"drop splits", []Obs{row(1, 0, .5, nil), row(2, 5, .6, nil), row(3, 10, .05, nil), row(4, 15, .1, nil)}, [][]int64{{1, 2}, {3, 4}}},
		{"drop of exactly eps does not split", []Obs{row(1, 0, .5, nil), row(2, 5, .49, nil)}, [][]int64{{1, 2}}},
		{"drop just over eps splits", []Obs{row(1, 0, .5, nil), row(2, 5, .48, nil)}, [][]int64{{1}, {2}}},
		{"resets_at jump > 60s splits", []Obs{row(1, 0, .5, ip(r0)), row(2, 5, .6, ip(r0+61))}, [][]int64{{1}, {2}}},
		{"resets_at jump of 60s does not", []Obs{row(1, 0, .5, ip(r0)), row(2, 5, .6, ip(r0+60))}, [][]int64{{1, 2}}},
		{"observed after resets_at splits", []Obs{row(1, 0, .5, ip(t0.Add(3*time.Minute).Unix())), row(2, 5, .6, nil)}, [][]int64{{1}, {2}}},
		{"unsorted input and nil utilisation ignored", []Obs{row(3, 10, .3, nil), {ID: 9, ObservedAt: t0.Add(7 * time.Minute)}, row(1, 0, .1, nil)}, [][]int64{{1, 3}}},
		{"ties by id", []Obs{row(2, 0, .2, nil), row(1, 0, .1, nil)}, [][]int64{{1, 2}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ids(Segments(tc.rows, 0.01))
			if !eqIDs(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

// TestBudgetSegmentsProperty: seeded sawtooth series split exactly at the
// drops and resets_at jumps; small drops never split; no BurnRate pair spans
// a split.
func TestBudgetSegmentsProperty(t *testing.T) {
	const eps = 0.01
	rng := rand.New(rand.NewSource(20261103))
	for c := 0; c < 600; c++ {
		var rows []Obs
		var wantSplits []int64 // id of the first row after each split
		id := int64(0)
		u := 0.5 + rng.Float64()*0.3
		min := 0.0
		reset := t0.Add(1000 * time.Hour).Unix() // far future
		useResets := rng.Intn(2) == 0
		n := 5 + rng.Intn(25)
		for i := 0; i < n; i++ {
			id++
			min += 1 + rng.Float64()*9
			kind := 0
			if i > 0 {
				kind = rng.Intn(6)
			}
			switch kind {
			case 1: // real drop, by > eps
				if d := eps + 0.005 + rng.Float64()*0.1; u-d >= 0 {
					u -= d
					wantSplits = append(wantSplits, id)
				}
			case 2: // tiny drop, never splits
				u -= rng.Float64() * eps * 0.99
			case 3: // resets_at jump
				if useResets {
					reset += 61 + int64(rng.Intn(1000))
					wantSplits = append(wantSplits, id)
				}
				u += rng.Float64() * 0.01
			default:
				u += rng.Float64() * 0.005
			}
			var ra *int64
			if useResets {
				v := reset
				ra = &v
			}
			rows = append(rows, row(id, min, u, ra))
		}
		shuffled := append([]Obs(nil), rows...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		segs := Segments(shuffled, eps)
		var gotSplits []int64
		for _, s := range segs[1:] {
			gotSplits = append(gotSplits, s[0].ID)
		}
		if len(gotSplits) != len(wantSplits) {
			t.Fatalf("case %d: splits %v want %v", c, gotSplits, wantSplits)
		}
		for i := range gotSplits {
			if gotSplits[i] != wantSplits[i] {
				t.Fatalf("case %d: splits %v want %v", c, gotSplits, wantSplits)
			}
		}
		// No pair used by BurnRate spans a split: with the split's first row
		// as the only row of its segment's pair set, the rate must equal the
		// sum over within-segment pairs only.
		now := rows[len(rows)-1].ObservedAt
		rate := BurnRate(segs, now, 1000*time.Hour, Fit{})
		within := 0
		for _, s := range segs {
			within += len(s) - 1
		}
		if rate.Pairs != within {
			t.Fatalf("case %d: pairs %d, within-segment pairs %d", c, rate.Pairs, within)
		}
	}
}

package localdecider

import (
	"math"
	"testing"
)

func unitVector(v ...float64) []float32 {
	var n float64
	for _, x := range v {
		n += x * x
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(x / math.Sqrt(n))
	}
	return out
}

// The expected probabilities are scikit-learn 1.9.1's, for
// LogisticRegression(C=10, class_weight="balanced") on the same data at a
// temperature of 0.5.
func TestFitMatchesScikitLearn(t *testing.T) {
	points := [][3]float64{{1, 0, 0}, {.9, .3, 0},
		{0, 1, 0}, {.2, .9, .1}, {0, .8, .5},
		{0, 0, 1}, {.1, 0, 1}, {0, .3, .9}, {.4, 0, .8}, {.3, .3, .8}, {.6, .2, .6}, {0, .6, .7}}
	x := make([][]float32, 0, len(points))
	for _, v := range points {
		x = append(x, unitVector(v[0], v[1], v[2]))
	}
	y := []int{0, 0, 1, 1, 1, 2, 2, 2, 2, 2, 2, 2}
	c := fit(x, y, 3)
	for i, tc := range []struct {
		q    []float32
		want [3]float64
	}{
		{unitVector(1, 0, 0), [3]float64{0.996899, 0.000327, 0.002774}},
		{unitVector(.5, .5, .5), [3]float64{0.116958, 0.190485, 0.692557}},
		{unitVector(0, 1, .2), [3]float64{0.000675, 0.997141, 0.002185}},
		{unitVector(.7, 0, .7), [3]float64{0.073579, 0.000766, 0.925655}},
	} {
		got := softmax(c.scores(tc.q), 0.5)
		for k := range got {
			if math.Abs(got[k]-tc.want[k]) > 2e-3 {
				t.Errorf("query %d: got %.6f, want %.6f", i, got, tc.want)
				break
			}
		}
	}
}

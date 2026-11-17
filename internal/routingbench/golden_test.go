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
	"fmt"
	"strings"
	"testing"
)

// goldenGroup is a fixed group of judged pairs with no randomness of its own.
func goldenGroup() Group {
	g := Group{GroupKey: GroupKey{Agent: "a", Category: "c", Capable: "big", Efficient: "small"}, RoutedCapableCalls: 100, WindowDays: 7}
	for i := range 45 {
		res := Equal
		switch i % 5 {
		case 0, 1:
			res = CapableBetter
		case 2:
			res = EfficientBetter
		}
		g.Pairs = append(g.Pairs, Pair{
			Result: res, RoutedCapable: i%3 == 0,
			CapableCostUSD: 0.01 + float64(i%4)*0.002, EfficientCostUSD: 0.001 + float64(i%3)*0.0005,
		})
	}
	return g
}

// TestGoldenStatistics pins the exact outputs of the statistics the benchmark
// rests on, so moving them into a shared package cannot change a finding.
func TestGoldenStatistics(t *testing.T) {
	var b strings.Builder
	for _, c := range [][2]int{{0, 0}, {0, 7}, {7, 7}, {13, 40}, {81, 263}} {
		w := Wilson(c[0], c[1], 0.95)
		fmt.Fprintf(&b, "wilson %d/%d %.12g %.12g\n", c[0], c[1], w.Lo, w.Hi)
	}
	w90 := Wilson(13, 40, 0.9)
	fmt.Fprintf(&b, "wilson90 %.12g %.12g\n", w90.Lo, w90.Hi)
	fmt.Fprintf(&b, "wilson-bad-confidence %.12g\n", Wilson(13, 40, 2).Lo)
	d, ci := NewcombeDiff(56, 70, 48, 80, 0.95)
	fmt.Fprintf(&b, "newcombe %.12g %.12g %.12g\n", d, ci.Lo, ci.Hi)
	vals := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 30}
	mean := func(idx []int) (float64, bool) {
		s := 0.0
		for _, i := range idx {
			s += vals[i]
		}
		return s / float64(len(idx)), true
	}
	bs := bootstrap(len(vals), 500, 0.95, 42, mean)
	fmt.Fprintf(&b, "bootstrap %.12g %.12g\n", bs.Lo, bs.Hi)
	bs = bootstrap(len(vals), 300, 0.9, 7, mean)
	fmt.Fprintf(&b, "bootstrap90 %.12g %.12g\n", bs.Lo, bs.Hi)
	w := Evaluate(goldenGroup(), Options{})
	fmt.Fprintf(&b, "worth n=%d net=%.12g dcost=%.12g netCI=%.12g,%.12g costCI=%.12g,%.12g\n",
		w.N, w.NetWin, w.DeltaCostUSD, w.NetWinCI.Lo, w.NetWinCI.Hi, w.DeltaCostUSDCI.Lo, w.DeltaCostUSDCI.Hi)
	fmt.Fprintf(&b, "lift %.12g %.12g %.12g\n", *w.RoutingLift, w.RoutingLiftCI.Lo, w.RoutingLiftCI.Hi)
	for _, f := range Detect(thresholdInput(scoreCalls("a", 200, 0.30))) {
		e := f.Evidence
		fmt.Fprintf(&b, "finding %s %s n=%d dcost=%.9g loss=%.9g lossHi=%.9g %.2f->%.2f frontier=%d\n",
			f.Detector, f.Summary, e.Samples, e.DeltaCostUSD, e.LossChange, e.LossChangeHi, e.FromThreshold, e.ToThreshold, len(e.Frontier))
	}
	if got := b.String(); got != goldenStatistics {
		t.Errorf("statistics changed.\ngot:\n%s\nwant:\n%s", got, goldenStatistics)
	}
}

const goldenStatistics = `wilson 0/0 0 1
wilson 0/7 0 0.354330435436
wilson 7/7 0.645669564564 1
wilson 13/40 0.200845022237 0.479822538228
wilson 81/263 0.255288519838 0.366209577031
wilson90 0.217676207482 0.454497499498
wilson-bad-confidence 0.200845022237
newcombe 0.2 0.0524314722813 0.333872654137
bootstrap 4.225 13.1386363636
bootstrap90 4.63181818182 12
worth n=45 net=0.2 dcost=0.0114333333333 netCI=-0.0222222222222,0.4 costCI=0.0107441666667,0.0121222222222
lift 0 -0.268257454412 0.287343869288
finding threshold Moving sel's base threshold from 0.50 to 0.30 would have cost $0.0018 less per call (at most $0.0023 less, at least $0.0013) across 200 judged calls, with no more than 0% more calls routed to the model whose answer lost n=200 dcost=-0.0018 loss=0 lossHi=0 0.50->0.30 frontier=7
`

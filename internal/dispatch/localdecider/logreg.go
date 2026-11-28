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

package localdecider

import "math"

// A multinomial logistic regression with an L2 penalty, fitted with L-BFGS. It
// matches scikit-learn's LogisticRegression(C=10, class_weight="balanced"),
// which the settings in hack/dispatch-eval/bench were measured with. The fit
// is small: a few hundred vectors for a tenant's agents.
const (
	inverseRegularization = 10.0 // C
	maxIterations         = 500
	gradientTolerance     = 1e-4
	history               = 10
)

// classifier scores a vector against each class: weights[c] holds one weight
// per dimension and then the intercept.
type classifier struct{ weights [][]float64 }

func (c classifier) scores(v []float32) []float64 {
	out := make([]float64, len(c.weights))
	for k, w := range c.weights {
		s := w[len(v)]
		for i, x := range v {
			s += w[i] * float64(x)
		}
		out[k] = s
	}
	return out
}

type problem struct {
	x       [][]float32
	y       []int
	weight  []float64 // per sample
	total   float64   // sum of weight
	classes int
	width   int // dimensions plus the intercept
}

// loss returns the penalised mean cross-entropy at w and writes its gradient.
func (p *problem) loss(w, grad []float64) float64 {
	clear(grad)
	var f float64
	z := make([]float64, p.classes)
	for i, x := range p.x {
		for k := range z {
			row := w[k*p.width : (k+1)*p.width]
			s := row[len(x)]
			for j, v := range x {
				s += row[j] * float64(v)
			}
			z[k] = s
		}
		total := logSumExp(z)
		f += p.weight[i] * (total - z[p.y[i]])
		for k := range z {
			g := math.Exp(z[k] - total)
			if k == p.y[i] {
				g--
			}
			g *= p.weight[i]
			row := grad[k*p.width : (k+1)*p.width]
			for j, v := range x {
				row[j] += g * float64(v)
			}
			row[len(x)] += g
		}
	}
	// The intercept is not penalised.
	for i, v := range w {
		if i%p.width != p.width-1 {
			f += v * v / (2 * inverseRegularization)
			grad[i] += v / inverseRegularization
		}
	}
	for i := range grad {
		grad[i] /= p.total
	}
	return f / p.total
}

// fit trains on x, where y[i] is the class of x[i]. Every class weighs the same
// however many samples it has: otherwise an agent with many examples takes
// requests from one with few.
func fit(x [][]float32, y []int, classes int) classifier {
	count := make([]int, classes)
	for _, c := range y {
		count[c]++
	}
	p := &problem{x: x, y: y, classes: classes, width: len(x[0]) + 1, weight: make([]float64, len(x))}
	for i, c := range y {
		p.weight[i] = float64(len(y)) / (float64(classes) * float64(count[c]))
		p.total += p.weight[i]
	}
	w := minimise(p.loss, make([]float64, classes*p.width))
	out := classifier{weights: make([][]float64, classes)}
	for k := range out.weights {
		out.weights[k] = w[k*p.width : (k+1)*p.width]
	}
	return out
}

// minimise is L-BFGS with a backtracking line search, for a smooth convex loss.
func minimise(loss func(w, grad []float64) float64, w []float64) []float64 {
	n := len(w)
	grad := make([]float64, n)
	f := loss(w, grad)
	var steps, changes [][]float64
	next, nextGrad := make([]float64, n), make([]float64, n)
	for range maxIterations {
		if largest(grad) <= gradientTolerance {
			break
		}
		dir := direction(grad, steps, changes)
		slope := dot(grad, dir)
		if slope >= 0 { // the history no longer points downhill
			steps, changes = nil, nil
			dir = direction(grad, nil, nil)
			slope = dot(grad, dir)
		}
		step, fNext := 1.0, 0.0
		for {
			for i := range next {
				next[i] = w[i] + step*dir[i]
			}
			fNext = loss(next, nextGrad)
			if fNext <= f+1e-4*step*slope {
				break
			}
			if step *= 0.5; step < 1e-12 {
				return w
			}
		}
		s, d := make([]float64, n), make([]float64, n)
		for i := range s {
			s[i], d[i] = next[i]-w[i], nextGrad[i]-grad[i]
		}
		if dot(s, d) > 1e-12 {
			steps, changes = append(steps, s), append(changes, d)
			if len(steps) > history {
				steps, changes = steps[1:], changes[1:]
			}
		}
		copy(w, next)
		copy(grad, nextGrad)
		f = fNext
	}
	return w
}

// direction is the L-BFGS two-loop recursion.
func direction(grad []float64, steps, changes [][]float64) []float64 {
	q := make([]float64, len(grad))
	for i, g := range grad {
		q[i] = -g
	}
	alpha := make([]float64, len(steps))
	for i := len(steps) - 1; i >= 0; i-- {
		alpha[i] = dot(steps[i], q) / dot(steps[i], changes[i])
		for j := range q {
			q[j] -= alpha[i] * changes[i][j]
		}
	}
	if last := len(steps) - 1; last >= 0 {
		scale := dot(steps[last], changes[last]) / dot(changes[last], changes[last])
		for j := range q {
			q[j] *= scale
		}
	}
	for i := range steps {
		beta := dot(changes[i], q) / dot(steps[i], changes[i])
		for j := range q {
			q[j] += steps[i][j] * (alpha[i] - beta)
		}
	}
	return q
}

func dot(a, b []float64) float64 {
	var s float64
	for i, v := range a {
		s += v * b[i]
	}
	return s
}

func largest(v []float64) float64 {
	var m float64
	for _, x := range v {
		m = max(m, math.Abs(x))
	}
	return m
}

func logSumExp(z []float64) float64 {
	m := z[0]
	for _, v := range z[1:] {
		m = max(m, v)
	}
	var s float64
	for _, v := range z {
		s += math.Exp(v - m)
	}
	return m + math.Log(s)
}

// softmax turns scores into probabilities at a temperature.
func softmax(scores []float64, temperature float64) []float64 {
	z := make([]float64, len(scores))
	for i, s := range scores {
		z[i] = s / temperature
	}
	total := logSumExp(z)
	for i := range z {
		z[i] = math.Exp(z[i] - total)
	}
	return z
}

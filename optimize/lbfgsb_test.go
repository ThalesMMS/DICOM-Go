package optimize

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"reflect"
	"sort"
	"sync"
	"testing"
	"unsafe"
)

type boundedQuadratic struct {
	q         [][]float64
	b, lo, hi []float64
	gradient  []float64
	calls     int
	invalid   bool
	cancel    context.CancelFunc
}

func (f *boundedQuadratic) Value(x []float64) (float64, error) {
	v, _, err := f.ValueAndGradient(x)
	return v, err
}
func (f *boundedQuadratic) ValueAndGradient(x []float64) (float64, []float64, error) {
	f.calls++
	if f.cancel != nil && f.calls == 3 {
		f.cancel()
	}
	if f.gradient == nil {
		f.gradient = make([]float64, len(x))
	}
	var value float64
	for i := range x {
		if math.IsNaN(x[i]) || math.IsInf(x[i], 0) || f.lo != nil && x[i] < f.lo[i] || f.hi != nil && x[i] > f.hi[i] {
			f.invalid = true
		}
		var qx float64
		for j := range x {
			qx += f.q[i][j] * x[j]
		}
		f.gradient[i] = qx - f.b[i]
		value += .5*x[i]*qx - f.b[i]*x[i]
	}
	return value, f.gradient, nil
}
func boundedNear(t *testing.T, got, want []float64, tolerance float64) {
	t.Helper()
	for i := range want {
		if math.Abs(got[i]-want[i]) > tolerance {
			t.Fatalf("component %d: %.17g want %.17g (tol %g)", i, got[i], want[i], tolerance)
		}
	}
}

// Independent dense Cholesky solver for tiny SPD test oracles.
func boundedDenseSolve(a [][]float64, b []float64) []float64 {
	n := len(b)
	l := make([][]float64, n)
	for i := range l {
		l[i] = make([]float64, n)
		for j := 0; j <= i; j++ {
			v := a[i][j]
			for k := 0; k < j; k++ {
				v -= l[i][k] * l[j][k]
			}
			if i == j {
				l[i][j] = math.Sqrt(v)
			} else {
				l[i][j] = v / l[j][j]
			}
		}
	}
	out := append([]float64(nil), b...)
	for i := range out {
		for j := 0; j < i; j++ {
			out[i] -= l[i][j] * out[j]
		}
		out[i] /= l[i][i]
	}
	for i := n - 1; i >= 0; i-- {
		for j := i + 1; j < n; j++ {
			out[i] -= l[j][i] * out[j]
		}
		out[i] /= l[i][i]
	}
	return out
}

// Exhaust every active face, solve its unconstrained SPD quadratic, then keep
// only feasible KKT points. This is not the product's iterative algorithm.
func boundedFaceOracle(q [][]float64, b, lo, hi []float64) []float64 {
	n := len(b)
	faces := 1
	for range b {
		faces *= 3
	}
	best := math.Inf(1)
	var answer []float64
	for face := 0; face < faces; face++ {
		x := make([]float64, n)
		state := make([]int, n)
		var free []int
		code := face
		valid := true
		for i := range x {
			state[i] = code % 3
			code /= 3
			switch state[i] {
			case 0:
				free = append(free, i)
			case 1:
				x[i] = lo[i]
			case 2:
				x[i] = hi[i]
			}
			if math.IsInf(x[i], 0) {
				valid = false
			}
		}
		if !valid {
			continue
		}
		a := make([][]float64, len(free))
		rhs := make([]float64, len(free))
		for ii, i := range free {
			a[ii] = make([]float64, len(free))
			rhs[ii] = b[i]
			for j := range x {
				if state[j] != 0 {
					rhs[ii] -= q[i][j] * x[j]
				}
			}
			for jj, j := range free {
				a[ii][jj] = q[i][j]
			}
		}
		solution := boundedDenseSolve(a, rhs)
		for ii, i := range free {
			x[i] = solution[ii]
		}
		var value float64
		for i := range x {
			if x[i] < lo[i]-1e-10 || x[i] > hi[i]+1e-10 || math.IsNaN(x[i]) {
				valid = false
			}
			var qx float64
			for j := range x {
				qx += q[i][j] * x[j]
			}
			g := qx - b[i]
			if state[i] == 0 && math.Abs(g) > 1e-9 || state[i] == 1 && g < -1e-9 || state[i] == 2 && g > 1e-9 {
				valid = false
			}
			value += .5*x[i]*qx - b[i]*x[i]
		}
		if valid && value < best {
			best = value
			answer = x
		}
	}
	return answer
}

func TestLBFGSBCoupledActiveBoundsAndOwnership(t *testing.T) {
	lo, hi, x0 := []float64{0, -1}, []float64{1, 1}, []float64{100, -100}
	originalLo, originalHi, originalX := append([]float64(nil), lo...), append([]float64(nil), hi...), append([]float64(nil), x0...)
	f := &boundedQuadratic{q: [][]float64{{4, 1}, {1, 2}}, b: []float64{6, 1}, lo: lo, hi: hi}
	r, err := (LBFGSB{LowerBounds: lo, UpperBounds: hi, ProjectedGradientTolerance: 1e-12, ValueTolerance: 1e-16}).Minimize(f, x0)
	if err != nil {
		t.Fatal(err)
	}
	boundedNear(t, r.X, []float64{1, 0}, 1e-10)
	if r.Stop != StopProjectedGradient || f.invalid || f.calls != r.Evaluations {
		t.Fatalf("result %+v calls%d invalid%v", r, f.calls, f.invalid)
	}
	if !reflect.DeepEqual(lo, originalLo) || !reflect.DeepEqual(hi, originalHi) || !reflect.DeepEqual(x0, originalX) {
		t.Fatal("source changed")
	}
	// Nonzero raw gradient is correct at a constrained optimum.
	_, g, _ := f.ValueAndGradient(r.X)
	boundedNear(t, g, []float64{-2, 0}, 1e-10)
}
func TestLBFGSBIndependentActiveFaceOracle(t *testing.T) {
	random := rand.New(rand.NewSource(1344))
	for n := 2; n <= 6; n++ {
		for sample := 0; sample < 4; sample++ {
			a := make([][]float64, n)
			q := make([][]float64, n)
			for i := range a {
				a[i] = make([]float64, n)
				q[i] = make([]float64, n)
				for j := range a[i] {
					a[i][j] = random.Float64() - .5
				}
			}
			for i := range q {
				for j := range q[i] {
					for k := 0; k < n; k++ {
						q[i][j] += a[k][i] * a[k][j]
					}
					if i == j {
						q[i][j] += 1
					}
				}
			}
			b, lo, hi, x0 := make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
			for i := range b {
				b[i] = 6*random.Float64() - 3
				lo[i] = -.5 - random.Float64()
				hi[i] = .2 + random.Float64()
				x0[i] = 4*random.Float64() - 2
			}
			if sample == 1 {
				lo[0], hi[0] = .1, .1
			}
			if sample == 2 {
				lo[0] = math.Inf(-1)
			}
			if sample == 3 {
				hi[0] = math.Inf(1)
			}
			want := boundedFaceOracle(q, b, lo, hi)
			if want == nil {
				t.Fatal("oracle failed")
			}
			f := &boundedQuadratic{q: q, b: b, lo: lo, hi: hi}
			r, err := (LBFGSB{LowerBounds: lo, UpperBounds: hi, ProjectedGradientTolerance: 1e-9, ValueTolerance: 1e-16, MaximumIterations: 400}).Minimize(f, x0)
			if err != nil {
				t.Fatalf("n%d sample%d: %v", n, sample, err)
			}
			t.Run(string(rune('0'+n))+string(rune('a'+sample)), func(t *testing.T) {
				boundedNear(t, r.X, want, 1e-7)
				if f.invalid {
					t.Fatal("infeasible callback")
				}
				vf, gradient, _ := f.ValueAndGradient(r.X)
				// Independent coordinate projection/KKT residual at the returned
				// point, rather than the most recently rejected trial.
				residual := 0.0
				for i, xi := range r.X {
					projected := xi - gradient[i]
					if projected < lo[i] {
						projected = lo[i]
					}
					if projected > hi[i] {
						projected = hi[i]
					}
					residual = math.Max(residual, math.Abs(xi-projected))
				}
				if residual > 1e-6 {
					t.Fatalf("residual%g stop%s", residual, r.Stop)
				}
				vo, _, _ := f.ValueAndGradient(want)
				if math.Abs(vf-vo) > 1e-9 {
					t.Fatalf("value gap %g", vf-vo)
				}
			})
		}
	}
}

func TestLBFGSBBoundedRosenbrock(t *testing.T) {
	r, err := (LBFGSB{LowerBounds: []float64{-2, -1}, UpperBounds: []float64{.8, 2}, Memory: 8, MaximumIterations: 500, ProjectedGradientTolerance: 1e-10, ValueTolerance: 1e-16}).Minimize(rosenbrock{}, []float64{-1.2, 1})
	if err != nil {
		t.Fatal(err)
	}
	boundedNear(t, r.X, []float64{.8, .64}, 1e-6)
	if math.Abs(r.Value-.04) > 1e-9 {
		t.Fatalf("value %.17g stop%s", r.Value, r.Stop)
	}
}

// Reconstruct the dense BFGS Hessian with full outer products, independently of
// the product's compact K and row operations.
func boundedDenseBFGS(s, y [][]float64) [][]float64 {
	n := len(s[0])
	sy, yy := 0.0, 0.0
	last := len(s) - 1
	for i := 0; i < n; i++ {
		sy += s[last][i] * y[last][i]
		yy += y[last][i] * y[last][i]
	}
	b := make([][]float64, n)
	for i := range b {
		b[i] = make([]float64, n)
		b[i][i] = yy / sy
	}
	for k := range s {
		bs := make([]float64, n)
		sy, sbs := 0.0, 0.0
		for i := 0; i < n; i++ {
			for j := 0; j < n; j++ {
				bs[i] += b[i][j] * s[k][j]
			}
			sy += s[k][i] * y[k][i]
			sbs += s[k][i] * bs[i]
		}
		for i := 0; i < n; i++ {
			for j := 0; j < n; j++ {
				b[i][j] += y[k][i]*y[k][j]/sy - bs[i]*bs[j]/sbs
			}
		}
	}
	return b
}
func boundedDenseCauchy(b [][]float64, x, g, lo, hi []float64) []float64 {
	times := []float64{0}
	for i := range x {
		if g[i] > 0 {
			times = append(times, (x[i]-lo[i])/g[i])
		} else if g[i] < 0 {
			times = append(times, (x[i]-hi[i])/g[i])
		}
	}
	sort.Float64s(times)
	times = append(times, math.Inf(1))
	t := 0.0
	for _, next := range times {
		if next <= t {
			continue
		}
		z, d := make([]float64, len(x)), make([]float64, len(x))
		for i := range x {
			p := math.Max(lo[i], math.Min(hi[i], x[i]-t*g[i]))
			z[i] = p - x[i]
			if p > lo[i] && p < hi[i] || p == lo[i] && g[i] < 0 || p == hi[i] && g[i] > 0 {
				d[i] = -g[i]
			}
		}
		first, second := 0.0, 0.0
		for i := range x {
			first += g[i] * d[i]
			for j := range x {
				first += d[i] * b[i][j] * z[j]
				second += d[i] * b[i][j] * d[j]
			}
		}
		if first >= 0 || second == 0 {
			break
		}
		dt := -first / second
		if t+dt <= next {
			t += dt
			break
		}
		t = next
	}
	out := make([]float64, len(x))
	for i := range out {
		out[i] = math.Max(lo[i], math.Min(hi[i], x[i]-t*g[i]))
	}
	return out
}
func TestLBFGSBCompactModelAndCauchyDenseOracle(t *testing.T) {
	s := [][]float64{{1, 0, 1}, {0, 1, 1}, {1, -1, 0}}
	q := [][]float64{{4, 1, .2}, {1, 3, .3}, {.2, .3, 2}}
	y := make([][]float64, len(s))
	m := newLBFGSBModel(context.Background(), 3, 3, make([]float64, 18))
	for k := range s {
		y[k] = make([]float64, 3)
		for i := 0; i < 3; i++ {
			for j := 0; j < 3; j++ {
				y[k][i] += q[i][j] * s[k][j]
			}
		}
		if err := m.add(s[k], y[k]); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.build(); err != nil {
		t.Fatal(err)
	}
	b := boundedDenseBFGS(s, y)
	v, got, want := []float64{1, -2, .5}, make([]float64, 3), make([]float64, 3)
	if err := m.product(v, got); err != nil {
		t.Fatal(err)
	}
	for i := range want {
		for j := range v {
			want[i] += b[i][j] * v[j]
		}
	}
	boundedNear(t, got, want, 1e-10)
	for _, g := range [][]float64{{4, 4, 1}, {-2, 3, -1}, {1, -1, 2}} {
		x := []float64{0, 0, 0}
		lo, hi := []float64{-.5, -.5, -.2}, []float64{.5, .5, .4}
		xc, d, active := make([]float64, 3), make([]float64, 3), make([]bool, 3)
		if err := m.cauchy(x, g, lo, hi, xc, d, active, make([]lbfgsbBreakpoint, 0, 3)); err != nil {
			t.Fatal(err)
		}
		boundedNear(t, xc, boundedDenseCauchy(b, x, g, lo, hi), 1e-10)
		before := append([]float64(nil), xc...)
		z, bz := make([]float64, 3), make([]float64, 3)
		if err := m.subspace(x, g, lo, hi, xc, z, bz, active); err != nil {
			t.Fatal(err)
		}
		var free []int
		for i := range x {
			if !active[i] {
				free = append(free, i)
			}
		}
		a := make([][]float64, len(free))
		rhs := make([]float64, len(free))
		for ii, i := range free {
			a[ii] = make([]float64, len(free))
			rhs[ii] = -g[i]
			for j := range x {
				rhs[ii] -= b[i][j] * (before[j] - x[j])
			}
			for jj, j := range free {
				a[ii][jj] = b[i][j]
			}
		}
		direction := boundedDenseSolve(a, rhs)
		alpha := 1.0
		for ii, i := range free {
			if direction[ii] > 0 {
				alpha = math.Min(alpha, (hi[i]-before[i])/direction[ii])
			} else if direction[ii] < 0 {
				alpha = math.Min(alpha, (lo[i]-before[i])/direction[ii])
			}
		}
		expected := append([]float64(nil), before...)
		for ii, i := range free {
			expected[i] += alpha * direction[ii]
		}
		boundedNear(t, xc, expected, 1e-10)
	}
	m.reset()
	if err := m.build(); err != nil {
		t.Fatal(err)
	}
	if err := m.product(v, got); err != nil {
		t.Fatal(err)
	}
	boundedNear(t, got, v, 0)
}
func TestLBFGSBAdmissionBudgetsCancelAndFixed(t *testing.T) {
	f := &boundedQuadratic{q: [][]float64{{2, 0}, {0, 2}}, b: []float64{3, 4}}
	c := LBFGSB{}
	bytes, err := c.WorkspaceBytes(2)
	if err != nil {
		t.Fatal(err)
	}
	c.Limits.MaxWorkspaceBytes = bytes - 1
	if _, err := c.Minimize(f, []float64{1, 2}); !errors.Is(err, ErrLBFGSBResourceLimit) || f.calls != 0 {
		t.Fatalf("admission %v calls%d", err, f.calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (LBFGSB{}).MinimizeContext(ctx, f, []float64{1, 2}); !errors.Is(err, context.Canceled) || f.calls != 0 {
		t.Fatal("cancel admission", err)
	}
	r, err := (LBFGSB{LowerBounds: []float64{1, 2}, UpperBounds: []float64{1, 2}, MaximumEvaluations: 1}).Minimize(f, []float64{100, -100})
	if err != nil || r.Stop != StopProjectedGradient || r.Evaluations != 1 {
		t.Fatalf("fixed %+v %v", r, err)
	}
	r, err = (LBFGSB{MaximumEvaluations: 2}).Minimize(rosenbrock{}, []float64{-1.2, 1})
	if err != nil || r.Evaluations > 2 || r.Stop != StopMaximumEvaluations {
		t.Fatalf("budget %+v %v", r, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	f = &boundedQuadratic{q: [][]float64{{2, 1}, {1, 3}}, b: []float64{4, -2}, cancel: cancel}
	r, err = (LBFGSB{InitialStepLength: .01}).MinimizeContext(ctx, f, []float64{10, -10})
	if !errors.Is(err, context.Canceled) || f.calls != 3 || r.X != nil {
		t.Fatalf("callback cancel %+v %v calls%d", r, err, f.calls)
	}
	for _, o := range []LBFGSB{{Memory: 65}, {MaximumIterations: -1}, {ProjectedGradientTolerance: math.NaN()}, {LowerBounds: []float64{2, 0}, UpperBounds: []float64{1, 1}}, {LowerBounds: []float64{math.Inf(1), 0}}, {UpperBounds: []float64{math.Inf(-1), 0}}, {LowerBounds: []float64{0}}} {
		if _, err := o.Minimize(f, []float64{0, 0}); !errors.Is(err, ErrOptions) {
			t.Fatalf("options %+v: %v", o, err)
		}
	}
	if n := testing.AllocsPerRun(5, func() {
		_, err = (LBFGSB{}).WorkspaceBytes(100)
		if err != nil {
			panic(err)
		}
	}); n != 0 {
		t.Fatalf("dry measure allocated %g", n)
	}
}

type boundedCallback struct {
	call func([]float64) (float64, []float64, error)
}

func (f boundedCallback) Value(x []float64) (float64, error)                       { v, _, err := f.call(x); return v, err }
func (f boundedCallback) ValueAndGradient(x []float64) (float64, []float64, error) { return f.call(x) }
func TestLBFGSBCallbackErrorsDomainAndOwnedResult(t *testing.T) {
	marker := errors.New("test callback failure")
	for _, tc := range []struct {
		name     string
		value    float64
		gradient []float64
		failure  error
		want     error
	}{
		{"nan", math.NaN(), []float64{0}, nil, ErrOptions},
		{"minus-inf", math.Inf(-1), []float64{0}, nil, ErrOptions},
		{"start-plus-inf", math.Inf(1), nil, nil, ErrOptions},
		{"gradient-length", 0, nil, nil, ErrOptions},
		{"gradient-nan", 0, []float64{math.NaN()}, nil, ErrOptions},
		{"gradient-inf", 0, []float64{math.Inf(1)}, nil, ErrOptions},
		{"callback", 0, nil, marker, marker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r, err := (LBFGSB{}).Minimize(boundedCallback{func([]float64) (float64, []float64, error) { calls++; return tc.value, tc.gradient, tc.failure }}, []float64{0})
			if !errors.Is(err, tc.want) || r.X != nil || calls != 1 {
				t.Fatalf("%+v %v calls%d", r, err, calls)
			}
		})
	}
	// Finite-domain rejected trials may omit a gradient. The line search must
	// shorten its step while staying within the user's box.
	calls, rejected := 0, 0
	shared := []float64{0}
	lo, hi := []float64{-2}, []float64{2}
	sourceLo, sourceHi := append([]float64(nil), lo...), append([]float64(nil), hi...)
	f := boundedCallback{func(x []float64) (float64, []float64, error) {
		calls++
		if x[0] < sourceLo[0] || x[0] > sourceHi[0] {
			t.Fatal("infeasible domain callback")
		}
		if calls == 1 {
			lo[0], hi[0] = 100, 101
		} // confirms the already-admitted snapshot
		if x[0] >= .9 {
			rejected++
			return math.Inf(1), nil, nil
		}
		shared[0] = 2 * (x[0] - .8)
		return (x[0] - .8) * (x[0] - .8), shared, nil
	}}
	r, err := (LBFGSB{LowerBounds: lo, UpperBounds: hi, InitialStepLength: 2, ProjectedGradientTolerance: 1e-10, ValueTolerance: 1e-16}).Minimize(f, []float64{0})
	if err != nil || rejected == 0 || calls != r.Evaluations || math.Abs(r.X[0]-.8) > 1e-10 {
		t.Fatalf("domain result %+v %v rejected%d calls%d", r, err, rejected, calls)
	}
	shared[0] = 100
	if r.X[0] != .8 || cap(r.X) != len(r.X) {
		t.Fatal("result retains callback or workspace storage")
	}
}
func TestLBFGSBWorkspaceCapacityAndConcurrentCalls(t *testing.T) {
	for _, n := range []int{1, 7, 2048} {
		for _, memory := range []int{1, 5, 64} {
			history := make([]float64, 2*memory*n)
			model := newLBFGSBModel(context.Background(), n, memory, history)
			// Count capacities of every distinct backing store, not every aliased
			// row. The run's 16 remaining vectors and independently owned Result.X
			// coexist with the model's history; breakpoints and bool flags add N.
			bytes := int64(8*(cap(history)+16*n+2*n)) + int64(n)*int64(unsafe.Sizeof(lbfgsbBreakpoint{})+unsafe.Sizeof(bool(false)))
			bytes += int64(8 * (cap(model.matrix) + cap(model.reduced) + cap(model.factor.data) + cap(model.reducedFactor.data)))
			bytes += int64(cap(model.s)+cap(model.y)) * int64(unsafe.Sizeof([]float64{}))
			bytes += int64(cap(model.factor.pivot)+cap(model.reducedFactor.pivot)) * int64(unsafe.Sizeof(int(0)))
			for _, work := range model.work {
				bytes += int64(cap(work)) * 8
			}
			bytes += int64(unsafe.Sizeof(*model)+unsafe.Sizeof(lbfgsbSearch{})) + 4096 // scalar/slice/closure/header reserve
			ceiling, err := (LBFGSB{Memory: memory, Limits: LBFGSBLimits{MaxWorkspaceBytes: 1 << 30}}).WorkspaceBytes(n)
			if err != nil || bytes > ceiling {
				t.Fatalf("N%d m%d capacities%d ceiling%d %v", n, memory, bytes, ceiling, err)
			}
			t.Logf("N%d m%d capacity+header count%d <= ceiling%d", n, memory, bytes, ceiling)
		}
	}
	lo, hi := []float64{-1, -1}, []float64{1, 1}
	o := LBFGSB{LowerBounds: lo, UpperBounds: hi}
	var group sync.WaitGroup
	for k := 0; k < 6; k++ {
		group.Add(1)
		go func() {
			defer group.Done()
			f := &boundedQuadratic{q: [][]float64{{4, 1}, {1, 2}}, b: []float64{6, 1}, lo: lo, hi: hi}
			r, err := o.Minimize(f, []float64{-1, 1})
			if err != nil || math.Abs(r.X[0]-1) > 1e-7 || math.Abs(r.X[1]) > 1e-7 {
				t.Errorf("concurrent %+v %v", r, err)
			}
		}()
	}
	group.Wait()
}

func TestLBFGSBNilFunctionAndContext(t *testing.T) {
	var f *boundedQuadratic
	if _, err := (LBFGSB{}).Minimize(f, []float64{0}); !errors.Is(err, ErrOptions) {
		t.Fatal("typed nil function", err)
	}
	if _, err := (LBFGSB{}).MinimizeContext(nil, rosenbrock{}, []float64{0, 0}); !errors.Is(err, ErrOptions) {
		t.Fatal("nil context", err)
	}
}

func TestLBFGSBProjectedResidualLargeCoordinates(t *testing.T) {
	x := []float64{1e16, -1e16, 1e16}
	g := []float64{1, -2, 100}
	lo := []float64{math.Inf(-1), math.Inf(-1), 1e16}
	hi := []float64{math.Inf(1), math.Inf(1), 1e16}
	residual, err := lbfgsbProjectedNorm(context.Background(), x, g, lo, hi)
	if err != nil || residual != 2 {
		t.Fatalf("projected residual lost gradient through subtraction: %g %v", residual, err)
	}
	linear := boundedCallback{func(x []float64) (float64, []float64, error) { return x[0], []float64{1}, nil }}
	r, err := (LBFGSB{}).Minimize(linear, []float64{1e16})
	if err != nil || r.Stop == StopProjectedGradient {
		t.Fatalf("unrepresentable step reported false KKT convergence: %+v %v", r, err)
	}
}

type boundedCheckedContext struct {
	context.Context
	check func()
}

func (c boundedCheckedContext) Err() error { c.check(); return c.Context.Err() }
func TestLBFGSBCancelInsideTiedBreakpoints(t *testing.T) {
	const n = 4096
	x, g, lo, hi, xc, d := make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	active := make([]bool, n)
	for i := range g {
		g[i] = -1
		hi[i] = .01
	}
	background, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := boundedCheckedContext{background, func() {
		if active[n/2] {
			cancel()
		}
	}}
	model := newLBFGSBModel(ctx, n, 1, make([]float64, 2*n))
	if err := model.build(); err != nil {
		t.Fatal(err)
	}
	err := model.cauchy(x, g, lo, hi, xc, d, active, make([]lbfgsbBreakpoint, 0, n))
	frozen := 0
	for _, a := range active {
		if a {
			frozen++
		}
	}
	if !errors.Is(err, context.Canceled) || frozen <= n/2 || frozen == n {
		t.Fatalf("tied-batch cancellation %v frozen%d/%d", err, frozen, n)
	}
}

// C1 polynomial controls distinguish accepted Wolfe state from budget fallback.
func TestLBFGSBWolfeAcceptedTrial(t *testing.T) {
	t.Run("offset-C1", func(t *testing.T) {
		f := boundedCallback{func(x []float64) (float64, []float64, error) { v := x[0] - 1; return 1e20 + v*v, []float64{2 * v}, nil }}
		r, err := (LBFGSB{}).Minimize(f, []float64{0})
		t.Logf("C1 quadratic offset result %+v err%v", r, err)
		if err != nil || r.X[0] != 1 || r.Stop != StopProjectedGradient {
			t.Fatalf("accepted strong-Wolfe point was discarded: %+v %v", r, err)
		}
	})
	t.Run("Hermite-C1", func(t *testing.T) {
		f := boundedCallback{func(x []float64) (float64, []float64, error) {
			v := x[0]
			return 200*v*v*v - 290*v*v - 10*v, []float64{600*v*v - 580*v - 10}, nil
		}}
		config, _ := (LBFGSB{}).resolve(context.Background(), 1)
		s := lbfgsbSearch{ctx: context.Background(), f: f, config: config, lo: []float64{math.Inf(-1)}, hi: []float64{math.Inf(1)}, trialX: make([]float64, 1), trialG: make([]float64, 1), bestX: make([]float64, 1), bestG: make([]float64, 1)}
		value, ok, err := s.run([]float64{0}, 0, -10, []float64{1}, 1)
		v, g, _ := f.ValueAndGradient(s.bestX)
		t.Logf("Hermite accepted x%.17g f%.17g g%.17g calls%d", s.bestX[0], value, s.bestG[0], s.evaluations)
		if err != nil || !ok || value != v || s.bestG[0] != g[0] || math.Abs(g[0]) > 9 {
			t.Fatalf("Wolfe contract lost value%g ok%v err%v", value, ok, err)
		}
	})
	t.Run("budget-Armijo-fallback", func(t *testing.T) {
		f := boundedCallback{func(x []float64) (float64, []float64, error) {
			v := x[0]
			return -10*v + v*v, []float64{-10 + 2*v}, nil
		}}
		config, _ := (LBFGSB{MaximumLineSearchEvaluations: 1}).resolve(context.Background(), 1)
		s := lbfgsbSearch{ctx: context.Background(), f: f, config: config, lo: []float64{math.Inf(-1)}, hi: []float64{math.Inf(1)}, trialX: make([]float64, 1), trialG: make([]float64, 1), bestX: make([]float64, 1), bestG: make([]float64, 1)}
		value, ok, err := s.run([]float64{0}, 0, -10, []float64{1}, .01)
		if err != nil || !ok || s.bestX[0] != .01 || value != -10*.01+.01*.01 || math.Abs(s.bestG[0]) <= 9 || s.evaluations != 1 {
			t.Fatalf("Armijo fallback changed value%g ok%v err%v point%v", value, ok, err, s.bestX)
		}
	})
}

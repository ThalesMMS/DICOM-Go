package optimize

import (
	"context"
	"errors"
	"math"
)

var errLBFGSBModel = errors.New("optimize: unusable L-BFGS-B numerical model")

// B = theta*I - W*K^-1*W^T, W = [Y theta*S], with the chronological
// correction pairs defining K = [-D L^T; L theta*S^T*S]. W is never stored.
type lbfgsbModel struct {
	ctx                   context.Context
	n, m, count, head     int
	theta                 float64
	s, y                  [][]float64
	matrix, reduced       []float64
	factor, reducedFactor lbfgsbLU
	work                  [12][]float64
}

func newLBFGSBModel(ctx context.Context, n, m int, history []float64) *lbfgsbModel {
	b := &lbfgsbModel{ctx: ctx, n: n, m: m, theta: 1, s: make([][]float64, m), y: make([][]float64, m), matrix: make([]float64, 4*m*m), reduced: make([]float64, 4*m*m), factor: newLBFGSBLU(ctx, 2*m), reducedFactor: newLBFGSBLU(ctx, 2*m)}
	for i := 0; i < m; i++ {
		b.s[i] = history[i*n : (i+1)*n : (i+1)*n]
		at := (m + i) * n
		b.y[i] = history[at : at+n : at+n]
	}
	small := make([]float64, 24*m)
	for i := range b.work {
		b.work[i] = small[i*2*m : (i+1)*2*m : (i+1)*2*m]
	}
	return b
}
func (b *lbfgsbModel) pair(i int) ([]float64, []float64) {
	j := (b.head + i) % b.m
	return b.s[j], b.y[j]
}
func (b *lbfgsbModel) reset() {
	b.count = 0
	b.head = 0
	b.theta = 1
	b.factor.n = 0
	b.reducedFactor.n = 0
}
func (b *lbfgsbModel) add(s, y []float64) error {
	sy, err := lbfgsbDot(b.ctx, s, y)
	if err != nil {
		if b.ctx.Err() != nil {
			return b.ctx.Err()
		}
		return nil
	}
	ss, err := lbfgsbDot(b.ctx, s, s)
	if err != nil {
		if b.ctx.Err() != nil {
			return b.ctx.Err()
		}
		return nil
	}
	yy, err := lbfgsbDot(b.ctx, y, y)
	if err != nil {
		if b.ctx.Err() != nil {
			return b.ctx.Err()
		}
		return nil
	}
	if !(sy > 1e-10*math.Sqrt(ss)*math.Sqrt(yy)) || !(yy > 0) || math.IsInf(yy/sy, 0) {
		return nil
	}
	at := (b.head + b.count) % b.m
	if b.count == b.m {
		at = b.head
		b.head = (b.head + 1) % b.m
	} else {
		b.count++
	}
	copy(b.s[at], s)
	copy(b.y[at], y)
	return nil
}
func (b *lbfgsbModel) build() error {
	b.theta = 1
	if b.count == 0 {
		return b.ctx.Err()
	}
	lastS, lastY := b.pair(b.count - 1)
	sy, err := lbfgsbDot(b.ctx, lastS, lastY)
	if err != nil {
		return err
	}
	yy, err := lbfgsbDot(b.ctx, lastY, lastY)
	if err != nil {
		return err
	}
	b.theta = yy / sy
	if !(b.theta > 0) || math.IsInf(b.theta, 0) {
		return errLBFGSBModel
	}
	k := 2 * b.count
	a := b.matrix[:k*k]
	clear(a)
	for i := 0; i < b.count; i++ {
		si, yi := b.pair(i)
		diagonal, err := lbfgsbDot(b.ctx, si, yi)
		if err != nil {
			return err
		}
		a[i*k+i] = -diagonal
		for j := 0; j < b.count; j++ {
			sj, yj := b.pair(j)
			if i > j {
				v, err := lbfgsbDot(b.ctx, si, yj)
				if err != nil {
					return err
				}
				a[(b.count+i)*k+j] = v
				a[j*k+b.count+i] = v
			}
			v, err := lbfgsbDot(b.ctx, si, sj)
			if err != nil {
				return err
			}
			a[(b.count+i)*k+b.count+j] = b.theta * v
		}
	}
	return b.factor.factor(a, k)
}
func (b *lbfgsbModel) row(index int, out []float64) {
	for j := 0; j < b.count; j++ {
		s, y := b.pair(j)
		out[j] = y[index]
		out[b.count+j] = b.theta * s[index]
	}
}
func (b *lbfgsbModel) transpose(v, out []float64) error {
	for j := 0; j < b.count; j++ {
		s, y := b.pair(j)
		u, err := lbfgsbDot(b.ctx, y, v)
		if err != nil {
			return err
		}
		out[j] = u
		u, err = lbfgsbDot(b.ctx, s, v)
		if err != nil {
			return err
		}
		out[b.count+j] = b.theta * u
	}
	return nil
}
func (b *lbfgsbModel) product(v, out []float64) error {
	k := 2 * b.count
	wt := b.work[8][:k]
	sol := b.work[9][:k]
	row := b.work[10][:k]
	if err := b.transpose(v, wt); err != nil {
		return err
	}
	if err := b.factor.solve(wt, sol); err != nil {
		return err
	}
	for i := range v {
		if i&1023 == 0 {
			if err := b.ctx.Err(); err != nil {
				return err
			}
		}
		b.row(i, row)
		var correction float64
		for j := range row {
			correction += row[j] * sol[j]
		}
		out[i] = b.theta*v[i] - correction
		if math.IsNaN(out[i]) || math.IsInf(out[i], 0) {
			return errLBFGSBModel
		}
	}
	return nil
}

// Partial pivoting is needed because K and its reduced update are indefinite,
// even though the represented full BFGS Hessian is positive definite.
type lbfgsbLU struct {
	ctx   context.Context
	data  []float64
	pivot []int
	n     int
}

func newLBFGSBLU(ctx context.Context, n int) lbfgsbLU {
	return lbfgsbLU{ctx: ctx, data: make([]float64, n*n), pivot: make([]int, n)}
}
func (l *lbfgsbLU) factor(a []float64, n int) error {
	l.n = n
	v := l.data[:n*n]
	copy(v, a)
	var scale float64
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return errLBFGSBModel
		}
		scale = math.Max(scale, math.Abs(x))
	}
	if n > 0 && !(scale > 0) {
		return errLBFGSBModel
	}
	for k := 0; k < n; k++ {
		if err := l.ctx.Err(); err != nil {
			return err
		}
		pivot := k
		for i := k + 1; i < n; i++ {
			if math.Abs(v[i*n+k]) > math.Abs(v[pivot*n+k]) {
				pivot = i
			}
		}
		if math.Abs(v[pivot*n+k]) <= 64*2.220446049250313e-16*scale {
			return errLBFGSBModel
		}
		l.pivot[k] = pivot
		if pivot != k {
			for j := 0; j < n; j++ {
				v[k*n+j], v[pivot*n+j] = v[pivot*n+j], v[k*n+j]
			}
		}
		for i := k + 1; i < n; i++ {
			v[i*n+k] /= v[k*n+k]
			for j := k + 1; j < n; j++ {
				v[i*n+j] -= v[i*n+k] * v[k*n+j]
			}
		}
	}
	return nil
}
func (l *lbfgsbLU) solve(rhs, out []float64) error {
	n := l.n
	if len(rhs) != n || len(out) != n {
		return errLBFGSBModel
	}
	copy(out, rhs)
	for k := 0; k < n; k++ {
		out[k], out[l.pivot[k]] = out[l.pivot[k]], out[k]
	}
	for i := 0; i < n; i++ {
		if err := l.ctx.Err(); err != nil {
			return err
		}
		for j := 0; j < i; j++ {
			out[i] -= l.data[i*n+j] * out[j]
		}
	}
	for i := n - 1; i >= 0; i-- {
		for j := i + 1; j < n; j++ {
			out[i] -= l.data[i*n+j] * out[j]
		}
		out[i] /= l.data[i*n+i]
		if math.IsNaN(out[i]) || math.IsInf(out[i], 0) {
			return errLBFGSBModel
		}
	}
	return nil
}

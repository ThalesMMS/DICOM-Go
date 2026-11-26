package optimize

import "math"

// The reduced Hessian is theta*I - Wf*K^-1*Wf^T. Woodbury gives the
// free-variable Newton direction using (K - Wf^T*Wf/theta), a 2m system.
func (b *lbfgsbModel) subspace(x, g, lo, hi, xc, z, bz []float64, active []bool) error {
	for i := range x {
		if i&1023 == 0 {
			if err := b.ctx.Err(); err != nil {
				return err
			}
		}
		z[i] = xc[i] - x[i]
	}
	if err := b.product(z, bz); err != nil {
		return err
	}
	k := 2 * b.count
	a := b.reduced[:k*k]
	copy(a, b.matrix[:k*k])
	rhs, sol, row := b.work[5][:k], b.work[6][:k], b.work[7][:k]
	clear(rhs)
	for i := range x {
		if i&1023 == 0 {
			if err := b.ctx.Err(); err != nil {
				return err
			}
		}
		z[i] = 0
		if active[i] {
			continue
		}
		r := g[i] + bz[i]
		bz[i] = r
		b.row(i, row)
		for j := range row {
			rhs[j] += row[j] * r
			for l := range row {
				a[j*k+l] -= row[j] * row[l] / b.theta
			}
		}
	}
	if k > 0 {
		if err := b.reducedFactor.factor(a, k); err != nil {
			return err
		}
		if err := b.reducedFactor.solve(rhs, sol); err != nil {
			return err
		}
	}
	alpha := 1.0
	for i := range x {
		if i&1023 == 0 {
			if err := b.ctx.Err(); err != nil {
				return err
			}
		}
		if active[i] {
			continue
		}
		b.row(i, row)
		correction := 0.0
		for j := range row {
			correction += row[j] * sol[j]
		}
		z[i] = -(bz[i] + correction/b.theta) / b.theta
		if math.IsNaN(z[i]) || math.IsInf(z[i], 0) {
			return errLBFGSBModel
		}
		if z[i] > 0 {
			alpha = math.Min(alpha, (hi[i]-xc[i])/z[i])
		} else if z[i] < 0 {
			alpha = math.Min(alpha, (lo[i]-xc[i])/z[i])
		}
	}
	if !(alpha >= 0) {
		return errLBFGSBModel
	}
	for i := range xc {
		if i&1023 == 0 {
			if err := b.ctx.Err(); err != nil {
				return err
			}
		}
		xc[i] = lbfgsbClamp(xc[i]+alpha*z[i], lo[i], hi[i])
	}
	return nil
}

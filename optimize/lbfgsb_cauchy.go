package optimize

import (
	"math"
	"sort"
)

type lbfgsbBreakpoint struct {
	time  float64
	index int
}

// cauchy follows the projected steepest-descent path. Direction/quadratic
// derivatives are updated at breakpoints using only compact rows (no N-vector
// sweep per breakpoint). Identical times are processed together.
func (b *lbfgsbModel) cauchy(x, g, lo, hi, xc, d []float64, active []bool, breaks []lbfgsbBreakpoint) error {
	breaks = breaks[:0]
	copy(xc, x)
	var gd, dd float64
	moving := 0
	for i := range x {
		if i&1023 == 0 {
			if err := b.ctx.Err(); err != nil {
				return err
			}
		}
		active[i] = lo[i] == hi[i] || x[i] == lo[i] && g[i] >= 0 || x[i] == hi[i] && g[i] <= 0
		d[i] = -g[i]
		if active[i] {
			d[i] = 0
		}
		if d[i] != 0 {
			moving++
			bound := hi[i]
			if d[i] < 0 {
				bound = lo[i]
			}
			t := (bound - x[i]) / d[i]
			if !math.IsInf(t, 1) {
				if math.IsNaN(t) || t < 0 {
					return errLBFGSBModel
				}
				breaks = append(breaks, lbfgsbBreakpoint{t, i})
			}
		}
		gd += g[i] * d[i]
		dd += d[i] * d[i]
	}
	if math.IsInf(gd, 0) || math.IsInf(dd, 0) {
		return errLBFGSBModel
	}
	sort.Slice(breaks, func(i, j int) bool {
		if breaks[i].time == breaks[j].time {
			return breaks[i].index < breaks[j].index
		}
		return breaks[i].time < breaks[j].time
	})
	if err := b.ctx.Err(); err != nil {
		return err
	}
	k := 2 * b.count
	p, c := b.work[0][:k], b.work[1][:k]
	sp, sc, row := b.work[2][:k], b.work[3][:k], b.work[4][:k]
	clear(c)
	if err := b.transpose(d, p); err != nil {
		return err
	}
	time, zd := 0.0, 0.0
	at := 0
	for dd > 0 {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		if err := b.factor.solve(p, sp); err != nil {
			return err
		}
		if err := b.factor.solve(c, sc); err != nil {
			return err
		}
		first, second := gd+b.theta*zd, b.theta*dd
		for j := range p {
			first -= p[j] * sc[j]
			second -= p[j] * sp[j]
		}
		if math.IsNaN(first) || math.IsInf(first, 0) || !(second > 0) || math.IsInf(second, 0) {
			return errLBFGSBModel
		}
		if first >= 0 {
			break
		}
		delta := -first / second
		next := math.Inf(1)
		if at < len(breaks) {
			next = breaks[at].time
		}
		if delta <= next-time {
			time += delta
			break
		}
		delta = next - time
		for j := range c {
			c[j] += delta * p[j]
		}
		zd += delta * dd
		time = next
		for at < len(breaks) && breaks[at].time == time {
			if at&1023 == 0 {
				if err := b.ctx.Err(); err != nil {
					return err
				}
			}
			i := breaks[at].index
			di := d[i]
			bound := hi[i]
			if di < 0 {
				bound = lo[i]
			}
			zi := bound - x[i]
			b.row(i, row)
			// Restore an exact bound when computing the breakpoint ratio rounded.
			correction := zi - time*di
			for j := range c {
				c[j] += correction * row[j]
				p[j] -= di * row[j]
			}
			zd -= time * di * di
			gd -= g[i] * di
			dd -= di * di
			xc[i] = bound
			d[i] = 0
			active[i] = true
			moving--
			at++
		}
		if moving == 0 {
			dd = 0
		} else if dd < 0 {
			return errLBFGSBModel
		}
	}
	for i := range x {
		if i&1023 == 0 {
			if err := b.ctx.Err(); err != nil {
				return err
			}
		}
		if !active[i] {
			xc[i] = lbfgsbClamp(x[i]+time*d[i], lo[i], hi[i])
			active[i] = xc[i] == lo[i] || xc[i] == hi[i]
		}
		if math.IsNaN(xc[i]) || math.IsInf(xc[i], 0) {
			return errLBFGSBModel
		}
	}
	return nil
}

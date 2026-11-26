package imagefilter

import "math/bits"

// Small reuse is intrinsically bounded. The larger exact selector polls
// partition work and falls back to cancellable heapsort after a finite depth;
// there is no adversarial quadratic quickselect trajectory.
func rankSelect(v []float32, k int, p *rankPoll) (float32, error) {
	if err := p.check(); err != nil {
		return 0, err
	}
	if len(v) <= 32 {
		return selectRank(v, k), nil
	}
	return rankSelectDepth(v, k, 2*bits.Len(uint(len(v))), p)
}
func rankSelectDepth(v []float32, k, depth int, p *rankPoll) (float32, error) {
	lo, hi := 0, len(v)-1
	for lo < hi {
		if depth == 0 {
			if err := rankHeapSort(v[lo:hi+1], p); err != nil {
				return 0, err
			}
			return v[k], nil
		}
		depth--
		mid := lo + (hi-lo)/2
		if v[mid] < v[lo] {
			v[mid], v[lo] = v[lo], v[mid]
		}
		if v[hi] < v[lo] {
			v[hi], v[lo] = v[lo], v[hi]
		}
		if v[hi] < v[mid] {
			v[hi], v[mid] = v[mid], v[hi]
		}
		pivot := v[mid]
		i, j := lo, hi
		for i <= j {
			for v[i] < pivot {
				if err := p.step(); err != nil {
					return 0, err
				}
				i++
			}
			for pivot < v[j] {
				if err := p.step(); err != nil {
					return 0, err
				}
				j--
			}
			if err := p.step(); err != nil {
				return 0, err
			}
			if i <= j {
				v[i], v[j] = v[j], v[i]
				i++
				j--
			}
		}
		switch {
		case k <= j:
			hi = j
		case k >= i:
			lo = i
		default:
			return v[k], p.check()
		}
	}
	return v[k], p.check()
}
func rankHeapSort(v []float32, p *rankPoll) error {
	sift := func(root, n int) error {
		for {
			if err := p.step(); err != nil {
				return err
			}
			child := 2*root + 1
			if child >= n {
				return nil
			}
			if child+1 < n && v[child] < v[child+1] {
				child++
			}
			if v[root] >= v[child] {
				return nil
			}
			v[root], v[child] = v[child], v[root]
			root = child
		}
	}
	for root := len(v)/2 - 1; root >= 0; root-- {
		if err := sift(root, len(v)); err != nil {
			return err
		}
	}
	for end := len(v) - 1; end > 0; end-- {
		v[0], v[end] = v[end], v[0]
		if err := sift(0, end); err != nil {
			return err
		}
	}
	return p.check()
}

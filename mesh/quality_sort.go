package mesh

// Stable, cancellable merge sort. Each comparison/copy consumes work; no
// uninterruptible library sort and no recursion/extra allocation are used.
func qualityMerge[T any](q *qualityRun, v, scratch []T, less func(T, T) bool) error {
	n := len(v)
	src, dst := v, scratch
	for width := 1; width < n; {
		for lo := 0; lo < n; lo += 2 * width {
			mid := lo + width
			if mid > n {
				mid = n
			}
			hi := mid + width
			if hi > n {
				hi = n
			}
			i, j := lo, mid
			for k := lo; k < hi; k++ {
				if err := q.step(); err != nil {
					return err
				}
				if i < mid && (j >= hi || !less(src[j], src[i])) {
					dst[k] = src[i]
					i++
				} else {
					dst[k] = src[j]
					j++
				}
			}
		}
		src, dst = dst, src
		if width > n/2 {
			break
		}
		width *= 2
	}
	for i := range v {
		if err := q.step(); err != nil {
			return err
		}
		v[i] = src[i]
	}
	return nil
}

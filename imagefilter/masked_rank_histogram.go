package imagefilter

// The histogram is the existing exact two-level median kernel; only its row
// admission/mask and changing order statistic are new. K>=2R admits sliding:
// first K reads plus 2R per later output do not exceed M*K per row. Clearing
// counts reads no source payload and is polled; no cross-line scratch leaks.
func (r *rankRequest) histogramLine(s *rankScratch, out *MaskedRankResult, line, y, z int) error {
	h := s.hist
	for i := range h.fine {
		if err := s.poll.step(); err != nil {
			return err
		}
		h.fine[i] = 0
	}
	for i := range h.coarse {
		h.coarse[i] = 0
	}
	h.med, h.below = 0, 0
	n := 0
	add := func(row rankRow, cx int, delta int32) error {
		if err := s.poll.step(); err != nil {
			return err
		}
		if r.opts.Boundary == ClipBoundary && (cx < 0 || cx >= r.src.Dims[0]) {
			return nil
		}
		j := row.base + clampAxis(cx, r.src.Dims[0])
		if !r.eligible(j) {
			return nil
		}
		b := int(r.src.Data[j] - r.lo)
		h.fine[b] += delta
		h.coarse[b>>8] += delta
		n += int(delta)
		if b < h.med {
			h.below += int(delta)
		}
		return nil
	}
	x0 := r.region.Origin[0]
	for _, row := range s.rows {
		if row.base < 0 {
			continue
		}
		for dx := -row.half; dx <= row.half; dx++ {
			if err := add(row, x0+dx, 1); err != nil {
				return err
			}
		}
	}
	for x := 0; x < r.region.Size[0]; x++ {
		if err := s.poll.step(); err != nil {
			return err
		}
		gx := x0 + x
		i := line*r.region.Size[0] + x
		good := r.opts.Centers == AnyValidNeighborhood || r.eligible((z*r.src.Dims[1]+y)*r.src.Dims[0]+gx)
		if good && n > 0 {
			// Existing lookup crosses at most 256 coarse and 512 fine bins. Poll
			// immediately before it, so this intrinsically bounded reuse stays short.
			if err := s.poll.check(); err != nil {
				return err
			}
			out.Values.Data[i], out.Valid[i] = rankZero(r.lo+float32(h.median(rankIndex(r.opts.Rank, n)))), true
		} else {
			out.Values.Data[i] = rankZero(r.opts.EmptyValue)
		}
		if x+1 < r.region.Size[0] {
			for _, row := range s.rows {
				if row.base < 0 {
					continue
				}
				if err := add(row, gx-row.half, -1); err != nil {
					return err
				}
				if err := add(row, gx+row.half+1, 1); err != nil {
					return err
				}
			}
		}
	}
	return s.poll.check()
}

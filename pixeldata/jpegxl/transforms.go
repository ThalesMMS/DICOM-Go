package jpegxl

// Modular transforms (ISO/IEC 18181-1 Annex H.6): channel bookkeeping when
// the header is read and the inverse transforms after decoding.

func checkEqualChannels(img *modularImage, begin, end int) error {
	if begin < 0 || end >= len(img.channels) || begin > end {
		return corrupt("transform channel range")
	}
	if begin < img.nbMeta && end >= img.nbMeta {
		return corrupt("transform mixes meta and image channels")
	}
	c0 := img.channels[begin]
	for i := begin + 1; i <= end; i++ {
		c := img.channels[i]
		if c.w != c0.w || c.h != c0.h || c.hshift != c0.hshift || c.vshift != c0.vshift {
			return corrupt("transform over unequal channels")
		}
	}
	return nil
}

func metaApply(img *modularImage, t *transformInfo) error {
	switch t.id {
	case transformRCT:
		return checkEqualChannels(img, t.beginC, t.beginC+2)
	case transformPalette:
		end := t.beginC + t.numC - 1
		if err := checkEqualChannels(img, t.beginC, end); err != nil {
			return err
		}
		if t.beginC >= img.nbMeta {
			img.nbMeta++
		} else {
			if img.nbMeta < 2-t.numC {
				return corrupt("palette of meta channels")
			}
			img.nbMeta += 2 - t.numC
		}
		img.channels = append(img.channels[:t.beginC+1], img.channels[end+1:]...)
		size := t.nbColours + t.nbDeltas
		if size*t.numC > 1<<26 {
			return corrupt("palette too large")
		}
		pal := newChannel(size, t.numC)
		pal.hshift, pal.vshift = -1, -1
		img.channels = append([]channel{pal}, img.channels...)
		return nil
	case transformSqueeze:
		return metaSqueeze(img, t)
	}
	return corrupt("invalid transform")
}

func defaultSqueeze(img *modularImage) []squeezeParams {
	var ps []squeezeParams
	first := img.nbMeta
	nb := len(img.channels) - first
	if nb <= 0 {
		return nil
	}
	w, h := img.channels[first].w, img.channels[first].h
	if nb > 2 && img.channels[first+1].w == w && img.channels[first+1].h == h {
		ps = append(ps, squeezeParams{horizontal: true, beginC: first + 1, numC: 2})
		ps = append(ps, squeezeParams{horizontal: false, beginC: first + 1, numC: 2})
	}
	p := squeezeParams{beginC: first, numC: nb, inPlace: true}
	const maxFirst = 8
	if w <= h && h > maxFirst {
		p.horizontal = false
		ps = append(ps, p)
		h = (h + 1) / 2
	}
	for w > maxFirst || h > maxFirst {
		if w > maxFirst {
			p.horizontal = true
			ps = append(ps, p)
			w = (w + 1) / 2
		}
		if h > maxFirst {
			p.horizontal = false
			ps = append(ps, p)
			h = (h + 1) / 2
		}
	}
	return ps
}

func metaSqueeze(img *modularImage, t *transformInfo) error {
	if len(t.squeezes) == 0 {
		t.squeezes = defaultSqueeze(img)
	}
	for _, p := range t.squeezes {
		begin, end := p.beginC, p.beginC+p.numC-1
		if begin < 0 || end >= len(img.channels) || p.numC <= 0 {
			return corrupt("squeeze channel range")
		}
		if begin < img.nbMeta {
			if end >= img.nbMeta {
				return corrupt("squeeze mixes meta and image channels")
			}
			if !p.inPlace {
				return corrupt("squeeze of meta channels must be in place")
			}
			img.nbMeta += p.numC
		}
		offset := len(img.channels)
		if p.inPlace {
			offset = end + 1
		}
		for c := begin; c <= end; c++ {
			ch := &img.channels[c]
			if ch.hshift > 30 || ch.vshift > 30 {
				return corrupt("too many squeezes")
			}
			w, h := ch.w, ch.h
			if w == 0 || h == 0 {
				return corrupt("squeezing an empty channel")
			}
			if p.horizontal {
				ch.w = (w + 1) / 2
				if ch.hshift >= 0 {
					ch.hshift++
				}
				w -= ch.w
			} else {
				ch.h = (h + 1) / 2
				if ch.vshift >= 0 {
					ch.vshift++
				}
				h -= ch.h
			}
			ch.data = make([]int32, ch.w*ch.h)
			res := newChannel(w, h)
			res.hshift, res.vshift = ch.hshift, ch.vshift
			pos := offset + (c - begin)
			img.channels = append(img.channels[:pos], append([]channel{res}, img.channels[pos:]...)...)
		}
	}
	return nil
}

func undoTransforms(img *modularImage, ts []transformInfo, wp wpHeader) error {
	for i := len(ts) - 1; i >= 0; i-- {
		t := ts[i]
		var err error
		switch t.id {
		case transformRCT:
			err = invRCT(img, t.beginC, t.rctType)
		case transformPalette:
			err = invPalette(img, t, wp)
		case transformSqueeze:
			err = invSqueeze(img, t.squeezes)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func invRCT(img *modularImage, m, rctType int) error {
	if m+2 >= len(img.channels) {
		return corrupt("RCT channel range")
	}
	perm := rctType / 7
	kind := rctType % 7
	c0, c1, c2 := img.channels[m], img.channels[m+1], img.channels[m+2]
	o0 := m + perm%3
	o1 := m + (perm+1+perm/3)%3
	o2 := m + (perm+2-perm/3)%3
	if kind == 0 {
		img.channels[o0], img.channels[o1], img.channels[o2] = c0, c1, c2
		return nil
	}
	n := len(c0.data)
	in0, in1, in2 := c0.data, c1.data, c2.data
	out0, out1, out2 := make([]int32, n), make([]int32, n), make([]int32, n)
	second, third := kind>>1, kind&1
	for i := 0; i < n; i++ {
		if kind == 6 {
			Y, Co, Cg := in0[i], in1[i], in2[i]
			tmp := Y - (Cg >> 1)
			G := Cg + tmp
			B := tmp - (Co >> 1)
			R := B + Co
			out0[i], out1[i], out2[i] = R, G, B
			continue
		}
		a, b, c := in0[i], in1[i], in2[i]
		if third == 1 {
			c += a
		}
		switch second {
		case 1:
			b += a
		case 2:
			b += (a + c) >> 1
		}
		out0[i], out1[i], out2[i] = a, b, c
	}
	img.channels[o0] = channel{w: c0.w, h: c0.h, hshift: c0.hshift, vshift: c0.vshift, data: out0}
	img.channels[o1] = channel{w: c0.w, h: c0.h, hshift: c0.hshift, vshift: c0.vshift, data: out1}
	img.channels[o2] = channel{w: c0.w, h: c0.h, hshift: c0.hshift, vshift: c0.vshift, data: out2}
	return nil
}

func scalePalette(v, bitDepth, denom int64) int64 {
	return (v * ((1 << uint(bitDepth)) - 1)) / denom
}

// paletteValue implements the implicit palette entries; negative indices
// (delta palette entries) are rejected as unsupported.
func paletteValue(pal *channel, index int, c int, bitDepth int) (int64, error) {
	size := pal.w
	switch {
	case index < 0:
		return 0, unsupported("delta palette entries")
	case index < size:
		return int64(pal.data[c*size+index]), nil
	case index < size+64:
		if c >= 3 {
			return 0, nil
		}
		i := (index - size) >> (2 * c)
		return scalePalette(int64(i%4), int64(bitDepth), 4) + int64(1)<<uint(max(0, bitDepth-3)), nil
	default:
		if c >= 3 {
			return 0, nil
		}
		i := index - size - 64
		for k := 0; k < c; k++ {
			i /= 5
		}
		return scalePalette(int64(i%5), int64(bitDepth), 4), nil
	}
}

func invPalette(img *modularImage, t transformInfo, wph wpHeader) error {
	if img.nbMeta < 1 || len(img.channels) < 2 {
		return corrupt("palette without palette channel")
	}
	nb := img.channels[0].h
	c0 := t.beginC + 1
	if c0 >= len(img.channels) || nb < 1 {
		return corrupt("palette channel range")
	}
	pal := img.channels[0]
	idx := img.channels[c0]
	w, h := idx.w, idx.h
	bitDepth := min(img.bitDepth, 24)
	out := make([]channel, nb)
	for c := range out {
		out[c] = channel{w: w, h: h, hshift: idx.hshift, vshift: idx.vshift, data: make([]int32, w*h)}
	}
	for c := 0; c < nb; c++ {
		data := out[c].data
		var wp *wpState
		if t.dPred == predWeighted && t.nbDeltas > 0 {
			wp = newWPState(wph, w)
		}
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				index := int(idx.data[y*w+x])
				pv, err := paletteValue(&pal, index, c, bitDepth)
				if err != nil {
					return err
				}
				v := pv
				if index < t.nbDeltas {
					v += predictAt(data, w, x, y, uint8(t.dPred), wp)
				}
				data[y*w+x] = int32(v)
				if wp != nil {
					wp.update(int64(data[y*w+x]), x, y)
				}
			}
		}
	}
	chans := append([]channel{}, img.channels[1:c0]...)
	chans = append(chans, out...)
	chans = append(chans, img.channels[c0+1:]...)
	img.channels = chans
	img.nbMeta--
	return nil
}

// predictAt evaluates a predictor without a tree (palette deltas).
func predictAt(data []int32, w, x, y int, p uint8, wp *wpState) int64 {
	var W, N, NW, NE, WW, NN, NEE int64
	switch {
	case x > 0:
		W = int64(data[y*w+x-1])
	case y > 0:
		W = int64(data[(y-1)*w+x])
	}
	if y > 0 {
		N = int64(data[(y-1)*w+x])
	} else {
		N = W
	}
	if x > 0 && y > 0 {
		NW = int64(data[(y-1)*w+x-1])
	} else {
		NW = W
	}
	if x+1 < w && y > 0 {
		NE = int64(data[(y-1)*w+x+1])
	} else {
		NE = N
	}
	if x > 1 {
		WW = int64(data[y*w+x-2])
	} else {
		WW = W
	}
	if y > 1 {
		NN = int64(data[(y-2)*w+x])
	} else {
		NN = N
	}
	if x+2 < w && y > 0 {
		NEE = int64(data[(y-1)*w+x+2])
	} else {
		NEE = NE
	}
	var wpPred int64
	if wp != nil {
		wpPred, _ = wp.predictWP(x, y, N, W, NE, NW, NN)
	}
	return predictValue(p, W, N, NW, NE, NN, WW, NEE, wpPred)
}

func smoothTendency(b, a, n int64) int64 {
	var diff int64
	if b >= a && a >= n {
		diff = (4*b - 3*n - a + 6) / 12
		if diff-(diff&1) > 2*(b-a) {
			diff = 2*(b-a) + 1
		}
		if diff+(diff&1) > 2*(a-n) {
			diff = 2 * (a - n)
		}
	} else if b <= a && a <= n {
		diff = (4*b - 3*n - a - 6) / 12
		if diff+(diff&1) < 2*(b-a) {
			diff = 2*(b-a) - 1
		}
		if diff-(diff&1) < 2*(a-n) {
			diff = 2 * (a - n)
		}
	}
	return diff
}

func invSqueeze(img *modularImage, ps []squeezeParams) error {
	for i := len(ps) - 1; i >= 0; i-- {
		p := ps[i]
		begin, end := p.beginC, p.beginC+p.numC-1
		offset := len(img.channels) + begin - end - 1
		if p.inPlace {
			offset = end + 1
		}
		if begin < img.nbMeta {
			img.nbMeta -= p.numC
		}
		for c := begin; c <= end; c++ {
			rc := offset + c - begin
			if rc >= len(img.channels) {
				return corrupt("squeeze residual channel")
			}
			avg, res := &img.channels[c], &img.channels[rc]
			if avg.w < res.w || avg.h < res.h {
				return corrupt("squeeze dimensions")
			}
			if p.horizontal {
				invHSqueeze(avg, res)
			} else {
				invVSqueeze(avg, res)
			}
		}
		img.channels = append(img.channels[:offset], img.channels[offset+p.numC:]...)
	}
	return nil
}

func invHSqueeze(avg, res *channel) {
	if res.w == 0 {
		avg.hshift--
		return
	}
	if avg.h != res.h {
		avg.hshift--
		return
	}
	ow := avg.w + res.w
	out := make([]int32, ow*avg.h)
	for y := 0; y < avg.h; y++ {
		pa := avg.data[y*avg.w : (y+1)*avg.w]
		pr := res.data[y*res.w : (y+1)*res.w]
		po := out[y*ow : (y+1)*ow]
		for x := 0; x < res.w; x++ {
			a := int64(pa[x])
			next := a
			if x+1 < avg.w {
				next = int64(pa[x+1])
			}
			left := a
			if x > 0 {
				left = int64(po[2*x-1])
			}
			diff := int64(pr[x]) + smoothTendency(left, a, next)
			A := a + diff/2
			po[2*x] = int32(A)
			po[2*x+1] = int32(A - diff)
		}
		if avg.w > res.w {
			po[2*res.w] = pa[res.w]
		}
	}
	avg.w = ow
	avg.data = out
	avg.hshift--
}

func invVSqueeze(avg, res *channel) {
	if res.h == 0 {
		avg.vshift--
		return
	}
	w := avg.w
	oh := avg.h + res.h
	out := make([]int32, w*oh)
	for y := 0; y < res.h; y++ {
		for x := 0; x < w; x++ {
			a := int64(avg.data[y*w+x])
			next := a
			if y+1 < avg.h {
				next = int64(avg.data[(y+1)*w+x])
			}
			top := a
			if y > 0 {
				top = int64(out[(2*y-1)*w+x])
			}
			diff := int64(res.data[y*res.w+x]) + smoothTendency(top, a, next)
			A := a + diff/2
			out[2*y*w+x] = int32(A)
			out[(2*y+1)*w+x] = int32(A - diff)
		}
	}
	if avg.h > res.h {
		copy(out[2*res.h*w:], avg.data[res.h*w:(res.h+1)*w])
	}
	avg.h = oh
	avg.data = out
	avg.vshift--
}

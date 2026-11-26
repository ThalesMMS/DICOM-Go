package jpegxl

// Modular image sub-bitstream (ISO/IEC 18181-1 Annex H).

const (
	transformRCT     = 0
	transformPalette = 1
	transformSqueeze = 2

	predWeighted = 6
	numPredictor = 14
)

type channel struct {
	w, h           int
	hshift, vshift int
	data           []int32
}

func newChannel(w, h int) channel {
	return channel{w: w, h: h, data: make([]int32, w*h)}
}

type modularImage struct {
	channels []channel
	nbMeta   int
	bitDepth int
}

type wpHeader struct {
	p1C, p2C, p3Ca, p3Cb, p3Cc, p3Cd, p3Ce int64
	w                                      [4]uint32
}

func readWPHeader(br *bitReader) wpHeader {
	h := wpHeader{16, 10, 7, 7, 7, 0, 0, [4]uint32{0xd, 0xc, 0xc, 0xc}}
	if br.bool() {
		return h
	}
	h.p1C = int64(br.u(5))
	h.p2C = int64(br.u(5))
	h.p3Ca = int64(br.u(5))
	h.p3Cb = int64(br.u(5))
	h.p3Cc = int64(br.u(5))
	h.p3Cd = int64(br.u(5))
	h.p3Ce = int64(br.u(5))
	for i := range h.w {
		h.w[i] = br.u(4)
	}
	return h
}

type squeezeParams struct {
	horizontal bool
	inPlace    bool
	beginC     int
	numC       int
}

type transformInfo struct {
	id        int
	beginC    int
	rctType   int
	numC      int
	nbColours int
	nbDeltas  int
	dPred     int
	squeezes  []squeezeParams
}

type groupHeader struct {
	useGlobalTree bool
	wp            wpHeader
	transforms    []transformInfo
}

func readGroupHeader(br *bitReader) (groupHeader, error) {
	var g groupHeader
	g.useGlobalTree = br.bool()
	g.wp = readWPHeader(br)
	n := br.u32(val(0), val(1), bitsOff(4, 2), bitsOff(8, 18))
	if n > 512 {
		return g, corrupt("too many transforms")
	}
	beginC := func() int {
		return int(br.u32(bitsN(3), bitsOff(6, 8), bitsOff(10, 72), bitsOff(13, 1096)))
	}
	for i := uint32(0); i < n; i++ {
		var t transformInfo
		t.id = int(br.u32(val(0), val(1), val(2), val(3)))
		switch t.id {
		case transformRCT:
			t.beginC = beginC()
			t.rctType = int(br.u32(val(6), bitsN(2), bitsOff(4, 2), bitsOff(6, 10)))
			if t.rctType >= 42 {
				return g, corrupt("RCT type")
			}
		case transformPalette:
			t.beginC = beginC()
			t.numC = int(br.u32(val(1), val(3), val(4), bitsOff(13, 1)))
			t.nbColours = int(br.u32(bitsOff(8, 0), bitsOff(10, 256), bitsOff(12, 1280), bitsOff(16, 5376)))
			t.nbDeltas = int(br.u32(val(0), bitsOff(8, 1), bitsOff(10, 257), bitsOff(16, 1281)))
			t.dPred = int(br.u(4))
			if t.dPred >= numPredictor {
				return g, corrupt("palette predictor")
			}
		case transformSqueeze:
			ns := br.u32(val(0), bitsOff(4, 1), bitsOff(6, 9), bitsOff(8, 41))
			for k := uint32(0); k < ns; k++ {
				var s squeezeParams
				s.horizontal = br.bool()
				s.inPlace = br.bool()
				s.beginC = beginC()
				s.numC = int(br.u32(val(1), val(2), val(3), bitsOff(4, 4)))
				t.squeezes = append(t.squeezes, s)
			}
		default:
			return g, corrupt("invalid transform")
		}
		g.transforms = append(g.transforms, t)
		if br.err != nil {
			return g, br.err
		}
	}
	return g, br.err
}

type treeNode struct {
	property   int32 // -1 for a leaf
	splitVal   int32
	lchild     int32 // context id for a leaf
	rchild     int32
	predictor  uint8
	offset     int64
	multiplier int64
}

type maTree struct {
	nodes   []treeNode
	maxProp int
	usesWP  bool
}

func readTree(br *bitReader, sizeLimit int) (*maTree, error) {
	code, err := readEntropyCode(br, 6)
	if err != nil {
		return nil, err
	}
	sr := newSymbolReader(code, br, 0)
	t := &maTree{maxProp: -1}
	toDecode := 1
	leaf := int32(0)
	for toDecode > 0 {
		if len(t.nodes)&255 == 0 {
			if err := br.checkpoint(); err != nil {
				return nil, err
			}
		}
		if br.err != nil {
			return nil, br.err
		}
		if len(t.nodes) > sizeLimit {
			return nil, corrupt("MA tree too large")
		}
		toDecode--
		prop := sr.readUint(br, 1)
		if prop > 256 {
			return nil, corrupt("tree property")
		}
		if prop == 0 {
			pred := sr.readUint(br, 2)
			if pred >= numPredictor {
				return nil, corrupt("tree predictor")
			}
			off := unpackSigned64(uint64(sr.readUint(br, 3)))
			mulLog := sr.readUint(br, 4)
			if mulLog >= 31 {
				return nil, corrupt("tree multiplier log")
			}
			mulBits := sr.readUint(br, 5)
			if uint64(mulBits) >= (uint64(1)<<(31-mulLog))-1 {
				return nil, corrupt("tree multiplier")
			}
			t.nodes = append(t.nodes, treeNode{
				property: -1, lchild: leaf, predictor: uint8(pred), offset: off,
				multiplier: int64(mulBits+1) << mulLog,
			})
			if pred == predWeighted {
				t.usesWP = true
			}
			leaf++
			continue
		}
		split := unpackSigned(sr.readUint(br, 0))
		n := int32(len(t.nodes))
		t.nodes = append(t.nodes, treeNode{
			property: int32(prop - 1), splitVal: split,
			lchild: n + int32(toDecode) + 1, rchild: n + int32(toDecode) + 2,
		})
		t.maxProp = maxInt(t.maxProp, int(prop-1))
		if prop-1 == 15 {
			t.usesWP = true
		}
		toDecode += 2
	}
	if !sr.finalStateOK() {
		return nil, corrupt("MA tree ANS final state")
	}
	return t, br.err
}

func (t *maTree) numLeaves() int { return (len(t.nodes) + 1) / 2 }

// modularContext carries the global tree and histograms shared by streams.
type modularContext struct {
	tree *maTree
	code *entropyCode
}

// decodeModular reads one modular sub-bitstream into img (Annex H.2). With
// undo=false the transforms stay applied and are returned for a later
// inverse.
func decodeModular(br *bitReader, img *modularImage, streamID int, global *modularContext, maxChanSize int, undo bool) (groupHeader, error) {
	if len(img.channels) == 0 {
		return groupHeader{}, nil
	}
	reqW := make([]int, len(img.channels))
	reqH := make([]int, len(img.channels))
	for i, c := range img.channels {
		reqW[i], reqH[i] = c.w, c.h
	}
	hdr, err := readGroupHeader(br)
	if err != nil {
		return hdr, err
	}
	if hdr.useGlobalTree && (global == nil || global.tree == nil) {
		return hdr, corrupt("global tree used but absent")
	}
	for i := range hdr.transforms {
		if br.ctx != nil {
			if err := br.checkpoint(); err != nil {
				return hdr, err
			}
			transform := hdr.transforms[i]
			if transform.id == transformPalette && transform.numC > 0 &&
				transform.nbColours+transform.nbDeltas > (1<<22)/transform.numC {
				return hdr, ErrLimit
			}
		}
		if err := metaApply(img, &hdr.transforms[i]); err != nil {
			return hdr, err
		}
	}
	distMult := 0
	numChans := 0
	for i := range img.channels {
		c := &img.channels[i]
		if c.w == 0 || c.h == 0 {
			continue
		}
		if i >= img.nbMeta && (c.w > maxChanSize || c.h > maxChanSize) {
			break
		}
		distMult = maxInt(distMult, c.w)
		numChans++
	}
	if numChans > 0 {
		var tree *maTree
		var code *entropyCode
		if hdr.useGlobalTree {
			tree, code = global.tree, global.code
		} else {
			limit := 1024
			for _, c := range img.channels {
				limit += c.w * c.h
			}
			limit = min(limit, 1<<20)
			if tree, err = readTree(br, limit); err != nil {
				return hdr, err
			}
			if code, err = readEntropyCode(br, tree.numLeaves()); err != nil {
				return hdr, err
			}
		}
		sr := newSymbolReader(code, br, distMult)
		for i := range img.channels {
			c := &img.channels[i]
			if c.w == 0 || c.h == 0 {
				continue
			}
			if i >= img.nbMeta && (c.w > maxChanSize || c.h > maxChanSize) {
				break
			}
			if err := decodeChannel(br, sr, tree, hdr.wp, img, i, streamID); err != nil {
				return hdr, err
			}
		}
		if !sr.finalStateOK() {
			return hdr, corrupt("modular ANS final state")
		}
	}
	if undo {
		if err := undoTransforms(img, hdr.transforms, hdr.wp); err != nil {
			return hdr, err
		}
		if len(img.channels) != len(reqW) {
			return hdr, corrupt("transforms changed the channel count")
		}
		for i, c := range img.channels {
			if c.w != reqW[i] || c.h != reqH[i] {
				return hdr, corrupt("transforms changed channel dimensions")
			}
		}
	}
	return hdr, br.err
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func clampedGradient(n, w, l int64) int64 {
	lo, hi := min(n, w), max(n, w)
	grad := n + w - l
	if l < lo {
		return hi
	}
	if l > hi {
		return lo
	}
	return grad
}

func selectPredictor(a, b, c int64) int64 {
	p := a + b - c
	if abs64(p-a) < abs64(p-b) {
		return a
	}
	return b
}

// wpState is the self-correcting weighted predictor (Annex H.5).
type wpState struct {
	h          wpHeader
	predErr    [4][]uint32
	err        []int32
	prediction [4]int64
	pred       int64
	w          int
}

var wpDivLookup = func() (t [64]uint32) {
	for i := range t {
		t[i] = (1 << 24) / uint32(i+1)
	}
	return t
}()

func newWPState(h wpHeader, w int) *wpState {
	s := &wpState{h: h, w: w}
	for i := range s.predErr {
		s.predErr[i] = make([]uint32, (w+2)*2)
	}
	s.err = make([]int32, (w+2)*2)
	return s
}

func errorWeight(x uint64, maxWeight uint32) uint32 {
	shift := int(floorLog2(x+1)) - 5
	if shift < 0 {
		shift = 0
	}
	return 4 + uint32((uint64(maxWeight)*uint64(wpDivLookup[x>>uint(shift)]))>>uint(shift))
}

// predictWP returns the prediction and the maximum-error property.
func (s *wpState) predictWP(x, y int, N, W, NE, NW, NN int64) (int64, int64) {
	xs := s.w
	cur, prev := xs+2, 0
	if y&1 == 1 {
		cur, prev = 0, xs+2
	}
	posN := prev + x
	posNE, posNW := posN, posN
	if x < xs-1 {
		posNE = posN + 1
	}
	if x > 0 {
		posNW = posN - 1
	}
	var weights [4]uint32
	for i := 0; i < 4; i++ {
		e := uint64(s.predErr[i][posN]) + uint64(s.predErr[i][posNE]) + uint64(s.predErr[i][posNW])
		weights[i] = errorWeight(uint64(uint32(e)), s.h.w[i])
	}
	N, W, NE, NW, NN = N<<3, W<<3, NE<<3, NW<<3, NN<<3
	var teW int64
	if x > 0 {
		teW = int64(s.err[cur+x-1])
	}
	teN := int64(s.err[posN])
	teNW := int64(s.err[posNW])
	teNE := int64(s.err[posNE])
	sumWN := teN + teW
	prop := teW
	if abs64(teN) > abs64(prop) {
		prop = teN
	}
	if abs64(teNW) > abs64(prop) {
		prop = teNW
	}
	if abs64(teNE) > abs64(prop) {
		prop = teNE
	}
	s.prediction[0] = W + NE - N
	s.prediction[1] = N - (((sumWN + teNE) * s.h.p1C) >> 5)
	s.prediction[2] = W - (((sumWN + teNW) * s.h.p2C) >> 5)
	s.prediction[3] = N - ((teNW*s.h.p3Ca + teN*s.h.p3Cb + teNE*s.h.p3Cc + (NN-N)*s.h.p3Cd + (NW-W)*s.h.p3Ce) >> 5)
	// Weighted average.
	wsum := uint32(0)
	for _, w := range weights {
		wsum += w
	}
	logw := floorLog2(uint64(wsum))
	wsum = 0
	for i := range weights {
		weights[i] >>= logw - 4
		wsum += weights[i]
	}
	sum := int64(wsum>>1) - 1
	for i := 0; i < 4; i++ {
		sum += s.prediction[i] * int64(weights[i])
	}
	s.pred = (sum * int64(wpDivLookup[wsum-1])) >> 24
	if ((teN ^ teW) | (teN ^ teNW)) > 0 {
		return (s.pred + 3) >> 3, prop
	}
	mx := max(W, max(NE, N))
	mn := min(W, min(NE, N))
	s.pred = max(mn, min(mx, s.pred))
	return (s.pred + 3) >> 3, prop
}

func (s *wpState) update(val int64, x, y int) {
	xs := s.w
	cur, prev := xs+2, 0
	if y&1 == 1 {
		cur, prev = 0, xs+2
	}
	val <<= 3
	s.err[cur+x] = int32(s.pred - val)
	for i := 0; i < 4; i++ {
		e := (abs64(s.prediction[i]-val) + 3) >> 3
		s.predErr[i][cur+x] = uint32(e)
		s.predErr[i][prev+x+1] += uint32(e)
	}
}

func predictValue(p uint8, W, N, NW, NE, NN, WW, NEE, wp int64) int64 {
	switch p {
	case 0:
		return 0
	case 1:
		return W
	case 2:
		return N
	case 3:
		return (W + N) / 2
	case 4:
		return selectPredictor(W, N, NW)
	case 5:
		return clampedGradient(N, W, NW)
	case 6:
		return wp
	case 7:
		return NE
	case 8:
		return NW
	case 9:
		return WW
	case 10:
		return (W + NW) / 2
	case 11:
		return (N + NW) / 2
	case 12:
		return (N + NE) / 2
	case 13:
		return (6*N - 2*NN + 7*W + WW + NEE + 3*NE + 8) / 16
	}
	return 0
}

func decodeChannel(br *bitReader, sr *symbolReader, tree *maTree, wph wpHeader, img *modularImage, ci int, streamID int) error {
	return walkChannel(tree, wph, img, ci, streamID, func(x, y int, node *treeNode, pred int64, _ int32) int32 {
		token := sr.readUint(br, int(node.lchild))
		return int32(int64(unpackSigned(token))*node.multiplier + node.offset + pred)
	}, br.checkpoint)
}

// pixelCoder decodes or encodes one sample given its tree leaf and
// prediction; cur is the sample already present in the channel (the source
// value when encoding). It returns the sample to store.
type pixelCoder func(x, y int, node *treeNode, pred int64, cur int32) int32

// walkChannel visits every sample of channel ci in coding order, computing
// the MA-tree properties and predictions shared by the decoder and encoder.
func walkChannel(tree *maTree, wph wpHeader, img *modularImage, ci int, streamID int, code pixelCoder, rowErr func() error) error {
	ch := &img.channels[ci]
	w, h := ch.w, ch.h
	nodes := tree.nodes
	numProps := 16
	numRef := 0
	if tree.maxProp >= 16 {
		numRef = (tree.maxProp - 16 + 4) / 4
		numProps = 16 + 4*numRef
	}
	// Reference channels, most recent first.
	var refs []int
	for j := ci - 1; j >= 0 && len(refs) < numRef; j-- {
		o := &img.channels[j]
		if o.w == w && o.h == h && o.hshift == ch.hshift && o.vshift == ch.vshift {
			refs = append(refs, j)
		}
	}
	singleLeaf := len(nodes) == 1
	var wp *wpState
	if tree.usesWP {
		wp = newWPState(wph, w)
	}
	props := make([]int64, numProps)
	props[0] = int64(ci)
	props[1] = int64(streamID)
	data := ch.data
	for y := 0; y < h; y++ {
		row := data[y*w : (y+1)*w]
		var top, top2 []int32
		if y > 0 {
			top = data[(y-1)*w : y*w]
		}
		if y > 1 {
			top2 = data[(y-2)*w : (y-1)*w]
		}
		props[2] = int64(y)
		props[9] = 0
		for x := 0; x < w; x++ {
			var W, N, NW, NE, WW, NN, NEE int64
			switch {
			case x > 0:
				W = int64(row[x-1])
			case y > 0:
				W = int64(top[x])
			}
			if y > 0 {
				N = int64(top[x])
			} else {
				N = W
			}
			if x > 0 && y > 0 {
				NW = int64(top[x-1])
			} else {
				NW = W
			}
			if x+1 < w && y > 0 {
				NE = int64(top[x+1])
			} else {
				NE = N
			}
			if x > 1 {
				WW = int64(row[x-2])
			} else {
				WW = W
			}
			if y > 1 {
				NN = int64(top2[x])
			} else {
				NN = N
			}
			if x+2 < w && y > 0 {
				NEE = int64(top[x+2])
			} else {
				NEE = NE
			}
			var wpPred int64
			if wp != nil {
				wpPred, props[15] = wp.predictWP(x, y, N, W, NE, NW, NN)
			}
			node := &nodes[0]
			if !singleLeaf {
				props[3] = int64(x)
				props[4] = abs64(N)
				props[5] = abs64(W)
				props[6] = N
				props[7] = W
				props[8] = W - props[9]
				props[9] = W + N - NW
				props[10] = W - NW
				props[11] = NW - N
				props[12] = N - NE
				props[13] = N - NN
				props[14] = W - WW
				for r, j := range refs {
					rc := img.channels[j].data
					v := int64(rc[y*w+x])
					var vl, vt, vtl int64
					if x > 0 {
						vl = int64(rc[y*w+x-1])
					}
					if y > 0 {
						vt = int64(rc[(y-1)*w+x])
					} else {
						vt = vl
					}
					if x > 0 && y > 0 {
						vtl = int64(rc[(y-1)*w+x-1])
					} else {
						vtl = vl
					}
					pv := clampedGradient(vt, vl, vtl)
					base := 16 + 4*r
					props[base] = abs64(v)
					props[base+1] = v
					props[base+2] = abs64(v - pv)
					props[base+3] = v - pv
				}
				for node.property >= 0 {
					if props[node.property] > int64(node.splitVal) {
						node = &nodes[node.lchild]
					} else {
						node = &nodes[node.rchild]
					}
				}
			}
			pred := predictValue(node.predictor, W, N, NW, NE, NN, WW, NEE, wpPred)
			row[x] = code(x, y, node, pred, row[x])
			if wp != nil {
				wp.update(int64(row[x]), x, y)
			}
		}
		if err := rowErr(); err != nil {
			return err
		}
	}
	return rowErr()
}

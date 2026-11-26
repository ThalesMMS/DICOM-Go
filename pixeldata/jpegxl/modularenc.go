package jpegxl

// Modular encoding: fixed MA trees, tree serialization and channel tokens.

type tnode struct {
	prop        int
	split       int32
	left, right *tnode // left is taken when the property exceeds split
	pred        uint8
	offset      int64
}

func leafNode(pred uint8) *tnode { return &tnode{prop: -1, pred: pred} }

// balancedSplit builds a binary tree over sorted cutoffs of one property.
func balancedSplit(prop int, cutoffs []int32, leaf func() *tnode) *tnode {
	if len(cutoffs) == 0 {
		return leaf()
	}
	mid := len(cutoffs) / 2
	return &tnode{
		prop:  prop,
		split: cutoffs[mid],
		left:  balancedSplit(prop, cutoffs[mid+1:], leaf),
		right: balancedSplit(prop, cutoffs[:mid], leaf),
	}
}

var wpCutoffs = []int32{-500, -392, -255, -191, -127, -95, -63, -47, -31, -23, -15, -11, -7, -4, -3, -1, 0, 1, 3, 5, 7, 11, 15, 23, 31, 47, 63, 95, 127, 191, 255, 392, 500}

// weightedTree is a self-correcting-predictor tree with contexts from the
// predictor's error estimate, scaled to the sample bit depth.
func weightedTree(bitDepth int) *tnode {
	scale := int32(1)
	if bitDepth > 8 {
		scale = 1 << uint(min(bitDepth-8, 16))
	}
	cut := make([]int32, len(wpCutoffs))
	for i, c := range wpCutoffs {
		cut[i] = c * scale
	}
	return balancedSplit(15, cut, func() *tnode { return leafNode(predWeighted) })
}

// flatten converts a tree to the decoder's breadth-first node array.
func flattenTree(root *tnode) (*maTree, []*tnode) {
	t := &maTree{maxProp: -1}
	order := []*tnode{root}
	for i := 0; i < len(order); i++ {
		n := order[i]
		if n.prop >= 0 {
			order = append(order, n.left, n.right)
		}
	}
	leaf := int32(0)
	toDecode := 1
	for _, n := range order {
		toDecode--
		if n.prop < 0 {
			t.nodes = append(t.nodes, treeNode{property: -1, lchild: leaf, predictor: n.pred, offset: n.offset, multiplier: 1})
			if n.pred == predWeighted {
				t.usesWP = true
			}
			leaf++
			continue
		}
		idx := int32(len(t.nodes))
		t.nodes = append(t.nodes, treeNode{property: int32(n.prop), splitVal: n.split,
			lchild: idx + int32(toDecode) + 1, rchild: idx + int32(toDecode) + 2})
		t.maxProp = max(t.maxProp, n.prop)
		if n.prop == 15 {
			t.usesWP = true
		}
		toDecode += 2
	}
	return t, order
}

// writeTree serializes a tree with its own entropy code (Annex H.4).
func writeTree(w *bitWriter, order []*tnode) {
	ts := &tokenStream{numContexts: 6}
	for _, n := range order {
		if n.prop < 0 {
			ts.add(1, 0)
			ts.add(2, uint32(n.pred))
			ts.add(3, packSigned(int32(n.offset)))
			ts.add(4, 0)
			ts.add(5, 0)
			continue
		}
		ts.add(1, uint32(n.prop+1))
		ts.add(0, packSigned(n.split))
	}
	enc := buildEntropyEncoder(6, ts)
	enc.writeHeader(w)
	enc.writeStream(w, ts.tokens)
}

// modularStream is one modular sub-bitstream awaiting serialization.
type modularStream struct {
	header     groupHeader
	transforms []transformInfo
	tokens     tokenStream
	hasData    bool
}

// encodeModularStream computes the tokens of img (with transforms already
// applied to its channels) for the given stream id.
func encodeModularStream(img *modularImage, tree *maTree, wp wpHeader, streamID int, maxChanSize int, numContexts int) (*modularStream, error) {
	ms := &modularStream{tokens: tokenStream{numContexts: numContexts}}
	for i := range img.channels {
		c := &img.channels[i]
		if c.w == 0 || c.h == 0 {
			continue
		}
		if i >= img.nbMeta && (c.w > maxChanSize || c.h > maxChanSize) {
			break
		}
		ms.hasData = true
		err := walkChannel(tree, wp, img, i, streamID, func(x, y int, node *treeNode, pred int64, cur int32) int32 {
			r := int64(cur) - pred - node.offset
			ms.tokens.add(int(node.lchild), packSigned(int32(r)))
			return cur
		}, func() error { return nil })
		if err != nil {
			return nil, err
		}
	}
	return ms, nil
}

// writeGroupHeader writes a modular group header using the global tree.
func writeGroupHeader(w *bitWriter, useGlobal bool, transforms []transformInfo) {
	w.bool(useGlobal)
	w.bool(true) // default weighted-predictor parameters
	w.u32(uint32(len(transforms)), val(0), val(1), bitsOff(4, 2), bitsOff(8, 18))
	for _, t := range transforms {
		w.u32(uint32(t.id), val(0), val(1), val(2), val(3))
		switch t.id {
		case transformRCT:
			w.u32(uint32(t.beginC), bitsN(3), bitsOff(6, 8), bitsOff(10, 72), bitsOff(13, 1096))
			w.u32(uint32(t.rctType), val(6), bitsN(2), bitsOff(4, 2), bitsOff(6, 10))
		default:
			panic("jpegxl: encoder transform not implemented")
		}
	}
}

var defaultWP = wpHeader{16, 10, 7, 7, 7, 0, 0, [4]uint32{0xd, 0xc, 0xc, 0xc}}

// forwardYCoCg applies RCT type 6 (YCoCg-R) to three channels in place.
func forwardYCoCg(r, g, b []int32) {
	for i := range r {
		R, G, B := r[i], g[i], b[i]
		Co := R - B
		tmp := B + (Co >> 1)
		Cg := G - tmp
		Y := tmp + (Cg >> 1)
		r[i], g[i], b[i] = Y, Co, Cg
	}
}

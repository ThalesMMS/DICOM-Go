package jpegxl

import (
	"math"
	"sort"
)

// Entropy encoding with clustered ANS histograms (the encoder side of
// entropy.go). Every stream uses the hybrid integer configuration (4, 1, 0).

var encConfig = hybridConfig{splitExp: 4, msb: 1, lsb: 0}

const maxEncClusters = 255

func (c hybridConfig) encode(v uint32) (token uint32, nbits uint, extra uint32) {
	split := uint32(1) << c.splitExp
	if v < split {
		return v, 0, 0
	}
	n := floorLog2u32(v)
	m := v - 1<<n
	token = split + uint32(n-uint(c.splitExp))<<(c.msb+c.lsb) + (m>>(n-uint(c.msb)))<<c.lsb + m&(1<<c.lsb-1)
	nbits = n - uint(c.msb) - uint(c.lsb)
	extra = (v >> c.lsb) & (1<<nbits - 1)
	return
}

type tokenRef struct {
	ctx   int32
	value uint32
}

// tokenStream collects the tokens of one or more streams sharing a code.
type tokenStream struct {
	numContexts int
	tokens      []tokenRef
}

func (t *tokenStream) add(ctx int, v uint32) {
	t.tokens = append(t.tokens, tokenRef{int32(ctx), v})
}

type entropyEncoder struct {
	contextMap []uint8
	clusters   int
	// ANS coding.
	ans      bool
	logAlpha uint32
	dists    [][]int32
	plans    []ansPlan
	reverse  [][][]uint16 // per cluster, per symbol: table slot of each offset
}

func histogramCost(h []uint32) float64 {
	var total uint32
	for _, c := range h {
		total += c
	}
	if total == 0 {
		return 0
	}
	cost := 0.0
	ft := float64(total)
	for _, c := range h {
		if c > 0 {
			cost -= float64(c) * math.Log2(float64(c)/ft)
		}
	}
	return cost
}

func mergedCostIncrease(a, b []uint32, costA, costB float64) float64 {
	n := max(len(a), len(b))
	m := make([]uint32, n)
	for i := range a {
		m[i] += a[i]
	}
	for i := range b {
		m[i] += b[i]
	}
	return histogramCost(m) - costA - costB
}

// buildEntropyEncoder clusters the context histograms of streams and builds
// one prefix code per cluster.
func buildEntropyEncoder(numContexts int, streams ...*tokenStream) *entropyEncoder {
	hist := make([][]uint32, numContexts)
	for _, s := range streams {
		for _, t := range s.tokens {
			tok, _, _ := encConfig.encode(t.value)
			h := hist[t.ctx]
			for int(tok) >= len(h) {
				h = append(h, 0)
			}
			h[tok]++
			hist[t.ctx] = h
		}
	}
	var used []int
	for c, h := range hist {
		if len(h) > 0 {
			used = append(used, c)
		}
	}
	e := &entropyEncoder{contextMap: make([]uint8, numContexts)}
	if len(used) == 0 {
		e.clusters = 1
		e.buildANS([][]uint32{{1}})
		return e
	}
	// Choose cluster centres greedily by cost distance, then assign.
	costs := make([]float64, numContexts)
	for _, c := range used {
		costs[c] = histogramCost(hist[c])
	}
	sort.Slice(used, func(i, j int) bool {
		return costs[used[i]] > costs[used[j]] || (costs[used[i]] == costs[used[j]] && used[i] < used[j])
	})
	centres := []int{used[0]}
	dist := make([]float64, len(used))
	for i, c := range used {
		dist[i] = mergedCostIncrease(hist[c], hist[centres[0]], costs[c], costs[centres[0]])
	}
	for len(centres) < maxEncClusters && len(centres) < len(used) {
		best, bestD := -1, 64.0 // merging costs under 64 bits is not worth a code
		for i, d := range dist {
			if d > bestD {
				best, bestD = i, d
			}
		}
		if best < 0 {
			break
		}
		centres = append(centres, used[best])
		for i, c := range used {
			if d := mergedCostIncrease(hist[c], hist[used[best]], costs[c], costs[used[best]]); d < dist[i] {
				dist[i] = d
			}
		}
	}
	assign := make(map[int]int, len(used))
	for i, c := range centres {
		assign[c] = i
	}
	merged := make([][]uint32, len(centres))
	for i, c := range centres {
		merged[i] = append([]uint32{}, hist[c]...)
	}
	for _, c := range used {
		if _, ok := assign[c]; ok {
			continue
		}
		best, bestD := 0, math.Inf(1)
		for i, k := range centres {
			if d := mergedCostIncrease(hist[c], hist[k], costs[c], costs[k]); d < bestD {
				best, bestD = i, d
			}
		}
		assign[c] = best
		for len(merged[best]) < len(hist[c]) {
			merged[best] = append(merged[best], 0)
		}
		for s, n := range hist[c] {
			merged[best][s] += n
		}
	}
	for c := range e.contextMap {
		e.contextMap[c] = uint8(assign[c]) // unused contexts map to cluster 0
	}
	e.clusters = len(centres)
	e.buildANS(merged)
	return e
}

// quantizeDistribution scales h to a distribution summing to 4096 with every
// used symbol keeping a non-zero count.
func quantizeDistribution(h []uint32) []int32 {
	n := len(h)
	for n > 0 && h[n-1] == 0 {
		n--
	}
	h = h[:n]
	var total uint64
	for _, c := range h {
		total += uint64(c)
	}
	d := make([]int32, n)
	sum := int32(0)
	largest := 0
	for s, c := range h {
		if c == 0 {
			continue
		}
		v := int32((uint64(c)*ansTabSize + total/2) / total)
		if v < 1 {
			v = 1
		}
		d[s] = v
		sum += v
		if c > h[largest] {
			largest = s
		}
	}
	// Move the rounding error to and from the largest symbols.
	for sum != ansTabSize {
		best := -1
		for s := range d {
			if d[s] == 0 {
				continue
			}
			if sum > ansTabSize && d[s] <= 1 {
				continue
			}
			if best < 0 || d[s] > d[best] {
				best = s
			}
		}
		step := int32(1)
		if sum > ansTabSize {
			step = -min(sum-ansTabSize, d[best]-1)
		} else {
			step = ansTabSize - sum
		}
		d[best] += step
		sum += step
	}
	_ = largest
	return d
}

func (e *entropyEncoder) buildANS(hists [][]uint32) {
	e.ans = true
	maxAlpha := 1
	for _, h := range hists {
		n := len(h)
		for n > 0 && h[n-1] == 0 {
			n--
		}
		maxAlpha = max(maxAlpha, n)
	}
	e.logAlpha = 5
	for 1<<e.logAlpha < maxAlpha {
		e.logAlpha++
	}
	e.dists = make([][]int32, len(hists))
	e.plans = make([]ansPlan, len(hists))
	e.reverse = make([][][]uint16, len(hists))
	for i, h := range hists {
		e.plans[i] = planANSDistribution(h)
		d := e.plans[i].dist
		if len(d) == 0 {
			d = []int32{ansTabSize}
			e.plans[i] = ansPlan{dist: d, shift: 13}
		}
		e.dists[i] = d
		table, err := buildAliasTable(d, e.logAlpha)
		if err != nil {
			panic("jpegxl: alias table: " + err.Error())
		}
		rev := make([][]uint16, len(d))
		for s := range d {
			rev[s] = make([]uint16, d[s])
		}
		logEntry := ansLogTabSize - e.logAlpha
		for slot := uint32(0); slot < ansTabSize; slot++ {
			idx := slot >> logEntry
			pos := slot & (1<<logEntry - 1)
			en := &table[idx]
			sym, off := idx, pos
			if pos >= en.cutoff {
				sym, off = en.right, en.offsets1+pos
			}
			if int(sym) < len(rev) && int(off) < len(rev[sym]) {
				rev[sym][off] = uint16(slot)
			}
		}
		e.reverse[i] = rev
	}
}

func writeVarLenUint8(w *bitWriter, v int) {
	if v == 0 {
		w.write(1, 0)
		return
	}
	n := floorLog2u32(uint32(v))
	w.write(1, 1)
	w.write(3, uint64(n))
	w.write(n, uint64(v-1<<n))
}

var logCountEncode = [14][2]uint8{
	{17, 5}, {11, 4}, {15, 4}, {3, 4}, {9, 4}, {7, 4}, {4, 3}, {2, 3},
	{5, 3}, {6, 3}, {0, 3}, {33, 6}, {1, 7}, {65, 7},
}

// ansPlan is a distribution chosen for one histogram with the shift used
// to write it.
type ansPlan struct {
	dist  []int32
	shift uint32
	omit  int
	logc  []uint32
}

// planANSDistribution picks the count precision (shift) minimizing header
// plus data bits; counts are rounded down to the precision grid and the most
// frequent symbol absorbs the remainder.
func planANSDistribution(h []uint32) ansPlan {
	n := len(h)
	for n > 0 && h[n-1] == 0 {
		n--
	}
	h = h[:n]
	exact := quantizeDistribution(h)
	used := 0
	for _, c := range h {
		if c > 0 {
			used++
		}
	}
	if used <= 2 {
		return ansPlan{dist: exact, shift: 13}
	}
	var total float64
	for _, c := range h {
		total += float64(c)
	}
	length := max(n, 3)
	best := ansPlan{}
	bestCost := math.Inf(1)
	for shift := uint32(0); shift <= 13; shift++ {
		target := make([]float64, length)
		logc := make([]uint32, length)
		for i, c := range h {
			target[i] = float64(c) * ansTabSize / total
		}
		omit, omitLog := -1, -1
		for i := range h {
			if h[i] == 0 {
				continue
			}
			l := floorLog2u32(uint32(max(1, int(target[i])))) + 1
			if int(l) > omitLog {
				omitLog, omit = int(l), i
			}
		}
		d := make([]int32, length)
		sum := int32(0)
		for i := range h {
			if h[i] == 0 || i == omit {
				continue
			}
			v := int32(target[i])
			if v < 1 {
				v = 1
			}
			l := floorLog2u32(uint32(v)) + 1
			if l > 1 {
				bc := popcountPrecision(uint32(l-1), shift)
				drop := uint(l-1) - uint(bc)
				v = v >> drop << drop
			}
			d[i] = v
			logc[i] = uint32(floorLog2u32(uint32(v))) + 1
			sum += v
		}
		if sum >= ansTabSize {
			continue
		}
		d[omit] = ansTabSize - sum
		logc[omit] = uint32(omitLog)
		// Header bits.
		bits := 2.0 + 3 + float64(shift>>0&0) // flags and shift code
		for i := 0; i < length; i++ {
			bits += float64(logCountEncode[logc[i]][1])
			if i != omit && logc[i] > 1 {
				bits += float64(popcountPrecision(logc[i]-1, shift))
			}
		}
		for i, c := range h {
			if c > 0 {
				bits += float64(c) * math.Log2(ansTabSize/float64(d[i]))
			}
		}
		if bits < bestCost {
			bestCost = bits
			best = ansPlan{dist: d[:n], shift: shift, omit: omit, logc: logc}
		}
	}
	if best.dist == nil {
		return ansPlan{dist: exact, shift: 13}
	}
	return best
}

func writeANSDistribution(w *bitWriter, p ansPlan) {
	d := p.dist
	var used []int
	for s, c := range d {
		if c > 0 {
			used = append(used, s)
		}
	}
	if len(used) == 1 {
		w.write(1, 1)
		w.write(1, 0)
		writeVarLenUint8(w, used[0])
		return
	}
	if len(used) == 2 {
		w.write(1, 1)
		w.write(1, 1)
		writeVarLenUint8(w, used[0])
		writeVarLenUint8(w, used[1])
		w.write(ansLogTabSize, uint64(d[used[0]]))
		return
	}
	w.write(1, 0) // not simple
	w.write(1, 0) // not flat
	// shift = (u(log) | 1<<log) - 1 with log written in unary (at most 3).
	shift := p.shift + 1
	log := uint(floorLog2u32(shift))
	for i := uint(0); i < log; i++ {
		w.write(1, 1)
	}
	if log < 3 {
		w.write(1, 0)
	}
	w.write(log, uint64(shift-1<<log))
	n := max(len(d), 3)
	writeVarLenUint8(w, n-3)
	logc := p.logc
	omit := p.omit
	if logc == nil {
		logc = make([]uint32, n)
		omitLog := -1
		for i := 0; i < n; i++ {
			if i < len(d) && d[i] > 0 {
				logc[i] = uint32(floorLog2u32(uint32(d[i]))) + 1
			}
			if int(logc[i]) > omitLog {
				omitLog, omit = int(logc[i]), i
			}
		}
	}
	for i := 0; i < n; i++ {
		e := logCountEncode[logc[i]]
		w.write(uint(e[1]), uint64(e[0]))
	}
	for i := 0; i < n; i++ {
		if i == omit || logc[i] <= 1 {
			continue
		}
		bc := popcountPrecision(logc[i]-1, p.shift)
		w.write(uint(bc), uint64(d[i]-1<<(logc[i]-1))>>(logc[i]-1-bc))
	}
}

func (e *entropyEncoder) writeHeader(w *bitWriter) {
	w.bool(false) // no LZ77
	if len(e.contextMap) > 1 {
		writeContextMap(w, e.contextMap, e.clusters)
	}
	{
		w.bool(false) // ANS
		w.write(2, uint64(e.logAlpha-5))
		cfgBits := ceilLog2(uint64(e.logAlpha) + 1)
		for i := 0; i < e.clusters; i++ {
			w.write(cfgBits, uint64(encConfig.splitExp))
			w.write(ceilLog2(uint64(encConfig.splitExp)+1), uint64(encConfig.msb))
			w.write(ceilLog2(uint64(encConfig.splitExp-encConfig.msb)+1), uint64(encConfig.lsb))
		}
		for _, p := range e.plans {
			writeANSDistribution(w, p)
		}
		return
	}
}

func writeContextMap(w *bitWriter, m []uint8, clusters int) {
	var best *bitWriter
	if clusters <= 8 {
		simple := &bitWriter{}
		bitsPer := ceilLog2(uint64(clusters))
		simple.bool(true)
		simple.write(2, uint64(bitsPer))
		for _, v := range m {
			simple.write(bitsPer, uint64(v))
		}
		best = simple
	}
	if len(m) > 2 {
		coded := &bitWriter{}
		coded.bool(false)
		coded.bool(false) // no move-to-front
		ts := &tokenStream{numContexts: 1}
		for _, v := range m {
			ts.add(0, uint32(v))
		}
		inner := buildEntropyEncoder(1, ts)
		inner.writeHeader(coded)
		inner.writeStream(coded, ts.tokens)
		if best == nil || coded.bitLen() < best.bitLen() {
			best = coded
		}
	}
	w.appendWriter(best)
}

// writeStream writes one entropy-coded stream: the initial ANS state (when
// ANS is used) followed by every token and its raw bits. An empty stream
// writes nothing, matching decoders that create no reader for it.
func (e *entropyEncoder) writeStream(w *bitWriter, tokens []tokenRef) {
	if len(tokens) == 0 {
		return
	}
	// Encode backwards, remembering the 16-bit word each symbol emits.
	words := make([]int32, len(tokens))
	state := uint32(ansSignature)
	for i := len(tokens) - 1; i >= 0; i-- {
		t := tokens[i]
		tok, _, _ := encConfig.encode(t.value)
		c := e.contextMap[t.ctx]
		f := uint32(e.dists[c][tok])
		words[i] = -1
		if uint64(state) >= uint64(f)<<20 {
			words[i] = int32(state & 0xFFFF)
			state >>= 16
		}
		state = (state/f)<<ansLogTabSize + uint32(e.reverse[c][tok][state%f])
	}
	w.write(32, uint64(state))
	for i, t := range tokens {
		if words[i] >= 0 {
			w.write(16, uint64(words[i]))
		}
		_, nbits, extra := encConfig.encode(t.value)
		w.write(nbits, uint64(extra))
	}
}

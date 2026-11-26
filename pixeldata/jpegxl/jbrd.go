package jpegxl

import (
	"math/bits"

	"github.com/ThalesMMS/dicom-go/internal/brotli"
)

// JPEG bitstream reconstruction data (ISO/IEC 18181-2, jbrd box).

const (
	appUnknown = 0
	appICC     = 1
	appExif    = 2
	appXMP     = 3
)

var iccProfileTag = []byte("ICC_PROFILE\x00")
var exifTag = []byte("Exif\x00\x00")
var xmpTag = []byte("http://ns.adobe.com/xap/1.0/\x00")

type jpegQuantTable struct {
	values    [64]int32 // natural (row-major) order
	precision uint32
	index     uint32
	isLast    bool
}

type jpegComponent struct {
	id           uint32
	hSamp, vSamp int
	quantIdx     uint32
	widthBlocks  int
	heightBlocks int
	coeffs       []int16 // 64 per block, natural order
}

type jpegHuffmanCode struct {
	slotID uint32
	isLast bool
	counts [17]uint32
	values []uint32
}

type jpegScanComponent struct {
	compIdx, acTbl, dcTbl uint32
}

type extraZeroRun struct {
	blockIdx uint32
	numRuns  uint32
}

type jpegScanInfo struct {
	numComponents  uint32
	ss, se, al, ah uint32
	components     [4]jpegScanComponent
	resetPoints    []uint32
	extraZeroRuns  []extraZeroRun
}

type jpegData struct {
	width, height   int
	markerOrder     []uint8
	appData         [][]byte
	appMarkerType   []uint32
	comData         [][]byte
	quant           []jpegQuantTable
	components      []jpegComponent
	huffman         []jpegHuffmanCode
	scans           []jpegScanInfo
	restartInterval uint32
	interMarker     [][]byte
	tailData        []byte
	hasZeroPadding  bool
	paddingBits     []uint8
}

func readJPEGData(payload []byte) (*jpegData, error) {
	br := newBitReader(payload)
	j := &jpegData{}
	isGray := br.bool()
	var numApp, numCom, numScans, numInter int
	hasDRI := false
	for {
		m := uint8(br.u(6)) + 0xC0
		j.markerOrder = append(j.markerOrder, m)
		if len(j.markerOrder) > 16384 {
			return nil, corrupt("too many JPEG markers")
		}
		switch {
		case m >= 0xE0 && m <= 0xEF:
			numApp++
		case m == 0xFE:
			numCom++
		case m == 0xDA:
			numScans++
		case m == 0xFF:
			numInter++
		case m == 0xDD:
			hasDRI = true
		}
		if m == 0xD9 || br.err != nil {
			break
		}
	}
	j.appData = make([][]byte, numApp)
	j.appMarkerType = make([]uint32, numApp)
	for i := range j.appData {
		t := br.u32(val(0), val(1), bitsOff(1, 2), bitsOff(2, 4))
		if t > appXMP {
			return nil, corrupt("unknown app marker type")
		}
		j.appMarkerType[i] = t
		n := int(br.u(16)) + 1
		if n < 3 {
			return nil, corrupt("app marker too small")
		}
		j.appData[i] = make([]byte, n)
	}
	j.comData = make([][]byte, numCom)
	for i := range j.comData {
		n := int(br.u(16)) + 1
		if n < 3 {
			return nil, corrupt("COM marker too small")
		}
		j.comData[i] = make([]byte, n)
	}
	nq := br.u32(val(1), val(2), val(3), val(4))
	if nq == 4 {
		return nil, corrupt("quant table count")
	}
	j.quant = make([]jpegQuantTable, nq)
	for i := range j.quant {
		j.quant[i].precision = br.u(1)
		j.quant[i].index = br.u(2)
		j.quant[i].isLast = br.bool()
	}
	compType := br.u(2)
	var nc uint32
	switch compType {
	case 0: // gray
		nc = 1
	case 1, 2: // YCbCr, RGB
		nc = 3
	default:
		nc = br.u32(val(1), val(2), val(3), val(4))
		if nc != 1 && nc != 3 {
			return nil, corrupt("JPEG component count")
		}
	}
	if isGray != (nc == 1) {
		return nil, corrupt("JPEG gray flag mismatch")
	}
	j.components = make([]jpegComponent, nc)
	switch compType {
	case 0:
		j.components[0].id = 1
	case 1:
		j.components[0].id, j.components[1].id, j.components[2].id = 1, 2, 3
	case 2:
		j.components[0].id, j.components[1].id, j.components[2].id = 'R', 'G', 'B'
	default:
		for i := range j.components {
			j.components[i].id = br.u(8)
		}
	}
	for i := range j.components {
		j.components[i].quantIdx = br.u(2)
		if int(j.components[i].quantIdx) >= len(j.quant) {
			return nil, corrupt("component quant index")
		}
	}
	nh := br.u32(val(4), bitsOff(3, 2), bitsOff(4, 10), bitsOff(6, 26))
	j.huffman = make([]jpegHuffmanCode, nh)
	for i := range j.huffman {
		hc := &j.huffman[i]
		isAC := br.bool()
		id := br.u(2)
		hc.slotID = id
		if isAC {
			hc.slotID |= 0x10
		}
		hc.isLast = br.bool()
		total := uint32(0)
		for k := 0; k <= 16; k++ {
			hc.counts[k] = br.u32(val(0), val(1), bitsOff(3, 2), bitsN(8))
			total += hc.counts[k]
		}
		if total < 1 || total > 257 {
			return nil, corrupt("Huffman table size")
		}
		var slots [5]uint64
		hc.values = make([]uint32, total)
		for k := range hc.values {
			v := br.u32(bitsN(2), bitsOff(2, 4), bitsOff(4, 8), bitsOff(8, 1))
			if v > 256 {
				return nil, corrupt("Huffman symbol")
			}
			hc.values[k] = v
			slots[v>>6] |= 1 << (v & 63)
		}
		if br.err != nil {
			return nil, br.err
		}
		if hc.values[total-1] != 256 || slots[4] != 1 {
			return nil, corrupt("Huffman table without EOI symbol")
		}
		distinct := 1
		for k := 0; k < 4; k++ {
			distinct += bits.OnesCount64(slots[k])
		}
		if uint32(distinct) != total {
			return nil, corrupt("duplicate Huffman symbols")
		}
		if !isAC && (bits.OnesCount64(slots[0])+1 != distinct || slots[0]>>12 != 0) {
			return nil, corrupt("DC Huffman symbols out of range")
		}
	}
	j.scans = make([]jpegScanInfo, numScans)
	for i := range j.scans {
		s := &j.scans[i]
		s.numComponents = br.u32(val(1), val(2), val(3), val(4))
		if s.numComponents >= 4 {
			return nil, corrupt("scan component count")
		}
		s.ss, s.se, s.al, s.ah = br.u(6), br.u(6), br.u(4), br.u(4)
		for k := uint32(0); k < s.numComponents; k++ {
			s.components[k].compIdx = br.u(2)
			if s.components[k].compIdx >= nc {
				return nil, corrupt("scan component index")
			}
			s.components[k].acTbl = br.u(2)
			s.components[k].dcTbl = br.u(2)
		}
		br.u32(val(0), val(1), val(2), bitsOff(3, 3)) // last needed pass
	}
	if hasDRI {
		j.restartInterval = br.u(16)
	}
	for i := range j.scans {
		s := &j.scans[i]
		n := br.u32(val(0), bitsOff(2, 1), bitsOff(4, 4), bitsOff(16, 20))
		if n > 1<<24 {
			return nil, corrupt("reset point count")
		}
		last := int64(-1)
		for k := uint32(0); k < n; k++ {
			v := int64(br.u32(val(0), bitsOff(3, 1), bitsOff(5, 9), bitsOff(28, 41))) + last + 1
			if v >= 3<<26 {
				return nil, corrupt("reset point")
			}
			s.resetPoints = append(s.resetPoints, uint32(v))
			last = v
		}
		n = br.u32(val(0), bitsOff(2, 1), bitsOff(4, 4), bitsOff(16, 20))
		if n > 1<<24 {
			return nil, corrupt("extra zero run count")
		}
		last = -1
		for k := uint32(0); k < n; k++ {
			runs := br.u32(val(1), bitsOff(2, 2), bitsOff(4, 5), bitsOff(8, 20))
			v := int64(br.u32(val(0), bitsOff(3, 1), bitsOff(5, 9), bitsOff(28, 41))) + last + 1
			if v > 3<<26 {
				return nil, corrupt("extra zero run block")
			}
			s.extraZeroRuns = append(s.extraZeroRuns, extraZeroRun{blockIdx: uint32(v), numRuns: runs})
			last = v
		}
		if br.err != nil {
			return nil, br.err
		}
	}
	interSizes := make([]int, numInter)
	for i := range interSizes {
		interSizes[i] = int(br.u(16))
	}
	tailLen := br.u32(val(0), bitsOff(8, 1), bitsOff(16, 257), bitsOff(22, 65793))
	j.hasZeroPadding = br.bool()
	if j.hasZeroPadding {
		n := br.u(24)
		if uint64(n) > uint64(len(payload))*8 {
			return nil, corrupt("padding bit count")
		}
		j.paddingBits = make([]uint8, n)
		for i := range j.paddingBits {
			j.paddingBits[i] = uint8(br.u(1))
		}
	}
	if err := j.checkTableOrder(); err != nil {
		return nil, err
	}
	br.zeroPadToByte()
	if br.err != nil {
		return nil, br.err
	}
	rest := payload[br.bytePos():]
	limit := maxBoxPayload
	plain, err := brotli.Decode(rest, limit)
	if err != nil {
		return nil, corrupt("jbrd Brotli stream: %v", err)
	}
	take := func(n int) ([]byte, error) {
		if n > len(plain) {
			return nil, corrupt("jbrd Brotli stream too short")
		}
		b := plain[:n]
		plain = plain[n:]
		return b, nil
	}
	numICC := 0
	for i, m := range j.appData {
		if j.appMarkerType[i] != appUnknown {
			n := len(m) - 1
			m[1], m[2] = byte(n>>8), byte(n)
			if j.appMarkerType[i] == appICC {
				if len(m) < 17 {
					return nil, corrupt("ICC marker too small")
				}
				m[0] = 0xE2
				copy(m[3:], iccProfileTag)
				numICC++
				m[15] = byte(numICC)
			}
			continue
		}
		b, err := take(len(m))
		if err != nil {
			return nil, err
		}
		copy(m, b)
		if int(m[1])<<8|int(m[2])+1 != len(m) {
			return nil, corrupt("app marker length")
		}
	}
	for i, m := range j.appData {
		switch j.appMarkerType[i] {
		case appICC:
			m[16] = byte(numICC)
		case appExif:
			if len(m) < 3+len(exifTag) {
				return nil, corrupt("Exif marker too small")
			}
			m[0] = 0xE1
			copy(m[3:], exifTag)
		case appXMP:
			if len(m) < 3+len(xmpTag) {
				return nil, corrupt("XMP marker too small")
			}
			m[0] = 0xE1
			copy(m[3:], xmpTag)
		}
	}
	for _, m := range j.comData {
		b, err := take(len(m))
		if err != nil {
			return nil, err
		}
		copy(m, b)
		if int(m[1])<<8|int(m[2])+1 != len(m) {
			return nil, corrupt("COM marker length")
		}
	}
	j.interMarker = make([][]byte, numInter)
	for i, n := range interSizes {
		if j.interMarker[i], err = take(n); err != nil {
			return nil, err
		}
	}
	if j.tailData, err = take(int(tailLen)); err != nil {
		return nil, err
	}
	if len(plain) != 0 {
		return nil, corrupt("excess jbrd Brotli data")
	}
	return j, nil
}

func (j *jpegData) checkTableOrder() error {
	dht, scan := 0, 0
	progressive := false
	var acOK, dcOK [4]bool
	for _, m := range j.markerOrder {
		switch m {
		case 0xC2:
			progressive = true
		case 0xC4:
			for dht < len(j.huffman) {
				h := j.huffman[dht]
				dht++
				if h.slotID&0x10 != 0 {
					acOK[h.slotID&3] = true
				} else {
					dcOK[h.slotID&3] = true
				}
				if h.isLast {
					break
				}
			}
		case 0xDA:
			if scan >= len(j.scans) {
				return corrupt("scan count")
			}
			s := j.scans[scan]
			scan++
			for k := uint32(0); k < s.numComponents; k++ {
				c := s.components[k]
				if (!progressive || s.ss == 0) && !dcOK[c.dcTbl] {
					return corrupt("DC Huffman table used before definition")
				}
				if (!progressive || s.ss != 0 || s.se != 0) && !acOK[c.acTbl] {
					return corrupt("AC Huffman table used before definition")
				}
			}
		}
	}
	return nil
}

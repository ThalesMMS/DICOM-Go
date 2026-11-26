package jpegxl

import "encoding/binary"

// ICC profile stream (ISO/IEC 18181-1 Annex E): the entropy-decoded bytes are
// a command and data stream that predicts the ICC header, tag table and
// content.

const iccHeaderSize = 128

var iccTagStrings = []string{"cprt", "wtpt", "bkpt", "rXYZ", "gXYZ", "bXYZ", "kXYZ", "rTRC", "gTRC", "bTRC", "kTRC", "chad", "desc", "chrm", "dmnd", "dmdd", "lumi"}
var iccTypeStrings = []string{"XYZ ", "desc", "text", "mluc", "para", "curv", "sf32", "gbd "}

func iccVarInt(b []byte, pos *int) uint64 {
	var v uint64
	i := 0
	for ; *pos+i < len(b) && i < 10; i++ {
		v |= uint64(b[*pos+i]&127) << (7 * uint(i))
		if b[*pos+i]&128 == 0 {
			break
		}
	}
	*pos += i + 1
	return v
}

func iccShuffle(data []byte, width int) {
	size := len(data)
	height := (size + width - 1) / width
	out := make([]byte, size)
	s, j := 0, 0
	for i := 0; i < size; i++ {
		out[i] = data[j]
		j += height
		if j >= size {
			s++
			j = s
		}
	}
	copy(data, out)
}

func iccPredict(p1, p2, p3 uint64, order int) uint64 {
	switch order {
	case 0:
		return p1
	case 1:
		return 2*p1 - p2
	case 2:
		return 3*p1 - 3*p2 + p3
	}
	return 0
}

func iccLinearPredict(data []byte, start, i, stride, width, order int) byte {
	pos := start + i
	switch width {
	case 1:
		return byte(iccPredict(uint64(data[pos-stride]), uint64(data[pos-2*stride]), uint64(data[pos-3*stride]), order))
	case 2:
		p := start + i&^1
		get := func(q int) uint64 { return uint64(uint16(data[q])<<8 | uint16(data[q+1])) }
		pred := uint16(iccPredict(get(p-stride), get(p-2*stride), get(p-3*stride), order))
		if i&1 == 1 {
			return byte(pred)
		}
		return byte(pred >> 8)
	}
	p := start + i&^3
	get := func(q int) uint64 {
		if q+4 > pos {
			return 0
		}
		return uint64(binary.BigEndian.Uint32(data[q:]))
	}
	pred := uint32(iccPredict(get(p-stride), get(p-2*stride), get(p-3*stride), order))
	return byte(pred >> (8 * uint(3-i&3)))
}

func unpredictICC(enc []byte) ([]byte, error) {
	bad := corrupt("ICC stream")
	pos := 0
	if pos >= len(enc) {
		return nil, bad
	}
	osize := iccVarInt(enc, &pos)
	if pos >= len(enc) {
		return nil, bad
	}
	csize := iccVarInt(enc, &pos)
	if osize > 1<<28 || csize > uint64(len(enc)) || uint64(pos)+csize > uint64(len(enc)) {
		return nil, bad
	}
	cpos := pos
	cend := cpos + int(csize)
	pos = cend
	out := make([]byte, 0, osize)
	header := make([]byte, iccHeaderSize)
	header[8] = 4
	copy(header[12:], "mntrRGB XYZ ")
	copy(header[36:], "acsp")
	copy(header[68:], []byte{0, 0, 246, 214, 0, 1, 0, 0, 0, 0, 211, 45})
	binary.BigEndian.PutUint32(header, uint32(osize))
	for i := 0; i <= iccHeaderSize; i++ {
		if uint64(len(out)) == osize {
			if cpos != cend || pos != len(enc) {
				return nil, bad
			}
			return out, nil
		}
		if i == iccHeaderSize {
			break
		}
		switch {
		case i == 8 && len(out) >= 8:
			copy(header[80:84], out[4:8])
		case i == 41 && len(out) >= 41:
			if out[40] == 'A' {
				copy(header[41:44], "PPL")
			}
			if out[40] == 'M' {
				copy(header[41:44], "SFT")
			}
		case i == 42 && len(out) >= 42:
			if out[40] == 'S' && out[41] == 'G' {
				copy(header[42:44], "I ")
			}
			if out[40] == 'S' && out[41] == 'U' {
				copy(header[42:44], "NW")
			}
		}
		if pos >= len(enc) {
			return nil, bad
		}
		out = append(out, enc[pos]+header[i])
		pos++
	}
	if cpos >= cend {
		return nil, bad
	}
	appendU32 := func(v uint64) { out = binary.BigEndian.AppendUint32(out, uint32(v)) }
	numTags := iccVarInt(enc, &cpos)
	if numTags != 0 {
		numTags--
		if numTags > 1<<20 {
			return nil, bad
		}
		appendU32(numTags)
		prevStart := uint64(iccHeaderSize) + numTags*12
		prevSize := uint64(0)
		for {
			if uint64(len(out)) > osize || cpos > cend {
				return nil, bad
			}
			if cpos == cend {
				break
			}
			cmd := enc[cpos]
			cpos++
			code := cmd & 63
			var tag string
			switch {
			case code == 0:
			case code == 1:
				if pos+4 > len(enc) {
					return nil, bad
				}
				tag = string(enc[pos : pos+4])
				pos += 4
			case code == 2:
				tag = "rTRC"
			case code == 3:
				tag = "rXYZ"
			case int(code)-4 < len(iccTagStrings):
				tag = iccTagStrings[code-4]
			default:
				return nil, bad
			}
			if code == 0 {
				break
			}
			out = append(out, tag...)
			size := prevSize
			switch tag {
			case "rXYZ", "gXYZ", "bXYZ", "kXYZ", "wtpt", "bkpt", "lumi":
				size = 20
			}
			start := prevStart + prevSize
			if cmd&64 != 0 {
				if cpos >= cend {
					return nil, bad
				}
				start = iccVarInt(enc, &cpos)
			}
			if cmd&128 != 0 {
				if cpos >= cend {
					return nil, bad
				}
				size = iccVarInt(enc, &cpos)
			}
			if start > 1<<32-1 || size > 1<<32-1 {
				return nil, bad
			}
			appendU32(start)
			appendU32(size)
			prevStart, prevSize = start, size
			if code == 2 {
				out = append(out, "gTRC"...)
				appendU32(start)
				appendU32(size)
				out = append(out, "bTRC"...)
				appendU32(start)
				appendU32(size)
			}
			if code == 3 {
				out = append(out, "gXYZ"...)
				appendU32(start + size)
				appendU32(size)
				out = append(out, "bXYZ"...)
				appendU32(start + 2*size)
				appendU32(size)
			}
		}
	}
	for {
		if uint64(len(out)) > osize || cpos > cend {
			return nil, bad
		}
		if cpos == cend {
			break
		}
		cmd := enc[cpos]
		cpos++
		switch {
		case cmd == 1:
			if cpos >= cend {
				return nil, bad
			}
			n := iccVarInt(enc, &cpos)
			if n > uint64(len(enc)-pos) {
				return nil, bad
			}
			out = append(out, enc[pos:pos+int(n)]...)
			pos += int(n)
		case cmd == 2 || cmd == 3:
			if cpos >= cend {
				return nil, bad
			}
			n := iccVarInt(enc, &cpos)
			if n > uint64(len(enc)-pos) {
				return nil, bad
			}
			buf := append([]byte{}, enc[pos:pos+int(n)]...)
			iccShuffle(buf, map[byte]int{2: 2, 3: 4}[cmd])
			out = append(out, buf...)
			pos += int(n)
		case cmd == 4:
			if cpos+2 > cend {
				return nil, bad
			}
			flags := enc[cpos]
			cpos++
			width := int(flags&3) + 1
			order := int(flags&12) >> 2
			if width == 3 || order == 3 {
				return nil, bad
			}
			stride := uint64(width)
			if flags&16 != 0 {
				if cpos >= cend {
					return nil, bad
				}
				stride = iccVarInt(enc, &cpos)
				if stride < uint64(width) {
					return nil, bad
				}
			}
			if len(out) == 0 || uint64(len(out)-1)>>2 < stride {
				return nil, bad
			}
			if cpos >= cend {
				return nil, bad
			}
			n := iccVarInt(enc, &cpos)
			if n > uint64(len(enc)-pos) {
				return nil, bad
			}
			buf := append([]byte{}, enc[pos:pos+int(n)]...)
			if width > 1 {
				iccShuffle(buf, width)
			}
			start := len(out)
			for i := range buf {
				out = append(out, iccLinearPredict(out, start, i, int(stride), width, order)+buf[i])
			}
			pos += int(n)
		case cmd == 10:
			out = append(out, "XYZ "...)
			out = append(out, 0, 0, 0, 0)
			if pos+12 > len(enc) {
				return nil, bad
			}
			out = append(out, enc[pos:pos+12]...)
			pos += 12
		case cmd >= 16 && int(cmd)-16 < len(iccTypeStrings):
			out = append(out, iccTypeStrings[cmd-16]...)
			out = append(out, 0, 0, 0, 0)
		default:
			return nil, bad
		}
	}
	if pos != len(enc) || uint64(len(out)) != osize {
		return nil, bad
	}
	return out, nil
}

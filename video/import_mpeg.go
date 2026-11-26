package video

import (
	"bytes"
	"context"
	"encoding/binary"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// inspectMPEG reads Program Stream packet framing and MPEG-2 sequence/picture
// headers. It does not decode slices or audio. Other streams are refused rather
// than being relabeled as this single-video subset.
func inspectMPEG(ctx context.Context, b []byte, limits ImportLimits) (SourceInfo, error) {
	var es []byte
	packets := 0
	for at := 0; at < len(b); {
		if e := ctx.Err(); e != nil {
			return SourceInfo{}, e
		}
		packets++
		if packets > 4096 {
			return SourceInfo{}, ErrTooManyFragments
		}
		if at+4 > len(b) || !bytes.Equal(b[at:at+3], []byte{0, 0, 1}) {
			return SourceInfo{}, importError("MPEG packet framing")
		}
		code := b[at+3]
		if code == 0xb9 {
			at += 4
			if at != len(b) {
				return SourceInfo{}, ErrImportMedia
			}
			break
		}
		if code == 0xba {
			if at+14 > len(b) || b[at+4]&0xc0 != 0x40 || b[at+4]&4 == 0 || b[at+6]&4 == 0 || b[at+8]&4 == 0 || b[at+9]&1 == 0 || b[at+12]&3 != 3 || b[at+13]&0xf8 != 0xf8 {
				return SourceInfo{}, importError("MPEG2 pack header")
			}
			n := 14 + int(b[at+13]&7)
			if n > len(b)-at {
				return SourceInfo{}, ErrImportMedia
			}
			at += n
			continue
		}
		if at+6 > len(b) {
			return SourceInfo{}, ErrImportMedia
		}
		size := int(binary.BigEndian.Uint16(b[at+4:]))
		end := at + 6 + size
		if size == 0 && code == 0xe0 {
			var e error
			end, e = nextSystemStart(ctx, b, at+6)
			if e != nil {
				return SourceInfo{}, e
			}
		}
		if end > len(b) || end < at+6 {
			return SourceInfo{}, ErrImportMedia
		}
		switch code {
		case 0xbb, 0xbe: // system header and padding, retained in original payload
		case 0xe0:
			start := at + 6
			if start+3 > end || b[start]&0xc0 != 0x80 {
				return SourceInfo{}, importError("MPEG2 PES header")
			}
			head := int(b[start+2])
			start += 3 + head
			if start > end {
				return SourceInfo{}, ErrImportMedia
			}
			if int64(end-start) > limits.MaxBytes-int64(len(es)) {
				return SourceInfo{}, ErrMediaTooLarge
			}
			es = append(es, b[start:end]...)
		default:
			return SourceInfo{}, importError("multiple or unsupported ProgramStream tracks")
		}
		at = end
	}
	if len(es) == 0 {
		return SourceInfo{}, ErrImportMedia
	}
	var result SourceInfo
	sequence, extension := false, false
	level := byte(0)
	pictures := 0
	baseRate := 0.0
	aspect := byte(0)
	for at := 0; at+4 <= len(es); {
		if e := ctx.Err(); e != nil {
			return SourceInfo{}, e
		}
		position, e := importStartCode(ctx, es, at)
		if e != nil {
			return SourceInfo{}, e
		}
		if position < 0 {
			break
		}
		at = position
		code := es[at+3]
		next, e := importStartCode(ctx, es, at+4)
		if e != nil {
			return SourceInfo{}, e
		}
		end := len(es)
		if next >= 0 {
			end = next
		}
		v := es[at+4 : end]
		switch code {
		case 0xb3:
			if len(v) < 8 {
				return SourceInfo{}, ErrImportMedia
			}
			columns := int(v[0])<<4 | int(v[1]>>4)
			rows := int(v[1]&15)<<8 | int(v[2])
			rate, ok := mpegRate(v[3] & 15)
			if !ok || columns < 1 || rows < 1 {
				return SourceInfo{}, ErrImportMedia
			}
			if sequence && (result.Columns != columns || result.Rows != rows) {
				return SourceInfo{}, importError("changing MPEG geometry")
			}
			if !sequence {
				result.Columns, result.Rows, result.FrameRate = columns, rows, rate
			}
			if sequence && aspect != v[3]>>4 {
				return SourceInfo{}, importError("changing MPEG aspect")
			}
			aspect = v[3] >> 4
			sequence = true
			baseRate = rate
		case 0xb5:
			if len(v) == 0 {
				return SourceInfo{}, ErrImportMedia
			}
			if v[0]>>4 == 1 {
				if !sequence || len(v) < 6 {
					return SourceInfo{}, ErrImportMedia
				}
				profileLevel := (v[0]&15)<<4 | v[1]>>4
				profile := (profileLevel >> 4) & 7
				newLevel := profileLevel & 15
				chroma := (v[1] >> 1) & 3
				progressive := v[1]&8 != 0
				if profileLevel&128 != 0 || profile != 4 || (newLevel != 8 && newLevel != 4) || chroma != 1 || !progressive || v[1]&1 != 0 || v[2]&0xe0 != 0 {
					return SourceInfo{}, importError("MPEG2 profile/chroma/field geometry")
				}
				rate := baseRate
				rate *= float64(((v[5]>>5)&3)+1) / float64((v[5]&31)+1)
				if extension && (level != newLevel || result.FrameRate != rate) {
					return SourceInfo{}, importError("changing MPEG timing/profile")
				}
				result.FrameRate = rate
				level = newLevel
				extension = true
			}
		case 0x00:
			if !extension || len(v) < 2 {
				return SourceInfo{}, ErrImportMedia
			}
			pictureType := (v[1] >> 3) & 7
			if pictureType < 1 || pictureType > 3 {
				return SourceInfo{}, ErrImportMedia
			}
			pictures++
			if pictures > limits.MaxFrames {
				return SourceInfo{}, ErrMediaTooLarge
			}
		}
		at = end
	}
	if !sequence || !extension || pictures == 0 {
		return SourceInfo{}, importError("missing MPEG2 sequence/pictures")
	}
	if level == 8 {
		rowsCap := 480
		if result.FrameRate == 25 {
			rowsCap = 576
		}
		if result.Columns > 720 || result.Rows > rowsCap || (result.FrameRate != 25 && result.FrameRate != 30 && result.FrameRate != 30000.0/1001) {
			return SourceInfo{}, importError("DICOM MPEG2 Main Level dimensions/timing")
		}
	} else {
		if aspect != 3 || !(result.Rows == 720 && result.Columns == 1280 || result.Rows == 1080 && result.Columns == 1920) || !(result.FrameRate == 25 || result.FrameRate == 30 || result.FrameRate == 30000.0/1001 || result.Rows == 720 && (result.FrameRate == 50 || result.FrameRate == 60 || result.FrameRate == 60000.0/1001)) {
			return SourceInfo{}, importError("DICOM MPEG2 High Level dimensions/aspect/timing")
		}
	}
	result.Frames = pictures
	result.TransferSyntax = transfer.MPEG2MPML
	if level != 8 {
		result.TransferSyntax = transfer.MPEG2MPHL
	}
	result.Container = ContainerMPEGProgram
	result.CompressionMethod = "ISO_13818_2"
	return result, nil
}
func nextSystemStart(ctx context.Context, b []byte, start int) (int, error) {
	for at := start; at+4 <= len(b); {
		n, e := importStartCode(ctx, b, at)
		if e != nil {
			return 0, e
		}
		if n < 0 {
			return len(b), nil
		}
		if b[n+3] >= 0xb9 {
			return n, nil
		}
		at = n + 4
	}
	return len(b), nil
}

// importStartCode searches bounded chunks, retaining two overlap bytes so a
// prefix crossing a chunk boundary is found. Only complete four-byte codes
// are returned, including when the input ends with a truncated prefix.
func importStartCode(ctx context.Context, b []byte, start int) (int, error) {
	for at := start; at+4 <= len(b); {
		if e := ctx.Err(); e != nil {
			return 0, e
		}
		end := len(b)
		if len(b)-at > 4096 {
			end = at + 4096
		}
		n := bytes.Index(b[at:end], []byte{0, 0, 1})
		if n >= 0 {
			n += at
			if n+4 > len(b) {
				return -1, nil
			}
			return n, nil
		}
		if end == len(b) {
			break
		}
		at = end - 2
	}
	return -1, nil
}
func mpegRate(code byte) (float64, bool) {
	switch code {
	case 1:
		return 24000.0 / 1001, true
	case 2:
		return 24, true
	case 3:
		return 25, true
	case 4:
		return 30000.0 / 1001, true
	case 5:
		return 30, true
	case 6:
		return 50, true
	case 7:
		return 60000.0 / 1001, true
	case 8:
		return 60, true
	}
	return 0, false
}

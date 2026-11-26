package encapimage

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"github.com/ThalesMMS/dicom-go/transfer"
	"strconv"
)

type portableTokens struct {
	data  []byte
	at    int
	ctx   context.Context
	limit int
}

func portableSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n' || b == '\v' || b == '\f'
}
func (p *portableTokens) next() (string, error) {
	for p.at < len(p.data) {
		if p.limit > 0 && p.at >= p.limit {
			return "", ErrImportLimit
		}
		if p.at&4095 == 0 && p.ctx != nil {
			if e := p.ctx.Err(); e != nil {
				return "", e
			}
		}
		if portableSpace(p.data[p.at]) {
			p.at++
			continue
		}
		if p.data[p.at] == '#' {
			for p.at < len(p.data) && p.data[p.at] != '\n' {
				if p.limit > 0 && p.at >= p.limit {
					return "", ErrImportLimit
				}
				if p.at&4095 == 0 && p.ctx != nil {
					if e := p.ctx.Err(); e != nil {
						return "", e
					}
				}
				p.at++
			}
			continue
		}
		break
	}
	start := p.at
	for p.at < len(p.data) && !portableSpace(p.data[p.at]) && p.data[p.at] != '#' {
		p.at++
		if p.at-start > 64 {
			return "", ErrMalformedImage
		}
	}
	if start == p.at {
		return "", ErrMalformedImage
	}
	return string(p.data[start:p.at]), nil
}
func (p *portableTokens) number() (int, error) {
	s, e := p.next()
	if e != nil {
		return 0, e
	}
	n, e := strconv.ParseUint(s, 10, 32)
	if e != nil || n > 65535 {
		return 0, ErrMalformedImage
	}
	return int(n), nil
}
func binarySeparator(b []byte, at int) (int, error) {
	if at >= len(b) || !portableSpace(b[at]) {
		return 0, ErrMalformedImage
	}
	if b[at] == '\r' && at+1 < len(b) && b[at+1] == '\n' {
		return at + 2, nil
	}
	return at + 1, nil
}
func readPortable(ctx context.Context, b []byte, format Format, limits ImportLimits) (sourceFrame, error) {
	if format == PGX {
		return readPGX(ctx, b, limits)
	}
	p := portableTokens{data: b, ctx: ctx, limit: 65536}
	magic, e := p.next()
	if e != nil {
		return sourceFrame{}, e
	}
	if magic != "P2" && magic != "P3" && magic != "P5" && magic != "P6" {
		return sourceFrame{}, ErrUnsupportedImage
	}
	cols, e := p.number()
	if e != nil {
		return sourceFrame{}, e
	}
	rows, e := p.number()
	if e != nil {
		return sourceFrame{}, e
	}
	maximum, e := p.number()
	if e != nil {
		return sourceFrame{}, e
	}
	if maximum != 255 && maximum != 65535 {
		return sourceFrame{}, fmt.Errorf("%w: PNM maxval must be 255 or 65535", ErrUnsupportedImage)
	}
	if p.at > 65536 {
		return sourceFrame{}, ErrImportLimit
	}
	p.limit = 0
	l := imageLayout{rows: rows, columns: cols, samples: 1, bits: 8, photometric: "MONOCHROME2", process: "native", lossless: true}
	if magic == "P3" || magic == "P6" {
		l.samples = 3
		l.photometric = "RGB"
	}
	if maximum == 65535 {
		l.bits = 16
	}
	n, e := admitImage(l, limits)
	if e != nil {
		return sourceFrame{}, e
	}
	data := make([]byte, int(n))
	width := 1
	if l.bits > 8 {
		width = 2
	}
	if magic == "P5" || magic == "P6" {
		at, e := binarySeparator(b, p.at)
		if e != nil || int64(len(b)-at) != n {
			return sourceFrame{}, ErrMalformedImage
		}
		copy(data, b[at:])
		if width == 2 {
			for pos := 0; pos < len(data); pos += 2 {
				if pos&65535 == 0 {
					if e := ctx.Err(); e != nil {
						return sourceFrame{}, e
					}
				}
				v := binary.BigEndian.Uint16(data[pos:])
				binary.LittleEndian.PutUint16(data[pos:], v)
			}
		}
	} else {
		for pos := 0; pos < len(data); pos += width {
			if pos&4095 == 0 {
				if e := ctx.Err(); e != nil {
					return sourceFrame{}, e
				}
			}
			v, e := p.number()
			if e != nil || v > maximum {
				return sourceFrame{}, ErrMalformedImage
			}
			if width == 1 {
				data[pos] = byte(v)
			} else {
				binary.LittleEndian.PutUint16(data[pos:], uint16(v))
			}
		}
		if _, e = p.next(); e == nil || p.at != len(p.data) {
			return sourceFrame{}, ErrMalformedImage
		}
	}
	return sourceFrame{imageLayout: l, data: data, syntax: transfer.ExplicitVRLittleEndian, format: PNM}, nil
}
func readPGX(ctx context.Context, b []byte, limits ImportLimits) (sourceFrame, error) {
	end := bytes.IndexByte(b, '\n')
	if end < 0 || end > 256 {
		return sourceFrame{}, ErrMalformedImage
	}
	p := portableTokens{data: b[:end], ctx: ctx}
	magic, e := p.next()
	if e != nil || magic != "PG" {
		return sourceFrame{}, ErrMalformedImage
	}
	order, e := p.next()
	if e != nil || (order != "ML" && order != "LM") {
		return sourceFrame{}, ErrUnsupportedImage
	}
	sign, e := p.next()
	if e != nil || (sign != "+" && sign != "-") {
		return sourceFrame{}, ErrMalformedImage
	}
	bits, e := p.number()
	if e != nil || bits < 1 || bits > 16 {
		return sourceFrame{}, ErrUnsupportedImage
	}
	cols, e := p.number()
	if e != nil {
		return sourceFrame{}, e
	}
	rows, e := p.number()
	if e != nil {
		return sourceFrame{}, e
	}
	if _, e = p.next(); e == nil || p.at != len(p.data) {
		return sourceFrame{}, ErrMalformedImage
	}
	l := imageLayout{rows: rows, columns: cols, samples: 1, bits: bits, photometric: "MONOCHROME2", process: "native", lossless: true}
	if sign == "-" {
		l.signed = 1
	}
	n, e := admitImage(l, limits)
	if e != nil {
		return sourceFrame{}, e
	}
	if int64(len(b)-end-1) != n {
		return sourceFrame{}, ErrMalformedImage
	}
	data := append([]byte(nil), b[end+1:]...)
	if bits > 8 && order == "ML" {
		for pos := 0; pos < len(data); pos += 2 {
			if pos&65535 == 0 {
				if e := ctx.Err(); e != nil {
					return sourceFrame{}, e
				}
			}
			v := binary.BigEndian.Uint16(data[pos:])
			binary.LittleEndian.PutUint16(data[pos:], v)
		}
	}
	if e = checkCodes(ctx, data, l); e != nil {
		return sourceFrame{}, e
	}
	return sourceFrame{imageLayout: l, data: data, syntax: transfer.ExplicitVRLittleEndian, format: PGX}, nil
}

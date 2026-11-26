package encapimage

import (
	"context"
	"encoding/binary"
	"fmt"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func readCompressed(ctx context.Context, b []byte, format Format, opts ImageOptions, limits ImportLimits) (sourceFrame, error) {
	if opts.J2KLossless && format != J2K {
		return sourceFrame{}, ErrInvalidOption
	}
	if format == JPEG {
		stream, err := parseJPEGStreamLimited(ctx, b, 4096)
		if err != nil {
			return sourceFrame{}, err
		}
		syntax, process, err := jpegTransferSyntax(stream, opts.AllowRetiredProgressive)
		if err != nil {
			return sourceFrame{}, err
		}
		photo, sub, err := jpegPhotometric(stream, process == "lossless")
		if err != nil {
			return sourceFrame{}, err
		}
		l := imageLayout{rows: stream.rows, columns: stream.columns, bits: stream.precision, samples: len(stream.components), photometric: photo, process: process, lossless: process == "lossless", method: "ISO_10918_1"}
		if _, err = admitImage(l, limits); err != nil {
			return sourceFrame{}, err
		}
		rep := Report{Process: process, TransferSyntaxUID: syntax.UID, PhotometricInterpretation: photo, Subsampling: sub}
		data, icc := strippedStream(b, stream, opts.JPEGOptions, &rep)
		rep.TrailingBytesRemoved = len(b) - stream.eoi
		if err = ctx.Err(); err != nil {
			return sourceFrame{}, err
		}
		l.payloadBytes = len(data)
		return sourceFrame{imageLayout: l, data: data, syntax: syntax, icc: icc, report: rep, format: format}, nil
	}
	if format == JLS {
		h, err := jpegls.InspectCodestream(ctx, b, limits.MaxInputBytes)
		if err != nil {
			return sourceFrame{}, err
		}
		if h.MaxValue != (1<<uint(h.Precision))-1 {
			return sourceFrame{}, fmt.Errorf("%w: custom JPEG-LS MAXVAL", ErrUnsupportedImage)
		}
		l := imageLayout{rows: h.Rows, columns: h.Columns, bits: h.Precision, samples: h.Components, photometric: "MONOCHROME2", process: "jpeg-ls", lossless: h.Near == 0, method: "ISO_14495_1", payloadBytes: len(b)}
		if l.samples == 3 {
			l.photometric = "RGB"
		}
		if _, err = admitImage(l, limits); err != nil {
			return sourceFrame{}, err
		}
		syntax := transfer.JPEGLSLossless
		if !l.lossless {
			syntax = transfer.JPEGLSNearLossless
		}
		return sourceFrame{imageLayout: l, data: b, syntax: syntax, format: format, report: Report{Process: l.process}}, nil
	}
	if err := encapsulated.ValidateFrame(ctx, b, encapsulated.JPEG2000); err != nil {
		return sourceFrame{}, err
	}
	l, err := j2kLayout(ctx, b, opts.J2KLossless)
	if err != nil {
		return sourceFrame{}, err
	}
	if _, err = admitImage(l, limits); err != nil {
		return sourceFrame{}, err
	}
	syntax := transfer.JPEG2000
	if opts.J2KLossless {
		syntax = transfer.JPEG2000LosslessOnly
	}
	return sourceFrame{imageLayout: l, data: b, syntax: syntax, format: J2K, report: Report{Process: "jpeg2000"}}, nil
}
func j2kLayout(ctx context.Context, b []byte, declared bool) (imageLayout, error) {
	l := imageLayout{process: "jpeg2000", lossless: declared, method: "ISO_15444_1", payloadBytes: len(b)}
	var siz, cod, qcd bool
	var transform, mct int
	tileCount := uint64(0)
	seen := map[uint16]bool{}
	for at := 2; at+2 <= len(b); {
		if err := ctx.Err(); err != nil {
			return l, err
		}
		if b[at] != 255 {
			return l, ErrMalformedImage
		}
		code := b[at+1]
		if code == 0x90 {
			if !siz || !cod || !qcd {
				return l, ErrMalformedImage
			}
			// Every tile header is checked; coding overrides/ROI/progression changes
			// are deliberately outside this importer rather than silently relabeled.
			for at+12 <= len(b) && b[at] == 255 && b[at+1] == 0x90 {
				if err := ctx.Err(); err != nil {
					return l, err
				}
				if binary.BigEndian.Uint16(b[at+2:]) != 10 {
					return l, ErrMalformedImage
				}
				psot := uint64(binary.BigEndian.Uint32(b[at+6:]))
				if psot < 14 || psot > uint64(len(b)-at) {
					return l, ErrUnsupportedImage
				}
				tile := binary.BigEndian.Uint16(b[at+4:])
				if uint64(tile) >= tileCount || seen[tile] || b[at+10] != 0 || b[at+11] != 1 {
					return l, fmt.Errorf("%w: JPEG2000 tile parts", ErrUnsupportedImage)
				}
				seen[tile] = true
				end := at + int(psot)
				h := at + 12
				for h+2 <= end && !(b[h] == 255 && b[h+1] == 0x93) {
					if b[h] != 255 || h+4 > end {
						return l, ErrMalformedImage
					}
					marker := b[h+1]
					if marker != 0x64 {
						return l, fmt.Errorf("%w: JPEG2000 tile header override", ErrUnsupportedImage)
					}
					n := int(binary.BigEndian.Uint16(b[h+2:]))
					if n < 2 || n > end-h-2 {
						return l, ErrMalformedImage
					}
					h += n + 2
				}
				if h+2 > end {
					return l, ErrMalformedImage
				}
				at = end
			}
			if uint64(len(seen)) != tileCount {
				return l, ErrMalformedImage
			}
			if at+2 > len(b) || b[at] != 255 || b[at+1] != 0xd9 {
				return l, ErrMalformedImage
			}
			break
		}
		if at+4 > len(b) {
			return l, ErrMalformedImage
		}
		n := int(binary.BigEndian.Uint16(b[at+2:]))
		if n < 2 || n > len(b)-at-2 {
			return l, ErrMalformedImage
		}
		v := b[at+4 : at+n+2]
		switch code {
		case 0x51:
			if siz || len(v) < 36 || binary.BigEndian.Uint16(v) != 0 {
				return l, ErrUnsupportedImage
			}
			siz = true
			xs, ys, xo, yo := binary.BigEndian.Uint32(v[2:]), binary.BigEndian.Uint32(v[6:]), binary.BigEndian.Uint32(v[10:]), binary.BigEndian.Uint32(v[14:])
			if xs <= xo || ys <= yo || xo != 0 || yo != 0 {
				return l, ErrUnsupportedImage
			}
			l.columns, l.rows = int(xs-xo), int(ys-yo)
			xt, yt, xto, yto := uint64(binary.BigEndian.Uint32(v[18:])), uint64(binary.BigEndian.Uint32(v[22:])), binary.BigEndian.Uint32(v[26:]), binary.BigEndian.Uint32(v[30:])
			if xt == 0 || yt == 0 || xto != 0 || yto != 0 {
				return l, ErrUnsupportedImage
			}
			tileCount = ((uint64(xs) + xt - 1) / xt) * ((uint64(ys) + yt - 1) / yt)
			if tileCount == 0 || tileCount > 4096 {
				return l, ErrImportLimit
			}
			l.samples = int(binary.BigEndian.Uint16(v[34:]))
			if (l.samples != 1 && l.samples != 3) || len(v) != 36+3*l.samples {
				return l, ErrUnsupportedImage
			}
			l.bits = int(v[36]&127) + 1
			l.signed = int(v[36] >> 7)
			for i := 0; i < l.samples; i++ {
				p := 36 + i*3
				if v[p] != v[36] || v[p+1] != 1 || v[p+2] != 1 {
					return l, ErrUnsupportedImage
				}
			}
		case 0x52:
			if cod || len(v) != 10 || v[0] != 0 || v[1] > 4 {
				return l, ErrUnsupportedImage
			}
			cod = true
			layers := int(binary.BigEndian.Uint16(v[2:]))
			mct = int(v[4])
			transform = int(v[9])
			if layers < 1 || layers > 32 || mct > 1 || transform > 1 {
				return l, ErrUnsupportedImage
			}
			if declared && transform != 1 {
				return l, ErrInvalidOption
			}
		case 0x5c:
			if qcd || len(v) < 2 {
				return l, ErrMalformedImage
			}
			qcd = true
			if declared && v[0]&31 != 0 {
				return l, ErrInvalidOption
			}
		case 0x64: // COM is copied unchanged; this importer is not de-identification.
		default:
			return l, fmt.Errorf("%w: JPEG2000 coding marker", ErrUnsupportedImage)
		}
		at += n + 2
	}
	if !siz || !cod || !qcd {
		return l, ErrMalformedImage
	}
	l.lossyUnknown = !declared && transform == 1
	l.photometric = "MONOCHROME2"
	if l.samples == 3 {
		l.photometric = "RGB"
		if mct == 1 {
			l.photometric = "YBR_ICT"
			if transform == 1 {
				l.photometric = "YBR_RCT"
			}
		}
	}
	return l, nil
}

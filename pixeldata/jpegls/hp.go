package jpegls

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func validateHPHeader(h frameHeader) error {
	if h.hpTransform < 1 || h.hpTransform > 3 || h.components != 3 ||
		(h.precision != 8 && h.precision != 16) || h.near != 0 ||
		(h.ilv != 1 && h.ilv != 2) || h.preset.maxval != (1<<h.precision)-1 {
		return fmt.Errorf("%w: HP transforms require RGB8/RGB16, NEAR=0, ILV=1/2 and full-range MAXVAL", ErrInvalidCodestream)
	}
	return nil
}

func validateHPMetadata(h frameHeader, m pixeldata.Metadata) error {
	if m.SamplesPerPixel != 3 || m.PixelRepresentation != 0 || m.PlanarConfiguration != 0 ||
		!strings.EqualFold(strings.TrimSpace(m.PhotometricInterpretation), "RGB") ||
		int(m.BitsAllocated) != h.precision || int(m.BitsStored) != h.precision || m.HighBit != m.BitsStored-1 {
		return fmt.Errorf("%w: HP transform requires canonical unsigned RGB at the codestream precision", pixeldata.ErrIncompatiblePixelData)
	}
	return nil
}

func hpDiagnostic(hp int) string {
	return []string{"", pixeldata.DiagnosticJPEGLSHP1, pixeldata.DiagnosticJPEGLSHP2, pixeldata.DiagnosticJPEGLSHP3}[hp]
}

// inverseHP reverses the modular channel differences and integer lifting.
// All arithmetic uses full integers before reduction modulo 2^precision.
// HP3's middle/last channels are B-G and R-G biased by half the range;
// its first channel lifts G by their quarter-sum, biased by a quarter range.
// These equations are independently checked against synthetic CharLS output.
func inverseHP(ctx context.Context, native []byte, precision, hp int) error {
	step := precision / 8
	if step != 1 && step != 2 || hp < 1 || hp > 3 || len(native)%(3*step) != 0 {
		return fmt.Errorf("%w: HP sample layout", ErrInvalidCodestream)
	}
	mask, half, quarter := (1<<precision)-1, 1<<(precision-1), 1<<(precision-2)
	for offset := 0; offset < len(native); offset += 3 * step {
		if offset%(1024*3*step) == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		x, y, z := int(native[offset]), int(native[offset+step]), int(native[offset+2*step])
		if step == 2 {
			x = int(binary.LittleEndian.Uint16(native[offset:]))
			y = int(binary.LittleEndian.Uint16(native[offset+step:]))
			z = int(binary.LittleEndian.Uint16(native[offset+2*step:]))
		}
		var r, g, b int
		switch hp {
		case 1:
			g = y
			r, b = (x+g-half)&mask, (z+g-half)&mask
		case 2:
			g = y
			r = (x + g - half) & mask
			b = (z + (r+g)/2 - half) & mask
		case 3:
			g = (x - (y+z)/4 + quarter) & mask
			r, b = (z+g-half)&mask, (y+g-half)&mask
		}
		if step == 1 {
			native[offset], native[offset+1], native[offset+2] = byte(r), byte(g), byte(b)
		} else {
			binary.LittleEndian.PutUint16(native[offset:], uint16(r))
			binary.LittleEndian.PutUint16(native[offset+2:], uint16(g))
			binary.LittleEndian.PutUint16(native[offset+4:], uint16(b))
		}
	}
	return ctx.Err()
}

package jpegls

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"strings"

	"github.com/ThalesMMS/dicom-go/internal/decoderecovery"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
)

var (
	ErrInvalidCodestream          = errors.New("dicom: invalid JPEG-LS codestream")
	ErrUnsupportedBitsAllocated   = errors.New("dicom: unsupported JPEG-LS BitsAllocated")
	ErrUnsupportedSamplesPerPixel = errors.New("dicom: unsupported JPEG-LS SamplesPerPixel")
)

type frameHeader struct {
	precision    int
	rows         int
	columns      int
	components   int
	componentIDs []byte
	near         int
	ilv          int
	preset       presetCodingParameters
	// explicitThresholds reports an LSE segment that sets T1, T2 or T3.
	explicitThresholds bool
	hpTransform        int
}

type presetCodingParameters struct {
	near   int
	maxval int
	t1     int
	t2     int
	t3     int
	reset  int
}

type scanData struct {
	componentIDs []byte
	entropy      []byte
}

const maxJPEGLSWorkingSetBytes = 512 << 20

// Codec decodes qualified JPEG-LS frames. Its zero value and New enforce NEAR=0;
// only NewNearLossless enables the separately registered .81 profiles.
type Codec struct{ nearLossless bool }

// New returns a JPEG-LS Lossless decoder for the package's qualified subset.
func New() *Codec {
	return &Codec{}
}

// Register registers the JPEG-LS Lossless decoder in registry.
func Register(registry pixeldata.Registry) error {
	if registry == nil {
		return pixeldata.ErrCodecRegistryNil
	}
	return registry.RegisterCodec(UID, New())
}

// RegisterDefault registers the decoder in pixeldata.DefaultRegistry.
func RegisterDefault() error {
	return pixeldata.RegisterCodec(UID, New())
}

func (c *Codec) Decode(pixel pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
	return c.DecodeContext(context.Background(), pixel, obj)
}

func (c *Codec) DecodeContext(ctx context.Context, pixel pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.Frames{}, err
	}
	if !pixel.Encapsulated {
		return pixeldata.Frames{}, fmt.Errorf("%w: JPEG-LS requires encapsulated pixel data", pixeldata.ErrIncompatiblePixelData)
	}
	metadata, err := pixeldata.ExtractMetadata(obj)
	if err != nil {
		return pixeldata.Frames{}, err
	}
	if err := c.validateDecodeMetadata(metadata); err != nil {
		return pixeldata.Frames{}, err
	}
	codestreams, err := encapsulated.FromFragments(ctx, pixel.Sequence, obj, metadata.NumberOfFrames, encapsulated.JPEGLS, encapsulated.Limits{})
	if err != nil {
		if errors.Is(err, encapsulated.ErrFrameCount) {
			err = errors.Join(pixeldata.ErrPixelDataSizeMismatch, err)
		}
		return pixeldata.Frames{}, err
	}
	frames := make([][]byte, codestreams.Len())
	recovery := decoderecovery.From(ctx)
	var result pixeldata.Frames
	if codestreams.Recovery().BasicOffsetTableIgnored {
		result.AddDiagnostic(pixeldata.DiagnosticBasicOffsetTableIgnored)
	}
	for i := range frames {
		codestream, err := codestreams.Frame(ctx, i)
		if err != nil {
			return pixeldata.Frames{}, err
		}
		decoded, err := c.decodeNativeFrame(ctx, codestream.Data, metadata, nil, recovery, &result)
		if err != nil {
			return pixeldata.Frames{}, fmt.Errorf("decode JPEG-LS frame %d: %w", i, err)
		}
		frames[i] = decoded
	}
	result.Rows, result.Columns, result.Data = int(metadata.Rows), int(metadata.Columns), frames
	return result, nil
}

func (c *Codec) validateDecodeMetadata(metadata pixeldata.Metadata) error {
	if _, err := validateEncoderInput(metadata); err != nil {
		return err
	}
	if c.nearLossless && strings.EqualFold(strings.TrimSpace(metadata.PhotometricInterpretation), "PALETTE COLOR") {
		return fmt.Errorf("%w: palette color requires the JPEG-LS Lossless transfer syntax", pixeldata.ErrUnsupportedPhotometricInterpretation)
	}
	return validateDecodeRequestAllocation(metadata)
}

func (c *Codec) decodeNativeFrame(ctx context.Context, stream []byte, metadata pixeldata.Metadata, dst []byte, recovery decoderecovery.Options, result *pixeldata.Frames) ([]byte, error) {
	pad, padded := encapsulated.TrailingPad(stream)
	if padded {
		stream = stream[:len(stream)-1]
	}
	pixels, jai, hp, err := decodeFrameRecoveries(ctx, stream, metadata, c.nearLossless, recovery.JPEGLSJAIThresholds, recovery.JPEGLSHPColorTransforms, dst)
	if err != nil {
		return pixels, err
	}
	// PS3.5 A.4 does not fix the value of the Item pad byte.
	if padded && pad != 0 {
		result.AddDiagnostic(pixeldata.DiagnosticNonZeroFramePad)
	}
	if jai {
		result.AddDiagnostic(pixeldata.DiagnosticJPEGLSJAIThresholds)
	}
	if hp != 0 {
		result.AddDiagnostic(hpDiagnostic(hp))
		result.AddDiagnostic(pixeldata.DiagnosticJPEGLSHPBackendPureGo)
	}
	return pixels, nil
}

func decodeFrame(stream []byte, metadata pixeldata.Metadata) ([]byte, error) {
	return decodeFrameContext(context.Background(), stream, metadata)
}

func decodeFrameContext(ctx context.Context, stream []byte, metadata pixeldata.Metadata) ([]byte, error) {
	return decodeFrameModeContext(ctx, stream, metadata, false)
}

func decodeFrameModeContext(ctx context.Context, stream []byte, metadata pixeldata.Metadata, nearLossless bool) ([]byte, error) {
	decoded, _, err := decodeFrameRecoveryContext(ctx, stream, metadata, nearLossless, false)
	return decoded, err
}

// decodeFrameRecoveryContext decodes a frame; with jaiThresholds, a frame of
// more than 12 bits without LSE thresholds uses the JAI encoder's
// thresholds, and jai reports it.
func decodeFrameRecoveryContext(ctx context.Context, stream []byte, metadata pixeldata.Metadata, nearLossless, jaiThresholds bool) (decoded []byte, jai bool, err error) {
	decoded, jai, _, err = decodeFrameRecoveries(ctx, stream, metadata, nearLossless, jaiThresholds, false, nil)
	return
}

func decodeFrameRecoveries(ctx context.Context, stream []byte, metadata pixeldata.Metadata, nearLossless, jaiThresholds, allowHP bool, dst []byte) (decoded []byte, jai bool, hp int, err error) {
	header, scans, err := splitScansWithHP(stream, allowHP)
	if err != nil {
		return nil, false, 0, err
	}
	if header.hpTransform != 0 {
		if nearLossless {
			return nil, false, 0, fmt.Errorf("%w: HP color transforms require the Lossless syntax", ErrInvalidCodestream)
		}
		if err := validateHPMetadata(header, metadata); err != nil {
			return nil, false, 0, err
		}
	}
	if jaiThresholds && header.precision > 12 && !header.explicitThresholds {
		preset, err := withJAIThresholds(header.preset, header.precision)
		if err != nil {
			return nil, false, 0, err
		}
		header.preset, jai = preset, true
	}
	decoded, err = decodeSplitFrame(ctx, header, scans, metadata, nearLossless, dst)
	if err == nil && header.hpTransform != 0 {
		err = inverseHP(ctx, decoded, header.precision, header.hpTransform)
	}
	return decoded, jai && err == nil, header.hpTransform, err
}

// withJAIThresholds sets the thresholds JAI ImageIO computes for a frame of
// precision bits: the T.87 defaults with FACTOR = (MAXVAL + 128) / 256 for
// MAXVAL = 2^precision - 1, without the standard's min(MAXVAL, 4095).
func withJAIThresholds(preset presetCodingParameters, precision int) (presetCodingParameters, error) {
	factor := ((1 << precision) - 1 + 128) / 256
	near := preset.near
	preset.t1 = factor*(basicT1-2) + 2 + 3*near
	preset.t2 = factor*(basicT2-3) + 3 + 5*near
	preset.t3 = factor*(basicT3-4) + 4 + 7*near
	if preset.t1 < near+1 || preset.t1 > preset.t2 || preset.t2 > preset.t3 || preset.t3 > preset.maxval {
		return presetCodingParameters{}, fmt.Errorf("%w: JAI thresholds do not fit MAXVAL %d", ErrInvalidCodestream, preset.maxval)
	}
	return preset, nil
}

func decodeSplitFrame(ctx context.Context, header frameHeader, scans []scanData, metadata pixeldata.Metadata, nearLossless bool, dst []byte) ([]byte, error) {
	if err := validateSplitFrame(header, metadata, nearLossless); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	native := dst
	if native == nil {
		native = make([]byte, metadata.FrameSize())
	}
	bounds := image.Rect(0, 0, header.columns, header.rows)
	for _, scan := range scans {
		component := 0
		for index, id := range header.componentIDs {
			if id == scan.componentIDs[0] {
				component = index
				break
			}
		}
		if err := decodeNativeScan(ctx, scan.entropy, header, header.preset, native, bounds, component, int(metadata.BitsAllocated/8)); err != nil {
			// A caller destination may contain partial rows or components.
			return dst, err
		}
	}
	return native, nil
}

func validateSplitFrame(header frameHeader, metadata pixeldata.Metadata, nearLossless bool) error {
	if header.near != 0 && !nearLossless {
		return fmt.Errorf("%w: NEAR=%d", ErrInvalidCodestream, header.near)
	}
	if header.rows != int(metadata.Rows) || header.columns != int(metadata.Columns) {
		return fmt.Errorf("%w: JPEG-LS size %dx%d metadata %dx%d", pixeldata.ErrPixelDataSizeMismatch, header.columns, header.rows, metadata.Columns, metadata.Rows)
	}
	if header.components != int(metadata.SamplesPerPixel) {
		return fmt.Errorf("%w: JPEG-LS components=%d SamplesPerPixel=%d", pixeldata.ErrPixelDataSizeMismatch, header.components, metadata.SamplesPerPixel)
	}
	if header.precision != int(metadata.BitsStored) && header.precision != int(metadata.BitsAllocated) {
		return fmt.Errorf("%w: JPEG-LS precision=%d BitsStored=%d BitsAllocated=%d", pixeldata.ErrPixelDataSizeMismatch, header.precision, metadata.BitsStored, metadata.BitsAllocated)
	}
	if (header.ilv != 0 || nearLossless) && header.precision != int(metadata.BitsStored) {
		return fmt.Errorf("%w: JPEG-LS precision must match BitsStored for this profile", pixeldata.ErrPixelDataSizeMismatch)
	}
	if err := validateDecodeAllocation(header, metadata); err != nil {
		return err
	}
	maxval := (1 << header.precision) - 1
	if (header.ilv != 0 || nearLossless) && header.preset.maxval != maxval {
		return fmt.Errorf("%w: custom MAXVAL is outside this qualified profile", ErrInvalidCodestream)
	}
	return nil
}

func parseFrameHeader(stream []byte) (frameHeader, error) {
	header, _, err := splitScans(stream)
	return header, err
}

func splitScans(stream []byte) (frameHeader, []scanData, error) {
	return splitScansWithHP(stream, false)
}

func splitScansWithHP(stream []byte, allowHP bool) (frameHeader, []scanData, error) {
	return splitScansWithHPContext(context.Background(), stream, allowHP)
}

func splitScansWithHPContext(ctx context.Context, stream []byte, allowHP bool) (frameHeader, []scanData, error) {
	var header frameHeader
	if len(stream) < 4 || stream[0] != 0xff || stream[1] != 0xd8 {
		return frameHeader{}, nil, fmt.Errorf("%w: missing SOI", ErrInvalidCodestream)
	}
	i := 2
	var scans []scanData
	seenSOF := false
	seenLSE := false
	seenHP := false
	seenScan := make(map[byte]bool)
	for i+1 < len(stream) {
		if err := ctx.Err(); err != nil {
			return frameHeader{}, nil, err
		}
		if stream[i] != 0xff {
			return frameHeader{}, nil, fmt.Errorf("%w: expected marker", ErrInvalidCodestream)
		}
		for i < len(stream) && stream[i] == 0xff {
			i++
			if i&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return frameHeader{}, nil, err
				}
			}
		}
		if i >= len(stream) {
			return frameHeader{}, nil, fmt.Errorf("%w: truncated marker", ErrInvalidCodestream)
		}
		marker := stream[i]
		i++
		switch marker {
		case 0xd9:
			if !seenSOF {
				return frameHeader{}, nil, fmt.Errorf("%w: missing SOF55", ErrInvalidCodestream)
			}
			if len(seenScan) != header.components {
				return frameHeader{}, nil, fmt.Errorf("%w: incomplete component coverage", ErrInvalidCodestream)
			}
			preset, err := resolveCodingParameters(header.preset, header.precision, header.near)
			if err != nil {
				return frameHeader{}, nil, err
			}
			header.preset = preset
			if header.hpTransform != 0 {
				if err := validateHPHeader(header); err != nil {
					return frameHeader{}, nil, err
				}
			}
			trailing := stream[i:]
			if len(trailing) > 1 || len(trailing) == 1 && trailing[0] != 0 {
				return frameHeader{}, nil, fmt.Errorf("%w: trailing data after EOI", ErrInvalidCodestream)
			}
			return header, scans, nil
		case 0xf7:
			if seenSOF || len(scans) != 0 {
				return frameHeader{}, nil, fmt.Errorf("%w: duplicate or out-of-order SOF55", ErrInvalidCodestream)
			}
			seg, next, err := readSegment(stream, i)
			if err != nil {
				return frameHeader{}, nil, err
			}
			parsed, err := parseSOF55(seg)
			if err != nil {
				return frameHeader{}, nil, err
			}
			parsed.hpTransform = header.hpTransform
			header = parsed
			seenSOF = true
			i = next
		case 0xda:
			if !seenSOF {
				return frameHeader{}, nil, fmt.Errorf("%w: SOS before SOF55", ErrInvalidCodestream)
			}
			seg, next, err := readSegment(stream, i)
			if err != nil {
				return frameHeader{}, nil, err
			}
			componentIDs, near, ilv, err := parseSOS(seg, header)
			if err != nil {
				return frameHeader{}, nil, err
			}
			for _, id := range componentIDs {
				if seenScan[id] {
					return frameHeader{}, nil, fmt.Errorf("%w: duplicate component scan %d", ErrInvalidCodestream, id)
				}
				seenScan[id] = true
			}
			if len(scans) == 0 {
				header.near = near
				header.ilv = ilv
			} else if header.near != near || header.ilv != ilv {
				return frameHeader{}, nil, fmt.Errorf("%w: mixed SOS parameters", ErrInvalidCodestream)
			}
			entropy, end, err := readEntropy(ctx, stream, next)
			if err != nil {
				return frameHeader{}, nil, err
			}
			scans = append(scans, scanData{componentIDs: componentIDs, entropy: entropy})
			i = end
		case 0xf8:
			if !seenSOF || len(scans) != 0 || seenLSE {
				return frameHeader{}, nil, fmt.Errorf("%w: duplicate or out-of-order LSE", ErrInvalidCodestream)
			}
			seg, next, err := readSegment(stream, i)
			if err != nil {
				return frameHeader{}, nil, err
			}
			preset, err := parseLSE(seg)
			if err != nil {
				return frameHeader{}, nil, err
			}
			header.preset = preset
			header.explicitThresholds = preset.t1 != 0 || preset.t2 != 0 || preset.t3 != 0
			seenLSE = true
			i = next
		case 0xe0, 0xe1, 0xe2, 0xe3, 0xe4, 0xe5, 0xe6, 0xe7,
			0xe8, 0xe9, 0xea, 0xeb, 0xec, 0xed, 0xee, 0xef, 0xfe:
			seg, next, err := readSegment(stream, i)
			if err != nil {
				return frameHeader{}, nil, err
			}
			if marker == 0xe8 && len(seg) >= 6 && string(seg[2:6]) == "mrfx" {
				if len(seg) != 7 || seenHP || len(scans) != 0 || seg[6] > 3 || seg[6] != 0 && !allowHP {
					return frameHeader{}, nil, fmt.Errorf("%w: unsupported, duplicate or misplaced HP color transform", ErrInvalidCodestream)
				}
				seenHP = true
				header.hpTransform = int(seg[6])
			}
			i = next
		default:
			return frameHeader{}, nil, fmt.Errorf("%w: unsupported marker 0x%02x", ErrInvalidCodestream, marker)
		}
	}
	return frameHeader{}, nil, fmt.Errorf("%w: missing EOI", ErrInvalidCodestream)
}

func readSegment(stream []byte, i int) ([]byte, int, error) {
	if i+1 >= len(stream) {
		return nil, 0, fmt.Errorf("%w: truncated segment length", ErrInvalidCodestream)
	}
	length := int(stream[i])<<8 | int(stream[i+1])
	if length < 2 || i+length > len(stream) {
		return nil, 0, fmt.Errorf("%w: invalid segment length", ErrInvalidCodestream)
	}
	return stream[i : i+length], i + length, nil
}

func parseSOF55(seg []byte) (frameHeader, error) {
	if len(seg) < 8 {
		return frameHeader{}, fmt.Errorf("%w: truncated SOF55", ErrInvalidCodestream)
	}
	nf := int(seg[7])
	if nf != 1 && nf != 3 || 8+3*nf != len(seg) {
		return frameHeader{}, fmt.Errorf("%w: invalid SOF55 component count", ErrInvalidCodestream)
	}
	precision := int(seg[2])
	rows := int(seg[3])<<8 | int(seg[4])
	columns := int(seg[5])<<8 | int(seg[6])
	if precision < 2 || precision > 16 || rows == 0 || columns == 0 {
		return frameHeader{}, fmt.Errorf("%w: invalid SOF55 geometry or precision", ErrInvalidCodestream)
	}
	ids := make([]byte, nf)
	seen := make(map[byte]bool, nf)
	for component := 0; component < nf; component++ {
		offset := 8 + 3*component
		id := seg[offset]
		if seen[id] || seg[offset+1] != 0x11 || seg[offset+2] != 0 {
			return frameHeader{}, fmt.Errorf("%w: invalid SOF55 component parameters", ErrInvalidCodestream)
		}
		seen[id] = true
		ids[component] = id
	}
	return frameHeader{
		precision:    precision,
		rows:         rows,
		columns:      columns,
		components:   nf,
		componentIDs: ids,
	}, nil
}

func parseSOS(seg []byte, header frameHeader) (componentIDs []byte, near, ilv int, err error) {
	if len(seg) < 3 {
		return nil, 0, 0, fmt.Errorf("%w: truncated SOS", ErrInvalidCodestream)
	}
	ns := int(seg[2])
	need := 3 + 2*ns + 3
	if ns != 1 && ns != header.components || len(seg) != need {
		return nil, 0, 0, fmt.Errorf("%w: invalid SOS", ErrInvalidCodestream)
	}
	near = int(seg[3+2*ns])
	ilv = int(seg[3+2*ns+1])
	if ilv > 2 || (ns == 1) != (ilv == 0) || seg[3+2*ns+2] != 0 {
		return nil, 0, 0, fmt.Errorf("%w: unsupported SOS parameters", ErrInvalidCodestream)
	}
	componentIDs = make([]byte, ns)
	for component := 0; component < ns; component++ {
		id := seg[3+2*component]
		found := false
		for _, frameID := range header.componentIDs {
			found = found || frameID == id
		}
		// Interleaved scans cover every component, in frame-header order (T.87 B.4).
		if !found || seg[4+2*component] != 0 || ilv != 0 && id != header.componentIDs[component] {
			return nil, 0, 0, fmt.Errorf("%w: invalid SOS component selector or order", ErrInvalidCodestream)
		}
		componentIDs[component] = id
	}
	return componentIDs, near, ilv, nil
}

func parseLSE(seg []byte) (presetCodingParameters, error) {
	if len(seg) != 13 || seg[2] != 1 {
		return presetCodingParameters{}, fmt.Errorf("%w: unsupported LSE parameters", ErrInvalidCodestream)
	}
	preset := presetCodingParameters{
		maxval: int(binary.BigEndian.Uint16(seg[3:5])),
		t1:     int(binary.BigEndian.Uint16(seg[5:7])),
		t2:     int(binary.BigEndian.Uint16(seg[7:9])),
		t3:     int(binary.BigEndian.Uint16(seg[9:11])),
		reset:  int(binary.BigEndian.Uint16(seg[11:13])),
	}
	// NEAR is in SOS, so default thresholds and their validation are deferred
	// until the complete scan parameters are known.
	return preset, nil
}

func resolveCodingParameters(preset presetCodingParameters, precision, near int) (presetCodingParameters, error) {
	maximumForPrecision := (1 << precision) - 1
	// Zero fields select their defaults, per T.87 C.2.4.1.1.
	if preset.maxval == 0 {
		preset.maxval = maximumForPrecision
	}
	if near < 0 || near > minInt(255, preset.maxval/2) {
		return presetCodingParameters{}, fmtInvalid("NEAR exceeds MAXVAL bound")
	}
	preset.near = near
	t1, t2, t3 := defaultNearThresholds(preset.maxval, near)
	if preset.t1 == 0 {
		preset.t1 = t1
	}
	if preset.t2 == 0 {
		preset.t2 = t2
	}
	if preset.t3 == 0 {
		preset.t3 = t3
	}
	if preset.reset == 0 {
		preset.reset = basicReset
	}
	if preset.maxval < 1 || preset.maxval > maximumForPrecision ||
		preset.t1 < near+1 || preset.t1 > preset.t2 || preset.t2 > preset.t3 || preset.t3 > preset.maxval ||
		preset.reset < 3 || preset.reset > maxInt(255, preset.maxval) {
		return presetCodingParameters{}, fmt.Errorf("%w: invalid LSE preset coding parameters", ErrInvalidCodestream)
	}
	return preset, nil
}

func readEntropy(ctx context.Context, stream []byte, start int) ([]byte, int, error) {
	for offset := start; offset < len(stream)-1; {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		end := offset + min(4096, len(stream)-1-offset)
		for offset < end {
			n := bytes.IndexByte(stream[offset:end], 0xff)
			if n < 0 {
				break
			}
			marker := offset + n
			// JPEG-LS byte stuffing leaves the next byte's high bit zero.
			if stream[marker+1] >= 0x80 {
				return stream[start:marker], marker, nil
			}
			offset = marker + 2
		}
		offset = max(offset, end)
	}
	return nil, 0, fmt.Errorf("%w: truncated entropy", ErrInvalidCodestream)
}

func decodeScan(entropy []byte, width, height, maxval int) ([]int, error) {
	t1, t2, t3 := defaultThresholds(maxval)
	return decodeScanContext(context.Background(), entropy, width, height, presetCodingParameters{maxval: maxval, t1: t1, t2: t2, t3: t3, reset: basicReset})
}

func decodeScanContext(ctx context.Context, entropy []byte, width, height int, preset presetCodingParameters) ([]int, error) {
	st := newLocoStateForPreset(preset)
	r := &bitReader{buf: entropy}
	recon := getSamples(width * height)
	for y := 0; y < height; y++ {
		if err := decodeLine(ctx, r, st, recon, width, y); err != nil {
			return nil, err
		}
	}
	if err := validateEntropyPadding(r); err != nil {
		return nil, err
	}
	return recon, nil
}

func decodeLine(ctx context.Context, r *bitReader, st *locoState, recon []int, width, y int) error {
	for x := 0; x < width; {
		if x&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		var ra, rb, rc, rd int
		if x > 0 && y > 0 && x+1 < width {
			// Interior sample: all four neighbors are real samples.
			i := y*width + x
			ra, rb, rc, rd = recon[i-1], recon[i-width], recon[i-width-1], recon[i-width+1]
		} else {
			ra, rb, rc, rd = neighbors(recon, width, x, y)
		}
		q1 := st.quantize(rd - rb)
		q2 := st.quantize(rb - rc)
		q3 := st.quantize(rc - ra)
		if q1 == 0 && q2 == 0 && q3 == 0 {
			next, err := decodeRun(r, st, recon, width, x, y, ra, rb)
			if err != nil {
				return fmt.Errorf("run mode at (%d,%d): %w", x, y, err)
			}
			x = next
			continue
		}
		ix, err := decodeRegular(r, st, ra, rb, rc, q1, q2, q3)
		if err != nil {
			return fmt.Errorf("regular mode at (%d,%d): %w", x, y, err)
		}
		recon[y*width+x] = ix
		x++
	}
	return nil
}

func validateEntropyPadding(r *bitReader) error {
	paddingBits := 0
	for r.count > 0 || r.pos < len(r.buf) {
		bit, err := r.readBit()
		if err != nil {
			return fmt.Errorf("%w: invalid entropy padding: %v", ErrInvalidCodestream, err)
		}
		paddingBits++
		if paddingBits > 7 || bit != 0 {
			return fmt.Errorf("%w: trailing entropy data", ErrInvalidCodestream)
		}
	}
	if r.lastWasFF {
		return fmt.Errorf("%w: missing stuffed padding after 0xff", ErrInvalidCodestream)
	}
	return nil
}

// errOutsideModularRange reports a decoded prediction error that the modulo
// reduction of T.87 A.4.5 cannot produce.
var errOutsideModularRange = fmt.Errorf("%w: prediction error outside the modulo range", ErrInvalidCodestream)

func decodeRegular(r *bitReader, st *locoState, ra, rb, rc, q1, q2, q3 int) (int, error) {
	q, sign := contextIndex(q1, q2, q3)
	cx := &st.ctx[q]
	px := clampInt(predict(ra, rb, rc)+sign*cx.c, 0, st.maxval)
	k := cx.k
	mapped, err := readLimitedGolomb(r, k, st.limit, st.qbpp)
	if err != nil {
		return 0, err
	}
	errval := st.unmapRegular(mapped, q, k)
	// A forged code can carry an error no encoder produces; accumulating it
	// would let A, and with it k and the length of the next code, grow
	// without bound.
	if !st.inModularRange(errval) {
		return 0, errOutsideModularRange
	}
	st.updateRegular(q, errval)
	if sign < 0 {
		errval = -errval
	}
	return st.reconstruct(px, errval), nil
}

func decodeRun(r *bitReader, st *locoState, recon []int, width, x, y, ra, rb int) (int, error) {
	runVal := ra
	xx := x
	for {
		bit, err := r.readBit()
		if err != nil {
			return 0, err
		}
		if bit == 1 {
			fill := 1 << runJ[st.runIdx]
			remaining := width - xx
			if remaining >= fill {
				for i := 0; i < fill; i++ {
					recon[y*width+xx] = runVal
					xx++
				}
				if st.runIdx < 31 {
					st.runIdx++
				}
				if xx == width {
					return width, nil
				}
				continue
			}
			for xx < width {
				recon[y*width+xx] = runVal
				xx++
			}
			return width, nil
		}
		bits := runJ[st.runIdx]
		rem := 0
		if bits > 0 {
			value, err := r.readBits(bits)
			if err != nil {
				return 0, err
			}
			rem = int(value)
		}
		for i := 0; i < rem; i++ {
			if xx >= width {
				return 0, fmt.Errorf("%w: run exceeds line", ErrInvalidCodestream)
			}
			recon[y*width+xx] = runVal
			xx++
		}
		if xx >= width {
			return 0, fmt.Errorf("%w: missing run interruption sample", ErrInvalidCodestream)
		}
		glimitIndex := st.runIdx
		if st.runIdx > 0 {
			st.runIdx--
		}
		// The interruption is relative to xx, not the start of the run.
		_, rb, _, _ = neighbors(recon, width, xx, y)
		ix, err := decodeRunInterruption(r, st, ra, rb, glimitIndex)
		if err != nil {
			return 0, err
		}
		recon[y*width+xx] = ix
		return xx + 1, nil
	}
}

func decodeRunInterruption(r *bitReader, st *locoState, ra, rb, glimitIndex int) (int, error) {
	riType := 0
	if absInt(ra-rb) <= st.near {
		riType = 1
	}
	return decodeRunInterruptionType(r, st, ra, rb, glimitIndex, riType)
}

func decodeRunInterruptionType(r *bitReader, st *locoState, ra, rb, glimitIndex, riType int) (int, error) {
	px := rb
	if riType == 1 {
		px = ra
	}
	q := 365 + riType
	temp := st.ctx[q].a
	if riType == 1 {
		temp = st.ctx[q].a + st.ctx[q].n>>1
	}
	k := st.golombK(temp, st.ctx[q].n)
	glimit := st.limit - runJ[glimitIndex] - 1
	mapped, err := readLimitedGolomb(r, k, glimit, st.qbpp)
	if err != nil {
		return 0, err
	}
	posBias := k == 0 && 2*st.nn[riType] < st.ctx[q].n
	t := mapped + riType
	var errval int
	if posBias {
		if t%2 != 0 {
			errval = (t + 1) / 2
		} else {
			errval = -t / 2
		}
	} else if t%2 == 0 {
		errval = t / 2
	} else {
		errval = -(t + 1) / 2
	}
	mapFlag := 0
	if (k == 0 && errval > 0 && 2*st.nn[riType] < st.ctx[q].n) || (errval < 0 && 2*st.nn[riType] >= st.ctx[q].n) || (errval < 0 && k != 0) {
		mapFlag = 1
	}
	if 2*absInt(errval)-riType-mapFlag != mapped {
		return 0, fmt.Errorf("%w: run interruption mapping", ErrInvalidCodestream)
	}
	if !st.inModularRange(errval) {
		return 0, errOutsideModularRange
	}
	if errval < 0 {
		st.nn[riType]++
	}
	st.ctx[q].a += (mapped + 1 - riType) >> 1
	if st.ctx[q].n == st.reset {
		st.ctx[q].a >>= 1
		st.ctx[q].n >>= 1
		st.nn[riType] >>= 1
	}
	st.ctx[q].n++
	// Adapt Nn using the mapped, sign-normalized error (T.87 A.7.2).
	if riType == 0 && ra > rb {
		errval = -errval
	}
	return st.reconstruct(px, errval), nil
}

func samplesToNative(planes [][]int, metadata pixeldata.Metadata, maxval int) ([]byte, error) {
	return samplesToNativeContext(context.Background(), planes, metadata, maxval)
}

func samplesToNativeContext(ctx context.Context, planes [][]int, metadata pixeldata.Metadata, maxval int) ([]byte, error) {
	comps := int(metadata.SamplesPerPixel)
	pixels := int(metadata.Rows) * int(metadata.Columns)
	if comps == 1 {
		return planeToNativeContext(ctx, planes[0], metadata, maxval)
	}
	bytesPerSample := int(metadata.BitsAllocated / 8)
	out := make([]byte, pixels*comps*bytesPerSample)
	for i := 0; i < pixels; i++ {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		for c := 0; c < comps; c++ {
			sample := planes[c][i]
			if sample < 0 || sample > maxval {
				return nil, fmt.Errorf("%w: reconstructed sample", ErrInvalidCodestream)
			}
			stored := sample
			index := i*comps + c
			if bytesPerSample == 1 {
				out[index] = byte(stored)
			} else {
				binary.LittleEndian.PutUint16(out[index*2:], uint16(stored))
			}
		}
	}
	return out, nil
}

func planeToNativeContext(ctx context.Context, samples []int, metadata pixeldata.Metadata, maxval int) ([]byte, error) {
	bytesPerSample := int(metadata.BitsAllocated / 8)
	out := make([]byte, len(samples)*bytesPerSample)
	if bytesPerSample == 1 {
		for i, sample := range samples {
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if sample < 0 || sample > maxval {
				return nil, fmt.Errorf("%w: reconstructed sample", ErrInvalidCodestream)
			}
			stored := sample
			out[i] = byte(stored)
		}
		return out, nil
	}
	for i, sample := range samples {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if sample < 0 || sample > maxval {
			return nil, fmt.Errorf("%w: reconstructed sample", ErrInvalidCodestream)
		}
		stored := sample
		binary.LittleEndian.PutUint16(out[i*2:], uint16(stored))
	}
	return out, nil
}

func validateDecodeAllocation(header frameHeader, metadata pixeldata.Metadata) error {
	maxInt := uint64(^uint(0) >> 1)
	pixels := uint64(header.rows) * uint64(header.columns)
	components := uint64(header.components)
	bytesPerSample := uint64(metadata.BitsAllocated / 8)
	if pixels == 0 || components == 0 || bytesPerSample == 0 ||
		pixels > maxInt || components > maxInt/pixels {
		return fmt.Errorf("%w: decoded frame size overflow", ErrInvalidCodestream)
	}
	samples := pixels * components
	if bytesPerSample > maxInt/samples {
		return fmt.Errorf("%w: decoded frame size overflow", ErrInvalidCodestream)
	}
	outputBytes := samples * bytesPerSample
	intBytes := nativeIntBytes()
	if intBytes == 0 || samples > ^uint64(0)/intBytes {
		return fmt.Errorf("%w: decoded working-set overflow", ErrInvalidCodestream)
	}
	workingBytes := samples*intBytes + outputBytes
	if workingBytes > maxJPEGLSWorkingSetBytes {
		return fmt.Errorf("%w: decoded frame exceeds working-set limit", ErrInvalidCodestream)
	}
	return nil
}

func validateDecodeRequestAllocation(metadata pixeldata.Metadata) error {
	frameSize := metadata.FrameSize()
	frameCount := metadata.NumberOfFrames
	if frameSize <= 0 || frameCount <= 0 {
		return fmt.Errorf("%w: invalid decoded request size", ErrInvalidCodestream)
	}
	frames := uint64(frameCount)
	frameBytes := uint64(frameSize)
	if frameBytes > ^uint64(0)/frames {
		return fmt.Errorf("%w: decoded request size overflow", ErrInvalidCodestream)
	}
	totalOutputBytes := frameBytes * frames

	intBytes := nativeIntBytes()
	// A [][]byte retains one three-word slice header per decoded frame.
	if frames > ^uint64(0)/(3*intBytes) {
		return fmt.Errorf("%w: decoded request overhead overflow", ErrInvalidCodestream)
	}
	frameSliceBytes := frames * 3 * intBytes

	// During a decode the completed native frames coexist with the current
	// frame's int reconstruction planes. Include that peak transient memory in
	// the request-wide bound, not only in the per-frame check.
	samplesPerFrame := uint64(metadata.Rows) * uint64(metadata.Columns) * uint64(metadata.SamplesPerPixel)
	if samplesPerFrame > ^uint64(0)/intBytes {
		return fmt.Errorf("%w: decoded request working-set overflow", ErrInvalidCodestream)
	}
	planeBytes := samplesPerFrame * intBytes
	if totalOutputBytes > ^uint64(0)-frameSliceBytes || totalOutputBytes+frameSliceBytes > ^uint64(0)-planeBytes {
		return fmt.Errorf("%w: decoded request working-set overflow", ErrInvalidCodestream)
	}
	if totalOutputBytes+frameSliceBytes+planeBytes > maxJPEGLSWorkingSetBytes {
		return fmt.Errorf("%w: decoded request exceeds working-set limit", ErrInvalidCodestream)
	}
	return nil
}

func nativeIntBytes() uint64 {
	return uint64(32<<(^uint(0)>>63)) / 8
}

func fmtInvalid(detail string) error {
	return fmt.Errorf("%w: %s", ErrInvalidCodestream, detail)
}

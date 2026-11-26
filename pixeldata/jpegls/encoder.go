package jpegls

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

const maxJPEGLSFragmentLength = uint64(1<<32 - 2)

// Encoder encodes canonical native frames. Its zero value and NewEncoder are
// lossless and write ILV=0. Nonzero NEAR and line or sample interleaving of
// RGB frames require NewEncoderWithOptions. Encoder has no mutable state and
// is safe for concurrent use.
type Encoder struct {
	near       int
	interleave InterleaveMode
}

// InterleaveMode is the JPEG-LS interleave mode (ILV, ITU-T T.87 C.2.3)
// written for three-component frames. One-component frames always use one
// scan with ILV=0.
type InterleaveMode int

const (
	// InterleaveNone writes one scan per component (ILV=0), the default.
	InterleaveNone InterleaveMode = 0
	// InterleaveLine writes one scan whose lines alternate between the
	// components (ILV=1, T.87 B.2).
	InterleaveLine InterleaveMode = 1
	// InterleaveSample writes one scan whose samples alternate between the
	// components (ILV=2, T.87 B.3).
	InterleaveSample InterleaveMode = 2
)

// EncoderOptions selects the maximum absolute error in unsigned stored sample
// units and the interleave mode of RGB frames. No clinical error is chosen by
// default. NEAR>0 requires AllowLossy; the dataset transcoder independently
// requires TranscodeOptions.AllowLossy.
type EncoderOptions struct {
	Near       int
	AllowLossy bool
	// Interleave selects ILV for three-component frames; the zero value is
	// InterleaveNone.
	Interleave InterleaveMode
}

// NewEncoderWithOptions selects lossless .80 for Near=0, or explicitly
// authorized near-lossless .81 for Near>0, and the interleave mode of RGB
// frames. The precision-specific upper bound min(255, MAXVAL/2) is checked
// against each frame's metadata before encoding.
func NewEncoderWithOptions(opts EncoderOptions) (*Encoder, error) {
	if opts.Near < 0 || opts.Near > 255 {
		return nil, unsupportedEncoderMetadata("NEAR")
	}
	if opts.Interleave < InterleaveNone || opts.Interleave > InterleaveSample {
		return nil, unsupportedEncoderMetadata("ILV")
	}
	if opts.Near > 0 && !opts.AllowLossy {
		return nil, pixeldata.ErrTranscodeLossyDisallowed
	}
	return &Encoder{near: opts.Near, interleave: opts.Interleave}, nil
}

// RegisterNearLosslessEncoder explicitly registers .81 in a caller-owned
// registry. It requires Near>0 and AllowLossy; RegisterEncoder stays lossless.
func RegisterNearLosslessEncoder(registry pixeldata.EncoderRegistry, opts EncoderOptions) error {
	if registry == nil {
		return pixeldata.ErrEncoderRegistryNil
	}
	if opts.Near == 0 {
		return unsupportedEncoderMetadata("NEAR")
	}
	encoder, err := NewEncoderWithOptions(opts)
	if err != nil {
		return err
	}
	return registry.RegisterEncoder(NearLosslessUID, encoder)
}

func (e *Encoder) nearParameter() int {
	if e == nil {
		return 0
	}
	return e.near
}

func (e *Encoder) interleaveMode() InterleaveMode {
	if e == nil {
		return InterleaveNone
	}
	return e.interleave
}

var _ pixeldata.FrameEncoder = (*Encoder)(nil)

// NewEncoder returns a pure-Go JPEG-LS Lossless frame encoder.
func NewEncoder() *Encoder {
	return &Encoder{}
}

// RegisterEncoder registers a JPEG-LS Lossless encoder in registry.
func RegisterEncoder(registry pixeldata.EncoderRegistry) error {
	if registry == nil {
		return pixeldata.ErrEncoderRegistryNil
	}
	return registry.RegisterEncoder(UID, NewEncoder())
}

// Capabilities describes the canonical native frames accepted by Encoder.
func (e *Encoder) Capabilities() pixeldata.EncoderCapabilities {
	caps := pixeldata.EncoderCapabilities{
		TransferSyntaxUID:          UID,
		BitsAllocated:              []uint16{8, 16},
		PixelRepresentations:       []uint16{0, 1},
		SamplesPerPixel:            []uint16{1, 3},
		PhotometricInterpretations: []string{"MONOCHROME1", "MONOCHROME2", "PALETTE COLOR", "RGB"},
		Lossless:                   true,
		SupportsMultiFrame:         true,
		Backend:                    "pure-go",
	}
	if e.nearParameter() > 0 {
		caps.TransferSyntaxUID = NearLosslessUID
		caps.PixelRepresentations = []uint16{0}
		caps.PhotometricInterpretations = []string{"MONOCHROME1", "MONOCHROME2", "RGB"}
		caps.Lossless = false
		caps.LossyMethod = "ISO_14495_1"
	}
	return caps
}

// EncodeFrame encodes one little-endian, sample-interleaved native frame.
func (e *Encoder) EncodeFrame(ctx context.Context, frame []byte, metadata pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	params, err := validateEncoderInput(metadata)
	if err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	near := e.nearParameter()
	if near > 0 {
		if near > minInt(255, params.maxval/2) {
			return pixeldata.EncodedFrame{}, unsupportedEncoderMetadata("NEAR")
		}
		// A small unsigned code error can cross a signed discontinuity or
		// select an unrelated palette entry. Neither profile is qualified.
		if metadata.PixelRepresentation != 0 {
			return pixeldata.EncodedFrame{}, unsupportedEncoderMetadata("PixelRepresentation")
		}
		if strings.EqualFold(strings.TrimSpace(metadata.PhotometricInterpretation), "PALETTE COLOR") {
			return pixeldata.EncodedFrame{}, unsupportedEncoderMetadata("PhotometricInterpretation")
		}
	}
	if uint64(len(frame)) != params.frameLength {
		return pixeldata.EncodedFrame{}, fmt.Errorf("%w: native frame length", pixeldata.ErrPixelDataSizeMismatch)
	}

	samples, err := nativeToSamplesContext(ctx, frame, metadata, params)
	if err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	params.samplesOwned = true
	ilv := e.interleaveMode()
	if metadata.SamplesPerPixel == 1 {
		ilv = InterleaveNone
	}
	encoded, err := encodeFrameWithNear(ctx, samples, metadata, params, near, ilv)
	putSamples(samples)
	if err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	if len(encoded) == 0 {
		return pixeldata.EncodedFrame{}, fmt.Errorf("%w: empty frame", pixeldata.ErrEncoderOutputInvalid)
	}
	return pixeldata.EncodedFrame{Data: encoded}, nil
}

type encodeParams struct {
	bytesPerSample int
	maxval         int
	frameLength    uint64
	maxEncoded     uint64
	// samplesOwned reports that the samples passed with these parameters
	// belong to the encoder (EncodeFrame's conversion of the native frame),
	// so a scan may reconstruct into them instead of into a copy.
	samplesOwned bool
}

func validateEncoderInput(metadata pixeldata.Metadata) (encodeParams, error) {
	if metadata.Rows == 0 {
		return encodeParams{}, unsupportedEncoderMetadata("Rows")
	}
	if metadata.Columns == 0 {
		return encodeParams{}, unsupportedEncoderMetadata("Columns")
	}
	if metadata.BitsAllocated != 8 && metadata.BitsAllocated != 16 {
		return encodeParams{}, unsupportedEncoderMetadata("BitsAllocated")
	}
	if metadata.BitsStored < 2 || metadata.BitsStored > metadata.BitsAllocated {
		return encodeParams{}, unsupportedEncoderMetadata("BitsStored")
	}
	if metadata.HighBit != metadata.BitsStored-1 {
		return encodeParams{}, unsupportedEncoderMetadata("HighBit")
	}
	if metadata.PixelRepresentation > 1 {
		return encodeParams{}, unsupportedEncoderMetadata("PixelRepresentation")
	}
	if metadata.SamplesPerPixel != 1 && metadata.SamplesPerPixel != 3 {
		return encodeParams{}, unsupportedEncoderMetadata("SamplesPerPixel")
	}
	if metadata.NumberOfFrames < 1 {
		return encodeParams{}, unsupportedEncoderMetadata("NumberOfFrames")
	}
	photometric := strings.ToUpper(strings.TrimSpace(metadata.PhotometricInterpretation))
	switch metadata.SamplesPerPixel {
	case 1:
		if metadata.PlanarConfigurationPresent && metadata.PlanarConfiguration != 0 {
			return encodeParams{}, unsupportedEncoderMetadata("PlanarConfiguration")
		}
		if photometric != "MONOCHROME1" && photometric != "MONOCHROME2" && photometric != "PALETTE COLOR" {
			return encodeParams{}, unsupportedEncoderMetadata("PhotometricInterpretation")
		}
		if photometric == "PALETTE COLOR" && metadata.PixelRepresentation != 0 {
			return encodeParams{}, unsupportedEncoderMetadata("PixelRepresentation")
		}
	case 3:
		if photometric != "RGB" {
			return encodeParams{}, unsupportedEncoderMetadata("PhotometricInterpretation")
		}
		if metadata.PixelRepresentation != 0 {
			return encodeParams{}, unsupportedEncoderMetadata("PixelRepresentation")
		}
		if !metadata.PlanarConfigurationPresent || metadata.PlanarConfiguration != 0 {
			return encodeParams{}, unsupportedEncoderMetadata("PlanarConfiguration")
		}
	}

	bytesPerSample := int(metadata.BitsAllocated / 8)
	pixels, ok := checkedMul(uint64(metadata.Rows), uint64(metadata.Columns))
	if !ok {
		return encodeParams{}, fmt.Errorf("%w: encoded frame length", pixeldata.ErrEncoderOutputInvalid)
	}
	samples, ok := checkedMul(pixels, uint64(metadata.SamplesPerPixel))
	if !ok {
		return encodeParams{}, fmt.Errorf("%w: encoded frame length", pixeldata.ErrEncoderOutputInvalid)
	}
	frameLength, ok := checkedMul(samples, uint64(bytesPerSample))
	if !ok {
		return encodeParams{}, fmt.Errorf("%w: encoded frame length", pixeldata.ErrEncoderOutputInvalid)
	}
	limitBits := uint64(2 * (maxInt(2, int(metadata.BitsStored)) + 16))
	maxEncoded, ok := checkedMul(samples, (limitBits+7)/8+2)
	if !ok {
		return encodeParams{}, fmt.Errorf("%w: encoded frame length", pixeldata.ErrEncoderOutputInvalid)
	}
	maxEncoded += 64 + uint64(metadata.SamplesPerPixel)*16
	if maxEncoded > maxJPEGLSFragmentLength || maxEncoded > uint64(int(^uint(0)>>1)) {
		return encodeParams{}, fmt.Errorf("%w: encoded frame length", pixeldata.ErrEncoderOutputInvalid)
	}
	return encodeParams{
		bytesPerSample: bytesPerSample,
		maxval:         (1 << metadata.BitsStored) - 1,
		frameLength:    frameLength,
		maxEncoded:     maxEncoded,
	}, nil
}

func unsupportedEncoderMetadata(field string) error {
	return &pixeldata.UnsupportedEncoderMetadataError{Field: field}
}

func nativeToSamples(frame []byte, metadata pixeldata.Metadata, params encodeParams) ([]int, error) {
	return nativeToSamplesContext(context.Background(), frame, metadata, params)
}

func nativeToSamplesContext(ctx context.Context, frame []byte, metadata pixeldata.Metadata, params encodeParams) ([]int, error) {
	count := int(metadata.Rows) * int(metadata.Columns) * int(metadata.SamplesPerPixel)
	if count < 0 || uint64(count) > uint64(math.MaxInt/4) {
		return nil, fmt.Errorf("%w: encoded frame length", pixeldata.ErrEncoderOutputInvalid)
	}
	samples := getSamples(count)
	mask := params.maxval
	// Native loads already fit BitsAllocated. Keep only BitsStored, including
	// signed DICOM's two's-complement code, and choose the load width once.
	for start := 0; start < count; start += 1024 {
		if err := ctx.Err(); err != nil {
			putSamples(samples)
			return nil, err
		}
		end := minInt(start+1024, count)
		if params.bytesPerSample == 1 {
			for i := start; i < end; i++ {
				samples[i] = int(frame[i]) & mask
			}
		} else {
			for i := start; i < end; i++ {
				samples[i] = (int(frame[2*i]) | int(frame[2*i+1])<<8) & mask
			}
		}
	}
	return samples, nil
}

func encodeFrameWithNear(ctx context.Context, samples []int, metadata pixeldata.Metadata, params encodeParams, near int, ilv InterleaveMode) ([]byte, error) {
	t1, t2, t3 := defaultNearThresholds(params.maxval, near)
	return encodeFrameWithPresetInterleave(ctx, samples, metadata, params, presetCodingParameters{maxval: params.maxval, near: near, t1: t1, t2: t2, t3: t3, reset: basicReset}, ilv)
}

// encodeFrameWithPreset encodes with the coding parameters of preset without
// an LSE segment, so a decoder applies its defaults: preset must hold them
// unless the stream is meant to reproduce an encoder that did not.
func encodeFrameWithPreset(ctx context.Context, samples []int, metadata pixeldata.Metadata, params encodeParams, preset presetCodingParameters) ([]byte, error) {
	return encodeFrameWithPresetInterleave(ctx, samples, metadata, params, preset, InterleaveNone)
}

func encodeFrameWithPresetInterleave(ctx context.Context, samples []int, metadata pixeldata.Metadata, params encodeParams, preset presetCodingParameters, ilv InterleaveMode) ([]byte, error) {
	near := preset.near
	rows := int(metadata.Rows)
	cols := int(metadata.Columns)
	comps := int(metadata.SamplesPerPixel)
	// Room for about 2:1 compression, so typical frames never regrow the
	// buffer; it is still bounded by the frame's encoded size limit.
	capacity := uint64(len(samples)*params.bytesPerSample)/2 + 64
	if capacity > params.maxEncoded {
		capacity = params.maxEncoded
	}
	w := &bitWriter{buf: make([]byte, 0, capacity)}
	w.buf = append(w.buf, 0xff, 0xd8)
	writeSOF55(w, rows, cols, comps, int(metadata.BitsStored))
	if ilv != InterleaveNone && comps > 1 {
		writeInterleavedSOS(w, comps, near, ilv)
		planes := make([][]int, comps)
		for c := range planes {
			planes[c] = extractComponent(samples, rows, cols, comps, c)
		}
		if err := encodeInterleavedScan(ctx, w, planes, cols, rows, ilv, preset); err != nil {
			return nil, err
		}
		w.padToByte()
		return finishFrame(w, params)
	}
	for c := 0; c < comps; c++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		writeSOS(w, c+1)
		w.buf[len(w.buf)-3] = byte(near)
		plane := extractComponent(samples, rows, cols, comps, c)
		// A component of a multi-component frame is always a fresh copy.
		owned := params.samplesOwned || comps > 1
		if err := encodeScanWithPreset(ctx, w, plane, cols, rows, preset, owned); err != nil {
			return nil, err
		}
		w.padToByte()
	}
	return finishFrame(w, params)
}

func finishFrame(w *bitWriter, params encodeParams) ([]byte, error) {
	w.buf = append(w.buf, 0xff, 0xd9)
	if uint64(len(w.buf)) > params.maxEncoded {
		return nil, fmt.Errorf("%w: encoded frame length", pixeldata.ErrEncoderOutputInvalid)
	}
	return w.buf, nil
}

func writeSOF55(w *bitWriter, rows, cols, comps, precision int) {
	length := 8 + 3*comps
	w.buf = append(w.buf, 0xff, 0xf7, byte(length>>8), byte(length), byte(precision))
	w.buf = append(w.buf, byte(rows>>8), byte(rows), byte(cols>>8), byte(cols), byte(comps))
	for c := 0; c < comps; c++ {
		w.buf = append(w.buf, byte(c+1), 0x11, 0x00)
	}
}

func writeSOS(w *bitWriter, componentID int) {
	w.buf = append(w.buf, 0xff, 0xda, 0x00, 0x08, 0x01, byte(componentID), 0x00, 0x00, 0x00, 0x00)
}

// writeInterleavedSOS writes one scan header naming every component in
// frame-header order (T.87 C.2.3): Ns, Ci with Tm=0, NEAR, ILV and Al=0.
func writeInterleavedSOS(w *bitWriter, comps, near int, ilv InterleaveMode) {
	length := 6 + 2*comps
	w.buf = append(w.buf, 0xff, 0xda, byte(length>>8), byte(length), byte(comps))
	for c := 0; c < comps; c++ {
		w.buf = append(w.buf, byte(c+1), 0x00)
	}
	w.buf = append(w.buf, byte(near), byte(ilv), 0x00)
}

func extractComponent(samples []int, rows, cols, comps, component int) []int {
	if comps == 1 {
		return samples
	}
	plane := make([]int, rows*cols)
	for i := range plane {
		plane[i] = samples[i*comps+component]
	}
	return plane
}

// encodeScanWithPreset codes one component. Coding replaces the samples
// behind the current line by their reconstruction, in samples itself when
// owned and in a copy otherwise.
func encodeScanWithPreset(ctx context.Context, w *bitWriter, samples []int, width, height int, preset presetCodingParameters, owned bool) error {
	st := newLocoStateForPreset(preset)
	recon := samples
	if !owned {
		recon = make([]int, width*height)
		copy(recon, samples)
	}
	for y := 0; y < height; y++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		encodeLine(w, st, recon, width, y)
	}
	return nil
}

// encodeLine codes line y of one component in regular and run mode; recon
// holds the source samples ahead of the line and the reconstruction behind.
func encodeLine(w *bitWriter, st *locoState, recon []int, width, y int) {
	for x := 0; x < width; {
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
			x = encodeRun(w, st, recon, width, x, y, ra, rb)
			continue
		}
		recon[y*width+x] = encodeRegular(w, st, recon[y*width+x], ra, rb, rc, q1, q2, q3)
		x++
	}
}

// encodeInterleavedScan codes every component in one scan. T.87 B.1 shares
// the adaptive contexts across the scan while each component keeps its own
// reconstructed neighbors. Line interleaving (B.2) codes line y of each
// component in turn with one run index per component; sample interleaving
// (B.3) codes the components of each sample in turn, enters run mode only
// when every component's gradients are zero, and codes one run for the
// sample vector whose interruption codes each component with RItype=0.
func encodeInterleavedScan(ctx context.Context, w *bitWriter, planes [][]int, width, height int, ilv InterleaveMode, preset presetCodingParameters) error {
	st := newLocoStateForPreset(preset)
	var runIndices [3]int
	for y := 0; y < height; y++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if ilv == InterleaveLine {
			for c, plane := range planes {
				st.runIdx = runIndices[c]
				encodeLine(w, st, plane, width, y)
				runIndices[c] = st.runIdx
			}
			continue
		}
		encodeSampleLine(w, st, planes, width, y)
	}
	return nil
}

func encodeSampleLine(w *bitWriter, st *locoState, planes [][]int, width, y int) {
	for x := 0; x < width; {
		var a, b, c, q1, q2, q3 [3]int
		run := true
		for component, plane := range planes {
			ra, rb, rc, rd := neighbors(plane, width, x, y)
			a[component], b[component], c[component] = ra, rb, rc
			q1[component], q2[component], q3[component] = st.quantize(rd-rb), st.quantize(rb-rc), st.quantize(rc-ra)
			run = run && q1[component] == 0 && q2[component] == 0 && q3[component] == 0
		}
		if run {
			x = encodeVectorRun(w, st, planes, width, x, y, a)
			continue
		}
		for component, plane := range planes {
			i := y*width + x
			plane[i] = encodeRegular(w, st, plane[i], a[component], b[component], c[component], q1[component], q2[component], q3[component])
		}
		x++
	}
}

// encodeVectorRun codes a run of samples whose every component is within
// NEAR of the run values (each component's Ra at the run start).
func encodeVectorRun(w *bitWriter, st *locoState, planes [][]int, width, x, y int, values [3]int) int {
	xx := x
	for xx < width {
		within := true
		for component, plane := range planes {
			if absInt(plane[y*width+xx]-values[component]) > st.near {
				within = false
				break
			}
		}
		if !within {
			break
		}
		for component, plane := range planes {
			plane[y*width+xx] = values[component]
		}
		xx++
	}
	run := xx - x
	for run >= 1<<runJ[st.runIdx] {
		w.writeBit(1)
		run -= 1 << runJ[st.runIdx]
		if st.runIdx < 31 {
			st.runIdx++
		}
	}
	if xx == width {
		if run > 0 {
			w.writeBit(1)
		}
		return width
	}
	w.writeBit(0)
	if bits := runJ[st.runIdx]; bits > 0 {
		w.writeBits(uint32(run), bits)
	}
	for component, plane := range planes {
		_, rb, _, _ := neighbors(plane, width, xx, y)
		i := y*width + xx
		plane[i] = encodeRunInterruptionType(w, st, plane[i], values[component], rb, st.runIdx, 0)
	}
	if st.runIdx > 0 {
		st.runIdx--
	}
	return xx + 1
}

func encodeRegular(w *bitWriter, st *locoState, ix, ra, rb, rc, q1, q2, q3 int) int {
	q, sign := contextIndex(q1, q2, q3)
	cx := &st.ctx[q]
	px := clampInt(predict(ra, rb, rc)+sign*cx.c, 0, st.maxval)
	err := quantizePredictionError(sign*(ix-px), st.near)
	rx := clampInt(px+sign*err*(2*st.near+1), 0, st.maxval)
	err = st.modRange(err)
	k := cx.k
	mapped := st.mapRegular(err, q, k)
	writeLimitedGolomb(w, mapped, k, st.limit, st.qbpp)
	st.updateRegular(q, err)
	return rx
}

// T.87 A.4.4 quantizes prediction error symmetrically, with reconstruction
// retained for subsequent predictions. This is distinct from gradient bins.
func quantizePredictionError(err, near int) int {
	if near == 0 {
		return err
	}
	if err < 0 {
		return -((near - err) / (2*near + 1))
	}
	return (err + near) / (2*near + 1)
}

func encodeRun(w *bitWriter, st *locoState, recon []int, width, x, y, ra, rb int) int {
	runVal := ra
	run := 0
	xx := x
	for xx < width && absInt(recon[y*width+xx]-runVal) <= st.near {
		recon[y*width+xx] = runVal
		run++
		xx++
	}
	interrupted := xx < width
	for run >= 1<<runJ[st.runIdx] {
		w.writeBit(1)
		run -= 1 << runJ[st.runIdx]
		if st.runIdx < 31 {
			st.runIdx++
		}
	}
	if interrupted {
		w.writeBit(0)
		if bits := runJ[st.runIdx]; bits > 0 {
			w.writeBits(uint32(run), bits)
		}
		glimitIndex := st.runIdx
		if st.runIdx > 0 {
			st.runIdx--
		}
		ix := recon[y*width+xx]
		_, rb, _, _ = neighbors(recon, width, xx, y)
		recon[y*width+xx] = encodeRunInterruption(w, st, ix, ra, rb, glimitIndex)
		return xx + 1
	}
	if run > 0 {
		w.writeBit(1)
	}
	return width
}

func encodeRunInterruption(w *bitWriter, st *locoState, ix, ra, rb, runIndexBefore int) int {
	riType := 0
	if absInt(ra-rb) <= st.near {
		riType = 1
	}
	return encodeRunInterruptionType(w, st, ix, ra, rb, runIndexBefore, riType)
}

func encodeRunInterruptionType(w *bitWriter, st *locoState, ix, ra, rb, runIndexBefore, riType int) int {
	px := rb
	if riType == 1 {
		px = ra
	}
	err := ix - px
	sign := 1
	if riType == 0 && ra > rb {
		err = -err
		sign = -1
	}
	err = quantizePredictionError(err, st.near)
	rx := clampInt(px+sign*err*(2*st.near+1), 0, st.maxval)
	err = st.modRange(err)
	q := 365 + riType
	temp := st.ctx[q].a
	if riType == 1 {
		temp = st.ctx[q].a + st.ctx[q].n>>1
	}
	k := st.golombK(temp, st.ctx[q].n)
	nn := st.nn[riType]
	mapFlag := 0
	if (k == 0 && err > 0 && 2*nn < st.ctx[q].n) || (err < 0 && 2*nn >= st.ctx[q].n) || (err < 0 && k != 0) {
		mapFlag = 1
	}
	mapped := 2*absInt(err) - riType - mapFlag
	glimit := st.limit - runJ[runIndexBefore] - 1
	writeLimitedGolomb(w, mapped, k, glimit, st.qbpp)
	if err < 0 {
		st.nn[riType]++
	}
	st.ctx[q].a += (mapped + 1 - riType) >> 1
	if st.ctx[q].n == st.reset {
		st.ctx[q].a >>= 1
		st.ctx[q].n >>= 1
		st.nn[riType] >>= 1
	}
	st.ctx[q].n++
	return rx
}

func checkedMul(a, b uint64) (uint64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a > math.MaxUint64/b {
		return 0, false
	}
	return a * b, true
}

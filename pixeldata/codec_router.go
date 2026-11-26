package pixeldata

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	// ErrInvalidCodecRouter reports CodecRouterOptions a router cannot be
	// built from.
	ErrInvalidCodecRouter = errors.New("dicom: invalid codec router")
	// ErrNoCodecBackend reports a frame no backend of a CodecRouter accepts.
	ErrNoCodecBackend = errors.New("dicom: no codec backend accepts the frame")
	// ErrCodecBackendsDisagree reports frames of one object that different
	// backends decoded to different photometric interpretations, which one
	// decoded object cannot hold.
	ErrCodecBackendsDisagree = errors.New("dicom: codec backends decoded the frames of one object differently")
)

// DiagnosticCodecBackendFallback: a frame was decoded by a later backend of
// a CodecRouter after an earlier one failed on it.
const DiagnosticCodecBackendFallback = "codec-backend-fallback"

// CodecBackend is one decoder a CodecRouter can choose for a frame.
type CodecBackend struct {
	// Name identifies the backend in decisions, statistics and shadow
	// reports ("core", "openjpeg"). It is unique within a router and must
	// not carry patient data.
	Name string
	// Codec decodes one frame at a time: the router hands it a one-frame
	// object (Number of Frames 1, the frame as its only Item).
	Codec Codec
	// Accept reports whether the backend takes a frame, from its encoded
	// bytes (which may end with an Item pad byte) and the frame's metadata,
	// for example by a codestream feature only some backends implement;
	// nil accepts every frame. It must not retain codestream.
	Accept func(codestream []byte, metadata Metadata) bool
}

// ShadowComparison decodes sampled frames a second time with another
// backend, the oracle, and compares the two results. The output is always
// the routed backend's: a mismatch or a failure of the shadow is reported,
// never acted on.
type ShadowComparison struct {
	Backend CodecBackend
	// SampleEvery compares one frame in every SampleEvery the shadow
	// accepts, counted across the router's decodes; 0 or 1 compares every
	// one.
	SampleEvery int
	// MaxAbsoluteError is the largest difference of a sample that still
	// matches (0: exact), for a lossy oracle that rounds differently.
	MaxAbsoluteError int64
	// Report receives each comparison. Calls are serialized.
	Report func(ShadowReport)
}

// CodecRouterOptions configures a CodecRouter.
type CodecRouterOptions struct {
	// TransferSyntaxUID is the syntax the router decodes and is registered
	// for; it decides how frames are found among the Items.
	TransferSyntaxUID string
	// Backends are tried in order for each frame: the first that accepts
	// the frame decodes it.
	Backends []CodecBackend
	// Fallback decides whether a frame a backend failed to decode goes to
	// the next backend that accepts it. nil falls back on every failure;
	// a function returning false forces the first accepting backend.
	// Cancellation never falls back.
	Fallback func(backend string, err error) bool
	// Shadow, when set, compares sampled frames with another backend.
	Shadow *ShadowComparison
	// Admission, when set, is acquired around each frame for its estimated
	// working set (FrameBytes, twice when the frame is shadowed), so that
	// frames decoded at once, by this router or others sharing the
	// admission, hold at most its capacity.
	Admission *MemoryAdmission
	// FrameBytes estimates the bytes one decode of a frame holds, from its
	// encoded size and its (output) metadata; nil counts the encoded frame
	// and the decoded one.
	FrameBytes func(codestreamBytes int, metadata Metadata) int64
	// Workers decodes up to that many frames of an object at once; 0 or 1
	// decodes them in order.
	Workers int
	// Observe receives the decision for each frame decoded. Calls are
	// serialized.
	Observe func(FrameCodecDecision)
}

// Reasons a backend was passed over for a frame (BackendDecline.Reason).
const (
	DeclineNotAccepted  = "not-accepted"
	DeclineDecodeFailed = "decode-failed"
)

// BackendDecline is a backend passed over for a frame, and why.
type BackendDecline struct {
	Backend string
	Reason  string
}

// FrameCodecDecision says which backend decoded a frame and why. It carries
// names and fixed codes only, no codec error text or sample values.
type FrameCodecDecision struct {
	TransferSyntaxUID string
	// Frame is the frame's index in the object; -1 for a region or partial
	// decode, which receives the codestream without its index.
	Frame    int
	Backend  string
	Declined []BackendDecline
	// Fallback: an earlier backend failed on this frame.
	Fallback bool
	// Partial: a region or partial decode (FrameRegionDecoder,
	// PartialFrameDecoder).
	Partial bool
	// AdmittedBytes is what the frame held of the Admission; 0 without one.
	AdmittedBytes int64
}

// ShadowOutcome is the result of one shadow comparison.
type ShadowOutcome uint8

const (
	ShadowMatched ShadowOutcome = iota + 1
	ShadowMismatched
	// ShadowFailed: the shadow backend could not decode the frame.
	ShadowFailed
)

func (o ShadowOutcome) String() string {
	switch o {
	case ShadowMatched:
		return "matched"
	case ShadowMismatched:
		return "mismatched"
	case ShadowFailed:
		return "failed"
	}
	return fmt.Sprintf("ShadowOutcome(%d)", uint8(o))
}

// Reasons of a mismatched or failed shadow comparison (ShadowReport.Reason).
const (
	ShadowReasonDecodeError = "decode-error"
	ShadowReasonGeometry    = "geometry" // rows, columns or byte length differ
	ShadowReasonPhotometric = "photometric"
	ShadowReasonSamples     = "samples"
)

// ShadowReport is one shadow comparison. It is free of patient data by
// construction: backend names, the frame's index and geometry, fixed reason
// codes and difference statistics, never sample values, attribute values or
// codec error text.
type ShadowReport struct {
	TransferSyntaxUID string
	Frame             int
	Primary, Shadow   string
	Outcome           ShadowOutcome
	// Reason is empty for a match and one of the ShadowReason codes
	// otherwise.
	Reason        string
	Rows, Columns int
	// Samples is the number of samples compared, DifferingSamples those
	// that differ at all (also within MaxAbsoluteError), and
	// MaxAbsoluteDifference the largest difference.
	Samples, DifferingSamples int
	MaxAbsoluteDifference     int64
	// FirstDifference is the index of the first differing sample in the
	// decoded frame's order, -1 when none differs.
	FirstDifference int
}

// CodecRouterStats counts what a router decided.
type CodecRouterStats struct {
	// Frames counts frames decoded (partial decodes included), Fallbacks
	// those decoded after an earlier backend failed, and Backends the
	// frames each backend decoded.
	Frames, Fallbacks int64
	Backends          map[string]int64
	// Shadow comparisons: Compared = Matched + Mismatched + Failed;
	// SampledOut were eligible but not sampled.
	Compared, Matched, Mismatched, Failed, SampledOut int64
}

// CodecRouter is a Codec that chooses a backend for each frame: the first of
// its backends that accepts the frame, falling back to the next on failure
// when the policy allows. It can compare sampled frames with an oracle
// backend (shadow execution) and hold each frame's working set in a shared
// MemoryAdmission. Register it for its transfer syntax in any Registry;
// FrameReader then routes single frames, regions and partial decodes through
// it too.
type CodecRouter struct {
	uid        string
	format     encapsulated.Format
	hasFormat  bool
	backends   []CodecBackend
	fallback   func(string, error) bool
	shadow     *ShadowComparison
	admission  *MemoryAdmission
	frameBytes func(int, Metadata) int64
	workers    int
	observe    func(FrameCodecDecision)

	shadowSeq atomic.Int64
	mu        sync.Mutex // serializes Observe, Report and stats
	stats     CodecRouterStats
}

// NewCodecRouter returns a router of options.
func NewCodecRouter(options CodecRouterOptions) (*CodecRouter, error) {
	uid := transfer.NormalizeUID(options.TransferSyntaxUID)
	if uid == "" {
		return nil, fmt.Errorf("%w: no transfer syntax", ErrInvalidCodecRouter)
	}
	if len(options.Backends) == 0 {
		return nil, fmt.Errorf("%w: no backends", ErrInvalidCodecRouter)
	}
	names := map[string]bool{}
	for _, backend := range options.Backends {
		if err := validBackend(backend); err != nil {
			return nil, err
		}
		if names[backend.Name] {
			return nil, fmt.Errorf("%w: two backends named %q", ErrInvalidCodecRouter, backend.Name)
		}
		names[backend.Name] = true
	}
	if options.Workers < 0 {
		return nil, fmt.Errorf("%w: %d workers", ErrInvalidCodecRouter, options.Workers)
	}
	r := &CodecRouter{
		uid:        uid,
		backends:   append([]CodecBackend(nil), options.Backends...),
		fallback:   options.Fallback,
		admission:  options.Admission,
		frameBytes: options.FrameBytes,
		workers:    max(options.Workers, 1),
		observe:    options.Observe,
		stats:      CodecRouterStats{Backends: map[string]int64{}},
	}
	r.format, r.hasFormat = streamFrameFormat(uid)
	if shadow := options.Shadow; shadow != nil {
		if err := validBackend(shadow.Backend); err != nil {
			return nil, err
		}
		if shadow.SampleEvery < 0 || shadow.MaxAbsoluteError < 0 {
			return nil, fmt.Errorf("%w: shadow sampling %d, tolerance %d", ErrInvalidCodecRouter, shadow.SampleEvery, shadow.MaxAbsoluteError)
		}
		copied := *shadow
		copied.SampleEvery = max(copied.SampleEvery, 1)
		r.shadow = &copied
	}
	return r, nil
}

func validBackend(backend CodecBackend) error {
	switch {
	case strings.TrimSpace(backend.Name) == "":
		return fmt.Errorf("%w: a backend without a name", ErrInvalidCodecRouter)
	case backend.Codec == nil:
		return fmt.Errorf("%w: backend %q has no codec", ErrInvalidCodecRouter, backend.Name)
	}
	return nil
}

// Register registers the router for its transfer syntax in registry.
func (r *CodecRouter) Register(registry Registry) error {
	if registry == nil || isNilMemoryRegistry(registry) {
		return ErrCodecRegistryNil
	}
	return registry.RegisterCodec(r.uid, r)
}

// TransferSyntaxUID is the syntax the router decodes.
func (r *CodecRouter) TransferSyntaxUID() string { return r.uid }

// Stats returns a snapshot of the router's counters.
func (r *CodecRouter) Stats() CodecRouterStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	stats := r.stats
	stats.Backends = make(map[string]int64, len(r.stats.Backends))
	for name, n := range r.stats.Backends {
		stats.Backends[name] = n
	}
	return stats
}

// Decode decodes every frame of pixel.
func (r *CodecRouter) Decode(pixel PixelData, obj *object.Object) (Frames, error) {
	return r.DecodeContext(context.Background(), pixel, obj)
}

// DecodeContext decodes every frame of pixel, each with the backend the
// router chooses for it, Workers frames at a time.
func (r *CodecRouter) DecodeContext(ctx context.Context, pixel PixelData, obj *object.Object) (Frames, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !pixel.Encapsulated {
		return Frames{}, fmt.Errorf("%w: a codec router decodes encapsulated Pixel Data", ErrIncompatiblePixelData)
	}
	metadata, err := ExtractMetadata(obj)
	if err != nil {
		return Frames{}, err
	}
	count := metadata.NumberOfFrames
	frameAt, recovered, err := r.framesOf(ctx, pixel, obj, count)
	if err != nil {
		return Frames{}, err
	}
	frameMetadata := metadata
	frameMetadata.NumberOfFrames = 1
	base := oneFrameElements(obj)

	results := make([]Frames, count)
	decode := func(ctx context.Context, i int) error {
		codestream, err := frameAt(ctx, i)
		if err != nil {
			return err
		}
		results[i], err = r.decodeFrame(ctx, i, codestream, frameMetadata, base)
		return err
	}
	if err := r.forEachFrame(ctx, count, decode); err != nil {
		return Frames{}, err
	}

	out := Frames{Rows: results[0].Rows, Columns: results[0].Columns, Data: make([][]byte, count), PhotometricInterpretation: results[0].PhotometricInterpretation}
	if recovered {
		out.AddDiagnostic(DiagnosticBasicOffsetTableIgnored)
	}
	for i, frames := range results {
		switch {
		case frames.Rows != out.Rows || frames.Columns != out.Columns:
			return Frames{}, fmt.Errorf("%w: frame %d is %dx%d, frame 0 %dx%d", ErrPixelDataSizeMismatch, i, frames.Columns, frames.Rows, out.Columns, out.Rows)
		case normalizedPhotometric(frames.PhotometricInterpretation) != normalizedPhotometric(out.PhotometricInterpretation):
			return Frames{}, fmt.Errorf("%w: frame %d decoded as %q, frame 0 as %q", ErrCodecBackendsDisagree, i, frames.PhotometricInterpretation, out.PhotometricInterpretation)
		}
		out.Data[i] = frames.Data[0]
		out.Lossy = out.Lossy || frames.Lossy
		for _, code := range frames.Diagnostics {
			out.AddDiagnostic(code)
		}
	}
	return out, nil
}

// forEachFrame runs decode for frames 0..count-1 on up to Workers
// goroutines; the first error cancels the others and is returned.
func (r *CodecRouter) forEachFrame(ctx context.Context, count int, decode func(context.Context, int) error) error {
	workers := min(r.workers, count)
	if workers <= 1 {
		for i := 0; i < count; i++ {
			if err := decode(ctx, i); err != nil {
				return err
			}
		}
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		next    atomic.Int64
		wg      sync.WaitGroup
		once    sync.Once
		failure error
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= count || ctx.Err() != nil {
					return
				}
				if err := decode(ctx, i); err != nil {
					once.Do(func() {
						failure = err
						cancel()
					})
					return
				}
			}
		}()
	}
	wg.Wait()
	if failure != nil {
		return failure
	}
	// Workers stop early without a failure only when the caller's context
	// ended.
	return ctx.Err()
}

// framesOf returns a function that reads frame i's encoded bytes, and
// whether an inconsistent Basic Offset Table was replaced by a marker scan.
func (r *CodecRouter) framesOf(ctx context.Context, pixel PixelData, obj *object.Object, count int) (func(context.Context, int) ([]byte, error), bool, error) {
	if count < 1 {
		return nil, false, fmt.Errorf("%w: %d frames", ErrInvalidMetadata, count)
	}
	if r.hasFormat {
		plan, err := encapsulated.FromFragments(ctx, pixel.Sequence, obj, count, r.format, encapsulated.Limits{})
		if err != nil {
			if errors.Is(err, encapsulated.ErrFrameCount) {
				err = errors.Join(ErrPixelDataSizeMismatch, err)
			}
			return nil, false, err
		}
		return func(ctx context.Context, i int) ([]byte, error) {
			view, err := plan.Frame(ctx, i)
			return view.Data, err
		}, plan.Recovery().BasicOffsetTableIgnored, nil
	}
	fragments := pixel.Sequence.Fragments
	switch {
	case len(fragments) == count:
		return func(_ context.Context, i int) ([]byte, error) { return fragments[i], nil }, false, nil
	case count == 1 && len(fragments) > 0:
		joined := bytes.Join(fragments, nil)
		return func(context.Context, int) ([]byte, error) { return joined, nil }, false, nil
	}
	return nil, false, fmt.Errorf("%w: %d Items for %d frames", ErrPixelDataSizeMismatch, len(fragments), count)
}

// oneFrameElements is obj without Pixel Data and its offset tables, with
// Number of Frames 1: the data set a backend decodes one frame in.
func oneFrameElements(obj *object.Object) []core.Element {
	var elements []core.Element
	for _, element := range obj.Elements() {
		switch element.Tag() {
		case core.TagPixelData, tagExtendedOffsetTable, tagExtendedOffsetTableLengths, tagEncapsulatedPixelDataValueTotalLength:
			continue
		case tagNumberOfFrames:
			element = core.Element{Header: core.ElementHeader{Tag: tagNumberOfFrames, VR: core.VRIS}, Value: core.StringValue{"1"}}
		}
		elements = append(elements, element)
	}
	return elements
}

// decodeWith decodes one frame's codestream with codec, in a one-frame
// object built on base.
func decodeWith(ctx context.Context, codec Codec, codestream []byte, base []core.Element) (Frames, error) {
	sequence := core.FragmentSequence{Fragments: [][]byte{codestream}}
	pixel := core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true}, Value: sequence}
	proxy := object.FromElements(append(append(make([]core.Element, 0, len(base)+1), base...), pixel), nil)
	frames, err := decodeRegisteredCodec(ctx, codec, PixelData{Encapsulated: true, Sequence: sequence}, proxy)
	if err != nil {
		return Frames{}, err
	}
	if len(frames.Data) != 1 {
		return Frames{}, fmt.Errorf("%w: a backend returned %d frames for one", ErrPixelDataSizeMismatch, len(frames.Data))
	}
	return frames, nil
}

// estimate is the bytes one decode of a frame holds.
func (r *CodecRouter) estimate(codestreamBytes int, metadata Metadata) int64 {
	if r.frameBytes != nil {
		return max(r.frameBytes(codestreamBytes, metadata), 1)
	}
	return max(int64(codestreamBytes)+metadata.FrameSize(), 1)
}

// admit acquires bytes of the admission, if any.
func (r *CodecRouter) admit(ctx context.Context, bytes int64) (func(), int64, error) {
	if r.admission == nil {
		return func() {}, 0, nil
	}
	release, err := r.admission.Acquire(ctx, bytes)
	if err != nil {
		return nil, 0, err
	}
	return release, bytes, nil
}

func (r *CodecRouter) mayFallBack(backend string, err error) bool {
	if isContextTermination(err) {
		return false
	}
	return r.fallback == nil || r.fallback(backend, err)
}

// shadowed decides whether this frame is compared: the shadow accepts it and
// it is the sampled one of SampleEvery.
func (r *CodecRouter) shadowed(codestream []byte, metadata Metadata) bool {
	if r.shadow == nil || r.shadow.Backend.Accept != nil && !r.shadow.Backend.Accept(codestream, metadata) {
		return false
	}
	if (r.shadowSeq.Add(1)-1)%int64(r.shadow.SampleEvery) == 0 {
		return true
	}
	r.mu.Lock()
	r.stats.SampledOut++
	r.mu.Unlock()
	return false
}

// decodeFrame decodes frame index with the first backend that accepts it.
func (r *CodecRouter) decodeFrame(ctx context.Context, index int, codestream []byte, metadata Metadata, base []core.Element) (Frames, error) {
	shadow := r.shadowed(codestream, metadata)
	need := r.estimate(len(codestream), metadata)
	if shadow {
		need *= 2
	}
	release, admitted, err := r.admit(ctx, need)
	if err != nil {
		return Frames{}, err
	}
	defer release()

	decision := FrameCodecDecision{TransferSyntaxUID: r.uid, Frame: index, AdmittedBytes: admitted}
	var failure error
	for _, backend := range r.backends {
		if backend.Accept != nil && !backend.Accept(codestream, metadata) {
			decision.Declined = append(decision.Declined, BackendDecline{Backend: backend.Name, Reason: DeclineNotAccepted})
			continue
		}
		frames, err := decodeWith(ctx, backend.Codec, codestream, base)
		if err == nil {
			decision.Backend = backend.Name
			if decision.Fallback {
				frames.AddDiagnostic(DiagnosticCodecBackendFallback)
			}
			if shadow && backend.Name != r.shadow.Backend.Name {
				r.compareShadow(ctx, index, codestream, metadata, base, backend.Name, frames)
			}
			r.decided(decision)
			return frames, nil
		}
		if err := ctx.Err(); err != nil {
			return Frames{}, err
		}
		if !r.mayFallBack(backend.Name, err) {
			return Frames{}, err
		}
		failure = err
		decision.Declined = append(decision.Declined, BackendDecline{Backend: backend.Name, Reason: DeclineDecodeFailed})
		decision.Fallback = true
	}
	if failure != nil {
		return Frames{}, failure
	}
	return Frames{}, fmt.Errorf("%w: frame %d of %s", ErrNoCodecBackend, index, r.uid)
}

// decided records a decision and passes it to Observe.
func (r *CodecRouter) decided(decision FrameCodecDecision) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stats.Frames++
	r.stats.Backends[decision.Backend]++
	if decision.Fallback {
		r.stats.Fallbacks++
	}
	if r.observe != nil {
		r.observe(decision)
	}
}

// compareShadow decodes the frame with the shadow backend and reports how
// it compares with got. Cancellation ends it without a report.
func (r *CodecRouter) compareShadow(ctx context.Context, index int, codestream []byte, metadata Metadata, base []core.Element, primary string, got Frames) {
	report := ShadowReport{TransferSyntaxUID: r.uid, Frame: index, Primary: primary, Shadow: r.shadow.Backend.Name,
		Rows: got.Rows, Columns: got.Columns, FirstDifference: -1}
	want, err := decodeWith(ctx, r.shadow.Backend.Codec, codestream, base)
	switch {
	case ctx.Err() != nil:
		return
	case err != nil:
		report.Outcome, report.Reason = ShadowFailed, ShadowReasonDecodeError
	case want.Rows != got.Rows || want.Columns != got.Columns || len(want.Data[0]) != len(got.Data[0]):
		report.Outcome, report.Reason = ShadowMismatched, ShadowReasonGeometry
	case normalizedPhotometric(want.PhotometricInterpretation) != normalizedPhotometric(got.PhotometricInterpretation):
		report.Outcome, report.Reason = ShadowMismatched, ShadowReasonPhotometric
	default:
		report.Samples, report.DifferingSamples, report.MaxAbsoluteDifference, report.FirstDifference = compareSamples(got.Data[0], want.Data[0], metadata)
		report.Outcome = ShadowMatched
		if report.MaxAbsoluteDifference > r.shadow.MaxAbsoluteError {
			report.Outcome, report.Reason = ShadowMismatched, ShadowReasonSamples
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stats.Compared++
	switch report.Outcome {
	case ShadowMatched:
		r.stats.Matched++
	case ShadowMismatched:
		r.stats.Mismatched++
	default:
		r.stats.Failed++
	}
	if r.shadow.Report != nil {
		r.shadow.Report(report)
	}
}

// compareSamples compares two decoded frames of equal length sample by
// sample, in the container BitsAllocated gives (8, 16 or 32 bits, two's
// complement when PixelRepresentation is 1; bytes otherwise).
func compareSamples(a, b []byte, metadata Metadata) (samples, differing int, worst int64, first int) {
	width := 1
	switch metadata.BitsAllocated {
	case 16:
		width = 2
	case 32:
		width = 4
	}
	signed := metadata.PixelRepresentation == 1
	value := func(data []byte, i int) int64 {
		switch width {
		case 2:
			v := binary.LittleEndian.Uint16(data[i:])
			if signed {
				return int64(int16(v))
			}
			return int64(v)
		case 4:
			v := binary.LittleEndian.Uint32(data[i:])
			if signed {
				return int64(int32(v))
			}
			return int64(v)
		}
		if signed && metadata.BitsAllocated == 8 {
			return int64(int8(data[i]))
		}
		return int64(data[i])
	}
	first = -1
	for i := 0; i+width <= len(a); i += width {
		samples++
		d := value(a, i) - value(b, i)
		if d < 0 {
			d = -d
		}
		if d == 0 {
			continue
		}
		if first < 0 {
			first = samples - 1
		}
		differing++
		worst = max(worst, d)
	}
	return samples, differing, worst, first
}

// DecodeFrameRegion decodes region of one frame with the first backend that
// accepts the frame, when that backend is a FrameRegionDecoder; otherwise it
// returns ErrRegionUnsupported, and FrameReader decodes the whole frame
// through the router, with the same backend.
func (r *CodecRouter) DecodeFrameRegion(ctx context.Context, codestream []byte, metadata Metadata, region image.Rectangle) (Frames, error) {
	return r.decodePart(ctx, codestream, metadata, region.Size(), ErrRegionUnsupported, func(codec Codec) (Frames, bool, error) {
		decoder, ok := codec.(FrameRegionDecoder)
		if !ok {
			return Frames{}, false, nil
		}
		frames, err := decoder.DecodeFrameRegion(ctx, codestream, metadata, region)
		return frames, true, err
	})
}

// PartialDecodeCapabilities reports the capabilities of the backend that
// would decode the frame.
func (r *CodecRouter) PartialDecodeCapabilities(codestream []byte, metadata Metadata) (PartialDecodeCapabilities, error) {
	for _, backend := range r.backends {
		if backend.Accept != nil && !backend.Accept(codestream, metadata) {
			continue
		}
		if decoder, ok := backend.Codec.(PartialFrameDecoder); ok {
			return decoder.PartialDecodeCapabilities(codestream, metadata)
		}
		_, region := backend.Codec.(FrameRegionDecoder)
		return PartialDecodeCapabilities{Region: region}, nil
	}
	return PartialDecodeCapabilities{}, nil
}

// DecodeFramePartial serves a partial decode with the first backend that
// accepts the frame, when it is a PartialFrameDecoder; otherwise it returns
// ErrPartialDecodeUnsupported and FrameReader falls back.
func (r *CodecRouter) DecodeFramePartial(ctx context.Context, codestream []byte, metadata Metadata, request PartialDecodeRequest) (Frames, error) {
	size := ReducedRegion(request.Region, request.ReductionLevel).Size()
	return r.decodePart(ctx, codestream, metadata, size, ErrPartialDecodeUnsupported, func(codec Codec) (Frames, bool, error) {
		decoder, ok := codec.(PartialFrameDecoder)
		if !ok {
			return Frames{}, false, nil
		}
		frames, err := decoder.DecodeFramePartial(ctx, codestream, metadata, request)
		return frames, true, err
	})
}

// decodePart runs a region or partial decode on the backend that accepts
// the frame, falling back like a whole frame. unsupported is the error that
// sends the caller to a whole-frame decode.
func (r *CodecRouter) decodePart(ctx context.Context, codestream []byte, metadata Metadata, size image.Point, unsupported error,
	call func(Codec) (Frames, bool, error)) (Frames, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	part := metadata
	part.Rows, part.Columns = uint16(min(max(size.Y, 0), 0xFFFF)), uint16(min(max(size.X, 0), 0xFFFF))
	release, admitted, err := r.admit(ctx, r.estimate(len(codestream), part))
	if err != nil {
		return Frames{}, err
	}
	defer release()

	decision := FrameCodecDecision{TransferSyntaxUID: r.uid, Frame: -1, Partial: true, AdmittedBytes: admitted}
	var failure error
	for _, backend := range r.backends {
		if backend.Accept != nil && !backend.Accept(codestream, metadata) {
			decision.Declined = append(decision.Declined, BackendDecline{Backend: backend.Name, Reason: DeclineNotAccepted})
			continue
		}
		frames, implemented, err := call(backend.Codec)
		switch {
		case !implemented:
			return Frames{}, unsupported
		case err == nil:
			decision.Backend = backend.Name
			if decision.Fallback {
				frames.AddDiagnostic(DiagnosticCodecBackendFallback)
			}
			r.decided(decision)
			return frames, nil
		case ctx.Err() != nil:
			return Frames{}, ctx.Err()
		case errors.Is(err, ErrRegionUnsupported), errors.Is(err, ErrPartialDecodeUnsupported),
			errors.Is(err, ErrInvalidReduction), errors.Is(err, ErrInvalidQualityLayers):
			// Not a failure of the backend: the caller falls back or
			// refuses.
			return Frames{}, err
		case !r.mayFallBack(backend.Name, err):
			return Frames{}, err
		}
		failure = err
		decision.Declined = append(decision.Declined, BackendDecline{Backend: backend.Name, Reason: DeclineDecodeFailed})
		decision.Fallback = true
	}
	if failure != nil {
		return Frames{}, failure
	}
	return Frames{}, fmt.Errorf("%w: a part of a frame of %s", ErrNoCodecBackend, r.uid)
}

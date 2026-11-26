package pixeldata

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// errStreamIneligible sends TranscodePath to the in-memory route; it is
// never returned to callers.
var errStreamIneligible = errors.New("pixeldata: input needs the in-memory transcode route")

// transcodeStreamStaged, when set by tests, is told each time the streaming
// route stages an output.
var transcodeStreamStaged func()

// transcodeStreamProgress, when set by tests, is told the bytes of each
// frame the streaming route has encoded to its spool and of each write of
// its output before it is made, after ordered frame consumption or before output writes.
var transcodeStreamProgress func(bytes int)

// transcodePathStream is TranscodePath's bounded frame-window route. The source is read
// with its Pixel Data deferred, each frame is read, decoded and (for an
// encapsulated target) encoded on its own, encoded frames are spooled to a
// private file beside the destination, and the output is written with
// object.WriteFileFrames. Output metadata is built by the same functions as
// the in-memory route, so both produce the same file. errStreamIneligible
// (before anything is staged) leaves the source rewound for that route.
func transcodePathStream(ctx context.Context, txn *transcodePathTxn, target transfer.Syntax, options TranscodeOptions, limits TranscodeLimits) (TranscodeReport, error) {
	if options.RecoverUN != nil || options.Rewrite != nil || options.Recovery != (DecodeRecovery{}) {
		return TranscodeReport{}, errStreamIneligible
	}
	stream, err := openTranscodeStream(ctx, txn, target, options, limits)
	if err != nil {
		if _, seekErr := txn.source.Seek(0, io.SeekStart); seekErr != nil {
			return TranscodeReport{}, &TranscodeError{Stage: "read", Err: ErrTranscodeTransaction}
		}
		return TranscodeReport{}, err
	}
	var (
		report TranscodeReport
		write  func(io.Writer) error
		items  int
	)
	switch stream.plan.mode {
	case transcodeNativeClone, transcodeEquivalentClone:
		report, write, err = stream.clone(ctx)
	case transcodeDecodeNative, transcodeDecodeEncapsulatedUncompressed:
		// Group Length measurement may replay an undefined encapsulated
		// Pixel Data value. A serial decoder's first frame is consumable only
		// once, so use the existing bounded spool when replay was requested.
		if options.Pipeline.Workers <= 1 && options.WriteOptions.GroupLength == object.GroupLengthPreserve {
			report, write, err = stream.decodeOnWrite(ctx)
		} else {
			var spool *stagedTranscodeFile
			spool, err = stageTranscodeSpool(txn)
			if err != nil {
				return TranscodeReport{}, err
			}
			defer spool.cleanup(txn.parent)
			report, write, err = stream.decodeToSpool(ctx, spool)
		}
	default:
		var spool *stagedTranscodeFile
		spool, err = stageTranscodeSpool(txn)
		if err != nil {
			return TranscodeReport{}, err
		}
		defer spool.cleanup(txn.parent)
		report, write, err = stream.encode(ctx, spool)
	}
	if err != nil {
		return TranscodeReport{}, err
	}
	report.Streaming = true
	// Items expected in encapsulated output: the Basic Offset Table and one
	// per frame, or the source's own Items when they are copied verbatim.
	items = stream.metadata.NumberOfFrames + 1
	if stream.outputFragments > 0 {
		items = stream.outputFragments + 1
	} else if stream.plan.mode == transcodeEquivalentClone {
		items = stream.items + 1
	}
	if progress := transcodeStreamProgress; progress != nil {
		writeOutput := write
		write = func(w io.Writer) error { return writeOutput(progressWriter{w: w, progress: progress}) }
	}
	staged, err := stageTranscodeWith(ctx, txn, limits, write)
	if err != nil {
		return TranscodeReport{}, err
	}
	defer staged.cleanup(txn.parent)
	report.Timings.DecodeWorkerTime += stream.serialDecodeTime
	if transcodeStreamStaged != nil {
		transcodeStreamStaged()
	}
	if err := validateStagedTranscodeStream(ctx, staged, limits, stream.output.TransferSyntax, items); err != nil {
		return TranscodeReport{}, err
	}
	if err := ctx.Err(); err != nil {
		return TranscodeReport{}, err
	}
	if err := precommitTranscode(txn, staged); err != nil {
		return TranscodeReport{}, err
	}
	reportAvailable, err := publishStagedTranscode(ctx, txn, staged)
	applyTranscodePublicationReport(&report, txn)
	if err != nil {
		if reportAvailable {
			return report, err
		}
		return TranscodeReport{}, err
	}
	return report, nil
}

// progressWriter reports each write to the transcodeStreamProgress hook
// while its data is still held.
type progressWriter struct {
	w        io.Writer
	progress func(bytes int)
}

func (p progressWriter) Write(data []byte) (int, error) {
	p.progress(len(data))
	return p.w.Write(data)
}

// transcodeStream is a source read with deferred Pixel Data and the frames
// it holds.
type transcodeStream struct {
	file     *object.File
	source   transfer.Syntax
	target   transfer.Syntax
	plan     transcodePlan
	options  TranscodeOptions
	limits   TranscodeLimits
	metadata Metadata // of the source, all frames
	// frames reads the source frames (decoding modes); a copy only needs
	// the value's size and, when encapsulated, its Item count.
	frames          *FrameReader
	pixelHeader     core.ElementHeader
	inputBytes      int64
	items           int
	output          *object.File
	outputFragments int
	// Serial decoding happens while the private output is staged. Its
	// durations are applied after writing, before the report is published.
	serialDecodeTime time.Duration
}

func openTranscodeStream(ctx context.Context, txn *transcodePathTxn, target transfer.Syntax, options TranscodeOptions, limits TranscodeLimits) (*transcodeStream, error) {
	if txn.sourceInfo == nil || (txn.rawInput == nil && txn.sourceInfo.Size() > limits.MaxInputBytes) {
		return nil, errStreamIneligible
	}
	var file *object.File
	var err error
	var frameInput io.ReaderAt = txn.source
	if txn.rawInput != nil {
		file, err = readRawTranscodeFile(ctx, txn.rawInput, limits)
		frameInput = txn.rawInput.reader
	} else {
		file, err = object.ReadFileWithOptions(&transcodeContextFile{File: txn.source, ctx: ctx}, object.ReadFileOptions{
			DeferPixelData:   true,
			MaxElementBytes:  limits.MaxInputBytes,
			MaxTotalBytes:    math.MaxInt64,
			MaxSequenceDepth: limits.MaxDepth,
			MaxElements:      limits.MaxElements,
			MaxFragments:     limits.MaxFragments,
		})
	}
	if err != nil {
		if txn.rawInput != nil {
			return nil, err
		}
		return nil, errStreamIneligible
	}
	source, err := strictFileTransferSyntax(file)
	if err != nil {
		return nil, errStreamIneligible
	}
	target, err = canonicalTargetTransferSyntax(target)
	if err != nil {
		return nil, errStreamIneligible
	}
	if jpeg2000Part2(source.UID) || jpeg2000Part2(target.UID) {
		// Frames in one Annex J collection require simultaneous decoding or
		// encoding. The bounded whole-object route keeps the collection intact.
		return nil, errStreamIneligible
	}
	if err := validateTargetVerificationCodec(source, target, options); err != nil {
		return nil, err
	}
	littleEndian := func(s transfer.Syntax) bool { return s.IsLittleEndian() && !s.Deflated }
	if !littleEndian(source) || !littleEndian(target) || len(file.Preamble) != 0 && len(file.Preamble) != 128 {
		return nil, errStreamIneligible
	}
	dataset := file.Dataset
	if err := validateTranscodeOutputOptions(dataset, target, options); err != nil {
		return nil, err
	}
	pixel, ok := dataset.Get(core.TagPixelData)
	locations := file.ValueLocations(core.TagPixelData)
	if !ok || pixel.Value != nil || len(locations) != 1 || dataset.Has(tagFloatPixelData) || dataset.Has(tagDoubleFloatPixelData) {
		return nil, errStreamIneligible
	}
	plan := buildTranscodePlan(source, target, options.ForceReencode, true)
	if plan.mode == transcodeNativeClone && options.PlanarOutput != PlanarOutputDefault {
		plan.mode = transcodeDecodeNative
	}
	if plan.mode == transcodeMissingPixelData || usesCodestreamTranscoder(plan, options) {
		return nil, errStreamIneligible
	}
	if nested, err := hasNestedPixelData(dataset, limits); err != nil || nested {
		if txn.rawInput != nil && err != nil {
			return nil, err
		}
		return nil, errStreamIneligible
	}
	metadata, err := ExtractMetadata(dataset)
	if err != nil || metadata.BitsAllocated < 8 || plan.mode == transcodeDecodeEncode && mayHaveEmbeddedOverlays(dataset) {
		if txn.rawInput != nil && err != nil {
			return nil, err
		}
		return nil, errStreamIneligible
	}
	stream := &transcodeStream{
		file: file, source: source, target: target, plan: plan, options: options, limits: limits,
		metadata: metadata, pixelHeader: pixel.Header,
	}
	switch plan.mode {
	case transcodeNativeClone:
		if pixel.Header.Length.IsUndefined() || locations[0].Length < metadata.FrameSize()*int64(metadata.NumberOfFrames) {
			return nil, errStreamIneligible
		}
		stream.inputBytes = locations[0].Length
	case transcodeEquivalentClone:
		items, basic, err := scanItems(frameInput, locations[0].ValueOffset, locations[0].Length, limits.MaxFragments, limits.MaxFrames)
		if err != nil {
			if txn.rawInput != nil {
				return nil, err
			}
			return nil, errStreamIneligible
		}
		stream.items, stream.inputBytes = len(items), int64(len(basic))
		for _, item := range items {
			stream.inputBytes += int64(item.Length)
		}
		if explicitEncapsulation(options) {
			frames, err := NewFrameReader(ctx, file, frameInput, outputFrameReaderOptions(options, limits))
			if err != nil {
				return nil, err
			}
			stream.frames = frames
		}
	default:
		frames, err := NewFrameReader(ctx, file, frameInput, sourceFrameReaderOptions(options, limits))
		if err != nil {
			if txn.rawInput != nil && !errors.Is(err, ErrFrameLayout) {
				return nil, err
			}
			return nil, errStreamIneligible
		}
		stream.frames, stream.inputBytes = frames, frames.inputBytes
	}
	return stream, nil
}

func streamFrameFormat(uid string) (encapsulated.Format, bool) {
	switch transfer.NormalizeUID(uid) {
	case transfer.JPEGBaseline.UID, transfer.JPEGExtended.UID, transfer.JPEGLosslessNonHierarchical.UID, transfer.JPEGLosslessSV1.UID:
		return encapsulated.JPEG, true
	case transfer.JPEGLSLossless.UID, transfer.JPEGLSNearLossless.UID:
		return encapsulated.JPEGLS, true
	case transfer.JPEG2000LosslessOnly.UID, transfer.JPEG2000.UID, transfer.HTJ2KLossless.UID, transfer.HTJ2KLosslessRPCL.UID, transfer.HTJ2K.UID:
		return encapsulated.JPEG2000, true
	}
	return 0, false
}

// mayHaveEmbeddedOverlays reports an overlay group without Overlay Data,
// whose bits may live in the Pixel Data; extracting them needs every frame,
// so such inputs take the in-memory route.
func mayHaveEmbeddedOverlays(dataset *object.Object) bool {
	for group := uint16(0x6000); group <= 0x601e; group += 2 {
		if dataset.Has(core.NewTag(group, 0x0010)) && !dataset.Has(core.NewTag(group, 0x3000)) {
			return true
		}
	}
	return false
}

// checkDecodeBudget applies the in-memory route's decompression limits to
// the whole source before any frame is decoded.
func (s *transcodeStream) checkDecodeBudget() error {
	return checkDecodeAdmission(transcodeDecompressLimits(s.limits), s.metadata, s.source, s.inputBytes)
}

// transcodeDecompressLimits are the decompression limits a transcode
// decodes its source under, on either route.
func transcodeDecompressLimits(limits TranscodeLimits) DecompressLimits {
	return DecompressLimits{
		MaxFrames:         limits.MaxFrames,
		MaxPixels:         int64(limits.MaxPixels),
		MaxInputBytes:     limits.MaxInputBytes,
		MaxNativeBytes:    limits.MaxOutputBytes,
		MaxExpansionRatio: limits.MaxExpansionRatio,
	}
}

// sourceFrameReaderOptions are the options the streaming route reads and
// decodes the source frames with.
func sourceFrameReaderOptions(options TranscodeOptions, limits TranscodeLimits) FrameReaderOptions {
	decode := transcodeDecompressLimits(limits)
	decode.MaxFrames = 0 // FrameReader admits each frame on its own
	return FrameReaderOptions{
		Registry:  options.DecoderRegistry,
		Limits:    decode,
		MaxFrames: limits.MaxFrames, MaxFragments: limits.MaxFragments,
	}
}

// nativeHeader is the decoded first frame's data set as the header of all
// frames: Number of Frames restored and no Pixel Data payload.
func (s *transcodeStream) nativeHeader(first *object.Object) *object.Object {
	elements := removeElements(first.Elements(), core.TagPixelData, tagNumberOfFrames)
	if original, ok := s.file.Dataset.Get(tagNumberOfFrames); ok {
		elements = append(elements, original)
	}
	pixel, _ := first.Get(core.TagPixelData)
	elements = append(elements, core.NewRawElement(core.TagPixelData, pixel.VR(), nil))
	header := object.FromElements(elements, nil)
	header.SetValueByteOrder(binary.LittleEndian)
	return header
}

// sameDecodedLayout checks that frame i decoded to the color layout of the
// first frame, which the header records for all of them.
func sameDecodedLayout(first, decoded *object.Object) bool {
	for _, tag := range []core.Tag{tagPhotometricInterpretation, tagSamplesPerPixel, tagPlanarConfiguration} {
		a, aok := first.Get(tag)
		b, bok := decoded.Get(tag)
		if aok != bok {
			return false
		}
		if !aok {
			continue
		}
		ra, _ := a.RawBytes()
		rb, _ := b.RawBytes()
		sa, _ := first.GetString(tag)
		sb, _ := decoded.GetString(tag)
		if !bytes.Equal(ra, rb) || sa != sb {
			return false
		}
	}
	return true
}

// clone writes the source data set unchanged but for its transfer syntax,
// streaming the deferred Pixel Data from the source, as the in-memory route
// clones it.
func (s *transcodeStream) clone(ctx context.Context) (TranscodeReport, func(io.Writer) error, error) {
	report := TranscodeReport{PixelDataPreserved: true, Frames: s.metadata.NumberOfFrames, NativeBytes: s.metadata.TotalSize()}
	if s.source.Encapsulated {
		report.EncodedBytes = s.inputBytes
	} else {
		report.EncodedBytes = int64(s.pixelHeader.Length)
	}
	dataset := s.file.Dataset
	dataset.SetValueByteOrder(binary.LittleEndian)
	// The deferred Pixel Data is not measured: the limits were checked on
	// the source file and its value length.
	if s.inputBytes > s.limits.MaxOutputBytes {
		return TranscodeReport{}, nil, transcodeLimitError("output_bytes")
	}
	if err := applyTranscodeIdentity(dataset, dataset, &report, s.options); err != nil {
		return TranscodeReport{}, nil, err
	}
	header := object.FromElements(removeElements(dataset.Elements(), core.TagPixelData), nil)
	if s.source.Encapsulated && explicitEncapsulation(s.options) {
		header = object.FromElements(encodedMetadataElements(header, nil), nil)
	}
	output, err := transcodeOutputFile(ctx, s.file, header, report, s.target, s.limits)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	s.output = output
	if s.source.Encapsulated && explicitEncapsulation(s.options) {
		source := &rawOutputFrameSource{ctx: ctx, reader: s.frames}
		if source.Frames() > s.limits.MaxFrames || uint64(s.metadata.Rows)*uint64(s.metadata.Columns)*uint64(source.Frames()) > s.limits.MaxPixels {
			return TranscodeReport{}, nil, transcodeLimitError("pixels_or_frames")
		}
		cap := s.options.Pipeline.MaxInFlightBytes
		if cap == 0 {
			cap = defaultTranscodeInFlightBytes
		}
		var peak int64
		for i := 0; i < source.Frames(); i++ {
			n, err := source.FrameLength(i)
			if err != nil {
				return TranscodeReport{}, nil, err
			}
			if n > cap {
				return TranscodeReport{}, nil, transcodeLimitError("in_flight_bytes")
			}
			peak = max(peak, n)
		}
		report.WorkerLimit, report.PeakInFlightFrames, report.PeakInFlightBytes = 1, 1, peak
		writeOptions, err := s.frameWriteOptions(source)
		if err != nil {
			return TranscodeReport{}, nil, err
		}
		return report, func(w io.Writer) error { return object.WriteFileFrames(w, output, source, writeOptions) }, nil
	}
	output.Dataset = dataset
	return report, func(w io.Writer) error { return object.WriteFileWithOptions(w, output, s.options.WriteOptions) }, nil
}

// encode processes bounded windows into an ordered spool. Unless explicitly
// skipped, each output frame is decoded (pixel-exact for lossless output), then
// returns the writer of the output with the frames copied from the spool.
func (s *transcodeStream) encode(ctx context.Context, spool *stagedTranscodeFile) (TranscodeReport, func(io.Writer) error, error) {
	if s.source.Encapsulated && s.source.UID != transfer.EncapsulatedUncompressedExplicitVRLittleEndian.UID && s.options.DecoderRegistry == nil {
		return TranscodeReport{}, nil, codecAvailabilityError(ErrCodecRegistryNil, s.source.UID, nil)
	}
	if err := s.checkDecodeBudget(); err != nil {
		return TranscodeReport{}, nil, err
	}
	if err := s.preflightPipelineFrame(true); err != nil {
		return TranscodeReport{}, nil, err
	}
	start := time.Now()
	var decodeReport DecompressReport
	first, firstFrame, err := s.frames.decodeFrameObject(ctx, 0, &decodeReport)
	firstDecodeTime := time.Since(start)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	header := s.nativeHeader(first)
	metadata, err := ExtractMetadata(header)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	nativeBytes := metadata.TotalSize()
	if nativeBytes <= 0 || nativeBytes > s.limits.MaxOutputBytes {
		return TranscodeReport{}, nil, transcodeLimitError("native_bytes")
	}
	report := TranscodeReport{Frames: metadata.NumberOfFrames, NativeBytes: nativeBytes, DecodeDiagnostics: decodeReport.Diagnostics}
	report.Timings.DecodeWorkerTime = firstDecodeTime
	resolved, err := resolveFrameEncoder(s.target, metadata, s.options)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	pipeline, err := newTranscodeFramePipeline(s.options, metadata.NumberOfFrames, metadata.FrameSize(), s.limits.MaxOutputBytes, true, s.frames.RawFrameLength)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	var (
		state   frameEncodeState
		lengths = make([]int64, metadata.NumberOfFrames)
		offsets = make([]int64, metadata.NumberOfFrames)
		spooled int64
	)
	if metadata.NumberOfFrames > s.limits.MaxFragments {
		return TranscodeReport{}, nil, transcodeLimitError("fragments")
	}
	// The layout reference contains no frame payload. Only frame zero's task
	// retains firstFrame, and its reference is cleared after consumption.
	layout := header
	first = nil
	err = pipeline.run(ctx, metadata.NumberOfFrames, &report, func(workCtx context.Context, i int) (transcodeFrameResult, error) {
		var result transcodeFrameResult
		start := time.Now()
		frame := firstFrame
		if i > 0 {
			decoded, next, decodeErr := s.frames.decodeFrameObject(workCtx, i, nil)
			if decodeErr != nil {
				return result, decodeErr
			}
			if !sameDecodedLayout(layout, decoded) {
				return result, fmt.Errorf("%w: frame %d decodes to another color layout", ErrIncompatiblePixelData, i+1)
			}
			frame = next
		}
		result.timings.DecodeWorkerTime = time.Since(start)
		if int64(len(frame)) != metadata.FrameSize() {
			return result, fmt.Errorf("%w: decoded frame size", ErrPixelDataSizeMismatch)
		}
		start = time.Now()
		encoded, encodeErr := encodeTranscodeFrame(workCtx, resolved, metadata, pipeline.encodedCap, frame)
		result.timings.EncodeWorkerTime = time.Since(start)
		if encodeErr != nil {
			return result, encodeErr
		}
		result.data = encoded.Data
		result.compatibilityProfile = encoded.CompatibilityProfile
		result.delta = encodedMetadataDelta{photometric: encoded.PhotometricInterpretation, planar: encoded.PlanarConfiguration}
		if !s.options.SkipVerification {
			verify := object.FromElements(encodedMetadataElements(header, &result.delta), nil)
			verify.SetValueByteOrder(binary.LittleEndian)
			start = time.Now()
			measured, verifyErr := verifyTranscodedFrame(workCtx, header, verify, encoded.Data, frame, s.target, s.options, s.limits, i, resolved.capabilities.Lossless)
			result.timings.VerifyWorkerTime = time.Since(start)
			if verifyErr != nil {
				return transcodeFrameResult{}, verifyErr
			}
			result.lossyVerification = measured
		}
		return result, nil
	}, func(i int, result transcodeFrameResult) error {
		encoded := EncodedFrame{Data: result.data, PhotometricInterpretation: result.delta.photometric, PlanarConfiguration: result.delta.planar, CompatibilityProfile: result.compatibilityProfile}
		if err := state.accept(metadata, resolved.capabilities, s.limits.MaxOutputBytes, i, encoded); err != nil {
			return err
		}
		addLossyVerification(&report.LossyVerification, result.lossyVerification)
		if err := spool.reserveAppend(int64(len(result.data))); err != nil {
			return err
		}
		if err := timedTranscodeSpoolWrite(ctx, &report, func() error {
			n, err := transcodeSpoolWriteAt(spool.file, result.data, spooled)
			if err != nil || n != len(result.data) {
				return &TranscodeError{Stage: "write", Err: ErrTranscodeTransaction}
			}
			return nil
		}); err != nil {
			return err
		}
		offsets[i], lengths[i] = spooled, int64(len(result.data))
		spooled += int64(len(result.data))
		if i == 0 {
			firstFrame = nil
		}
		if transcodeStreamProgress != nil {
			transcodeStreamProgress(int(metadata.FrameSize()))
		}
		return nil
	})
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	if s.options.SkipVerification {
		report.Verification = TranscodeVerificationSkipped
	} else if resolved.capabilities.Lossless {
		report.Verification = TranscodeVerificationPixelExact
	} else if report.LossyVerification != nil {
		report.Verification = TranscodeVerificationPixelError
	} else {
		report.Verification = TranscodeVerificationDecode
	}
	delta := state.delta()
	report.EncodeCompatibilityProfiles = encoderCompatibilityProfiles(state.compatibilityProfile)
	result, err := detachDataSet(ctx, shallowDataSetDraft{object: object.FromElements(encodedMetadataElements(header, &delta), nil)}, s.limits)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	result.object.SetValueByteOrder(binary.LittleEndian)
	report.EncodedBytes = state.encodedTotal
	report.Lossy = !resolved.capabilities.Lossless
	if report.Lossy {
		if err := finalizeLossyDataSet(result, nativeBytes, state.encodedTotal, resolved.capabilities.LossyMethod, s.options.UIDRoot); err != nil {
			return TranscodeReport{}, nil, err
		}
	}
	if err := applyTranscodeIdentity(s.file.Dataset, result.object, &report, s.options); err != nil {
		return TranscodeReport{}, nil, err
	}
	output, err := transcodeOutputFile(ctx, s.file, result.object, report, s.target, s.limits)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	s.output = output
	source := &spooledFrameSource{file: spool.file, offsets: offsets, lengths: lengths}
	writeOptions, err := s.frameWriteOptions(source)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	return report, func(w io.Writer) error {
		return object.WriteFileFrames(w, output, source, writeOptions)
	}, nil
}

// spooledFrameSource copies encoded frames back from the spool file.
type spooledFrameSource struct {
	file             io.ReaderAt
	offsets, lengths []int64
}

func (f *spooledFrameSource) Frames() int                      { return len(f.lengths) }
func (f *spooledFrameSource) FrameLength(i int) (int64, error) { return f.lengths[i], nil }
func (f *spooledFrameSource) WriteFrameTo(i int, w io.Writer) (int64, error) {
	return io.Copy(w, io.NewSectionReader(f.file, f.offsets[i], f.lengths[i]))
}

// stageTranscodeSpool creates the private file encoded frames wait in.
func stageTranscodeSpool(txn *transcodePathTxn) (*stagedTranscodeFile, error) {
	name, err := newTranscodeTempName()
	if err != nil {
		return nil, &TranscodeError{Stage: "temporary", Err: ErrTranscodeTransaction}
	}
	file, err := createTranscodeFileAt(txn.parent, name)
	if err != nil {
		return nil, &TranscodeError{Stage: "temporary", Err: ErrTranscodeTransaction}
	}
	spool := &stagedTranscodeFile{name: name, file: file, open: true, entryOwned: true, temporary: txn.temporary}
	if err := file.Chmod(0o600); err != nil {
		spool.cleanup(txn.parent)
		return nil, &TranscodeError{Stage: "temporary", Err: ErrTranscodeTransaction}
	}
	return spool, nil
}

// validateStagedTranscodeStream reads the staged file back with its Pixel
// Data deferred and checks its structure: the declared syntax, integer Pixel
// Data of the right kind and, when encapsulated, the expected number of Items
// (Basic Offset Table included), all within the output limits.
func validateStagedTranscodeStream(ctx context.Context, staged *stagedTranscodeFile, limits TranscodeLimits, syntax transfer.Syntax, expectedItems int) error {
	fail := &TranscodeError{Stage: "readback", Err: ErrTranscodeTransaction}
	if _, err := staged.file.Seek(0, io.SeekStart); err != nil {
		return fail
	}
	readback, err := object.ReadFileWithOptions(&transcodeContextFile{File: staged.file, ctx: ctx}, object.ReadFileOptions{
		DeferPixelData: true, MaxElementBytes: limits.MaxOutputBytes, MaxTotalBytes: limits.MaxOutputBytes,
		MaxSequenceDepth: limits.MaxDepth, MaxElements: limits.MaxElements, MaxFragments: limits.MaxFragments,
	})
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return fail
	}
	if readSyntax, err := strictFileTransferSyntax(readback); err != nil || readSyntax.UID != syntax.UID {
		return fail
	}
	pixel, ok := readback.Dataset.Get(core.TagPixelData)
	locations := readback.ValueLocations(core.TagPixelData)
	if !ok || len(locations) != 1 || readback.Dataset.Has(tagFloatPixelData) || readback.Dataset.Has(tagDoubleFloatPixelData) ||
		pixel.Header.Length.IsUndefined() != syntax.Encapsulated {
		return fail
	}
	if !syntax.Encapsulated {
		if _, err := ExtractMetadata(readback.Dataset); err != nil {
			return fail
		}
		return nil
	}
	if _, err := ExtractMetadata(readback.Dataset); err != nil {
		return fail
	}
	items := 0
	var header [8]byte
	offset, end := locations[0].ValueOffset, locations[0].ValueOffset+locations[0].Length
	for offset+8 <= end {
		if _, err := staged.file.ReadAt(header[:], offset); err != nil {
			return fail
		}
		tag := core.NewTag(binary.LittleEndian.Uint16(header[0:]), binary.LittleEndian.Uint16(header[2:]))
		offset += 8
		if tag == core.TagSequenceDelimitationItem {
			break
		}
		if tag != core.TagItem {
			return fail
		}
		items++
		offset += int64(binary.LittleEndian.Uint32(header[4:]))
	}
	if items != expectedItems || items-1 > limits.MaxFragments {
		return fail
	}
	return nil
}

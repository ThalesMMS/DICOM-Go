package pixeldata

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// decodeOnWrite preserves the legacy serial native-target route: one decoded
// frame is written directly to the private staged output, without a second
// full-image spool. Late decode/write failure is still handled by the staged
// file owner, before readback and atomic publication.
func (s *transcodeStream) decodeOnWrite(ctx context.Context) (TranscodeReport, func(io.Writer) error, error) {
	if s.source.Encapsulated && s.source.UID != transfer.EncapsulatedUncompressedExplicitVRLittleEndian.UID && s.options.DecoderRegistry == nil {
		return TranscodeReport{}, nil, codecAvailabilityError(ErrCodecRegistryNil, s.source.UID, nil)
	}
	if err := s.checkDecodeBudget(); err != nil {
		return TranscodeReport{}, nil, err
	}
	if err := s.preflightPipelineFrame(false); err != nil {
		return TranscodeReport{}, nil, err
	}
	start := time.Now()
	var decodeReport DecompressReport
	first, frame, err := s.frames.decodeFrameObject(ctx, 0, &decodeReport)
	firstDecodeTime := time.Since(start)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	decodedHeader := s.nativeHeader(first)
	header, err := s.nativeOutputHeader(first)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	frame, err = s.nativeOutputFrame(ctx, decodedHeader, frame)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	first = nil
	metadata, err := ExtractMetadata(header)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	nativeBytes := metadata.TotalSize()
	if nativeBytes <= 0 || nativeBytes > s.limits.MaxOutputBytes {
		return TranscodeReport{}, nil, transcodeLimitError("native_bytes")
	}
	pipeline, err := newTranscodeFramePipeline(s.options, metadata.NumberOfFrames, metadata.FrameSize(), s.limits.MaxOutputBytes, false, s.frames.RawFrameLength)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	// Charge every source frame, not just the first: compressed frame lengths
	// may vary, while the native output layout must remain canonical.
	var peakBytes int64
	for i := 0; i < metadata.NumberOfFrames; i++ {
		if err := ctx.Err(); err != nil {
			return TranscodeReport{}, nil, err
		}
		charge, err := pipeline.charge(i)
		if err != nil {
			return TranscodeReport{}, nil, err
		}
		if charge > pipeline.options.MaxInFlightBytes {
			return TranscodeReport{}, nil, transcodeLimitError("in_flight_bytes")
		}
		peakBytes = max(peakBytes, charge)
	}
	report := TranscodeReport{Frames: metadata.NumberOfFrames, NativeBytes: nativeBytes, DecodeDiagnostics: decodeReport.Diagnostics,
		WorkerLimit: 1, PeakInFlightFrames: 1, PeakInFlightBytes: peakBytes}
	report.Timings.DecodeWorkerTime = firstDecodeTime
	var dataset *object.Object
	if s.plan.mode == transcodeDecodeEncapsulatedUncompressed {
		dataset = object.FromElements(encodedMetadataElements(header, nil), nil)
		report.EncodedBytes = nativeBytes
	} else {
		dataset = header
	}
	dataset, err = cloneDetachedObject(ctx, dataset, outputCloneLimits(s.limits))
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	dataset.SetValueByteOrder(binary.LittleEndian)
	if err := applyTranscodeIdentity(s.file.Dataset, dataset, &report, s.options); err != nil {
		return TranscodeReport{}, nil, err
	}
	output, err := transcodeOutputFile(ctx, s.file, dataset, report, s.target, s.limits)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	s.output = output
	source := &serialDecodingFrameSource{ctx: ctx, stream: s, firstFrame: frame, header: decodedHeader,
		count: metadata.NumberOfFrames, size: metadata.FrameSize()}
	writeOptions, err := s.frameWriteOptions(source)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	return report, func(w io.Writer) error {
		return object.WriteFileFrames(w, output, source, writeOptions)
	}, nil
}

type serialDecodingFrameSource struct {
	ctx        context.Context
	stream     *transcodeStream
	firstFrame []byte
	header     *object.Object
	count      int
	size       int64
}

func (f *serialDecodingFrameSource) Frames() int                    { return f.count }
func (f *serialDecodingFrameSource) FrameLength(int) (int64, error) { return f.size, nil }
func (f *serialDecodingFrameSource) WriteFrameTo(i int, w io.Writer) (int64, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	frame := f.firstFrame
	if i > 0 {
		start := time.Now()
		decoded, next, err := f.stream.frames.decodeFrameObject(f.ctx, i, nil)
		f.stream.serialDecodeTime += time.Since(start)
		if err != nil {
			return 0, err
		}
		if !sameDecodedLayout(f.header, decoded) {
			return 0, fmt.Errorf("%w: decoded frame layout", ErrIncompatiblePixelData)
		}
		frame = next
		frame, err = f.stream.nativeOutputFrame(f.ctx, f.header, frame)
		if err != nil {
			return 0, err
		}
	}
	if int64(len(frame)) != f.size {
		return 0, fmt.Errorf("%w: decoded frame size", ErrPixelDataSizeMismatch)
	}
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.Write(frame)
	if i == 0 {
		f.firstFrame = nil
	}
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	return int64(n), err
}

// decodeToSpool overlaps independent frame decodes, retaining a bounded window
// until it can be written in order. The disk spool is not a second complete
// resident image: it is bounded by the total output cap and removed by the
// transaction owner after output construction, on success or failure.
func (s *transcodeStream) decodeToSpool(ctx context.Context, spool *stagedTranscodeFile) (TranscodeReport, func(io.Writer) error, error) {
	if s.source.Encapsulated && s.source.UID != transfer.EncapsulatedUncompressedExplicitVRLittleEndian.UID && s.options.DecoderRegistry == nil {
		return TranscodeReport{}, nil, codecAvailabilityError(ErrCodecRegistryNil, s.source.UID, nil)
	}
	if err := s.checkDecodeBudget(); err != nil {
		return TranscodeReport{}, nil, err
	}
	if err := s.preflightPipelineFrame(false); err != nil {
		return TranscodeReport{}, nil, err
	}
	start := time.Now()
	var decodeReport DecompressReport
	first, firstFrame, err := s.frames.decodeFrameObject(ctx, 0, &decodeReport)
	firstDecodeTime := time.Since(start)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	decodedHeader := s.nativeHeader(first)
	header, err := s.nativeOutputHeader(first)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	firstFrame, err = s.nativeOutputFrame(ctx, decodedHeader, firstFrame)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	metadata, err := ExtractMetadata(header)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	nativeBytes := metadata.TotalSize()
	if nativeBytes <= 0 || nativeBytes > s.limits.MaxOutputBytes {
		return TranscodeReport{}, nil, transcodeLimitError("native_bytes")
	}
	pipeline, err := newTranscodeFramePipeline(s.options, metadata.NumberOfFrames, metadata.FrameSize(), s.limits.MaxOutputBytes, false, s.frames.RawFrameLength)
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	report := TranscodeReport{Frames: metadata.NumberOfFrames, NativeBytes: nativeBytes, DecodeDiagnostics: decodeReport.Diagnostics}
	report.Timings.DecodeWorkerTime = firstDecodeTime
	lengths, offsets := make([]int64, metadata.NumberOfFrames), make([]int64, metadata.NumberOfFrames)
	var spooled int64
	first = nil
	err = pipeline.run(ctx, metadata.NumberOfFrames, &report, func(workCtx context.Context, i int) (transcodeFrameResult, error) {
		start := time.Now()
		frame := firstFrame
		if i > 0 {
			decoded, next, err := s.frames.decodeFrameObject(workCtx, i, nil)
			if err != nil {
				return transcodeFrameResult{}, err
			}
			if !sameDecodedLayout(decodedHeader, decoded) {
				return transcodeFrameResult{}, fmt.Errorf("%w: decoded frame layout", ErrIncompatiblePixelData)
			}
			frame = next
			frame, err = s.nativeOutputFrame(workCtx, decodedHeader, frame)
			if err != nil {
				return transcodeFrameResult{}, err
			}
		}
		if int64(len(frame)) != metadata.FrameSize() {
			return transcodeFrameResult{}, fmt.Errorf("%w: decoded frame size", ErrPixelDataSizeMismatch)
		}
		return transcodeFrameResult{data: frame, timings: TranscodeTimings{DecodeWorkerTime: time.Since(start)}}, nil
	}, func(i int, result transcodeFrameResult) error {
		if int64(len(result.data)) > s.limits.MaxOutputBytes-spooled {
			return transcodeLimitError("output_bytes")
		}
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
		return nil
	})
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	var dataset *object.Object
	if s.plan.mode == transcodeDecodeEncapsulatedUncompressed {
		dataset = object.FromElements(encodedMetadataElements(header, nil), nil)
		report.EncodedBytes = nativeBytes
	} else {
		dataset = header
	}
	dataset, err = cloneDetachedObject(ctx, dataset, outputCloneLimits(s.limits))
	if err != nil {
		return TranscodeReport{}, nil, err
	}
	dataset.SetValueByteOrder(binary.LittleEndian)
	if err := applyTranscodeIdentity(s.file.Dataset, dataset, &report, s.options); err != nil {
		return TranscodeReport{}, nil, err
	}
	output, err := transcodeOutputFile(ctx, s.file, dataset, report, s.target, s.limits)
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

// Reject a window that cannot admit even the first frame before materializing
// it. Decoded metadata is checked again after the first codec call, since
// legitimate color transforms can change the canonical frame layout.
func (s *transcodeStream) preflightPipelineFrame(encode bool) error {
	pipeline, err := newTranscodeFramePipeline(s.options, s.metadata.NumberOfFrames, s.metadata.FrameSize(), s.limits.MaxOutputBytes, encode, s.frames.RawFrameLength)
	if err != nil {
		return err
	}
	charge, err := pipeline.charge(0)
	if err != nil {
		return err
	}
	if charge > pipeline.options.MaxInFlightBytes {
		return transcodeLimitError("in_flight_bytes")
	}
	return nil
}

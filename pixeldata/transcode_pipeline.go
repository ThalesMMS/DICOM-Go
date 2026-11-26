package pixeldata

import (
	"context"
	"math"
	"os"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

const defaultTranscodeInFlightBytes = int64(256) << 20

// Narrow test seam for disk-full/short-write failures, without filling a disk.
var transcodeSpoolWriteAt = (*os.File).WriteAt

type transcodeFrameResult struct {
	data                 []byte
	delta                encodedMetadataDelta
	timings              TranscodeTimings
	lossyVerification    *LossyVerificationReport
	compatibilityProfile EncoderCompatibilityProfile
}

type transcodeFramePipeline struct {
	options      TranscodePipelineOptions
	workers      int
	encodedCap   int64
	nativeBytes  int64
	encode       bool
	verify       bool
	frameRawSize func(int) (int64, error)
	planarCopy   bool
}

func newTranscodeFramePipeline(options TranscodeOptions, count int, nativeBytes, maxOutput int64, encode bool, rawSize func(int) (int64, error)) (*transcodeFramePipeline, error) {
	if err := validateTranscodePolicy(options); err != nil {
		return nil, err
	}
	p := options.Pipeline
	// Registry/Codec's legacy contract does not require concurrent decode.
	// Only an explicit Workers>1 opts the caller into parallel codec calls.
	if p.Workers == 0 {
		p.Workers = 1
	}
	workers := max(1, min(parallel.Workers(p.Workers), count))
	if p.MaxInFlightFrames == 0 {
		p.MaxInFlightFrames = min(workers, 32)
	}
	if p.MaxInFlightBytes == 0 {
		p.MaxInFlightBytes = defaultTranscodeInFlightBytes
	}
	cap := int64(0)
	if encode {
		cap = p.MaxEncodedFrameBytes
		if cap == 0 {
			if nativeBytes <= 0 || nativeBytes > (math.MaxInt64-65536)/4 {
				return nil, transcodeLimitError("frame_bytes")
			}
			cap = min(maxOutput, 4*nativeBytes+65536)
		}
		if cap <= 0 || cap > maxOutput {
			return nil, transcodeLimitError("encoded_frame_bytes")
		}
	}
	return &transcodeFramePipeline{options: p, workers: workers, encodedCap: cap, nativeBytes: nativeBytes, encode: encode,
		verify: !options.SkipVerification, frameRawSize: rawSize, planarCopy: options.PlanarOutput != PlanarOutputDefault}, nil
}

// charge covers raw input, native clones passed to codecs/materialization, the
// detached output, and verification's encoded/native proxy copies. Codec
// allocations beyond returned frames are opaque and are not claimed here.
func (p *transcodeFramePipeline) charge(i int) (int64, error) {
	raw, err := p.frameRawSize(i)
	if err != nil {
		return 0, err
	}
	if raw <= 0 || p.nativeBytes <= 0 {
		return 0, transcodeLimitError("frame_bytes")
	}
	nativeCopies := int64(4)
	encodedCopies := int64(2)
	if p.verify && p.encode {
		nativeCopies = 8
		encodedCopies = 3
	}
	if p.planarCopy {
		nativeCopies++
	}
	if p.nativeBytes > math.MaxInt64/nativeCopies || p.encodedCap > math.MaxInt64/encodedCopies {
		return 0, transcodeLimitError("in_flight_bytes")
	}
	charge := nativeCopies * p.nativeBytes
	if raw > math.MaxInt64-charge || encodedCopies*p.encodedCap > math.MaxInt64-charge-raw {
		return 0, transcodeLimitError("in_flight_bytes")
	}
	return charge + raw + encodedCopies*p.encodedCap, nil
}

// run uses bounded windows rather than an unbounded reorder queue. Every
// window joins before ordered consumption, so no new work is admitted while
// its completed output is being spooled. Errors cancel every frame task and
// join all helpers before returning to the temporary-file owner.
func (p *transcodeFramePipeline) run(ctx context.Context, count int, report *TranscodeReport,
	produce func(context.Context, int) (transcodeFrameResult, error), consume func(int, transcodeFrameResult) error) error {
	for start := 0; start < count; {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := start
		var bytes int64
		for end < count && end-start < p.options.MaxInFlightFrames {
			charge, err := p.charge(end)
			if err != nil {
				return err
			}
			if charge > p.options.MaxInFlightBytes-bytes {
				if end == start {
					return transcodeLimitError("in_flight_bytes")
				}
				break
			}
			bytes += charge
			end++
		}
		results := make([]transcodeFrameResult, end-start)
		workCtx, cancel := context.WithCancel(ctx)
		var once sync.Once
		var failure error
		workers := min(p.workers, len(results))
		runErr := parallel.Run(workCtx, len(results), workers, 1, func() func(int) {
			return func(slot int) {
				result, err := produce(workCtx, start+slot)
				if err != nil {
					once.Do(func() { failure = err; cancel() })
					return
				}
				results[slot] = result
			}
		})
		cancel()
		if failure != nil {
			return failure
		}
		if runErr != nil {
			return runErr
		}
		report.PeakInFlightFrames = max(report.PeakInFlightFrames, len(results))
		report.PeakInFlightBytes = max(report.PeakInFlightBytes, bytes)
		// This is the requested bound, not a promise that the shared budget
		// granted that many helpers. The process-wide budget remains authoritative.
		report.WorkerLimit = max(report.WorkerLimit, workers)
		for slot, result := range results {
			if err := ctx.Err(); err != nil {
				return err
			}
			report.Timings.DecodeWorkerTime += result.timings.DecodeWorkerTime
			report.Timings.EncodeWorkerTime += result.timings.EncodeWorkerTime
			report.Timings.VerifyWorkerTime += result.timings.VerifyWorkerTime
			if err := consume(start+slot, result); err != nil {
				return err
			}
			results[slot] = transcodeFrameResult{}
		}
		start = end
	}
	return ctx.Err()
}

func timedTranscodeSpoolWrite(ctx context.Context, report *TranscodeReport, write func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	start := time.Now()
	err := write()
	report.Timings.SpoolWriteTime += time.Since(start)
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	return err
}

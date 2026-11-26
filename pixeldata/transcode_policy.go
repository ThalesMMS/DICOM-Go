package pixeldata

import (
	"fmt"
	"time"
)

// TranscodeDurability controls publication by TranscodePath. The zero value
// preserves the existing policy: sync the staged file and, where supported,
// its destination directory. Atomic publication omits those sync calls; it
// still uses a private staged file, structural readback, and atomic replacement,
// but does not promise persistence through a process, OS, or power failure.
type TranscodeDurability uint8

const (
	TranscodeDurable TranscodeDurability = iota
	TranscodeAtomic
)

func (p TranscodeDurability) String() string {
	switch p {
	case TranscodeDurable:
		return "durable"
	case TranscodeAtomic:
		return "atomic"
	default:
		return "invalid"
	}
}

// TranscodeVerification describes the additional output pixel check actually
// performed by TranscodePath. Structural validation is independent of this
// policy and remains mandatory, including when verification is skipped.
type TranscodeVerification uint8

const (
	TranscodeVerificationNotRequired TranscodeVerification = iota
	TranscodeVerificationPixelExact
	TranscodeVerificationDecode
	TranscodeVerificationSkipped
	TranscodeVerificationPixelError
)

func (p TranscodeVerification) String() string {
	switch p {
	case TranscodeVerificationNotRequired:
		return "not-required"
	case TranscodeVerificationPixelExact:
		return "pixel-exact"
	case TranscodeVerificationDecode:
		return "decode"
	case TranscodeVerificationSkipped:
		return "skipped"
	case TranscodeVerificationPixelError:
		return "pixel-error"
	default:
		return "invalid"
	}
}

// TranscodePipelineOptions bounds the frame work performed by TranscodePath.
// Workers=0 or Workers=1 preserves serial codec calls. Workers>1 explicitly
// opts into parallel frame work, bounded by the process-wide shared CPU budget.
// The window includes decoding, encoding, verification and completed buffers
// awaiting ordered spool writes. MaxInFlightBytes is a conservative charge for
// buffers managed by this package, not an RSS bound: codec-private workspace,
// metadata, indexes and the Go runtime are outside it. The private disk spool
// is separately bounded by TranscodeLimits.MaxOutputBytes.
//
// Zero byte/frame bounds use finite defaults. MaxEncodedFrameBytes=0 derives
// a conservative output cap of 4*native-frame-bytes + 64 KiB (clipped to the
// total output limit). A codec returning more is refused without publication;
// callers needing a larger representation must explicitly raise that cap and
// the in-flight byte budget. Codecs/registries must support concurrent calls
// only when the caller explicitly sets Workers greater than one; their legacy
// interfaces do not otherwise require decoder reentrancy.
type TranscodePipelineOptions struct {
	Workers              int
	MaxInFlightFrames    int
	MaxInFlightBytes     int64
	MaxEncodedFrameBytes int64
}

// TranscodeTimings separates host phases. Worker durations are sums over frame
// tasks and can exceed wall time when tasks overlap; they are not kernel-only
// timings. Publication write and sync durations are measured on the caller.
type TranscodeTimings struct {
	DecodeWorkerTime  time.Duration
	EncodeWorkerTime  time.Duration
	VerifyWorkerTime  time.Duration
	SpoolWriteTime    time.Duration
	OutputWriteTime   time.Duration
	FileSyncTime      time.Duration
	DirectorySyncTime time.Duration
}

func validateTranscodePolicy(options TranscodeOptions) error {
	if err := validateOutputPolicy(options); err != nil {
		return err
	}
	p := options.Pipeline
	if p.Workers < 0 || p.MaxInFlightFrames < 0 || p.MaxInFlightBytes < 0 || p.MaxEncodedFrameBytes < 0 {
		return fmt.Errorf("%w: invalid pipeline limits", ErrTranscodeResourceLimit)
	}
	if options.Durability != TranscodeDurable && options.Durability != TranscodeAtomic {
		return fmt.Errorf("%w: invalid durability policy", ErrTranscodeUnsupported)
	}
	return nil
}

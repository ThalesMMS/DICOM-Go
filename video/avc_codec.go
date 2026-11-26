package video

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	ErrCodecProfile = errors.New("dicom: unsupported or inconsistent video codec profile")
	ErrCodecBackend = errors.New("dicom: video codec backend failed or is unavailable")
	ErrLossyPolicy  = errors.New("dicom: AVC re-encoding requires an explicit lossy policy")
)

// AVCProfile describes presentation-ordered, constant-rate AVC video in
// classic MP4. Colour holds the H.264 colour primaries, transfer and matrix
// codes. Value 2 denotes unspecified. Decoded samples are 8-bit limited-range
// planar YUV 4:2:0, with Y, U then V bytes tightly packed for each frame.
type AVCProfile struct {
	Rows, Columns, Frames          int
	RateNumerator, RateDenominator uint32
	Colour                         [3]uint8
}

func (p AVCProfile) frameTime() float64 {
	return 1000 * float64(p.RateDenominator) / float64(p.RateNumerator)
}

func (p AVCProfile) validate() error {
	if p.Rows <= 0 || p.Columns <= 0 || p.Rows&1 != 0 || p.Columns&1 != 0 ||
		p.RateNumerator == 0 || p.RateDenominator == 0 ||
		p.RateNumerator > math.MaxInt32 || p.RateDenominator > math.MaxInt32 ||
		uint64(p.RateNumerator) > 1000*uint64(p.RateDenominator) {
		return ErrCodecProfile
	}
	return nil
}

func (p AVCProfile) frameBytes() int64 {
	pixels := int64(p.Rows) * int64(p.Columns)
	return pixels + pixels/2
}

func normalizeAVCRate(numerator, denominator uint32) (uint32, uint32) {
	first, second := numerator, denominator
	for second != 0 {
		first, second = second, first%second
	}
	return numerator / first, denominator / first
}

// AVCDecoder consumes inspected classic MP4 data synchronously and writes only
// complete frames, in presentation order and in AVCProfile's sample layout.
// It must return coding errors and honor cancellation and write errors. It
// must neither retain nor alter the input container.
type AVCDecoder interface {
	DecodeAVCMP4(ctx context.Context, mp4 []byte, profile AVCProfile, dst io.Writer) error
}

// AVCEncoder synchronously writes one classic MP4 container. The encoded
// stream must keep the supplied geometry, timing and colour signal. It must
// honor cancellation and write errors and leave the caller's samples intact.
type AVCEncoder interface {
	EncodeAVCMP4(ctx context.Context, samples []byte, profile AVCProfile, options AVCEncodeOptions, dst io.Writer) error
}

type AVCEncodeOptions struct {
	// CRF is an explicitly selected lossy quality factor on the AVC 1–51
	// scale. MaxBytes caps the MP4 container including its sample tables.
	CRF      int
	MaxBytes int64
}

type AVCCodecLimits struct {
	ImportLimits
	// MaxDuration applies to the operation including backend work. Zero
	// uses two minutes. A caller deadline may shorten this bound.
	MaxDuration time.Duration
}

func (l AVCCodecLimits) normalized() (AVCCodecLimits, error) {
	if l.MaxDuration < 0 || l.MaxDuration > time.Hour {
		return l, ErrCodecProfile
	}
	if l.MaxDuration == 0 {
		l.MaxDuration = 2 * time.Minute
	}
	limits, err := l.ImportLimits.normalize()
	l.ImportLimits = limits
	return l, err
}

// DecodedAVC owns a contiguous buffer of complete presentation-order frames.
type DecodedAVC struct {
	Profile AVCProfile
	Samples []byte
}

// DecodeAVCMP4 validates a DICOM AVC MP4 instance and decodes it through the
// explicitly supplied backend. It returns no frames on failure.
func DecodeAVCMP4(ctx context.Context, source *object.File, decoder AVCDecoder, limits AVCCodecLimits) (DecodedAVC, error) {
	if decoder == nil {
		return DecodedAVC{}, ErrCodecBackend
	}
	limits, err := limits.normalized()
	if err != nil {
		return DecodedAVC{}, err
	}
	ctx, cancel := avcContext(ctx, limits.MaxDuration)
	defer cancel()
	mp4, profile, err := readDICOMAVC(ctx, source, limits.ImportLimits)
	if err != nil {
		return DecodedAVC{}, err
	}
	var buffer bytes.Buffer
	if err := decodeAVC(ctx, mp4, profile, decoder, &buffer); err != nil {
		return DecodedAVC{}, err
	}
	return DecodedAVC{Profile: profile, Samples: buffer.Bytes()}, nil
}

func avcContext(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, limit)
}

func readDICOMAVC(ctx context.Context, source *object.File, limits ImportLimits) ([]byte, AVCProfile, error) {
	if err := ctx.Err(); err != nil {
		return nil, AVCProfile{}, err
	}
	info, err := Metadata(source)
	if err != nil {
		return nil, AVCProfile{}, err
	}
	if _, partial := source.PartialRead(); partial {
		return nil, AVCProfile{}, object.ErrPartialFile
	}
	if _, incomplete := source.IncompletePixelData(); incomplete {
		return nil, AVCProfile{}, object.ErrPartialFile
	}
	if source.Dataset.HasDiscardedValues() {
		return nil, AVCProfile{}, core.ErrDiscardedValue
	}
	if info.TransferSyntaxUID != transfer.MPEG4HP41.UID {
		return nil, AVCProfile{}, ErrCodecProfile
	}
	if source.Meta != nil {
		if declared, ok := source.Meta.GetString(core.NewTag(0x0002, 0x0010)); ok &&
			transfer.NormalizeUID(declared) != info.TransferSyntaxUID {
			return nil, AVCProfile{}, fmt.Errorf("%w: inconsistent File Meta transfer syntax", ErrCodecProfile)
		}
	}
	var mp4 bytes.Buffer
	info, err = Copy(ctx, source, &mp4, Limits{MaxBytes: limits.MaxBytes, MaxFragments: 1})
	if err != nil {
		return nil, AVCProfile{}, err
	}
	if info.Container != ContainerMP4 {
		return nil, AVCProfile{}, ErrCodecProfile
	}
	payload := mp4.Bytes()
	profile, err := inspectAVC(ctx, payload, limits)
	if err != nil && len(payload) >= 2 && len(payload)%2 == 0 && payload[len(payload)-1] == 0 && ctx.Err() == nil {
		// DICOM Item padding is outside the MP4 box structure.
		payload = payload[:len(payload)-1]
		profile, err = inspectAVC(ctx, payload, limits)
	}
	if err != nil {
		return nil, AVCProfile{}, err
	}
	if err := verifyDICOMAVCMetadata(source.Dataset, profile); err != nil {
		return nil, AVCProfile{}, err
	}
	return payload, profile, ctx.Err()
}

func inspectAVC(ctx context.Context, mp4 []byte, limits ImportLimits) (AVCProfile, error) {
	if int64(len(mp4)) > limits.MaxBytes {
		return AVCProfile{}, ErrMediaTooLarge
	}
	profile := AVCProfile{}
	_, err := inspectMP4Profile(ctx, mp4, limits, &profile)
	return profile, err
}

func avcLimitedColour(s *h264SPS) ([3]uint8, error) {
	colour := [3]uint8{2, 2, 2}
	r := newBitReader(rbsp(s.raw[1:]))
	r.pos = s.vuiBitOffset
	if !r.flag() {
		return colour, r.err
	}
	if r.flag() {
		_ = r.u(8) // inspectImportAVCAspect already required a square SAR.
	}
	if r.flag() {
		_ = r.flag() // overscan_appropriate_flag
	}
	if r.flag() {
		_ = r.u(3) // video_format
		if r.flag() {
			return colour, importError("full-range AVC samples")
		}
		if r.flag() {
			for i := range colour {
				colour[i] = uint8(r.u(8))
			}
		}
	}
	if r.flag() {
		if top, bottom := r.ue(), r.ue(); top != 0 || bottom != 0 {
			return colour, importError("nondefault AVC chroma location")
		}
	}
	return colour, r.err
}

func decodeAVC(ctx context.Context, mp4 []byte, profile AVCProfile, decoder AVCDecoder, dst io.Writer) error {
	span := profile.frameBytes() * int64(profile.Frames)
	w := boundedAVCWriter{ctx: ctx, dst: dst, remaining: span}
	err := decoder.DecodeAVCMP4(ctx, mp4, profile, &w)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if w.err != nil {
		return w.err
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCodecBackend, err)
	}
	if w.remaining != 0 {
		return fmt.Errorf("%w: decoded frame span differs from the admitted frame count", ErrCodecBackend)
	}
	return nil
}

type boundedAVCWriter struct {
	ctx       context.Context
	dst       io.Writer
	remaining int64
	err       error
}

func (w *boundedAVCWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.err = w.ctx.Err(); w.err != nil {
		return 0, w.err
	}
	if int64(len(p)) > w.remaining {
		w.err = ErrMediaTooLarge
		return 0, w.err
	}
	n, err := w.dst.Write(p)
	w.remaining -= int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	w.err = err
	return n, err
}

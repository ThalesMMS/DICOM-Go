package storetranscode

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/deflateframe"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeg"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeglossless"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
)

var ErrPreparedBinding = errors.New("dicom: prepared representation binding unavailable or changed")

// BuiltinPreparedBindingOptions configures immutable, in-process codecs. No
// caller registry, runtime path, backend name or claimed fingerprint is trusted.
// Optional runtimes and opaque mutable codecs use the ordinary NewSource API.
type BuiltinPreparedBindingOptions struct {
	AllowLossy  bool
	JPEGQuality int
}

// PreparedBinding owns private built-in registries and copied constructor
// parameters. It exposes no registration API or mutable codec implementation.
// Close rejects future acquisitions; existing attempt leases own their exact
// immutable configuration until their actual I/O completes.
type PreparedBinding struct {
	mu             sync.Mutex
	closed         bool
	token          [32]byte
	decoders       pixeldata.Registry
	encoders       pixeldata.EncoderRegistry
	calls          atomic.Uint64
	encodeDuration atomic.Int64
}

func NewBuiltinPreparedBinding(options BuiltinPreparedBindingOptions) (*PreparedBinding, error) {
	if options.JPEGQuality == 0 {
		options.JPEGQuality = 90
	}
	if options.JPEGQuality < jpeg.MinQuality || options.JPEGQuality > jpeg.MaxQuality {
		return nil, ErrOptions
	}
	decoders, err := builtin.NewRegistry()
	if err != nil {
		return nil, err
	}
	encoders := pixeldata.NewMemoryEncoderRegistry()
	for _, register := range []func(pixeldata.EncoderRegistry) error{rle.RegisterEncoder, jpegls.RegisterEncoder, jpeglossless.RegisterEncoder, deflateframe.RegisterEncoder} {
		if err := register(encoders); err != nil {
			return nil, err
		}
	}
	if options.AllowLossy {
		if err := jpeg.RegisterEncoder(encoders, options.JPEGQuality); err != nil {
			return nil, err
		}
	}
	// The supported implementations are constructed here, not supplied through
	// an interface. Their pure-Go runtime and configuration cannot be replaced.
	capabilities := make([]pixeldata.EncoderCapabilities, 0)
	for _, uid := range encoders.RegisteredEncoderUIDs() {
		encoder, _ := encoders.GetEncoder(uid)
		capabilities = append(capabilities, encoder.Capabilities())
	}
	raw, err := json.Marshal(struct {
		Schema             string
		Options            BuiltinPreparedBindingOptions
		Decoders, Encoders []string
		Profiles           []pixeldata.EncoderCapabilities
	}{"builtin-prepared-v1", options, decoders.RegisteredCodecUIDs(), encoders.RegisteredEncoderUIDs(), capabilities})
	if err != nil {
		return nil, ErrPreparedBinding
	}
	b := &PreparedBinding{token: sha256.Sum256(raw), decoders: decoders, encoders: encoders}
	b.encoders = countedPreparedEncoders{registry: encoders, calls: &b.calls, duration: &b.encodeDuration}
	return b, nil
}

func (b *PreparedBinding) acquire(ctx context.Context) ([32]byte, error) {
	if err := ctx.Err(); err != nil {
		return [32]byte{}, err
	}
	if b == nil {
		return [32]byte{}, ErrPreparedBinding
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return [32]byte{}, ErrPreparedBinding
	}
	return b.token, nil
}
func (b *PreparedBinding) Close() error {
	if b != nil {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()
	}
	return nil
}

// EncoderCalls counts actual frame encoder invocations, not preflight checks.
func (b *PreparedBinding) EncoderCalls() uint64 {
	if b == nil {
		return 0
	}
	return b.calls.Load()
}

type countedPreparedEncoders struct {
	registry pixeldata.EncoderRegistry
	calls    *atomic.Uint64
	duration *atomic.Int64
}

func (e countedPreparedEncoders) RegisterEncoder(string, pixeldata.FrameEncoder) error {
	return ErrPreparedBinding
}
func (e countedPreparedEncoders) GetEncoder(uid string) (pixeldata.FrameEncoder, bool) {
	v, ok := e.registry.GetEncoder(uid)
	if !ok {
		return nil, false
	}
	return countedPreparedEncoder{v, e.calls, e.duration}, true
}
func (e countedPreparedEncoders) EncodeFrame(ctx context.Context, uid string, f []byte, m pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	e.calls.Add(1)
	started := time.Now()
	defer func() { e.duration.Add(int64(time.Since(started))) }()
	return e.registry.EncodeFrame(ctx, uid, f, m)
}

type countedPreparedEncoder struct {
	pixeldata.FrameEncoder
	calls    *atomic.Uint64
	duration *atomic.Int64
}

func (e countedPreparedEncoder) EncodeFrame(ctx context.Context, f []byte, m pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	e.calls.Add(1)
	started := time.Now()
	defer func() { e.duration.Add(int64(time.Since(started))) }()
	return e.FrameEncoder.EncodeFrame(ctx, f, m)
}

// EncoderTime is summed wall time inside actual EncodeFrame calls. Concurrent
// frame totals are not elapsed preparation latency or a universal speedup.
func (b *PreparedBinding) EncoderTime() time.Duration {
	if b == nil {
		return 0
	}
	return time.Duration(b.encodeDuration.Load())
}

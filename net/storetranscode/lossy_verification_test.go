package storetranscode

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func lossyVerifiedSource(t testing.TB, limit *float64) (*Source, Options) {
	t.Helper()
	opts := testOptions(t, transfer.JPEGLSNearLossless)
	if err := jpegls.RegisterNearLossless(opts.Transcode.DecoderRegistry); err != nil {
		t.Fatal(err)
	}
	if err := jpegls.RegisterNearLosslessEncoder(opts.Transcode.EncoderRegistry, jpegls.EncoderOptions{Near: 1, AllowLossy: true}); err != nil {
		t.Fatal(err)
	}
	opts.Transcode.AllowLossy = true
	opts.Transcode.MaxPixelError = limit
	source, err := NewSource(dimse.NewFileStoreSource(testFile(), 0), opts)
	if err != nil {
		t.Fatal(err)
	}
	return source, opts
}

func TestStoreLossyVerificationInvalidFastPathPolicy(t *testing.T) {
	for _, target := range []transfer.Syntax{{}, transfer.ExplicitVRLittleEndian} {
		for _, opts := range []pixeldata.TranscodeOptions{{MaxPixelError: lossyLimit(math.NaN())}, {MaxPixelError: lossyLimit(math.Inf(1))}, {MaxPixelError: lossyLimit(-1)}, {AverageBlockSize: 2}, {AverageBlockSize: -1}, {MaxPixelError: lossyLimit(0), SkipVerification: true}} {
			base := &lossyCountingSource{StoreSource: dimse.NewFileStoreSource(testFile(), 0)}
			if source, err := NewSource(base, Options{Target: target, Transcode: opts}); source != nil || !errors.Is(err, pixeldata.ErrTranscodeVerificationOptions) || base.calls.Load() != 0 {
				t.Fatalf("invalid fast-path policy accepted: %v", err)
			}
		}
	}
	if source, err := NewSource(dimse.NewFileStoreSource(testFile(), 0), Options{Transcode: pixeldata.TranscodeOptions{MaxPixelError: lossyLimit(1)}}); source != nil || !errors.Is(err, ErrOptions) {
		t.Fatal("metric without target accepted", err)
	}
}
func lossyLimit(v float64) *float64 { return &v }

type lossyCountingSource struct {
	dimse.StoreSource
	calls atomic.Int64
}

func (s *lossyCountingSource) Inspect(ctx context.Context) (dimse.StoreDescriptor, error) {
	s.calls.Add(1)
	return s.StoreSource.Inspect(ctx)
}
func (s *lossyCountingSource) Open(ctx context.Context) (dimse.OpenedStoreSource, error) {
	s.calls.Add(1)
	return s.StoreSource.Open(ctx)
}

type lossyRetryEncoder struct{ delta atomic.Int32 }

func (*lossyRetryEncoder) Capabilities() pixeldata.EncoderCapabilities {
	return pixeldata.EncoderCapabilities{TransferSyntaxUID: transfer.JPEGLSNearLossless.UID, BitsAllocated: []uint16{8}, PixelRepresentations: []uint16{0}, SamplesPerPixel: []uint16{1}, PhotometricInterpretations: []string{"MONOCHROME2"}, LossyMethod: "ISO_14495_1", SupportsMultiFrame: true}
}
func (e *lossyRetryEncoder) EncodeFrame(_ context.Context, raw []byte, _ pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	out := core.CloneBytes(raw)
	for i, v := range out {
		if v < 250 {
			out[i] += byte(e.delta.Load())
			break
		}
	}
	return pixeldata.EncodedFrame{Data: out}, nil
}

type lossyRetryDecoder struct{}

func (lossyRetryDecoder) Decode(pixel pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
	m, err := pixeldata.ExtractMetadata(obj)
	if err != nil {
		return pixeldata.Frames{}, err
	}
	frames := make([][]byte, len(pixel.Sequence.Fragments))
	for i, data := range pixel.Sequence.Fragments {
		frames[i] = core.CloneBytes(data)
	}
	return pixeldata.Frames{Rows: int(m.Rows), Columns: int(m.Columns), Data: frames}, nil
}

func TestStoreLossyVerificationSelectedHandleAfterRetry(t *testing.T) {
	limit := 2.
	opts := testOptions(t, transfer.JPEGLSNearLossless)
	opts.Transcode.EncoderRegistry = pixeldata.NewMemoryEncoderRegistry()
	encoder := &lossyRetryEncoder{}
	encoder.delta.Store(1)
	if err := opts.Transcode.EncoderRegistry.RegisterEncoder(opts.Target.UID, encoder); err != nil {
		t.Fatal(err)
	}
	opts.Transcode.DecoderRegistry = pixeldata.NewMemoryRegistry()
	if err := opts.Transcode.DecoderRegistry.RegisterCodec(opts.Target.UID, lossyRetryDecoder{}); err != nil {
		t.Fatal(err)
	}
	opts.Transcode.AllowLossy = true
	opts.Transcode.MaxPixelError = &limit
	source, err := NewSource(dimse.NewFileStoreSource(testFile(), 0), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Inspect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if source.Report().LossyVerification.MaximumError != 1 {
		t.Fatal("preflight metric")
	}
	encoder.delta.Store(3)
	if _, err := source.Open(context.Background()); !errors.Is(err, pixeldata.ErrTranscodePixelError) {
		t.Fatal("retry refusal", err)
	}
	if source.Report().VerificationFailure == nil {
		t.Fatal("missing retry failure")
	}
	encoder.delta.Store(2)
	opened, err := source.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = encodeSource(t, opened, opts.Target)
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	report := source.Report()
	if report.LossyVerification == nil || report.LossyVerification.MaximumError != 2 || report.VerificationFailure != nil || report.SelectedTransferSyntaxUID != opts.Target.UID {
		t.Fatalf("selected handle metrics stale: %+v", report)
	}
	assertEmpty(t, opts.SpoolDirectory)
}

func TestStoreLossyVerificationAcceptReportOwnership(t *testing.T) {
	limit := 1.
	source, opts := lossyVerifiedSource(t, &limit)
	// Constructor snapshots policy for both preparation and later Open calls.
	limit = 0
	descriptor, err := source.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertEmpty(t, opts.SpoolDirectory)
	if len(descriptor.WritableTransferSyntaxUIDs) != 1 || descriptor.WritableTransferSyntaxUIDs[0] != opts.Target.UID {
		t.Fatal("unverified representation offered")
	}
	report := source.Report()
	metrics := report.LossyVerification
	if metrics == nil || metrics.MaximumError != 1 || metrics.Limit != 1 || metrics.Frames != 2 || report.VerificationFailure != nil {
		t.Fatalf("report %+v", report)
	}
	metrics.MaximumError = 999
	metrics.Limit = 999
	if source.Report().LossyVerification.MaximumError != 1 {
		t.Fatal("report aliases source")
	}
	opened, err := source.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wire := encodeSource(t, opened, opts.Target)
	if len(wire) == 0 {
		t.Fatal("missing accepted bytes")
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	assertEmpty(t, opts.SpoolDirectory)
}

func TestStoreLossyVerificationRefusalBeforeOpenAndDial(t *testing.T) {
	limit := 0.
	source, opts := lossyVerifiedSource(t, &limit)
	_, err := source.Inspect(context.Background())
	var diagnostic *pixeldata.LossyVerificationError
	if !errors.Is(err, pixeldata.ErrTranscodePixelError) || !errors.As(err, &diagnostic) || diagnostic.Measured != 1 || !strings.Contains(err.Error(), "measured=1") {
		t.Fatalf("diagnostic %v", err)
	}
	if opened, err := source.Open(context.Background()); err == nil || opened.WriteDataSet != nil {
		t.Fatal("failed verification supplied a writer")
	}
	assertEmpty(t, opts.SpoolDirectory)
	report := source.Report()
	if report.LossyVerification != nil || report.VerificationFailure == nil || len(report.Candidates) != 1 || report.Candidates[0].Producible || report.Candidates[0].Reason != "lossy-error-exceeded" {
		t.Fatalf("failed report %+v", report)
	}
	report.VerificationFailure.Measured = 999
	if source.Report().VerificationFailure.Measured != 1 {
		t.Fatal("failure report aliases source")
	}
	var dials atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "STORE", Context: ctx, SupportedAbstractSyntaxes: []string{"1.2.840.10008.5.1.4.1.1.7"}, SupportedTransferSyntaxes: []string{opts.Target.UID}})
		if err == nil {
			dials.Add(1)
			_ = assoc.Close()
		}
	}()
	session, err := dimse.NewStoreSession(listener.Addr().String(), dimse.StoreSessionOptions{DialOptions: ul.DialOptions{CalledAETitle: "STORE", CallingAETitle: "TEST", NegotiationTimeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.StoreBatch(ctx, []dimse.StoreSource{source})
	// StoreBatch deliberately redacts preparation causes. The source report
	// above remains the diagnostic authority; the batch proves no attempt.
	if err == nil || result.Associations != 0 || result.Succeeded != 0 || len(result.Items) != 1 || result.Items[0].Attempted || dials.Load() != 0 {
		t.Fatalf("refused source reached network: %v %+v", err, result)
	}
	_ = session.Close(ctx)
	cancel()
	_ = listener.Close()
	<-done
	assertEmpty(t, opts.SpoolDirectory)
}

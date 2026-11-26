package dicomweb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeg"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeglossless"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func compressedSTOWFixture(t *testing.T, syntax string, count int) (*object.Object, []StoreBulkData, [][]byte) {
	t.Helper()
	obj := object.New(nil)
	for tag, text := range map[core.Tag]string{core.NewTag(0x0008, 0x0016): dicomtest.TestSOPClassUID, core.NewTag(0x0008, 0x0018): dicomtest.TestSOPInstanceUID, core.NewTag(0x0020, 0x000d): dicomtest.TestStudyInstanceUID, core.NewTag(0x0020, 0x000e): dicomtest.TestSeriesInstanceUID} {
		obj.Put(core.NewRawElement(tag, core.VRUI, []byte(text)))
	}
	if count > 1 {
		obj.Put(core.NewRawElement(core.NewTag(0x0028, 0x0008), core.VRIS, []byte(strconv.Itoa(count))))
	}
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB}, Value: core.BulkDataValue{URI: "urn:synthetic:compressed-pixels"}})
	allocated, stored := uint16(8), uint16(8)
	if syntax == transfer.JPEGExtended.UID {
		allocated, stored = 16, 12
	}
	metadata := pixeldata.Metadata{Rows: 2, Columns: 3, SamplesPerPixel: 1, BitsAllocated: allocated, BitsStored: stored, HighBit: stored - 1, PhotometricInterpretation: "MONOCHROME2", NumberOfFrames: 1}
	var encoder pixeldata.FrameEncoder
	var err error
	switch syntax {
	case transfer.JPEGBaseline.UID:
		encoder, err = jpeg.NewEncoder(90)
	case transfer.JPEGExtended.UID:
		encoder, err = jpeg.NewExtendedEncoder(90)
	case transfer.JPEGLosslessNonHierarchical.UID:
		encoder, err = jpeglossless.NewProcess14Encoder(2)
	case transfer.JPEGLosslessSV1.UID:
		encoder = jpeglossless.NewSV1Encoder()
	case transfer.JPEGLSLossless.UID:
		encoder = jpegls.NewEncoder()
	case transfer.JPEGLSNearLossless.UID:
		encoder, err = jpegls.NewEncoderWithOptions(jpegls.EncoderOptions{Near: 1, AllowLossy: true})
	default:
		t.Fatal("unannounced synthetic syntax", syntax)
	}
	if err != nil {
		t.Fatal(err)
	}
	// Sign is not encoded in lossless JPEG/JPEG-LS: supply it explicitly.
	if syntax != transfer.JPEGBaseline.UID && syntax != transfer.JPEGExtended.UID {
		obj.Put(core.NewRawElement(core.NewTag(0x0028, 0x0103), core.VRUS, []byte{0, 0}))
	}
	media := "image/jpeg"
	if transfer.IsJPEGLSTransferSyntax(syntax) {
		media = "image/jls"
	}
	bulk := make([]StoreBulkData, count)
	frames := make([][]byte, count)
	for i := range bulk {
		native := make([]byte, 6*int(allocated/8))
		for sample := 0; sample < 6; sample++ {
			value := uint16(17 + i*31 + sample*7)
			if allocated == 8 {
				native[sample] = byte(value)
			} else {
				binary.LittleEndian.PutUint16(native[2*sample:], value)
			}
		}
		encoded, err := encoder.EncodeFrame(context.Background(), native, metadata)
		if err != nil {
			t.Fatal(err)
		}
		frames[i] = encoded.Data
		bulk[i] = StoreBulkData{URI: "urn:synthetic:compressed-pixels", ContentType: media + "; transfer-syntax=" + syntax, Data: encoded.Data}
		if count > 1 {
			bulk[i].FrameNumber = i + 1
		}
	}
	return obj, bulk, frames
}
func compressedSTOWEmpty(t *testing.T, parent string) {
	t.Helper()
	items, err := os.ReadDir(parent)
	if err != nil || len(items) != 0 {
		t.Fatalf("unremoved staging %d %v", len(items), err)
	}
}

func TestCompressedSTOWClientServerJSONXMLFrameMatrix(t *testing.T) {
	for _, syntax := range []string{transfer.JPEGBaseline.UID, transfer.JPEGExtended.UID, transfer.JPEGLosslessNonHierarchical.UID, transfer.JPEGLosslessSV1.UID, transfer.JPEGLSLossless.UID, transfer.JPEGLSNearLossless.UID} {
		for _, media := range []MetadataMediaType{MetadataMediaTypeDICOMJSON, MetadataMediaTypeDICOMXML} {
			for _, count := range []int{1, 2} {
				t.Run(fmt.Sprintf("%s/%s/%d", syntax, media, count), func(t *testing.T) {
					obj, bulk, frames := compressedSTOWFixture(t, syntax, count)
					parent := t.TempDir()
					backend := completeServerTestBackend()
					var calls atomic.Int32
					backend.store = func(ctx context.Context, request StoreRequest) StoreOutcome {
						calls.Add(1)
						file, err := object.ReadFile(request.Reader)
						if err != nil {
							return StoreOutcome{Status: StoreStatusFailed, Err: err}
						}
						defer file.Close()
						if request.TransferSyntaxUID != syntax || file.TransferSyntax.UID != syntax {
							return StoreOutcome{Status: StoreStatusFailed, Err: fmt.Errorf("syntax mismatch %s/%s", request.TransferSyntaxUID, file.TransferSyntax.UID)}
						}
						meta, _ := file.Meta.GetString(core.NewTag(0x0002, 0x0010))
						if meta != syntax {
							return StoreOutcome{Status: StoreStatusFailed, Err: fmt.Errorf("wrong FileMeta %s", meta)}
						}
						pixel, ok := file.Dataset.Get(core.TagPixelData)
						if !ok {
							return StoreOutcome{Status: StoreStatusFailed, Err: fmt.Errorf("missing PixelData")}
						}
						seq, ok := pixel.Value.(core.FragmentSequence)
						if !ok || len(seq.Fragments) != count || len(seq.OffsetTable) != 4*count {
							return StoreOutcome{Status: StoreStatusFailed, Err: fmt.Errorf("bad encapsulation")}
						}
						for i, frame := range frames {
							expected := append([]byte(nil), frame...)
							if len(expected)%2 == 1 {
								expected = append(expected, 0)
							}
							if !bytes.Equal(seq.Fragments[i], expected) {
								return StoreOutcome{Status: StoreStatusFailed, Err: fmt.Errorf("codestream changed at frame%d", i+1)}
							}
						}
						allocated, stored := int64(8), int64(8)
						if syntax == transfer.JPEGExtended.UID {
							allocated, stored = 16, 12
						}
						for tag, want := range map[core.Tag]int64{core.NewTag(0x0028, 0x0010): 2, core.NewTag(0x0028, 0x0011): 3, core.NewTag(0x0028, 0x0002): 1, core.NewTag(0x0028, 0x0100): allocated, core.NewTag(0x0028, 0x0101): stored, core.NewTag(0x0028, 0x0102): stored - 1, core.NewTag(0x0028, 0x0103): 0} {
							got, err := file.Dataset.GetInt(tag)
							if err != nil || got != want {
								return StoreOutcome{Status: StoreStatusFailed, Err: fmt.Errorf("wrong derived attribute %s", tag)}
							}
						}
						photo, _ := file.Dataset.GetString(core.NewTag(0x0028, 0x0004))
						framesCount, frameErr := storeMetadataFrameCount(file.Dataset)
						if photo != "MONOCHROME2" || frameErr != nil || framesCount != count {
							return StoreOutcome{Status: StoreStatusFailed, Err: fmt.Errorf("wrong photo/frame metadata")}
						}
						return StoreOutcome{Status: StoreStatusStored}
					}
					server, err := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: parent})
					if err != nil {
						t.Fatal(err)
					}
					peer := httptest.NewServer(server)
					defer peer.Close()
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					result, err := (Client{Endpoint: Endpoint{BaseURL: peer.URL}}).StoreMetadata(ctx, StoreMetadataRequest{MediaType: media, Instances: []*object.Object{obj}, BulkData: bulk})
					if err != nil || calls.Load() != 1 || len(result.Failed) > 0 {
						t.Fatalf("result=%+v err=%v calls=%d", result, err, calls.Load())
					}
					compressedSTOWEmpty(t, parent)
				})
			}
		}
	}
}

func compressedSTOWParts(t *testing.T, obj *object.Object, bulk []StoreBulkData) []stowMetadataTestPart {
	t.Helper()
	metadata, err := encodeStoreMetadata(context.Background(), MetadataMediaTypeDICOMJSON, []*object.Object{obj})
	if err != nil {
		t.Fatal(err)
	}
	parts := []stowMetadataTestPart{{contentType: "application/dicom+json", body: metadata[0]}}
	for _, b := range bulk {
		parts = append(parts, stowMetadataTestPart{contentType: b.ContentType, contentLocation: storeBulkContentLocation(b), body: b.Data})
	}
	return parts
}
func compressedSTOWSend(t *testing.T, server *Server, ctx context.Context, parts []stowMetadataTestPart, truncate bool) int {
	t.Helper()
	body, ct := stowMetadataMultipart(t, "application/dicom+json", parts...)
	if truncate {
		data := body.Bytes()
		body = bytes.NewBuffer(data[:len(data)-8])
	}
	request := httptest.NewRequest(http.MethodPost, "/studies", body).WithContext(ctx)
	request.Header.Set("Content-Type", ct)
	request.Header.Set("Accept", "application/dicom+json")
	w := httptest.NewRecorder()
	server.ServeHTTP(w, request)
	return w.Code
}

func TestCompressedSTOWContradictionsMissingAndTransactionIsolation(t *testing.T) {
	for _, name := range []string{"rows", "stored", "sign-required", "syntax", "MIME", "missing", "duplicate", "frame-count", "frame-gap", "entropy", "truncation", "other-request"} {
		t.Run(name, func(t *testing.T) {
			obj, bulk, _ := compressedSTOWFixture(t, transfer.JPEGLosslessSV1.UID, 2)
			parent := t.TempDir()
			backend := completeServerTestBackend()
			var calls atomic.Int32
			backend.store = func(context.Context, StoreRequest) StoreOutcome {
				calls.Add(1)
				return StoreOutcome{Status: StoreStatusStored}
			}
			switch name {
			case "rows":
				obj.Put(core.NewRawElement(core.NewTag(0x0028, 0x0010), core.VRUS, []byte{3, 0}))
			case "stored":
				obj.Put(core.NewRawElement(core.NewTag(0x0028, 0x0101), core.VRUS, []byte{9, 0}))
			case "sign-required":
				obj.Remove(core.NewTag(0x0028, 0x0103))
			case "syntax":
				bulk[0].ContentType = "image/jpeg; transfer-syntax=" + transfer.JPEGBaseline.UID
			case "MIME":
				bulk[0].ContentType = "image/jls; transfer-syntax=" + transfer.JPEGLSLossless.UID
			case "frame-count":
				obj.Put(core.NewRawElement(core.NewTag(0x0028, 0x0008), core.VRIS, []byte("1")))
			case "frame-gap":
				bulk[1].FrameNumber = 3
			case "entropy":
				bulk[0].Data = append(append([]byte(nil), bulk[0].Data[:len(bulk[0].Data)-4]...), 0xff, 0xd9)
			}
			parts := compressedSTOWParts(t, obj, bulk)
			if name == "missing" {
				parts = parts[:2]
			}
			if name == "duplicate" {
				parts = append(parts, parts[1])
			}
			server, err := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: parent})
			if err != nil {
				t.Fatal(err)
			}
			if name == "other-request" {
				if status := compressedSTOWSend(t, server, context.Background(), parts, false); status != http.StatusOK {
					t.Fatal(status)
				}
				calls.Store(0)
				parts = parts[:1]
			}
			status := compressedSTOWSend(t, server, context.Background(), parts, name == "truncation")
			if status != http.StatusBadRequest || calls.Load() != 0 {
				t.Fatalf("status=%d calls=%d", status, calls.Load())
			}
			compressedSTOWEmpty(t, parent)
		})
	}
}

func TestCompressedSTOWLimitsCancellationAndStagingCleanup(t *testing.T) {
	for _, name := range []string{"parts", "part-bytes", "body-bytes", "decoded-bytes", "cancelled", "disk"} {
		t.Run(name, func(t *testing.T) {
			obj, bulk, _ := compressedSTOWFixture(t, transfer.JPEGBaseline.UID, 2)
			parent := t.TempDir()
			options := ServerOptions{Backend: completeServerTestBackend(), AllowUnauthenticated: true, SpoolDirectory: parent}
			limits := DefaultServerLimits()
			ctx := context.Background()
			switch name {
			case "parts":
				limits.MaxParts = 2
			case "part-bytes":
				limits.MaxPartBytes = 8
			case "body-bytes":
				limits.MaxRequestBytes = 16
			case "decoded-bytes":
				limits.MaxDecodedPartBytes = 4
			case "cancelled":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			case "disk":
				blocked := parent + "/not-directory"
				if err := os.Mkdir(blocked, 0700); err != nil {
					t.Fatal(err)
				}
				options.SpoolDirectory = blocked
			}
			var calls atomic.Int32
			backend := completeServerTestBackend()
			backend.store = func(context.Context, StoreRequest) StoreOutcome {
				calls.Add(1)
				return StoreOutcome{Status: StoreStatusStored}
			}
			options.Backend = backend
			options.Limits = limits
			server, err := NewServer(options)
			if err != nil {
				t.Fatal(err)
			}
			// Construct with a valid directory, then cause a real request-time
			// filesystem failure. Constructor validation must not short-circuit it.
			if name == "disk" {
				if err := os.Remove(options.SpoolDirectory); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(options.SpoolDirectory, []byte("synthetic"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			status := compressedSTOWSend(t, server, ctx, compressedSTOWParts(t, obj, bulk), false)
			if status >= 200 && status < 300 || calls.Load() != 0 {
				t.Fatalf("status=%d calls=%d", status, calls.Load())
			}
			if name == "disk" {
				if status != http.StatusServiceUnavailable {
					t.Fatalf("request-time spool failure status=%d", status)
				}
				if err := os.Remove(options.SpoolDirectory); err != nil {
					t.Fatal(err)
				}
			}
			compressedSTOWEmpty(t, parent)
		})
	}
}

func TestCompressedSTOWPreservesPartialBackendOutcomes(t *testing.T) {
	obj, bulk, _ := compressedSTOWFixture(t, transfer.JPEGBaseline.UID, 1)
	second := object.FromElements(obj.Elements(), nil)
	second.Put(core.NewRawElement(core.NewTag(0x0008, 0x0018), core.VRUI, []byte(dicomtest.TestSOPInstanceUID+".2")))
	parent := t.TempDir()
	backend := completeServerTestBackend()
	var calls atomic.Int32
	backend.store = func(_ context.Context, r StoreRequest) StoreOutcome {
		if _, err := io.Copy(io.Discard, r.Reader); err != nil {
			return StoreOutcome{Status: StoreStatusFailed, Err: err}
		}
		if calls.Add(1) == 1 {
			return StoreOutcome{Status: StoreStatusStored}
		}
		return StoreOutcome{Status: StoreStatusConflict, FailureReason: 0x0110}
	}
	server, err := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: parent})
	if err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewServer(server)
	defer peer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := (Client{Endpoint: Endpoint{BaseURL: peer.URL}}).StoreMetadata(ctx, StoreMetadataRequest{Instances: []*object.Object{obj, second}, BulkData: bulk})
	if err != nil || calls.Load() != 2 || len(result.Failed) != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls.Load())
	}
	compressedSTOWEmpty(t, parent)
}

func TestCompressedSTOWClientRefusesDuplicateAndIncompleteFrames(t *testing.T) {
	obj, bulk, _ := compressedSTOWFixture(t, transfer.JPEGBaseline.UID, 2)
	for _, parts := range [][]StoreBulkData{bulk[:1], {bulk[0], bulk[0]}} {
		if err := validateStoreBulkData(StoreMetadataRequest{Instances: []*object.Object{obj}, BulkData: parts}); err == nil {
			t.Fatal("invalid frame graph accepted")
		}
	}
	if err := validateStoreBulkData(StoreMetadataRequest{Instances: []*object.Object{obj}, BulkData: bulk}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(storeBulkContentLocation(bulk[1]), "/frames/2") {
		t.Fatal("frame binding changed")
	}
}

type compressedSTOWCancellingBody struct {
	reader               io.Reader
	parent               string
	cancel               context.CancelFunc
	reached              bool
	reads, positiveFiles int
}

func (b *compressedSTOWCancellingBody) Read(p []byte) (int, error) {
	b.reads++
	if len(p) > 32 {
		p = p[:32]
	}
	entries, _ := os.ReadDir(b.parent)
	positive := 0
	for _, entry := range entries {
		info, err := entry.Info()
		if err == nil && info.Mode().IsRegular() && strings.HasPrefix(entry.Name(), ".dicomweb-stow-metadata-") && info.Size() > 0 {
			positive++
		}
	}
	b.positiveFiles = max(b.positiveFiles, positive)
	if positive >= 2 && !b.reached {
		b.reached = true
		b.cancel()
	}
	return b.reader.Read(p)
}
func (*compressedSTOWCancellingBody) Close() error { return nil }

func TestCompressedSTOWCancelAfterActualStaging(t *testing.T) {
	obj, bulk, _ := compressedSTOWFixture(t, transfer.JPEGBaseline.UID, 2)
	parent := t.TempDir()
	backend := completeServerTestBackend()
	var calls atomic.Int32
	backend.store = func(context.Context, StoreRequest) StoreOutcome {
		calls.Add(1)
		return StoreOutcome{Status: StoreStatusStored}
	}
	server, err := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: parent})
	if err != nil {
		t.Fatal(err)
	}
	bytesBody, ct := stowMetadataMultipart(t, "application/dicom+json", compressedSTOWParts(t, obj, bulk)...)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	body := &compressedSTOWCancellingBody{reader: bytesBody, parent: parent, cancel: stop}
	request := httptest.NewRequest(http.MethodPost, "/studies", body).WithContext(ctx)
	request.Header.Set("Content-Type", ct)
	request.Header.Set("Accept", "application/dicom+json")
	request.Body = body
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); server.ServeHTTP(w, request) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled staging did not join")
	}
	if !body.reached || body.reads == 0 || body.positiveFiles < 2 || calls.Load() != 0 || w.Code != statusClientClosedRequest {
		t.Fatalf("staging reached=%v reads=%d positiveFiles=%d calls=%d status=%d", body.reached, body.reads, body.positiveFiles, calls.Load(), w.Code)
	}
	compressedSTOWEmpty(t, parent)
}

type compressedSTOWTerminalBody struct {
	data                 []byte
	err                  error
	closed               int
	reads, terminalBytes int
}

func (b *compressedSTOWTerminalBody) Read(p []byte) (int, error) {
	b.reads++
	if len(b.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	if len(b.data) == 0 {
		b.terminalBytes = n
		return n, b.err
	}
	return n, nil
}
func (b *compressedSTOWTerminalBody) Close() error { b.closed++; return nil }

func TestCompressedSTOWRetainsErrorAlongsideTerminalBoundary(t *testing.T) {
	obj, bulk, _ := compressedSTOWFixture(t, transfer.JPEGBaseline.UID, 1)
	parent := t.TempDir()
	backend := completeServerTestBackend()
	var calls atomic.Int32
	backend.store = func(context.Context, StoreRequest) StoreOutcome {
		calls.Add(1)
		return StoreOutcome{Status: StoreStatusStored}
	}
	server, err := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: parent})
	if err != nil {
		t.Fatal(err)
	}
	encoded, ct := stowMetadataMultipart(t, "application/dicom+json", compressedSTOWParts(t, obj, bulk)...)
	body := &compressedSTOWTerminalBody{data: encoded.Bytes(), err: io.ErrUnexpectedEOF}
	request := httptest.NewRequest(http.MethodPost, "/studies", body)
	request.Header.Set("Content-Type", ct)
	request.Header.Set("Accept", "application/dicom+json")
	request.Body = body
	w := httptest.NewRecorder()
	server.ServeHTTP(w, request)
	if w.Code != http.StatusBadRequest || calls.Load() != 0 || body.reads == 0 || body.terminalBytes == 0 {
		t.Fatalf("status=%d calls=%d reads=%d terminalBytes=%d", w.Code, calls.Load(), body.reads, body.terminalBytes)
	}
	compressedSTOWEmpty(t, parent)
}

func TestCompressedSTOWAmbiguousColorRequiresMetadata(t *testing.T) {
	obj, bulk, _ := compressedSTOWFixture(t, transfer.JPEGBaseline.UID, 1)
	encoder, err := jpeg.NewEncoder(90)
	if err != nil {
		t.Fatal(err)
	}
	metadata := pixeldata.Metadata{Rows: 2, Columns: 3, SamplesPerPixel: 3, BitsAllocated: 8, BitsStored: 8, HighBit: 7, PhotometricInterpretation: "RGB", NumberOfFrames: 1, PlanarConfiguration: 0, PlanarConfigurationPresent: true}
	encoded, err := encoder.EncodeFrame(context.Background(), []byte{1, 2, 3, 14, 25, 36, 47, 58, 69, 71, 82, 93, 104, 115, 126, 137, 148, 159}, metadata)
	if err != nil {
		t.Fatal(err)
	}
	bulk[0].Data = encoded.Data
	parent := t.TempDir()
	backend := completeServerTestBackend()
	var calls atomic.Int32
	backend.store = func(context.Context, StoreRequest) StoreOutcome {
		calls.Add(1)
		return StoreOutcome{Status: StoreStatusStored}
	}
	server, err := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: parent})
	if err != nil {
		t.Fatal(err)
	}
	if status := compressedSTOWSend(t, server, context.Background(), compressedSTOWParts(t, obj, bulk), false); status != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatalf("ambiguous color status=%d calls=%d", status, calls.Load())
	}
	compressedSTOWEmpty(t, parent)
	photo := encoded.PhotometricInterpretation
	if photo == "" {
		photo = "RGB"
	}
	obj.Put(core.NewRawElement(core.NewTag(0x0028, 0x0004), core.VRCS, []byte(photo)))
	if status := compressedSTOWSend(t, server, context.Background(), compressedSTOWParts(t, obj, bulk), false); status != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("explicit color status=%d calls=%d", status, calls.Load())
	}
	compressedSTOWEmpty(t, parent)
}

func TestSTOWNativeOpaquePrefixReferencesRemainIndependent(t *testing.T) {
	for _, media := range []MetadataMediaType{MetadataMediaTypeDICOMJSON, MetadataMediaTypeDICOMXML} {
		t.Run(string(media), func(t *testing.T) {
			obj := object.New(nil)
			for tag, text := range map[core.Tag]string{core.NewTag(0x0008, 0x0016): dicomtest.TestSOPClassUID, core.NewTag(0x0008, 0x0018): dicomtest.TestSOPInstanceUID, core.NewTag(0x0020, 0x000d): dicomtest.TestStudyInstanceUID, core.NewTag(0x0020, 0x000e): dicomtest.TestSeriesInstanceUID} {
				obj.Put(core.NewRawElement(tag, core.VRUI, []byte(text)))
			}
			other := core.NewTag(0x0011, 0x1010)
			obj.Put(core.NewRawElement(core.NewTag(0x0011, 0x0010), core.VRLO, []byte("SYNTHETIC-STOW")))
			obj.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB}, Value: core.BulkDataValue{URI: "x"}})
			obj.Put(core.Element{Header: core.ElementHeader{Tag: other, VR: core.VROB}, Value: core.BulkDataValue{URI: "x/frames/1"}})
			expected := map[core.Tag][]byte{core.TagPixelData: {1, 2, 3, 4}, other: {5, 6, 7, 8}}
			parent := t.TempDir()
			backend := completeServerTestBackend()
			var calls atomic.Int32
			backend.store = func(_ context.Context, request StoreRequest) StoreOutcome {
				calls.Add(1)
				file, err := object.ReadFile(request.Reader)
				if err != nil {
					return StoreOutcome{Status: StoreStatusFailed, Err: err}
				}
				defer file.Close()
				if request.TransferSyntaxUID != explicitVRLittleEndianUID || file.TransferSyntax.UID != explicitVRLittleEndianUID {
					return StoreOutcome{Status: StoreStatusFailed, Err: fmt.Errorf("native syntax changed")}
				}
				for tag, want := range expected {
					element, ok := file.Dataset.Get(tag)
					raw, valid := element.RawBytes()
					if !ok || !valid || !bytes.Equal(raw, want) {
						return StoreOutcome{Status: StoreStatusFailed, Err: fmt.Errorf("independent native key changed %s", tag)}
					}
				}
				return StoreOutcome{Status: StoreStatusStored}
			}
			server, err := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: parent})
			if err != nil {
				t.Fatal(err)
			}
			peer := httptest.NewServer(server)
			defer peer.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result, err := (Client{Endpoint: Endpoint{BaseURL: peer.URL}}).StoreMetadata(ctx, StoreMetadataRequest{MediaType: media, Instances: []*object.Object{obj}, BulkData: []StoreBulkData{{URI: "x", ContentType: "application/octet-stream", Data: expected[core.TagPixelData]}, {URI: "x/frames/1", ContentType: "application/octet-stream", Data: expected[other]}}})
			if err != nil || calls.Load() != 1 || len(result.Failed) != 0 {
				t.Fatalf("result=%+v err=%v calls=%d", result, err, calls.Load())
			}
			compressedSTOWEmpty(t, parent)
		})
	}
}

func TestCompressedSTOWLosslessSeparateScansWithoutSyntaxParameter(t *testing.T) {
	// Independent RGB fixture/reference from jpeglossless/multicomponent_conformance_test.go:
	// libjpeg-turbo 3.1.0 predictors 1,2,7, exact djpeg -strict -rgb -pnm pixels.
	frame, err := base64.StdEncoding.DecodeString("/9j/7gAOQWRvYmUAZAAAAAAA/8MAEQgAAwAEA1IRAEcRAEIRAP/EABoAAAICAwAAAAAAAAAAAAAAAAcIAAEDBQb/2gAIAVIAAQAAX9/0Al7B7QDkLxl7f//EABgAAQADAQAAAAAAAAAAAAAAAAgAAgcG/9oACAFHAAIAAD/H+ALdDXQHxoYMX/8A/8QAGQABAAIDAAAAAAAAAAAAAAAACAAHAgMG/9oACAFCAAcAAD/I/wDOwG4Ir63dYgf/2Q==")
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(frame)) != "757c5ba6c9a2628e56f934e316125c34eb6ff4f86c7bff369a678acb6315892d" {
		t.Fatalf("fixture provenance: %v", err)
	}
	reference := []byte{0, 0, 0, 255, 0, 0, 0, 255, 0, 0, 0, 255, 1, 2, 3, 17, 33, 65, 254, 253, 252, 128, 64, 32, 5, 250, 125, 99, 100, 101, 200, 10, 220, 255, 255, 255}
	for _, media := range []MetadataMediaType{MetadataMediaTypeDICOMJSON, MetadataMediaTypeDICOMXML} {
		t.Run(string(media), func(t *testing.T) {
			obj := object.New(nil)
			for tag, text := range map[core.Tag]string{core.NewTag(0x0008, 0x0016): dicomtest.TestSOPClassUID, core.NewTag(0x0008, 0x0018): dicomtest.TestSOPInstanceUID, core.NewTag(0x0020, 0x000d): dicomtest.TestStudyInstanceUID, core.NewTag(0x0020, 0x000e): dicomtest.TestSeriesInstanceUID} {
				obj.Put(core.NewRawElement(tag, core.VRUI, []byte(text)))
			}
			obj.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB}, Value: core.BulkDataValue{URI: "rgb"}})
			obj.Put(core.NewRawElement(core.NewTag(0x0028, 0x0004), core.VRCS, []byte("RGB")))
			obj.Put(core.NewRawElement(core.NewTag(0x0028, 0x0103), core.VRUS, []byte{0, 0}))
			parent := t.TempDir()
			backend := completeServerTestBackend()
			var calls atomic.Int32
			backend.store = func(ctx context.Context, request StoreRequest) StoreOutcome {
				calls.Add(1)
				file, err := object.ReadFile(request.Reader)
				if err != nil {
					return StoreOutcome{Status: StoreStatusFailed, Err: err}
				}
				defer file.Close()
				uid := transfer.JPEGLosslessNonHierarchical.UID
				meta, _ := file.Meta.GetString(core.NewTag(0x0002, 0x0010))
				if request.TransferSyntaxUID != uid || file.TransferSyntax.UID != uid || meta != uid {
					return StoreOutcome{Status: StoreStatusFailed, Err: fmt.Errorf("mixed-predictor syntax %s/%s/%s", request.TransferSyntaxUID, file.TransferSyntax.UID, meta)}
				}
				element, _ := file.Dataset.Get(core.TagPixelData)
				seq, ok := element.Value.(core.FragmentSequence)
				want := append([]byte(nil), frame...)
				if len(want)%2 == 1 {
					want = append(want, 0)
				}
				if !ok || len(seq.Fragments) != 1 || len(seq.OffsetTable) != 4 || !bytes.Equal(seq.Fragments[0], want) {
					return StoreOutcome{Status: StoreStatusFailed, Err: fmt.Errorf("codestream changed")}
				}
				registry := pixeldata.NewMemoryRegistry()
				if err := jpeglossless.Register(registry); err != nil {
					return StoreOutcome{Status: StoreStatusFailed, Err: err}
				}
				decoded, err := registry.DecodeFramesContext(ctx, uid, pixeldata.PixelData{Sequence: seq, Encapsulated: true}, file.Dataset)
				if err != nil || decoded.Rows != 3 || decoded.Columns != 4 || len(decoded.Data) != 1 || !bytes.Equal(decoded.Data[0], reference) {
					return StoreOutcome{Status: StoreStatusFailed, Err: fmt.Errorf("independent RGB mismatch: %v", err)}
				}
				return StoreOutcome{Status: StoreStatusStored}
			}
			server, err := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: parent})
			if err != nil {
				t.Fatal(err)
			}
			peer := httptest.NewServer(server)
			defer peer.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result, err := (Client{Endpoint: Endpoint{BaseURL: peer.URL}}).StoreMetadata(ctx, StoreMetadataRequest{MediaType: media, Instances: []*object.Object{obj}, BulkData: []StoreBulkData{{URI: "rgb", ContentType: "image/jpeg", Data: frame}}})
			if err != nil || calls.Load() != 1 || len(result.Failed) != 0 {
				t.Fatalf("result=%+v err=%v calls=%d", result, err, calls.Load())
			}
			compressedSTOWEmpty(t, parent)
		})
	}
}

package dicomweb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
)

type clientTestRenderer func(context.Context, RenderRequest, func(RenderedPart) error) error

func (f clientTestRenderer) Render(ctx context.Context, request RenderRequest, yield func(RenderedPart) error) error {
	return f(ctx, request, yield)
}

// newClientRenderServer serves rendered resources under /dicomweb. Each
// rendered part carries its instance or frame identity in the payload.
func newClientRenderServer(t *testing.T, seen *RenderRequest) Client {
	t.Helper()
	renderer := clientTestRenderer(func(_ context.Context, request RenderRequest, yield func(RenderedPart) error) error {
		*seen = request
		contentType := request.Accept[0].MediaType
		emit := func(ref InstanceRef, frame int) error {
			payload := fmt.Sprintf("%s/%d", ref.SOPInstanceUID, frame)
			return yield(RenderedPart{Ref: ref, FrameNumber: frame, ContentType: contentType, Width: 2, Height: 2, Reader: strings.NewReader(payload), Size: int64(len(payload))})
		}
		switch request.Level {
		case RenderLevelStudy, RenderLevelSeries:
			count := 2
			if request.Thumbnail {
				count = 1
			}
			for index := 1; index <= count; index++ {
				ref := InstanceRef{StudyInstanceUID: request.Ref.StudyInstanceUID, SeriesInstanceUID: "1.2.3.1", SOPInstanceUID: fmt.Sprintf("1.2.3.1.%d", index)}
				if err := emit(ref, 0); err != nil {
					return err
				}
			}
		case RenderLevelInstance:
			return emit(request.Ref, 0)
		case RenderLevelFrames:
			frames := request.Frames
			if request.Thumbnail {
				frames = frames[:1]
			}
			for _, frame := range frames {
				if err := emit(request.Ref, frame); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return newMountedClientTestServer(t, completeServerTestBackend(), renderer)
}

// newMountedClientTestServer mounts the library server at /dicomweb, as a
// deployment would, and returns a client for that service root.
func newMountedClientTestServer(t *testing.T, backend any, renderer Renderer) Client {
	t.Helper()
	server, err := NewServer(ServerOptions{Backend: backend, Renderer: renderer, AllowUnauthenticated: true, ServiceRoot: "/dicomweb"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	httpServer := httptest.NewServer(http.StripPrefix("/dicomweb", server))
	t.Cleanup(httpServer.Close)
	return Client{Endpoint: Endpoint{BaseURL: httpServer.URL + "/dicomweb"}}
}

func TestClientRetrieveRenderedAgainstServer(t *testing.T) {
	sourceX, sourceWidth := 1.5, 10.0
	tests := []struct {
		name     string
		target   RenderTarget
		opts     RenderedOptions
		payloads []string
		level    RenderLevel
	}{
		{
			name:     "study",
			target:   RenderTarget{StudyInstanceUID: "1.2.3"},
			payloads: []string{"1.2.3.1.1/0", "1.2.3.1.2/0"},
			level:    RenderLevelStudy,
		},
		{
			name:     "series as png",
			target:   RenderTarget{StudyInstanceUID: "1.2.3", SeriesInstanceUID: "1.2.3.1"},
			opts:     RenderedOptions{MediaTypes: []string{"image/png"}},
			payloads: []string{"1.2.3.1.1/0", "1.2.3.1.2/0"},
			level:    RenderLevelSeries,
		},
		{
			name:   "instance with rendering parameters",
			target: RenderTarget{StudyInstanceUID: "1.2.3", SeriesInstanceUID: "1.2.3.1", SOPInstanceUID: "1.2.3.1.9"},
			opts: RenderedOptions{
				Viewport:    &RenderViewport{Width: 64, Height: 32, SourceX: &sourceX, SourceWidth: &sourceWidth},
				Window:      &RenderWindow{Center: 40, Width: 400},
				Quality:     80,
				Annotations: []string{"patient", "technique"},
				ICCProfile:  "srgb",
			},
			payloads: []string{"1.2.3.1.9/0"},
			level:    RenderLevelInstance,
		},
		{
			name:     "selected frames",
			target:   RenderTarget{StudyInstanceUID: "1.2.3", SeriesInstanceUID: "1.2.3.1", SOPInstanceUID: "1.2.3.1.9", Frames: []int{3, 1}},
			payloads: []string{"1.2.3.1.9/3", "1.2.3.1.9/1"},
			level:    RenderLevelFrames,
		},
		{
			name:     "single frame",
			target:   RenderTarget{StudyInstanceUID: "1.2.3", SeriesInstanceUID: "1.2.3.1", SOPInstanceUID: "1.2.3.1.9", Frames: []int{2}},
			payloads: []string{"1.2.3.1.9/2"},
			level:    RenderLevelFrames,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var seen RenderRequest
			client := newClientRenderServer(t, &seen)
			objects, err := client.RetrieveRendered(context.Background(), test.target, test.opts)
			if err != nil {
				t.Fatalf("RetrieveRendered: %v", err)
			}
			if seen.Level != test.level || seen.Thumbnail {
				t.Fatalf("server request = %+v", seen)
			}
			wantType := "image/jpeg"
			if len(test.opts.MediaTypes) > 0 {
				wantType = test.opts.MediaTypes[0]
			}
			if len(objects) != len(test.payloads) {
				t.Fatalf("objects = %d, want %d", len(objects), len(test.payloads))
			}
			for index, object := range objects {
				if string(object.Data) != test.payloads[index] || object.ContentType != wantType || !strings.HasSuffix(object.ContentLocation, "/rendered") {
					t.Fatalf("object %d = %q %q %q", index, object.Data, object.ContentType, object.ContentLocation)
				}
			}
			if test.opts.Viewport != nil {
				viewport := seen.Viewport
				if viewport == nil || viewport.Width != 64 || viewport.Height != 32 || viewport.SourceX == nil || *viewport.SourceX != 1.5 ||
					viewport.SourceY != nil || viewport.SourceWidth == nil || *viewport.SourceWidth != 10 || viewport.SourceHeight != nil {
					t.Fatalf("viewport = %+v", viewport)
				}
				if seen.Window == nil || seen.Window.Center != 40 || seen.Window.Width != 400 || seen.Window.Function != "linear" {
					t.Fatalf("window = %+v", seen.Window)
				}
				if !seen.QualitySet || seen.Quality != 80 || seen.ICCProfile != "srgb" || strings.Join(seen.Annotations, ",") != "patient,technique" {
					t.Fatalf("parameters = %+v", seen)
				}
			}
		})
	}
}

func TestClientRetrieveThumbnailAgainstServer(t *testing.T) {
	targets := []RenderTarget{
		{StudyInstanceUID: "1.2.3"},
		{StudyInstanceUID: "1.2.3", SeriesInstanceUID: "1.2.3.1"},
		{StudyInstanceUID: "1.2.3", SeriesInstanceUID: "1.2.3.1", SOPInstanceUID: "1.2.3.1.9"},
		{StudyInstanceUID: "1.2.3", SeriesInstanceUID: "1.2.3.1", SOPInstanceUID: "1.2.3.1.9", Frames: []int{4, 5}},
	}
	for _, target := range targets {
		t.Run(fmt.Sprintf("%+v", target), func(t *testing.T) {
			var seen RenderRequest
			client := newClientRenderServer(t, &seen)
			thumbnail, err := client.RetrieveThumbnail(context.Background(), target, ThumbnailOptions{
				MediaTypes: []string{"image/png", "image/jpeg"},
				Viewport:   &RenderViewport{Width: 16, Height: 16},
			})
			if err != nil {
				t.Fatalf("RetrieveThumbnail: %v", err)
			}
			if !seen.Thumbnail || seen.Viewport == nil || seen.Viewport.Width != 16 || len(thumbnail.Data) == 0 || thumbnail.ContentType != "image/png" {
				t.Fatalf("thumbnail = %+v request = %+v", thumbnail, seen)
			}
		})
	}
}

func TestClientRenderedRejectsInvalidOptionsBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer httpServer.Close()
	client := Client{Endpoint: Endpoint{BaseURL: httpServer.URL}}
	instance := RenderTarget{StudyInstanceUID: "1.2", SeriesInstanceUID: "1.3", SOPInstanceUID: "1.4"}
	source := 2.0
	tests := []struct {
		name string
		call func() error
	}{
		{"quality", func() error {
			_, err := client.RetrieveRendered(context.Background(), instance, RenderedOptions{Quality: 101})
			return err
		}},
		{"viewport", func() error {
			_, err := client.RetrieveRendered(context.Background(), instance, RenderedOptions{Viewport: &RenderViewport{Width: 0, Height: 1}})
			return err
		}},
		{"window", func() error {
			_, err := client.RetrieveRendered(context.Background(), instance, RenderedOptions{Window: &RenderWindow{Center: 1, Width: 0}})
			return err
		}},
		{"media type", func() error {
			_, err := client.RetrieveRendered(context.Background(), instance, RenderedOptions{MediaTypes: []string{"application/dicom"}})
			return err
		}},
		{"thumbnail video", func() error {
			_, err := client.RetrieveThumbnail(context.Background(), instance, ThumbnailOptions{MediaTypes: []string{"video/mp4"}})
			return err
		}},
		{"thumbnail crop", func() error {
			_, err := client.RetrieveThumbnail(context.Background(), instance, ThumbnailOptions{Viewport: &RenderViewport{Width: 1, Height: 1, SourceX: &source}})
			return err
		}},
		{"frames without instance", func() error {
			_, err := client.RetrieveRendered(context.Background(), RenderTarget{StudyInstanceUID: "1.2", Frames: []int{1}}, RenderedOptions{})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var webErr *Error
			if err := test.call(); !errors.As(err, &webErr) || webErr.Kind != ErrorKindInvalidEndpoint {
				t.Fatalf("error = %v, want invalid endpoint", err)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid options sent %d requests", requests.Load())
	}
}

func newClientBulkServer(t *testing.T) (Client, *atomic.Int32) {
	t.Helper()
	backend := completeServerTestBackend()
	var bulkRequests atomic.Int32
	payloads := map[string]string{"pixel/1": "PIXELBYTES", "nested/2": "NESTED"}
	backend.bulk = func(_ context.Context, request BulkDataRequest) (BulkDataPart, error) {
		bulkRequests.Add(1)
		payload, ok := payloads[request.Token]
		if !ok {
			return BulkDataPart{}, ErrNotFound
		}
		return BulkDataPart{ContentType: "application/octet-stream", Reader: io.NopCloser(strings.NewReader(payload)), Size: int64(len(payload))}, nil
	}
	backend.metadata = func(_ context.Context, _ MetadataRequest, yield func(Dataset) error) error {
		return yield(Dataset{
			"00080016": {VR: "UI", Value: []any{dicomtest.TestSOPClassUID}},
			"00080018": {VR: "UI", Value: []any{dicomtest.TestSOPInstanceUID}},
			"0020000D": {VR: "UI", Value: []any{dicomtest.TestStudyInstanceUID}},
			"0020000E": {VR: "UI", Value: []any{dicomtest.TestSeriesInstanceUID}},
			"00420011": {VR: "OB", BulkDataURI: "/dicomweb/bulkdata/pixel/1"},
			"00081111": {VR: "SQ", Value: []any{Dataset{"00283006": {VR: "OW", BulkDataURI: "/dicomweb/bulkdata/nested/2"}}}},
		})
	}
	return newMountedClientTestServer(t, backend, nil), &bulkRequests
}

func TestClientRetrieveBulkDataAgainstServer(t *testing.T) {
	client, _ := newClientBulkServer(t)
	parts, err := client.RetrieveBulkData(context.Background(), "/dicomweb/bulkdata/pixel/1")
	if err != nil {
		t.Fatalf("RetrieveBulkData: %v", err)
	}
	if len(parts) != 1 || string(parts[0].Data) != "PIXELBYTES" || parts[0].ContentType != "application/octet-stream" {
		t.Fatalf("parts = %+v", parts)
	}
	absolute := client.Endpoint.BaseURL + "/bulkdata/nested/2"
	parts, err = client.RetrieveBulkData(context.Background(), absolute)
	if err != nil || len(parts) != 1 || string(parts[0].Data) != "NESTED" {
		t.Fatalf("absolute same-origin parts = %+v, %v", parts, err)
	}
	if _, err := client.RetrieveBulkData(context.Background(), "/dicomweb/bulkdata/missing/3"); err == nil {
		t.Fatal("missing bulk data succeeded")
	}
}

func TestEndpointBulkDataURLConfinesToEndpointOrigin(t *testing.T) {
	endpoint := Endpoint{BaseURL: "https://pacs.example.invalid:8443/dicomweb"}
	tests := []struct {
		uri  string
		want string
	}{
		{"/dicomweb/bulkdata/a", "https://pacs.example.invalid:8443/dicomweb/bulkdata/a"},
		{"bulkdata/b?x=1#frag", "https://pacs.example.invalid:8443/dicomweb/bulkdata/b?x=1"},
		{"https://PACS.example.invalid:8443/other/c", "https://PACS.example.invalid:8443/other/c"},
		{"https://other.example.invalid:8443/bulkdata/d", ""},
		{"http://pacs.example.invalid:8443/bulkdata/e", ""},
		{"https://pacs.example.invalid/bulkdata/f", ""},
		{"//other.example.invalid/bulkdata/g", ""},
		{"https://user@pacs.example.invalid:8443/bulkdata/h", ""},
		{"file:///etc/hosts", ""},
		{"", ""},
	}
	for _, test := range tests {
		t.Run(test.uri, func(t *testing.T) {
			got, err := endpoint.BulkDataURL(test.uri)
			if test.want == "" {
				var webErr *Error
				if !errors.As(err, &webErr) || webErr.Kind != ErrorKindInvalidEndpoint {
					t.Fatalf("BulkDataURL = %v, %v; want invalid endpoint", got, err)
				}
				return
			}
			if err != nil || got.String() != test.want {
				t.Fatalf("BulkDataURL = %v, %v; want %s", got, err, test.want)
			}
		})
	}
}

func TestResolveBulkDataThroughClientAgainstServer(t *testing.T) {
	client, bulkRequests := newClientBulkServer(t)
	ref := InstanceRef{StudyInstanceUID: dicomtest.TestStudyInstanceUID, SeriesInstanceUID: dicomtest.TestSeriesInstanceUID, SOPInstanceUID: dicomtest.TestSOPInstanceUID}
	metadataObject := func(t *testing.T) *object.Object {
		t.Helper()
		datasets, err := client.InstanceMetadata(context.Background(), ref)
		if err != nil || len(datasets) != 1 {
			t.Fatalf("InstanceMetadata = %d, %v", len(datasets), err)
		}
		encoded, err := json.Marshal(datasets[0])
		if err != nil {
			t.Fatal(err)
		}
		obj, err := dicomjson.Unmarshal(encoded, std.Dictionary)
		if err != nil {
			t.Fatalf("Unmarshal metadata: %v", err)
		}
		return obj
	}

	obj := metadataObject(t)
	resolvers := object.BulkDataURIResolvers{"": client, "http": client, "https": client}
	if err := object.ResolveBulkData(context.Background(), obj, resolvers, object.ResolveBulkDataOptions{}); err != nil {
		t.Fatalf("ResolveBulkData: %v", err)
	}
	if raw, ok := obj.GetRaw(core.NewTag(0x0042, 0x0011)); !ok || string(raw) != "PIXELBYTES" {
		t.Fatalf("resolved top-level value = %q, %v", raw, ok)
	}
	items, _ := obj.GetSequence(core.NewTag(0x0008, 0x1111))
	if raw, ok := items[0].GetRaw(core.NewTag(0x0028, 0x3006)); !ok || string(raw) != "NESTED" {
		t.Fatalf("resolved nested value = %q, %v", raw, ok)
	}

	// The per-value limit stops a value larger than the caller accepts.
	before := bulkRequests.Load()
	err := object.ResolveBulkData(context.Background(), metadataObject(t), resolvers, object.ResolveBulkDataOptions{MaxValueBytes: 4})
	if !errors.Is(err, object.ErrBulkDataTooLarge) {
		t.Fatalf("limited ResolveBulkData error = %v", err)
	}
	// The client's own part limit also bounds resolution.
	limited := client
	limited.Options.ResponseLimits.MaxPartBytes = 3
	if _, err := limited.ResolveBulkDataURI(context.Background(), "/dicomweb/bulkdata/pixel/1", 0); !errors.Is(err, object.ErrBulkDataTooLarge) {
		t.Fatalf("client part limit error = %v", err)
	}
	if bulkRequests.Load() == before {
		t.Fatal("limit tests did not reach the server")
	}
	// A cross-origin reference never leaves the client.
	before = bulkRequests.Load()
	if _, err := client.ResolveBulkDataURI(context.Background(), "http://elsewhere.invalid/dicomweb/bulkdata/pixel/1", 0); err == nil {
		t.Fatal("cross-origin BulkDataURI resolved")
	}
	if bulkRequests.Load() != before {
		t.Fatal("cross-origin BulkDataURI reached the server")
	}
}

func stowMetadataTestObject(t *testing.T, bulkURI string) *object.Object {
	t.Helper()
	obj := object.New(std.Dictionary)
	for tag, value := range map[core.Tag]string{
		core.NewTag(0x0008, 0x0016): dicomtest.TestSOPClassUID,
		core.NewTag(0x0008, 0x0018): dicomtest.TestSOPInstanceUID,
		core.NewTag(0x0020, 0x000D): dicomtest.TestStudyInstanceUID,
		core.NewTag(0x0020, 0x000E): dicomtest.TestSeriesInstanceUID,
	} {
		obj.Put(core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRUI}, Value: core.StringValue{value}})
	}
	if bulkURI != "" {
		obj.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB}, Value: core.BulkDataValue{URI: bulkURI}})
	}
	return obj
}

func TestClientStoreMetadataAgainstServer(t *testing.T) {
	for _, mediaType := range []MetadataMediaType{"", MetadataMediaTypeDICOMJSON, MetadataMediaTypeDICOMXML} {
		t.Run(string(mediaType), func(t *testing.T) {
			backend := completeServerTestBackend()
			var stored atomic.Int32
			backend.store = func(_ context.Context, request StoreRequest) StoreOutcome {
				data, err := io.ReadAll(request.Reader)
				if err != nil {
					return StoreOutcome{Status: StoreStatusFailed, Err: err}
				}
				if len(data) < 132 || string(data[128:132]) != "DICM" || !bytes.Contains(data, []byte{9, 8, 7, 6}) {
					return StoreOutcome{Status: StoreStatusFailed, FailureReason: 0xA700}
				}
				stored.Add(1)
				return StoreOutcome{Status: StoreStatusStored}
			}
			_, httpServer := newOpenTestServer(t, backend, nil)
			client := Client{Endpoint: Endpoint{BaseURL: httpServer.URL}}
			request := StoreMetadataRequest{
				MediaType: mediaType,
				Instances: []*object.Object{stowMetadataTestObject(t, "bulk-1")},
				BulkData:  []StoreBulkData{{URI: "bulk-1", Reader: bytes.NewReader([]byte{9, 8, 7, 6})}},
			}
			result, err := client.StoreMetadataToStudy(context.Background(), dicomtest.TestStudyInstanceUID, request)
			if err != nil {
				t.Fatalf("StoreMetadata: %v", err)
			}
			if stored.Load() != 1 || len(result.Stored) != 1 || result.Stored[0].SOPInstanceUID != dicomtest.TestSOPInstanceUID || len(result.Failed) != 0 {
				t.Fatalf("result = %+v stored = %d", result, stored.Load())
			}
		})
	}
}

func TestClientStoreMetadataValidatesBulkDataBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer httpServer.Close()
	client := Client{Endpoint: Endpoint{BaseURL: httpServer.URL}}
	withMeta := stowMetadataTestObject(t, "")
	withMeta.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0002, 0x0010), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.1.2.1"}})
	tests := []struct {
		name    string
		request StoreMetadataRequest
	}{
		{"missing bulk data", StoreMetadataRequest{Instances: []*object.Object{stowMetadataTestObject(t, "bulk-1")}}},
		{"unreferenced bulk data", StoreMetadataRequest{Instances: []*object.Object{stowMetadataTestObject(t, "")}, BulkData: []StoreBulkData{{URI: "bulk-1", Data: []byte{1}}}}},
		{"duplicate bulk data", StoreMetadataRequest{Instances: []*object.Object{stowMetadataTestObject(t, "bulk-1")}, BulkData: []StoreBulkData{{URI: "bulk-1", Data: []byte{1}}, {URI: "bulk-1", Data: []byte{2}}}}},
		{"empty bulk data", StoreMetadataRequest{Instances: []*object.Object{stowMetadataTestObject(t, "bulk-1")}, BulkData: []StoreBulkData{{URI: "bulk-1"}}}},
		{"file meta", StoreMetadataRequest{Instances: []*object.Object{withMeta}}},
		{"media type", StoreMetadataRequest{MediaType: "application/json", Instances: []*object.Object{stowMetadataTestObject(t, "")}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := client.StoreMetadata(context.Background(), test.request); err == nil {
				t.Fatal("invalid request was accepted")
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid requests sent %d HTTP requests", requests.Load())
	}
}

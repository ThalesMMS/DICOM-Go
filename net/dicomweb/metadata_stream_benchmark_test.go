package dicomweb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
)

// The legacy cell preserves the pre-#1298 production pipeline (structural
// validation, encoding/json Marshal, and VR-aware full Unmarshal). The current
// cell keeps all those VR constraints and writes directly to staged output.
func BenchmarkMetadataJSON10K(b *testing.B) {
	server := &Server{limits: DefaultServerLimits()}
	datasets := benchmarkQIDODatasets(10_000)
	for _, mode := range []string{"legacy-full-reparse", "current-validated-writer"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(10_000, "datasets/op")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, dataset := range datasets {
					if mode == "legacy-full-reparse" {
						if _, err := legacyResponseEncodeForTest(server, dataset); err != nil {
							b.Fatal(err)
						}
					} else {
						if err := server.writeResponseDataset(context.Background(), io.Discard, dataset); err != nil {
							b.Fatal(err)
						}
					}
				}
			}
		})
	}
}

const benchmarkMetadataStudyUID = "1.2.826.0.1.3680043.9.7433.1298"

// One CT study/series with distinct instances; datasets are generated on demand
// rather than retained as a study-sized slice. This is synthetic metadata, not
// a live PACS fixture. Pixel bytes are represented by service-local bulk URIs.
func benchmarkStudyInstanceMetadata(index int) Dataset {
	sopUID := benchmarkMetadataStudyUID + ".2." + strconv.Itoa(index+1)
	return Dataset{
		"00080005": {VR: "CS", Value: []any{"ISO_IR 192"}},
		"00080016": {VR: "UI", Value: []any{"1.2.840.10008.5.1.4.1.1.2"}},
		"00080018": {VR: "UI", Value: []any{sopUID}},
		"00080020": {VR: "DA", Value: []any{"20261009"}},
		"00080030": {VR: "TM", Value: []any{"120000"}},
		"00080060": {VR: "CS", Value: []any{"CT"}},
		"00100010": {VR: "PN", Value: []any{dicomjson.PersonNameComponents{Alphabetic: "Synthetic^Example"}}},
		"00100020": {VR: "LO", Value: []any{"SYNTHETIC-1298"}},
		"00180050": {VR: "DS", Value: []any{"1"}},
		"0020000D": {VR: "UI", Value: []any{benchmarkMetadataStudyUID}},
		"0020000E": {VR: "UI", Value: []any{benchmarkMetadataStudyUID + ".1"}},
		"00200013": {VR: "IS", Value: []any{strconv.Itoa(index + 1)}},
		"00200032": {VR: "DS", Value: []any{"0", "0", strconv.Itoa(index)}},
		"00200037": {VR: "DS", Value: []any{"1", "0", "0", "0", "1", "0"}},
		"00280002": {VR: "US", Value: []any{1}},
		"00280004": {VR: "CS", Value: []any{"MONOCHROME2"}},
		"00280010": {VR: "US", Value: []any{512}},
		"00280011": {VR: "US", Value: []any{512}},
		"00280030": {VR: "DS", Value: []any{"0.625", "0.625"}},
		"00280100": {VR: "US", Value: []any{16}},
		"00280101": {VR: "US", Value: []any{12}},
		"00280102": {VR: "US", Value: []any{11}},
		"00280103": {VR: "US", Value: []any{0}},
		"00281052": {VR: "DS", Value: []any{"-1024"}},
		"00281053": {VR: "DS", Value: []any{"1"}},
		"7FE00010": {VR: "OW", BulkDataURI: "/bulkdata/" + sopUID},
	}
}

func newStudyMetadataBenchmarkServer(directory string) (*Server, error) {
	backend := completeServerTestBackend()
	backend.metadata = func(ctx context.Context, request MetadataRequest, yield func(Dataset) error) error {
		if request.Level != MetadataLevelStudy || request.StudyInstanceUID != benchmarkMetadataStudyUID || request.SeriesInstanceUID != "" || request.SOPInstanceUID != "" {
			return fmt.Errorf("unexpected study metadata request")
		}
		for index := 0; index < 10_000; index++ {
			if err := yield(benchmarkStudyInstanceMetadata(index)); err != nil {
				return err
			}
		}
		return nil
	}
	return NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: directory})
}

type metadataQualificationWriter struct {
	header http.Header
	out    io.Writer
	status int
	bytes  int64
}

func (w *metadataQualificationWriter) Header() http.Header { return w.header }
func (w *metadataQualificationWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *metadataQualificationWriter) Write(data []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	n, err := w.out.Write(data)
	w.bytes += int64(n)
	return n, err
}

func runStudyMetadataSpoolForBenchmark(server *Server, out io.Writer, legacy bool) (*metadataQualificationWriter, int, error) {
	request := httptest.NewRequest(http.MethodGet, "/studies/"+benchmarkMetadataStudyUID+"/metadata", nil)
	request.Header.Set("Accept", "application/dicom+json")
	writer := &metadataQualificationWriter{header: make(http.Header), out: out}
	response := &serverResponseWriter{ResponseWriter: writer, remaining: server.limits.MaxResponseBytes, limit: server.limits.MaxResponseBytes}
	var count int
	var err error
	if legacy {
		count, err = legacyStudyMetadataSpoolForBenchmark(server, response, request)
	} else {
		count, err = server.serveMetadata(response, request)
	}
	return writer, count, err
}

// This JSON-only test route preserves the pre-#1298 metadata handler's spool
// policy. It differs from serveMetadata only at JSON Append: structural checks,
// Marshal and full Unmarshal followed by Append(encoded), as on the old path.
// Both cells exclude the common serveCore routing/authentication middleware.
func legacyStudyMetadataSpoolForBenchmark(server *Server, writer *serverResponseWriter, request *http.Request) (int, error) {
	backend, ok := server.options.Backend.(MetadataBackend)
	if !ok {
		return 0, newServerStatusError(http.StatusNotImplemented, "not_implemented", ErrUnsupported)
	}
	representation, ok := selectMetadataRepresentation(requestAcceptHeader(request))
	if !ok || representation != metadataRepresentationJSON {
		return 0, newServerStatusError(http.StatusNotAcceptable, "not_acceptable", ErrUnsupported)
	}
	writer.Header().Set("Vary", "Accept")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	metadataRequest, err := server.metadataRequest(request.URL.Path)
	if err != nil {
		return 0, err
	}
	spool, err := newDatasetResponseSpool(server.options.SpoolDirectory, server.limits.MaxResponseBytes, representation)
	if err != nil {
		return 0, err
	}
	spoolClosed := false
	defer func() {
		if !spoolClosed {
			_ = spool.Close()
		}
	}()
	err = safeMetadata(backend, request.Context(), metadataRequest, func(dataset Dataset) error {
		if err := request.Context().Err(); err != nil {
			return err
		}
		if spool.count >= server.limits.MaxResults {
			return ErrResourceLimit
		}
		encoded, err := legacyResponseEncodeForTest(server, dataset)
		if err != nil {
			return err
		}
		if err := spool.json.Append(encoded); err != nil {
			return err
		}
		spool.count++
		return nil
	})
	if err != nil {
		return 0, classifyBackendError(err)
	}
	if err := spool.Finalize(request.Context(), server, false); err != nil {
		return 0, err
	}
	writer.Header().Set("Content-Type", spool.contentType)
	_, err = copyBounded(request.Context(), writer, spool.Reader(), server.limits.MaxResponseBytes)
	closeErr := spool.Close()
	spoolClosed = true
	if err != nil {
		return spool.count, err
	}
	return spool.count, closeErr
}

func BenchmarkStudyMetadataSpool10K(b *testing.B) {
	for _, mode := range []string{"legacy-prepared-spool", "current-prepared-spool"} {
		b.Run(mode, func(b *testing.B) {
			server, err := newStudyMetadataBenchmarkServer(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				writer, count, err := runStudyMetadataSpoolForBenchmark(server, io.Discard, mode == "legacy-prepared-spool")
				if err != nil || count != 10_000 || writer.status != http.StatusOK || writer.bytes == 0 {
					b.Fatalf("study spool count%d status%d bytes%d err%v", count, writer.status, writer.bytes, err)
				}
			}
			b.StopTimer()
			entries, err := os.ReadDir(server.options.SpoolDirectory)
			if err != nil || len(entries) != 0 {
				b.Fatalf("study spool cleanup %v/%v", entries, err)
			}
		})
	}
}

func TestStudyMetadataSpool10KOutputEquivalence(t *testing.T) {
	for index := 0; index < 10_000; index++ {
		dataset := benchmarkStudyInstanceMetadata(index)
		if dicomjson.ElementString(dataset, "0020000D") != benchmarkMetadataStudyUID || dicomjson.ElementString(dataset, "0020000E") != benchmarkMetadataStudyUID+".1" || dicomjson.ElementString(dataset, "00080018") != benchmarkMetadataStudyUID+".2."+strconv.Itoa(index+1) {
			t.Fatalf("instance%d must belong to the common study/series and have its distinct SOP UID", index)
		}
	}
	server, err := newStudyMetadataBenchmarkServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var expectedDigest []byte
	var expectedBytes int64
	for _, mode := range []string{"legacy", "current", "full-handler"} {
		digest := sha256.New()
		var writer *metadataQualificationWriter
		if mode == "full-handler" {
			writer = &metadataQualificationWriter{header: make(http.Header), out: digest}
			request := httptest.NewRequest(http.MethodGet, "/studies/"+benchmarkMetadataStudyUID+"/metadata", nil)
			request.Header.Set("Accept", "application/dicom+json")
			server.ServeHTTP(writer, request)
		} else {
			var count int
			writer, count, err = runStudyMetadataSpoolForBenchmark(server, digest, mode == "legacy")
			if err != nil || count != 10_000 {
				t.Fatalf("mode%s count%d err%v", mode, count, err)
			}
		}
		if writer.status != http.StatusOK || writer.header.Get("Content-Type") != "application/dicom+json" || writer.header.Get("Vary") != "Accept" || writer.header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("mode%s status%d headers%v", mode, writer.status, writer.header)
		}
		if mode == "legacy" {
			expectedDigest, expectedBytes = digest.Sum(nil), writer.bytes
		} else if !bytes.Equal(expectedDigest, digest.Sum(nil)) || expectedBytes != writer.bytes {
			t.Fatalf("mode%s full study array byte/hash mismatch", mode)
		}
		entries, err := os.ReadDir(server.options.SpoolDirectory)
		if err != nil || len(entries) != 0 {
			t.Fatalf("mode%s spool cleanup %v/%v", mode, entries, err)
		}
	}
	t.Logf("study=%s instances=10000 bytes=%d full_array_sha256=%x", benchmarkMetadataStudyUID, expectedBytes, expectedDigest)
}

// Opt-in RSS measurement of the same real preparation/spool path, with neither
// a resident fixture array nor a ResponseRecorder retaining the HTTP body.
func TestStudyMetadataSpoolRSSQualification(t *testing.T) {
	mode := os.Getenv("DICOMGO_STUDY_JSON_RSS")
	if mode == "" {
		t.Skip("opt-in study metadata process RSS qualification")
	}
	if mode != "legacy-prepared-spool" && mode != "current-prepared-spool" {
		t.Fatal("unknown study qualification mode")
	}
	server, err := newStudyMetadataBenchmarkServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	writer, count, err := runStudyMetadataSpoolForBenchmark(server, digest, mode == "legacy-prepared-spool")
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	if err != nil || count != 10_000 || writer.status != http.StatusOK {
		t.Fatalf("study mode%s count%d status%d err%v", mode, count, writer.status, err)
	}
	entries, err := os.ReadDir(server.options.SpoolDirectory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("study spool cleanup %v/%v", entries, err)
	}
	t.Logf("mode=%s study=%s instances=%d bytes=%d elapsed_ns=%d total_alloc_bytes=%d mallocs=%d heap_alloc_after_bytes=%d full_array_sha256=%x", mode, benchmarkMetadataStudyUID, count, writer.bytes, elapsed.Nanoseconds(), after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs, after.HeapAlloc, digest.Sum(nil))
}
func BenchmarkMetadataJSONClient10K(b *testing.B) {
	datasets := benchmarkQIDODatasets(10_000)
	body, err := json.Marshal(datasets)
	if err != nil {
		b.Fatal(err)
	}
	// The resident validation control applies the public represented-dataset
	// validator to the same admitted fixture. Unlike the raw legacy decoder it
	// checks VR rules. ValidateDatasetContext also emits to io.Discard, whereas
	// the streaming decoder's internal validation suppresses emission; this is
	// a policy-cost control, not an isolated measurement of streaming overhead.
	for _, mode := range []string{"legacy-resident-array", "resident-array-vr-validation", "current-stream-callback"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			b.ReportMetric(10_000, "datasets/op")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if mode != "current-stream-callback" {
					ds, err := datasetsFromDICOMJSON(body)
					if err != nil || len(ds) != 10_000 {
						b.Fatalf("resident count%d err%v", len(ds), err)
					}
					if mode == "resident-array-vr-validation" {
						for _, dataset := range ds {
							if err := dicomjson.ValidateDatasetContext(context.Background(), dataset, dicomjson.DatasetOptions{Limits: dicomjson.Limits{MaxJSONBytes: int64(len(body))}}); err != nil {
								b.Fatal(err)
							}
						}
					}
				} else {
					count := 0
					err := dicomjson.NewDecoder(bytes.NewReader(body), std.Dictionary, dicomjson.UnmarshalOptions{Limits: dicomjson.Limits{MaxJSONBytes: int64(len(body))}}).DecodeDatasetsContext(context.Background(), func(Dataset) error { count++; return nil })
					if err != nil || count != 10_000 {
						b.Fatalf("current count%d err%v", count, err)
					}
				}
			}
		})
	}
}
func TestMetadataJSON10KOutputEquivalence(t *testing.T) {
	server := &Server{limits: DefaultServerLimits()}
	datasets := benchmarkQIDODatasets(10_000)
	legacy := sha256.New()
	current := sha256.New()
	for _, dataset := range datasets {
		data, err := legacyResponseEncodeForTest(server, dataset)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = legacy.Write(data)
		if err := server.writeResponseDataset(context.Background(), current, dataset); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(legacy.Sum(nil), current.Sum(nil)) {
		t.Fatal("10k output hash mismatch")
	}
	t.Logf("10k exact dataset stream sha256=%s", fmt.Sprintf("%x", current.Sum(nil)))
}

// TestMetadataJSONRSSQualification is opt-in process-level qualification. Run
// each mode in a separate invocation with /usr/bin/time -l; its maximum RSS is
// a process high-water mark, not Go heap retention or a universal memory bound.
func TestMetadataJSONRSSQualification(t *testing.T) {
	mode := os.Getenv("DICOMGO_JSON_STREAM_RSS")
	if mode == "" {
		t.Skip("opt-in process RSS qualification")
	}
	if mode != "server-legacy" && mode != "server-current" && mode != "client-legacy" && mode != "client-current" {
		t.Fatal("unknown qualification mode")
	}
	dataset := benchmarkQIDODatasets(1)[0]
	server := &Server{limits: DefaultServerLimits()}
	digest := sha256.New()
	count := 0
	var body *os.File
	if strings.HasPrefix(mode, "client") {
		var err error
		body, err = os.CreateTemp(t.TempDir(), "metadata-input-*")
		if err != nil {
			t.Fatal(err)
		}
		defer body.Close()
		if _, err := body.Write([]byte{'['}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 10_000; i++ {
			if i > 0 {
				if _, err := body.Write([]byte{','}); err != nil {
					t.Fatal(err)
				}
			}
			encoded, err := json.Marshal(dataset)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := body.Write(encoded); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := body.Write([]byte{']'}); err != nil {
			t.Fatal(err)
		}
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	switch mode {
	case "server-legacy":
		for i := 0; i < 10_000; i++ {
			data, err := legacyResponseEncodeForTest(server, dataset)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = digest.Write(data)
			count++
		}
	case "server-current":
		for i := 0; i < 10_000; i++ {
			if err := server.writeResponseDataset(context.Background(), digest, dataset); err != nil {
				t.Fatal(err)
			}
			count++
		}
	case "client-legacy":
		data, err := io.ReadAll(body)
		if err != nil {
			t.Fatal(err)
		}
		datasets, err := datasetsFromDICOMJSON(data)
		if err != nil {
			t.Fatal(err)
		}
		for _, ds := range datasets {
			encoded, err := json.Marshal(ds)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = digest.Write(encoded)
			count++
		}
		runtime.KeepAlive(datasets)
	case "client-current":
		err := dicomjson.NewDecoder(body, std.Dictionary, dicomjson.UnmarshalOptions{Limits: dicomjson.Limits{MaxJSONBytes: 64 << 20}}).DecodeDatasetsContext(context.Background(), func(ds Dataset) error {
			encoded, err := json.Marshal(ds)
			if err != nil {
				return err
			}
			_, _ = digest.Write(encoded)
			count++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	if count != 10_000 {
		t.Fatalf("count%d", count)
	}
	t.Logf("mode=%s datasets=%d elapsed_ns=%d total_alloc_bytes=%d mallocs=%d heap_alloc_after_bytes=%d sha256=%x", mode, count, elapsed.Nanoseconds(), after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs, after.HeapAlloc, digest.Sum(nil))
}

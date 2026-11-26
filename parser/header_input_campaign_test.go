package parser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

// Attach only AFTER NewReader has established the concrete os.File gate and
// physical extent. Each Read below delegates exactly once to that same file.
// No synthetic read-size estimate or syscall claim is used in the campaign.
type headerInputFileCounter struct {
	*os.File
	Calls, Bytes, Seeks int64
	position            int64
	forbidden           []headerInputRange
}

func (f *headerInputFileCounter) Read(p []byte) (int, error) {
	for _, interval := range f.forbidden {
		if f.position < interval.end && f.position+int64(len(p)) > interval.start {
			return 0, fmt.Errorf("read crosses deferred payload: %d/%d", f.position, len(p))
		}
	}
	f.Calls++
	n, err := f.File.Read(p)
	f.Bytes += int64(n)
	f.position += int64(n)
	return n, err
}

func (f *headerInputFileCounter) Seek(offset int64, whence int) (int64, error) {
	f.Seeks++
	position, err := f.File.Seek(offset, whence)
	if err == nil {
		f.position = position
	}
	return position, err
}

func headerInputCountedFile(f *os.File, syntax transfer.Syntax, opts ReaderOptions, grouped bool) (*Reader, *headerInputFileCounter, error) {
	r := NewReader(f, syntax, opts)
	if !r.headerInputEnabled || r.slurpEligible {
		return nil, nil, errors.New("fixture did not select eligible streaming header route")
	}
	position, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, nil, err
	}
	counted := &headerInputFileCounter{File: f, position: position}
	r.counter.r = counted
	if !grouped {
		r.headerInputEnabled = false
	} // Original field I/O oracle only.
	return r, counted, nil
}

// The measurement uses exactly the 1024 LO/2-byte-value fixture already used
// by BenchmarkHeaderInputFile, with MaxElementBytes32 to exclude whole slurp.
type headerInputCampaignSample struct {
	Grouped        bool  `json:"grouped"`
	ElapsedNS      int64 `json:"elapsed_ns"`
	ReadCalls      int64 `json:"read_calls"`
	Bytes          int64 `json:"bytes_read"`
	Seeks          int64 `json:"seeks"`
	LogicalOffset  int64 `json:"logical_offset"`
	PhysicalOffset int64 `json:"physical_offset"`
}

func headerInput1295Pass(path string, grouped bool) (core.DataSet, headerInputCampaignSample, error) {
	f, err := os.Open(path)
	if err != nil {
		return core.DataSet{}, headerInputCampaignSample{}, err
	}
	defer f.Close()
	r, c, err := headerInputCountedFile(f, transfer.ExplicitVRLittleEndian, ReaderOptions{MaxElementBytes: 32}, grouped)
	if err != nil {
		return core.DataSet{}, headerInputCampaignSample{}, err
	}
	start := time.Now()
	ds, readErr := r.ReadDataSet()
	elapsed := time.Since(start)
	physical, seekErr := f.Seek(0, io.SeekCurrent) // Introspection is outside counters/timer.
	return ds, headerInputCampaignSample{grouped, elapsed.Nanoseconds(), c.Calls, c.Bytes, c.Seeks, r.Position(), physical}, errors.Join(readErr, seekErr)
}

func TestHeaderInput1295Campaign(t *testing.T) {
	dir := os.Getenv("DICOMGO_HEADER_CAMPAIGN_DIR")
	if dir == "" {
		t.Skip("explicit paired reader-call/time campaign only")
	}
	if !filepath.IsAbs(dir) || runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		t.Fatal("requires Windows/amd64 and absolute artifact directory")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	wire := headerInputMetadataFixture()
	path := filepath.Join(t.TempDir(), "metadata.dcm")
	if err := os.WriteFile(path, wire, 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(wire)
	plan := struct {
		Fixture, InputSHA256        string
		InputBytes                  int
		WorkerOrder                 [3][2]bool
		GOOS, GOARCH, GoVersion     string
		GOMAXPROCS, MaxElementBytes int
		Metric                      string
	}{"1024 ExplicitVRLittleEndian LO elements, each value AB, original BenchmarkHeaderInputFile fixture", hex.EncodeToString(hash[:]), len(wire), [3][2]bool{{false, true}, {true, false}, {false, true}}, runtime.GOOS, runtime.GOARCH, runtime.Version(), runtime.GOMAXPROCS(0), 32, "Read calls delegated one-for-one to os.File.Read; not kernel syscalls; API parse wall excluding open/close and setup"}
	writeHeaderInputCampaignJSON(t, filepath.Join(dir, "plan.json"), plan)
	// Untimed matched warmups also establish the complete dataset oracle.
	legacy, _, err := headerInput1295Pass(path, false)
	if err != nil {
		t.Fatal(err)
	}
	current, _, err := headerInput1295Pass(path, true)
	if err != nil || !reflect.DeepEqual(current, legacy) {
		t.Fatal("warmup changed dataset", err)
	}
	var samples []headerInputCampaignSample
	var fields, grouped []int64
	callReduction := true
	for _, order := range plan.WorkerOrder {
		var pair [2]headerInputCampaignSample
		for _, batch := range order {
			ds, sample, err := headerInput1295Pass(path, batch)
			if err != nil || !reflect.DeepEqual(ds, legacy) || sample.LogicalOffset != int64(len(wire)) || sample.PhysicalOffset != int64(len(wire)) || sample.Bytes != int64(len(wire)) {
				t.Fatalf("parse/cursor mismatch: %+v %v", sample, err)
			}
			samples = append(samples, sample)
			if batch {
				grouped = append(grouped, sample.ElapsedNS)
				pair[1] = sample
			} else {
				fields = append(fields, sample.ElapsedNS)
				pair[0] = sample
			}
		}
		callReduction = callReduction && pair[1].ReadCalls*5 <= pair[0].ReadCalls*4
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i] < fields[j] })
	sort.Slice(grouped, func(i, j int) bool { return grouped[i] < grouped[j] })
	noRegression := float64(grouped[1]) <= 1.1*float64(fields[1])
	writeHeaderInputCampaignJSON(t, filepath.Join(dir, "campaign.json"), struct {
		Plan                                                            any
		Samples                                                         []headerInputCampaignSample
		FieldsMedianNS, GroupedMedianNS                                 int64
		ReadCallsReducedAtLeast20Percent, NoTimeRegressionOver10Percent bool
	}{plan, samples, fields[1], grouped[1], callReduction, noRegression})
	if !callReduction || !noRegression {
		t.Error("finite campaign acceptance failed; all six samples retained")
	}
}

func writeHeaderInputCampaignJSON(t testing.TB, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestHeaderInputCountedRegularFileKeepsEligibilityAndPayloadContracts(t *testing.T) {
	for _, encapsulated := range []bool{false, true} {
		wire, blocked := headerInputPayloadFixture(encapsulated)
		for _, deferred := range []bool{false, true} {
			for _, limited := range []bool{false, true} {
				t.Run(fmt.Sprintf("encapsulated=%v/defer=%v/limit=%v", encapsulated, deferred, limited), func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "payload.dcm")
					if err := os.WriteFile(path, wire, 0600); err != nil {
						t.Fatal(err)
					}
					syntax := transfer.ExplicitVRLittleEndian
					if encapsulated {
						syntax = transfer.JPEGBaseline
					}
					opts := ReaderOptions{SkipPixelData: !deferred, DeferPixelData: deferred, BaseOffset: 37}
					if limited {
						opts.MaxTotalBytes = int64(len(wire)) + 36
					}
					var baseline core.DataSet
					var baselineError string
					var baselinePosition int64
					for _, batch := range []bool{false, true} {
						f, err := os.Open(path)
						if err != nil {
							t.Fatal(err)
						}
						r, c, err := headerInputCountedFile(f, syntax, opts, batch)
						if err != nil {
							f.Close()
							t.Fatal(err)
						}
						c.forbidden = []headerInputRange{blocked}
						ds, err := r.ReadDataSet()
						position, _ := f.Seek(0, io.SeekCurrent)
						if limited && !errors.Is(err, ErrMaxTotalBytesExceeded) {
							t.Fatalf("budget identity: %v", err)
						}
						if !limited && err != nil {
							t.Fatal(err)
						}
						if r.Position() != position+37 || c.Calls == 0 {
							t.Fatal("counter/cursor contract")
						}
						if !batch {
							baseline, baselineError, baselinePosition = ds, headerInputError(err), position
						} else if !reflect.DeepEqual(ds, baseline) || headerInputError(err) != baselineError || position != baselinePosition {
							t.Fatal("grouping changed output/error/offset")
						}
						if !limited {
							location, ok := r.ValueLocation(core.TagPixelData)
							if !ok || location.ValueOffset != 59 {
								t.Fatal(location)
							}
							if deferred {
								c.forbidden = nil
								var copied counterWriter
								n, err := r.CopyElementValueAt(location, &copied)
								after, _ := f.Seek(0, io.SeekCurrent)
								if err != nil || n != location.Length || copied.n != n || after != position || r.Position() != position+37 {
									t.Fatalf("defer replay/cursor: %d %v", n, err)
								}
							}
						}
						if err := f.Close(); err != nil {
							t.Fatal(err)
						}
					}
				})
			}
		}
	}
}

type counterWriter struct{ n int64 }

func (w *counterWriter) Write(p []byte) (int, error) { w.n += int64(len(p)); return len(p), nil }

func TestHeaderInputCountedFixtureMatchesBenchmarkAndExactCallCounts(t *testing.T) {
	wire := headerInputMetadataFixture()
	path := filepath.Join(t.TempDir(), "metadata.dcm")
	if err := os.WriteFile(path, wire, 0600); err != nil {
		t.Fatal(err)
	}
	var oracle core.DataSet
	for _, batch := range []bool{false, true} {
		ds, sample, err := headerInput1295Pass(path, batch)
		wantCalls := int64(5121)
		if batch {
			wantCalls = 2049
		}
		if err != nil || len(ds.Elements) != 1024 || sample.ReadCalls != wantCalls || sample.Bytes != 10240 || sample.LogicalOffset != 10240 || sample.PhysicalOffset != 10240 || sample.Seeks != 0 {
			t.Fatalf("sample=%+v err=%v elements=%d", sample, err, len(ds.Elements))
		}
		if batch && !reflect.DeepEqual(ds, oracle) {
			t.Fatal("counting changed dataset")
		}
		oracle = ds
	}
}

func TestHeaderInputRegularFileRestoresCursorAfterInvalidVR(t *testing.T) {
	wire := headerInputManual(transfer.ExplicitVRLittleEndian, core.NewTag(0x0008, 0x103e), "LO", 0)
	copy(wire[4:6], "ZZ")
	for _, tc := range []struct {
		name          string
		size          int
		budget, fence int64
		reads, seeks  int64
	}{
		{name: "complete header", size: 8, reads: 8, seeks: 1},
		{name: "short file", size: 7, reads: 6},
		{name: "short budget", size: 8, budget: 7, reads: 6},
		{name: "short container", size: 8, fence: 7, reads: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "invalid-vr.dcm")
			if err := os.WriteFile(path, wire[:tc.size], 0600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			r, counted, err := headerInputCountedFile(file, transfer.ExplicitVRLittleEndian,
				ReaderOptions{MaxElementBytes: 32, MaxTotalBytes: tc.budget}, true)
			if err != nil {
				t.Fatal(err)
			}
			r.counter.fence = tc.fence
			_, err = r.readHeader()
			var parseErr *ParseError
			if !errors.As(err, &parseErr) || parseErr.Op != OpReadVR || parseErr.Offset != 4 || errors.Is(err, ErrContainerOverrun) {
				t.Fatalf("invalid VR error changed: %v", err)
			}
			physical, seekErr := file.Seek(0, io.SeekCurrent)
			if seekErr != nil || physical != 6 || r.Position() != 6 {
				t.Fatalf("invalid VR cursor: physical=%d logical=%d err=%v", physical, r.Position(), seekErr)
			}
			if counted.Calls != 1 || counted.Bytes != tc.reads || counted.Seeks != tc.seeks {
				t.Fatalf("reads=%d bytes=%d seeks=%d, want 1/%d/%d", counted.Calls, counted.Bytes, counted.Seeks, tc.reads, tc.seeks)
			}
		})
	}
}

type headerInputCancelAfterRead struct {
	*os.File
	cancel context.CancelFunc
}

func (f *headerInputCancelAfterRead) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	f.cancel()
	return n, err
}

func TestHeaderInputSelectiveFileCancellationKeepsInvalidVRCursor(t *testing.T) {
	wire := headerInputManual(transfer.ExplicitVRLittleEndian, core.NewTag(0x0008, 0x103e), "LO", 0)
	copy(wire[4:6], "ZZ")
	path := filepath.Join(t.TempDir(), "invalid-vr.dcm")
	if err := os.WriteFile(path, wire, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := NewSelectiveReader(ctx, file, transfer.ExplicitVRLittleEndian, ReaderOptions{}, SelectiveReaderOptions{
		Select: func(context.Context, validation.Path, core.ElementHeader, int64) (SelectiveDisposition, error) {
			return SelectiveMaterialize, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	source := r.counter.r.(*selectiveContextReadSeeker)
	source.source = &headerInputCancelAfterRead{File: file, cancel: cancel}
	_, err = r.readHeader()
	var parseErr *ParseError
	if !errors.As(err, &parseErr) || parseErr.Op != OpReadVR || errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation changed the invalid VR error: %v", err)
	}
	physical, seekErr := file.Seek(0, io.SeekCurrent)
	if seekErr != nil || physical != 6 || r.Position() != 6 || ctx.Err() == nil {
		t.Fatalf("canceled header left physical=%d logical=%d context=%v seek=%v", physical, r.Position(), ctx.Err(), seekErr)
	}
}

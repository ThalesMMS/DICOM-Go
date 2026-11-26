package pixeldata_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/parallel"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type transcode1294Sample struct {
	ElapsedNS      int64                     `json:"elapsed_ns"`
	AllocatedBytes uint64                    `json:"allocated_bytes"`
	OutputBytes    int64                     `json:"output_bytes"`
	OutputSHA256   string                    `json:"output_sha256"`
	Report         pixeldata.TranscodeReport `json:"report"`
}
type transcode1294Cell struct {
	Codec         string                `json:"codec"`
	Operation     string                `json:"operation"`
	Workers       int                   `json:"workers_requested"`
	Verify        bool                  `json:"extra_verification"`
	Durability    string                `json:"durability"`
	Samples       []transcode1294Sample `json:"samples"`
	MedianNS      int64                 `json:"median_ns"`
	P95ObservedNS int64                 `json:"p95_observed_ns"`
}

// TestTranscode1294Campaign is opt-in hardware work, run only in an exclusive
// CPU window. It uses a fixed non-PHI fixture and reports the API's wall time,
// including spool/readback/publication, independently from summed frame phases.
// Output hashing, fixture creation and cleanup are outside timed intervals.
//
//	GOMAXPROCS=12 DICOMGO_TRANSCODE_CAMPAIGN_DIR=/absolute/artifact/directory \
//	  ./pixeldata.test -test.run '^TestTranscode1294Campaign$' -test.v -test.timeout=10m
func TestTranscode1294Campaign(t *testing.T) {
	artifactDir := os.Getenv("DICOMGO_TRANSCODE_CAMPAIGN_DIR")
	if artifactDir == "" {
		t.Skip("explicit exclusive CPU campaign only")
	}
	if runtime.GOMAXPROCS(0) != 12 {
		t.Fatal("campaign requires explicit GOMAXPROCS=12")
	}
	if !filepath.IsAbs(artifactDir) {
		t.Fatal("artifact directory must be absolute")
	}
	if err := os.MkdirAll(artifactDir, 0700); err != nil {
		t.Fatal(err)
	}
	switch profile := os.Getenv("DICOMGO_TRANSCODE_CAMPAIGN_PROFILE"); profile {
	case "acceptance":
		runTranscode1294Acceptance(t, artifactDir)
		return
	case "", "broad": // Preserve the original optional 72-cell campaign.
	default:
		t.Fatalf("unknown campaign profile %q", profile)
	}
	options := transcode1294Options(t)
	dir := t.TempDir()
	nativeObject := streamTestSource(512, 512, 48, 1, 16)
	nativeObject.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7.3"}})
	nativeObject.Put(core.NewRawElement(core.NewTag(0x0028, 0x0101), core.VRUS, binary.LittleEndian.AppendUint16(nil, 12)))
	nativeObject.Put(core.NewRawElement(core.NewTag(0x0028, 0x0102), core.VRUS, binary.LittleEndian.AppendUint16(nil, 11)))
	native := writeStreamTestFile(t, dir, "native.dcm", &object.File{Dataset: nativeObject, TransferSyntax: transfer.ExplicitVRLittleEndian})
	nativeHash, nativeSize := transcode1294Hash(t, native)
	loaded, err := object.OpenFile(native)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()
	nativeFrames, err := pixeldata.ExtractNativeFramesView(loaded.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := pixeldata.ExtractMetadata(loaded.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	var cells []transcode1294Cell
	inputHashes := map[string]string{"native": nativeHash}
	for _, codec := range []transfer.Syntax{transfer.RLELossless, transfer.JPEGLSLossless, transfer.JPEGLosslessSV1, transfer.DeflatedImageFrameCompression} {
		encodedPath := filepath.Join(dir, "fixture-"+codec.UID+".dcm")
		if _, err := transcode1294Run(context.Background(), native, encodedPath, codec, options, 1, true, "setup"); err != nil {
			t.Fatal(err)
		}
		encodedHash, _ := transcode1294Hash(t, encodedPath)
		inputHashes[codec.UID] = encodedHash
		// A separate encode-only frame API profile removes file IO, structural
		// readback, verification and durability. The full native fixture is
		// resident here; it is not the streaming-memory profile.
		encodedReader, err := pixeldata.OpenFrameReader(context.Background(), encodedPath, pixeldata.FrameReaderOptions{Registry: options.DecoderRegistry})
		if err != nil {
			t.Fatal(err)
		}
		storedFrames := make([][]byte, 48)
		for i := range storedFrames {
			storedFrames[i], err = encodedReader.RawFrame(context.Background(), i)
			if err != nil {
				encodedReader.Close()
				t.Fatal(err)
			}
		}
		if err := encodedReader.Close(); err != nil {
			t.Fatal(err)
		}
		for _, workers := range []int{1, 12} {
			cell := transcode1294Cell{Codec: codec.UID, Operation: "encode-frame-api", Workers: workers, Durability: "not-applicable"}
			for rep := 0; rep < 3; rep++ {
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				encodedFrames := make([]pixeldata.EncodedFrame, 48)
				var once sync.Once
				var failed error
				workCtx, cancel := context.WithCancel(context.Background())
				start := time.Now()
				runErr := parallel.Do(workCtx, 48, workers, func(i int) {
					frame, err := options.EncoderRegistry.EncodeFrame(workCtx, codec.UID, nativeFrames.Data[i], metadata)
					if err != nil {
						once.Do(func() { failed = err; cancel() })
						return
					}
					encodedFrames[i] = frame
				})
				elapsed := time.Since(start)
				cancel()
				runtime.ReadMemStats(&after)
				if failed != nil || runErr != nil {
					t.Fatalf("frame API failed: %v / %v", failed, runErr)
				}
				var bytes int64
				hash := sha256.New()
				for i, frame := range encodedFrames {
					data := frame.Data
					stored := storedFrames[i]
					// Only DICOM's optional zero even-length pad differs from
					// the encoder API payload; codestream bytes must be exact.
					if len(stored) != len(data)+len(data)%2 || !equalTranscode1294Bytes(stored[:len(data)], data) || len(data)%2 != 0 && stored[len(data)] != 0 {
						t.Fatalf("frame API bytes differ at frame %d", i)
					}
					_, _ = hash.Write(stored)
					bytes += int64(len(data))
				}
				cell.Samples = append(cell.Samples, transcode1294Sample{ElapsedNS: elapsed.Nanoseconds(), AllocatedBytes: after.TotalAlloc - before.TotalAlloc, OutputBytes: bytes, OutputSHA256: hex.EncodeToString(hash.Sum(nil))})
			}
			cells = append(cells, finishTranscode1294Cell(cell))
		}
		for _, operation := range []string{"encode-path", "decode-path"} {
			input, target, expected := native, codec, encodedHash
			if operation == "decode-path" {
				input, target, expected = encodedPath, transfer.ExplicitVRLittleEndian, nativeHash
			}
			for _, workers := range []int{1, 12} {
				for _, verify := range []bool{true, false} {
					for _, durability := range []string{"durable", "atomic"} {
						if !transcode1294Supported(workers, verify, durability) {
							continue
						}
						cell := transcode1294Cell{Codec: codec.UID, Operation: operation, Workers: workers, Verify: verify, Durability: durability}
						for rep := 0; rep < 3; rep++ {
							output := filepath.Join(dir, fmt.Sprintf("out-%d.dcm", rep))
							runtime.GC()
							var before, after runtime.MemStats
							runtime.ReadMemStats(&before)
							start := time.Now()
							report, err := transcode1294Run(context.Background(), input, output, target, options, workers, verify, durability)
							elapsed := time.Since(start)
							runtime.ReadMemStats(&after)
							if err != nil {
								t.Fatal(err)
							}
							if parallel.InUse() != 0 {
								t.Fatal("helpers retained after transcode")
							}
							hash, size := transcode1294Hash(t, output)
							if hash != expected {
								t.Fatalf("%s %s output differs", codec.UID, operation)
							}
							cell.Samples = append(cell.Samples, transcode1294Sample{ElapsedNS: elapsed.Nanoseconds(), AllocatedBytes: after.TotalAlloc - before.TotalAlloc, OutputBytes: size, OutputSHA256: hash, Report: report})
							if err := os.Remove(output); err != nil {
								t.Fatal(err)
							}
						}
						cells = append(cells, finishTranscode1294Cell(cell))
					}
				}
			}
		}
	}
	artifact := struct {
		Schema           int                 `json:"schema"`
		GoVersion        string              `json:"go_version"`
		GOOS             string              `json:"goos"`
		GOARCH           string              `json:"goarch"`
		GOMAXPROCS       int                 `json:"gomaxprocs"`
		LogicalCPUs      int                 `json:"logical_cpus"`
		Fixture          string              `json:"fixture"`
		NativeInputBytes int64               `json:"native_input_bytes"`
		InputSHA256      map[string]string   `json:"input_sha256"`
		NoPHI            bool                `json:"no_phi"`
		Note             string              `json:"note"`
		Implementation   string              `json:"implementation"`
		Cells            []transcode1294Cell `json:"cells"`
	}{1, runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0), runtime.NumCPU(), "512x512x48 MONOCHROME2 unsigned 12 stored in 16; sample=(i*7+frame*131+row*3)%4096; synthetic, distinct from historical comparebench phantom", nativeSize, inputHashes, true, "3 repetitions per cell; input and registry warmed by setup; elapsed is host API wall, not a codec kernel or process launch; worker phase durations are summed; allocated_bytes is TotalAlloc delta, not peak/RSS; spool/readback and sync are explicit; decoder-only codecs are outside the four-encoder profile; first parallel sample may allocate additional codec workspace", transcode1294Implementation, cells}
	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(artifactDir, "campaign.json")
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d cells / %d repetitions to campaign.json", len(cells), 3*len(cells))
}

func finishTranscode1294Cell(cell transcode1294Cell) transcode1294Cell {
	values := make([]int64, len(cell.Samples))
	for i, s := range cell.Samples {
		values[i] = s.ElapsedNS
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	cell.MedianNS = values[len(values)/2]
	cell.P95ObservedNS = values[len(values)-1] // Three samples: empirical nearest-rank p95 is their maximum.
	return cell
}
func equalTranscode1294Bytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func transcode1294Hash(t testing.TB, path string) (string, int64) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, f)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hash.Sum(nil)), n
}

// Small, untimed qualification of the campaign's frame-only comparison. This
// runs in ordinary focused tests without invoking the opt-in hardware campaign.
func TestTranscode1294FrameAPIProfileParity(t *testing.T) {
	options := transcode1294Options(t)
	dataset := streamTestSource(16, 20, 3, 1, 16)
	dataset.Put(core.NewRawElement(core.NewTag(0x0028, 0x0101), core.VRUS, binary.LittleEndian.AppendUint16(nil, 12)))
	dataset.Put(core.NewRawElement(core.NewTag(0x0028, 0x0102), core.VRUS, binary.LittleEndian.AppendUint16(nil, 11)))
	frames, err := pixeldata.ExtractNativeFramesView(dataset)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := pixeldata.ExtractMetadata(dataset)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	native := writeStreamTestFile(t, dir, "native.dcm", &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian})
	for _, codec := range []transfer.Syntax{transfer.RLELossless, transfer.JPEGLSLossless, transfer.JPEGLosslessSV1, transfer.DeflatedImageFrameCompression} {
		path := filepath.Join(dir, codec.UID+".dcm")
		if _, err := transcode1294Run(context.Background(), native, path, codec, options, 1, true, "setup"); err != nil {
			t.Fatal(err)
		}
		reader, err := pixeldata.OpenFrameReader(context.Background(), path, pixeldata.FrameReaderOptions{Registry: options.DecoderRegistry})
		if err != nil {
			t.Fatal(err)
		}
		for i, nativeFrame := range frames.Data {
			encoded, err := options.EncoderRegistry.EncodeFrame(context.Background(), codec.UID, nativeFrame, metadata)
			if err != nil {
				reader.Close()
				t.Fatal(err)
			}
			stored, err := reader.RawFrame(context.Background(), i)
			if err != nil {
				reader.Close()
				t.Fatal(err)
			}
			if len(stored) != len(encoded.Data)+len(encoded.Data)%2 || !equalTranscode1294Bytes(stored[:len(encoded.Data)], encoded.Data) || len(encoded.Data)%2 != 0 && stored[len(encoded.Data)] != 0 {
				reader.Close()
				t.Fatalf("%s frame %d differs from pipeline", codec.Name, i)
			}
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

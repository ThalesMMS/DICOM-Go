package render

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type volumeLoadSample struct {
	LoadMS, PrepareMS, ResliceMS, TotalMS float64
	AllocatedBytes                        uint64
}

// Explicit opt-in: the campaign writes/validates its own non-PHI 48-file CT
// fixture and measures fresh file/volume preparation with a warm OS file cache.
// Hashing, analytical qualification and JSON publication are outside timings.
func TestVolumeLoadCampaign(t *testing.T) {
	if os.Getenv("DICOM_VOLUME_LOAD_CAMPAIGN") != "1" {
		t.Skip("set DICOM_VOLUME_LOAD_CAMPAIGN=1 and explicit fixture/output paths")
	}
	root, output := os.Getenv("DICOM_VOLUME_LOAD_FIXTURES"), os.Getenv("DICOM_VOLUME_LOAD_REPORT")
	if root == "" || output == "" {
		t.Fatal("campaign needs DICOM_VOLUME_LOAD_FIXTURES and DICOM_VOLUME_LOAD_REPORT")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	const rows, cols, depth = 512, 512, 48
	paths := make([]string, depth)
	wantPayload := sha256.New()
	fixtureHashes := make([]string, depth)
	for z := range paths {
		pixels := make([]byte, rows*cols*2)
		for y := 0; y < rows; y++ {
			for x := 0; x < cols; x++ {
				binary.LittleEndian.PutUint16(pixels[2*(y*cols+x):], uint16(x+2*y+3*z))
			}
		}
		_, _ = wantPayload.Write(pixels)
		elements := []core.Element{
			dicomtest.NewUIElement(core.NewTag(0x0008, 0x0016), "1.2.840.10008.5.1.4.1.1.2"),
			dicomtest.NewUIElement(core.NewTag(0x0008, 0x0018), fmt.Sprintf("1.2.826.0.1.3680043.10.543.1306.%d", z+1)),
			dicomtest.NewStringElement(core.NewTag(0x0008, 0x0060), core.VRCS, "CT"),
			dicomtest.NewPNElement(core.NewTag(0x0010, 0x0010), "SYNTHETIC^VOLUME"),
			dicomtest.NewStringElement(core.NewTag(0x0010, 0x0020), core.VRLO, "OWN-1306"),
			dicomtest.NewStringElement(core.NewTag(0x0018, 0x0050), core.VRDS, "1"),
			dicomtest.NewUIElement(core.NewTag(0x0020, 0x000D), "1.2.826.0.1.3680043.10.543.1306.100"),
			dicomtest.NewUIElement(core.NewTag(0x0020, 0x000E), "1.2.826.0.1.3680043.10.543.1306.200"),
			dicomtest.NewStringElement(core.NewTag(0x0020, 0x0013), core.VRIS, fmt.Sprint(z+1)),
			dicomtest.NewStringElement(core.NewTag(0x0020, 0x0032), core.VRDS, fmt.Sprintf("0\\0\\%d", z)),
			dicomtest.NewStringElement(core.NewTag(0x0020, 0x0037), core.VRDS, "1\\0\\0\\0\\1\\0"),
			dicomtest.NewUIElement(core.NewTag(0x0020, 0x0052), "1.2.826.0.1.3680043.10.543.1306.300"),
			dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0002), 1),
			dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
			dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0010), rows),
			dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0011), cols),
			dicomtest.NewStringElement(core.NewTag(0x0028, 0x0030), core.VRDS, "1\\1"),
			dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0100), 16),
			dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0101), 12),
			dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0102), 11),
			dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0103), 0),
			dicomtest.NewStringElement(core.NewTag(0x0028, 0x1052), core.VRDS, "0"),
			dicomtest.NewStringElement(core.NewTag(0x0028, 0x1053), core.VRDS, "1"),
			core.NewRawElement(core.TagPixelData, core.VROW, pixels),
		}
		data, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian, elements...)
		if err != nil {
			t.Fatal(err)
		}
		paths[z] = filepath.Join(root, fmt.Sprintf("slice-%03d.dcm", z))
		if old, err := os.ReadFile(paths[z]); err == nil {
			if !bytes.Equal(old, data) {
				t.Fatalf("refusing to overwrite another fixture: %s", paths[z])
			}
		} else if os.IsNotExist(err) {
			if err := os.WriteFile(paths[z], data, 0600); err != nil {
				t.Fatal(err)
			}
		} else {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		fixtureHashes[z] = hex.EncodeToString(hash[:])
	}
	serial := os.Getenv("DICOM_VOLUME_LOAD_MODE") == "serial"
	clock, frequency := volumeLoadClock(t)
	milliseconds := func(start, end int64) float64 { return float64(end-start) * 1000 / float64(frequency) }
	var samples []volumeLoadSample
	var payloadHash string
	for repetition := 0; repetition < 8; repetition++ { // one excluded warmup
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		started := clock()
		opts := object.ReadFileOptions{MaxTotalBytes: 1 << 20, MaxElementBytes: 1 << 20, MaxPixelDataBytes: rows * cols * 2, MaxElements: 128, MaxSequenceDepth: 8, MaxFragments: 1}
		var files []*object.File
		var err error
		if serial {
			for _, path := range paths {
				file, openErr := object.OpenFileWithOptions(path, opts)
				if openErr != nil {
					t.Fatal(openErr)
				}
				files = append(files, file)
			}
		} else {
			files, err = object.OpenFilesContext(context.Background(), paths, opts, 4)
			if err != nil {
				t.Fatal(err)
			}
		}
		stack := &Stack{PixelSpacing: []float64{1, 1}, SliceThickness: 1}
		for _, file := range files {
			metadata, err := pixeldata.ExtractMetadata(file.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			pixel, _ := file.Dataset.Get(core.TagPixelData)
			data, _ := pixel.RawBytes()
			position, _ := file.GetFloats(core.NewTag(0x0020, 0x0032))
			orientation, _ := file.GetFloats(core.NewTag(0x0020, 0x0037))
			spacing, _ := file.GetFloats(core.NewTag(0x0028, 0x0030))
			stack.Frames = append(stack.Frames, &Frame{Metadata: metadata, PixelBytes: data, ByteOrder: binary.LittleEndian,
				ImagePosition: position, ImageOrientation: orientation, PixelSpacing: spacing, SliceThickness: 1, Rescale: Rescale{Slope: 1}})
		}
		loaded := clock()
		volume, err := BuildVolume(stack)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := volume.PrepareSnapshotContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		prepared := clock()
		plane := Plane{Origin: Vec3{100, 100, 10}, U: Vec3{300, 0, 20}, V: Vec3{0, 300, 5}}
		result, err := ResliceObliqueScalarContext(context.Background(), volume, plane, 512, 512)
		if err != nil {
			t.Fatal(err)
		}
		ended := clock()
		runtime.ReadMemStats(&after)
		if repetition > 0 {
			samples = append(samples, volumeLoadSample{milliseconds(started, loaded), milliseconds(loaded, prepared), milliseconds(prepared, ended), milliseconds(started, ended), after.TotalAlloc - before.TotalAlloc})
		}
		lease, err := volume.AcquireSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		snapshot, _ := lease.Snapshot()
		hash := sha256.New()
		if err := snapshot.WritePayloadTo(hash); err != nil {
			t.Fatal(err)
		}
		lease.Release()
		payloadHash = hex.EncodeToString(hash.Sum(nil))
		if !bytes.Equal(hash.Sum(nil), wantPayload.Sum(nil)) {
			t.Fatal("canonical volume differs from analytical stored CT samples")
		}
		for y := 0; y < 512; y++ {
			for x := 0; x < 512; x++ {
				point := plane.At(float64(x)/511, float64(y)/511)
				got, valid := result.ValueAt(x, y)
				if !valid || math.Abs(got-(point.X+2*point.Y+3*point.Z)) > 1e-8 {
					t.Fatalf("reslice differs at %d,%d: %g valid=%v", x, y, got, valid)
				}
			}
		}
		_ = volume.Close()
		for _, file := range files {
			_ = file.Close()
		}
	}
	report := struct {
		OS, Architecture, Go, Mode, PayloadSHA256 string
		GOMAXPROCS                                int
		ClockFrequency                            int64
		FixtureSHA256                             []string
		Samples                                   []volumeLoadSample
	}{runtime.GOOS, runtime.GOARCH, runtime.Version(), os.Getenv("DICOM_VOLUME_LOAD_MODE"), payloadHash, runtime.GOMAXPROCS(0), frequency, fixtureHashes, samples}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("qualified payload=%s; measured seven fresh load/prepare/reslice runs; report=%s", payloadHash, output)
}

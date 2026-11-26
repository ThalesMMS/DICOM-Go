package nifti_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/render"
)

func TestReferenceNIfTITimingAndIntentFields(t *testing.T) {
	// ITK itkNiftiImageIOTest14.cxx exercises spatial and temporal units.
	// VTK TestNIFTIReaderWriter.cxx exercises numeric intent and timing fields.
	const timeSpacing, timeOrigin, sliceDuration = 0.5, -0.25, 0.125
	const millimeters, meters, micrometers = 2, 1, 3
	const seconds, milliseconds, microseconds = 8, 16, 24
	for _, test := range []struct {
		name                  string
		version               int
		bigEndian             bool
		spatialUnit, timeUnit uint8
		spatialScale          float64 // Stored spatial units per mm.
		timeScale             float64 // Stored temporal units per second.
	}{
		{"v1-little-mm-sec", 1, false, millimeters, seconds, 1, 1},
		{"v1-big-um-ms", 1, true, micrometers, milliseconds, 1000, 1000},
		{"v2-little-meter-us", 2, false, meters, microseconds, 0.001, 1_000_000},
		{"v2-big-um-sec", 2, true, micrometers, seconds, 1000, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			spec := defaultSynth()
			spec.version, spec.bigEndian = test.version, test.bigEndian
			spec.dims = []int64{2, 1, 3, 2}
			spec.sformCode = 1
			u := test.spatialScale
			spec.pixdim = [8]float64{1, 1.5 * u, 2.25 * u, 4 * u, timeSpacing * test.timeScale, 1, 1, 1}
			spec.srow = [3][4]float64{{0, -2.25 * u, 0, 120 * u}, {1.5 * u, 0, 0, -40 * u}, {0, 0, 4 * u, 17 * u}}
			spec.xyztUnits = test.spatialUnit | test.timeUnit
			data := synthFile(spec, []float64{0, 1, 2, 3, 4, 5, 21, 22, 23, 24, 25, 26})
			order := synthByteOrder(spec)
			const intentCode, dimInfo, sliceCode, sliceStart, sliceEnd = 3, 0x39, 3, 1, 2
			intentParams := [3]float64{7.5, 0, 0}
			// NIfTI-2 numeric metadata is binary64; these two values distinguish it
			// from an accidental float32 conversion. intent code 3 is a t statistic.
			calMin, calMax := -14.5, 39.25
			if spec.version == 2 {
				calMin, calMax = -14.500000000000004, 39.25000000000001
				for index, value := range intentParams {
					order.PutUint64(data[80+8*index:], math.Float64bits(value))
				}
				order.PutUint64(data[192:], math.Float64bits(calMax))
				order.PutUint64(data[200:], math.Float64bits(calMin))
				order.PutUint64(data[208:], math.Float64bits(sliceDuration*test.timeScale))
				order.PutUint64(data[216:], math.Float64bits(timeOrigin*test.timeScale))
				order.PutUint64(data[224:], sliceStart)
				order.PutUint64(data[232:], sliceEnd)
				order.PutUint32(data[496:], sliceCode)
				order.PutUint32(data[504:], intentCode)
				data[524] = dimInfo
			} else {
				for index, value := range intentParams {
					order.PutUint32(data[56+4*index:], math.Float32bits(float32(value)))
				}
				order.PutUint32(data[124:], math.Float32bits(float32(calMax)))
				order.PutUint32(data[128:], math.Float32bits(float32(calMin)))
				order.PutUint32(data[132:], math.Float32bits(float32(sliceDuration*test.timeScale)))
				order.PutUint32(data[136:], math.Float32bits(float32(timeOrigin*test.timeScale)))
				order.PutUint16(data[74:], sliceStart)
				order.PutUint16(data[120:], sliceEnd)
				order.PutUint16(data[68:], intentCode)
				data[39], data[122] = dimInfo, sliceCode
			}

			info, err := nifti.Inspect(ctx, bytes.NewReader(data), nifti.ReadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			v, err := nifti.Read(ctx, bytes.NewReader(data), nifti.ReadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			h := info.Header
			if h != v.Header || h.Version != test.version || h.BigEndian != test.bigEndian ||
				info.Dimensions != [4]int{2, 1, 3, 2} || info.PayloadBytes != 12 {
				t.Fatalf("Inspect header/layout %+v, Read header %+v", info, v.Header)
			}
			if h.SpatialUnit() != nifti.SpatialUnit(test.spatialUnit) || h.TemporalUnit() != test.timeUnit ||
				h.PixDim[4] != timeSpacing*test.timeScale || h.TOffset != timeOrigin*test.timeScale ||
				h.SliceDuration != sliceDuration*test.timeScale || h.SliceStart != sliceStart ||
				h.SliceEnd != sliceEnd || h.SliceCode != sliceCode || h.DimInfo != dimInfo {
				t.Fatalf("timing metadata %+v", h)
			}
			if h.IntentCode != intentCode || h.IntentParams != intentParams || h.CalMin != calMin || h.CalMax != calMax {
				t.Fatalf("intent/calibration metadata %+v", h)
			}
			lastCenter := render.Vec3{X: -120, Y: 38.5, Z: 25}
			got := v.Geometry.IndexToLPS.TransformPoint(render.Vec3{X: 1, Z: 2})
			if !vecNear(got, lastCenter, 1e-5) || v.Value(v.Index(1, 0, 2, 1)) != 26 {
				t.Fatalf("last voxel center %v (want %v), last sample %v", got, lastCenter, v.Value(v.Len()-1))
			}
		})
	}
}

func TestReferenceNIfTI1WriteDimensionBoundary(t *testing.T) {
	// Exercise the dimension boundary at WriteVolume, through a valid NIfTI-2
	// source. This parallels the size rejection in ITK itkNiftiImageIOTest11.
	for _, test := range []struct {
		name  string
		width int
	}{
		{"last-nifti1-width", math.MaxInt16},
		{"first-nifti2-width", math.MaxInt16 + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			spec := defaultSynth()
			spec.version, spec.bigEndian, spec.qformCode = 2, true, 1
			spec.dims = []int64{int64(test.width), 1, 1}
			const firstSample, lastSample = 27, 232
			payload := make([]byte, test.width)
			payload[0], payload[len(payload)-1] = firstSample, lastSample
			data := append(synthFile(spec, nil), payload...)
			v, err := nifti.Read(ctx, bytes.NewReader(data), nifti.ReadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if v.Dimensions != [4]int{test.width, 1, 1, 1} || v.Value(v.Len()-1) != lastSample {
				t.Fatalf("NIfTI-2 dimensions %v, last sample %v", v.Dimensions, v.Value(v.Len()-1))
			}
			var output bytes.Buffer
			err = nifti.WriteVolume(&output, v, nifti.CompressionNone)
			if test.width > math.MaxInt16 {
				if err == nil || output.Len() != 0 {
					t.Fatalf("unrepresentable NIfTI-1 width wrote %d bytes with error %v", output.Len(), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			header, err := nifti.ParseHeader(output.Bytes())
			if err != nil || header.Version != 1 || header.Dim[1] != math.MaxInt16 {
				t.Fatalf("NIfTI-1 header %+v, error %v", header, err)
			}
			if !bytes.Equal(output.Bytes()[352:], payload) {
				t.Fatal("NIfTI-1 output changed voxel order or values")
			}
		})
	}
}

func TestReferenceNIfTIPayloadAcrossReadChunks(t *testing.T) {
	// Exercise a plane that straddles the Go reader's documented 8 MiB chunks.
	// ITK itkNiftiLargeImageRegionReadTest.cxx exercises its own large-region path.
	const cols, rows, slices, times = 31, 17, 1003, 4
	const planeSize, count = cols * rows, cols * rows * slices * times
	const seam = (8 << 20) / 4
	ctx := context.Background()
	spec := defaultSynth()
	spec.version, spec.bigEndian, spec.qformCode = 2, true, 1
	spec.datatype, spec.dims = nifti.DatatypeUint32, []int64{cols, rows, slices, times}
	spec.slope, spec.inter = -0.25, 97.5
	header := synthFile(spec, nil)
	payload := make([]byte, count*4)
	canonical := make([]byte, count*4)
	stored := func(index int) uint32 { return 0x81010307 + 131*uint32(index) }
	for i := 0; i < count; i++ {
		binary.BigEndian.PutUint32(payload[i*4:], stored(i))
		binary.LittleEndian.PutUint32(canonical[i*4:], stored(i))
	}
	wantHash := sha256.Sum256(canonical)
	exact := nifti.ReadOptions{Limits: nifti.ReadLimits{MaxVoxels: count, MaxPayloadBytes: count * 4}}

	t.Run("gzip-at-exact-budget", func(t *testing.T) {
		data := gzipBytes(t, append(header[:len(header):len(header)], payload...))
		v, err := nifti.Read(ctx, bytes.NewReader(data), exact)
		if err != nil {
			t.Fatal(err)
		}
		if v.Dimensions != [4]int{cols, rows, slices, times} || v.Len() != count || len(v.Warnings) != 0 {
			t.Fatalf("dimensions %v, voxel count %d, warnings %v", v.Dimensions, v.Len(), v.Warnings)
		}
		for _, index := range []int{0, seam - 1, seam, seam + 1, count - 1} {
			if got := v.Stored(index); got != float64(stored(index)) {
				t.Fatalf("voxel %d: stored %v, want %v", index, got, stored(index))
			}
		}
		plane := seam / planeSize
		values := v.SliceValues(plane%slices, plane/slices, nil)
		if len(values) != planeSize {
			t.Fatalf("plane length %d, want %d", len(values), planeSize)
		}
		for i, got := range values {
			if want := 97.5 - 0.25*float64(stored(plane*planeSize+i)); got != want {
				t.Fatalf("plane voxel %d: scaled %v, want %v", i, got, want)
			}
		}
		hash := sha256.New()
		if err := v.WritePayload(hash); err != nil {
			t.Fatal(err)
		}
		if got := hash.Sum(nil); !bytes.Equal(got, wantHash[:]) {
			t.Fatalf("normalized payload SHA-256 %x, want %x", got, wantHash)
		}
	})
	t.Run("truncated-after-first-chunk", func(t *testing.T) {
		source := io.MultiReader(bytes.NewReader(header), bytes.NewReader(payload[:len(payload)-1]))
		v, err := nifti.Read(ctx, source, exact)
		if v != nil || !errors.Is(err, nifti.ErrTruncatedData) {
			t.Fatalf("truncated payload returned volume=%t, error %v", v != nil, err)
		}
	})
	t.Run("byte-ceiling-before-payload", func(t *testing.T) {
		options := exact
		options.Limits.MaxPayloadBytes--
		// A header-only stream must hit the byte cap before the absent payload.
		v, err := nifti.Read(ctx, bytes.NewReader(header), options)
		if v != nil || !errors.Is(err, nifti.ErrLimitExceeded) {
			t.Fatalf("over-budget payload returned volume=%t, error %v", v != nil, err)
		}
	})
}

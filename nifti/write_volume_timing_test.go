package nifti_test

import (
	"bytes"
	"context"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/nifti"
)

// ITK's itkNiftiImageIOTest14.cxx exercises storage units and a nonzero time
// origin. Read and re-export a scalar series with the same physical timeline
// encoded three different ways, then recover the times from the output.
func TestWriteVolumePreservesPhysicalTimeline(t *testing.T) {
	const (
		seconds      = 8
		milliseconds = 16
		microseconds = 24
		timeStep     = 0.625
	)
	unitsPerSecond := map[uint8]float64{seconds: 1, milliseconds: 1000, microseconds: 1_000_000}
	for _, tt := range []struct {
		name      string
		unit      uint8
		version   int
		bigEndian bool
		origin    float64
	}{
		{name: "seconds_zero_origin", unit: seconds, version: 1},
		{name: "seconds_nonzero_origin", unit: seconds, version: 1, origin: 2.25},
		{name: "milliseconds_nonzero_origin", unit: milliseconds, version: 1, bigEndian: true, origin: 2.25},
		{name: "microseconds_nonzero_origin", unit: microseconds, version: 2, origin: 2.25},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			spec := defaultSynth()
			spec.version, spec.bigEndian, spec.qformCode = tt.version, tt.bigEndian, 1
			spec.dims = []int64{1, 1, 1, 3}
			spec.xyztUnits |= tt.unit
			scale := unitsPerSecond[tt.unit]
			spec.pixdim[4] = timeStep * scale
			samples := []float64{3, 5, 7}
			wire := synthFile(spec, samples)
			order := synthByteOrder(spec)
			if spec.version == 2 {
				order.PutUint64(wire[216:224], math.Float64bits(tt.origin*scale))
			} else {
				order.PutUint32(wire[136:140], math.Float32bits(float32(tt.origin*scale)))
			}
			input, err := nifti.Read(ctx, bytes.NewReader(wire), nifti.ReadOptions{})
			if err != nil {
				t.Fatalf("read input: %v", err)
			}
			if h := input.Header; h.TemporalUnit() != tt.unit || h.TOffset != tt.origin*scale || h.PixDim[4] != timeStep*scale {
				t.Fatalf("input timing metadata = unit %d, origin %g, step %g; want unit %d, origin %g, step %g", h.TemporalUnit(), h.TOffset, h.PixDim[4], tt.unit, tt.origin*scale, timeStep*scale)
			}
			var wireOut bytes.Buffer
			if err := nifti.WriteVolume(&wireOut, input, nifti.CompressionNone); err != nil {
				t.Fatalf("re-export input: %v", err)
			}
			output, err := nifti.Read(ctx, &wireOut, nifti.ReadOptions{})
			if err != nil {
				t.Fatalf("read re-exported volume: %v", err)
			}
			if output.Dimensions != [4]int{1, 1, 1, len(samples)} {
				t.Fatalf("output dimensions = %v; want [1 1 1 %d]", output.Dimensions, len(samples))
			}
			for frame, want := range samples {
				if got := output.Value(output.Index(0, 0, 0, frame)); got != want {
					t.Errorf("output frame %d value = %g; want %g", frame, got, want)
				}
			}
			h := output.Header
			outputScale, ok := unitsPerSecond[h.TemporalUnit()]
			if !ok {
				t.Fatalf("output temporal unit %d cannot express elapsed time", h.TemporalUnit())
			}
			gotOrigin := h.TOffset / outputScale
			gotStep := h.PixDim[4] / outputScale
			framesAfterFirst := float64(len(samples) - 1)
			gotLast := gotOrigin + framesAfterFirst*gotStep
			wantLast := tt.origin + framesAfterFirst*timeStep
			if gotOrigin != tt.origin || gotStep != timeStep || gotLast != wantLast {
				t.Errorf("output timeline (first, step, last) = (%g, %g, %g) s; want (%g, %g, %g) s", gotOrigin, gotStep, gotLast, tt.origin, timeStep, wantLast)
			}
		})
	}
}

package parametricmap

import (
	"errors"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
)

// The stored values are identical in both frames. A mapping attached to one
// frame must not supply calibration to a different frame.
func TestReferenceParametricMapFrameCalibration(t *testing.T) {
	for _, tc := range []struct {
		name       string
		secondUnit string
		wantErr    error
	}{
		{name: "independent mappings with a shared unit", secondUnit: "ms"},
		{name: "missing second mapping", wantErr: ErrMissingMapping},
		{name: "mismatched units", secondUnit: "s", wantErr: ErrUnsupportedDimensions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, payload := range []PayloadKind{PayloadInteger, PayloadFloat32, PayloadFloat64} {
				t.Run(string(payload), func(t *testing.T) {
					file := testMapFile(t, payload, []float64{6, 10, 14, 20, 6, 10, 14, 20}, 2, false, core.Tag{})
					groups, ok := file.Dataset.GetSequence(tagPerFrameFunctionalGroups)
					if !ok || len(groups) != 2 {
						t.Fatal("fixture must have two per-frame functional groups")
					}
					groups[0].Put(referenceFrameMapping(0.5, 2, "ms"))
					if tc.secondUnit != "" {
						groups[1].Put(referenceFrameMapping(2, -1, tc.secondUnit))
					}
					file.Dataset.Put(derivedio.Seq(tagPerFrameFunctionalGroups, groups[0].ToDataSet(), groups[1].ToDataSet()))
					wire := rereadPart10(t, file)
					defer wire.Close()
					m, err := Read(wire)
					if tc.wantErr != nil {
						if m != nil || !errors.Is(err, tc.wantErr) {
							t.Fatalf("Read() returned map %t, error %v; want nil and %v", m != nil, err, tc.wantErr)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					for i, want := range [][]float64{{5, 7, 9, 12}, {11, 19, 27, 39}} {
						got, err := m.FrameValues(i)
						if err != nil || !slices.Equal(got, want) {
							t.Errorf("FrameValues(%d) = %v, %v; want %v", i, got, err, want)
						}
					}
				})
			}
		})
	}
}

func referenceFrameMapping(slope, intercept float64, unit string) core.Element {
	return derivedio.Seq(tagRealWorldValueMappingSeq, derivedio.DataSet(
		derivedio.FD(tagRWVSlope, slope),
		derivedio.FD(tagRWVIntercept, intercept),
		derivedio.Seq(tagMeasurementUnitsCodeSeq, derivedio.DataSet(
			derivedio.SH(tagCodeValue, unit),
			derivedio.SH(tagCodingSchemeDesignator, "UCUM"),
			derivedio.LO(tagCodeMeaning, unit),
		)),
	))
}

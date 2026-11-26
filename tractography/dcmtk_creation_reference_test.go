package tractography_test

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/codes"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/tractography"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// DCMTK dcmtract/tests/tcreate.cc (dcmtract_create) saves a single two-point
// track with a track-level color in an Explicit VR Little Endian Part 10 file.
// Exercise that shape using independently authored data, plus the other two
// display-color scopes with their distinct DICOM attributes.
func TestTractographyDCMTKReferenceMinimalPart10(t *testing.T) {
	color := [3]uint16{0x0123, 0xabcd, 0x8000}
	pointColors := [][3]uint16{color, {0x7fff, 0x0405, 0xfedc}}
	const instanceUID = "1.2.826.0.1.3680043.10.999.902"
	for _, tc := range []struct {
		name        string
		setColor    *[3]uint16
		trackColor  *[3]uint16
		pointColors [][3]uint16
		setUS       []byte
		trackUS     []byte
		pointOW     []byte
	}{
		{name: "set", setColor: &color, setUS: []byte{0x23, 0x01, 0xcd, 0xab, 0x00, 0x80}},
		{name: "track", trackColor: &color, trackUS: []byte{0x23, 0x01, 0xcd, 0xab, 0x00, 0x80}},
		{
			name: "point", pointColors: pointColors,
			pointOW: []byte{0x23, 0x01, 0xcd, 0xab, 0x00, 0x80, 0xff, 0x7f, 0x05, 0x04, 0xdc, 0xfe},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			input := tractography.Results{
				SOPInstanceUID:      instanceUID,
				StudyInstanceUID:    "1.2.826.0.1.3680043.10.999.900",
				SeriesInstanceUID:   "1.2.826.0.1.3680043.10.999.901",
				FrameOfReferenceUID: "1.2.826.0.1.3680043.10.999.903",
				SeriesNumber:        47, InstanceNumber: 3,
				ContentDate: "20260704", ContentTime: "103500",
				Label: "TEST_TRACT", Description: "Synthetic minimum track",
				Origin: tractography.OtherSource,
				TrackSets: []tractography.TrackSet{{
					Number: 1, Label: "Two endpoints", Color: tc.setColor,
					Anatomy: tractography.FromCode(codes.Code{Value: "12738006", Scheme: "SCT", Meaning: "Brain"}),
					Model:   tractography.FromCode(codes.Code{Value: "113231", Scheme: "DCM", Meaning: "Single Tensor"}),
					Algorithms: []tractography.Algorithm{{
						Family: tractography.FromCode(codes.Code{Value: "SYNTH", Scheme: "99TEST", Meaning: "Synthetic"}),
						Name:   "Line generator", Version: "2", Parameters: "step=0.5\\unit=mm", Source: "Offline test",
					}},
					Tracks: []tractography.Track{{
						Points: [][3]float32{{1, -2, 0.5}, {-4, 8, 16}},
						Color:  tc.trackColor, PointColors: tc.pointColors,
					}},
				}},
			}
			dataset, err := tractography.Write(ctx, &input, tractography.WriteOptions{
				Common: object.DerivedCommon{Now: time.Date(2026, time.July, 4, 10, 35, 0, 0, time.UTC)},
			})
			if err != nil {
				t.Fatal(err)
			}
			var data bytes.Buffer
			if err := object.WriteFile(&data, &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
				t.Fatal(err)
			}
			file, err := object.ReadFile(&data)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			for _, meta := range []struct {
				tag  core.Tag
				want string
			}{
				{testTag(0x0002, 0x0002), tractography.Storage},
				{testTag(0x0002, 0x0003), instanceUID},
				{testTag(0x0002, 0x0010), transfer.ExplicitVRLittleEndian.UID},
			} {
				if got, ok := file.Meta.GetString(meta.tag); !ok || got != meta.want {
					t.Errorf("file meta %v = %q (present %t), want %q", meta.tag, got, ok, meta.want)
				}
			}
			requireIOD(t, file.Dataset)
			got := readGood(t, file.Dataset)
			if len(got.References) != 0 {
				t.Fatalf("decoded references = %+v, want none", got.References)
			}
			got.References = nil                      // Treat both Go representations of an empty result list alike.
			input.Origin = tractography.UnknownOrigin // A file with no source references cannot assert the initial origin.
			if actual := comparable(got); !reflect.DeepEqual(actual, input) {
				t.Fatalf("decoded model = %+v, want %+v", actual, input)
			}
			sets, ok := file.Dataset.GetSequence(testTag(0x0066, 0x0101))
			if !ok || len(sets) != 1 {
				t.Fatalf("track set items = %d, want 1", len(sets))
			}
			tracks, ok := sets[0].GetSequence(testTag(0x0066, 0x0102))
			if !ok || len(tracks) != 1 {
				t.Fatalf("track items = %d, want 1", len(tracks))
			}
			for _, attribute := range []struct {
				item *object.Object
				tag  core.Tag
				vr   core.VR
				want []byte
			}{
				{sets[0], testTag(0x0062, 0x000d), core.VRUS, tc.setUS},
				{tracks[0], testTag(0x0062, 0x000d), core.VRUS, tc.trackUS},
				{tracks[0], testTag(0x0066, 0x0103), core.VROW, tc.pointOW},
				{tracks[0], testTag(0x0066, 0x0016), core.VROF, []byte{
					0, 0, 0x80, 0x3f, 0, 0, 0, 0xc0, 0, 0, 0, 0x3f,
					0, 0, 0x80, 0xc0, 0, 0, 0, 0x41, 0, 0, 0x80, 0x41,
				}},
			} {
				element, present := attribute.item.Get(attribute.tag)
				if attribute.want == nil {
					if present {
						t.Errorf("unexpected attribute %v", attribute.tag)
					}
					continue
				}
				raw, ok := element.Value.(core.RawValue)
				if !present || !ok || element.Header.VR != attribute.vr || !bytes.Equal(raw, attribute.want) {
					t.Errorf("attribute %v = %v %x (present %t), want %v %x", attribute.tag, element.Header.VR, raw, present, attribute.vr, attribute.want)
				}
			}
		})
	}
}

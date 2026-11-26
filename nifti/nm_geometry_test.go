package nifti_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// syntheticSPECT is a three-slice RECON TOMO reconstruction whose geometry
// lives only in the Detector Information Sequence, Slice Vector and Spacing
// Between Slices (#1108). No patient data.
func syntheticSPECT(spacing float64) *object.File {
	le := binary.LittleEndian
	dataset := derivedio.Object(
		derivedio.UI(derivedio.TagSOPClassUID, "1.2.840.10008.5.1.4.1.1.20"),
		derivedio.UI(derivedio.TagSOPInstanceUID, testSeriesUID+".1108"),
		derivedio.UI(derivedio.TagSeriesInstanceUID, testSeriesUID),
		derivedio.UI(derivedio.TagFrameOfReferenceUID, testFORUID),
		derivedio.Strings(core.NewTag(0x0008, 0x0008), core.VRCS, []string{"ORIGINAL", "PRIMARY", "RECON TOMO", "EMISSION"}),
		derivedio.US(derivedio.TagSamplesPerPixel, 1),
		derivedio.CS(derivedio.TagPhotometricInterpretation, "MONOCHROME2"),
		derivedio.IS(derivedio.TagNumberOfFrames, 3),
		object.ATElement(core.NewTag(0x0028, 0x0009), le, core.NewTag(0x0054, 0x0080)),
		derivedio.US(derivedio.TagRows, 1),
		derivedio.US(derivedio.TagColumns, 2),
		derivedio.US(derivedio.TagBitsAllocated, 8),
		derivedio.US(derivedio.TagBitsStored, 8),
		derivedio.US(derivedio.TagHighBit, 7),
		derivedio.US(derivedio.TagPixelRepresentation, 0),
		derivedio.DS(tagPixelSpacing, 4, 4),
		derivedio.DS(core.NewTag(0x0018, 0x0050), 3),
		derivedio.DS(core.NewTag(0x0018, 0x0088), spacing),
		derivedio.US(core.NewTag(0x0054, 0x0080), 1, 2, 3),
		derivedio.US(core.NewTag(0x0054, 0x0081), 3),
		derivedio.Seq(core.NewTag(0x0054, 0x0022), derivedio.DataSet(
			derivedio.DS(tagImagePositionPatient, 0, 0, 10),
			derivedio.DS(tagImageOrientationPatient, 1, 0, 0, 0, 1, 0),
		)),
		derivedio.Raw(derivedio.TagPixelData, core.VROB, []byte{1, 2, 3, 4, 5, 6}),
	)
	return &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}
}

// TestWriteNMReconstructionUsesSliceVectorGeometry: an NM reconstruction now
// exports as a volume, whichever sign its Spacing Between Slices has, and
// the reader returns the same grid.
func TestWriteNMReconstructionUsesSliceVectorGeometry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		spacing   float64
		origin    float64
		voxels    []byte
		reordered bool
	}{
		{"positive spacing", 2.5, 10, []byte{1, 2, 3, 4, 5, 6}, false},
		{"negative spacing", -2.5, 5, []byte{5, 6, 3, 4, 1, 2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			report, err := nifti.WriteFiles(context.Background(), &output, []*object.File{syntheticSPECT(tc.spacing)}, nifti.DefaultOptions())
			if err != nil {
				t.Fatalf("WriteFiles: %v", err)
			}
			if report.Dimensions != [4]int{2, 1, 3, 1} || report.InputReordered != tc.reordered {
				t.Fatalf("dimensions %v reordered %v, want [2 1 3 1] and %v", report.Dimensions, report.InputReordered, tc.reordered)
			}
			want := [16]float64{-4, 0, 0, 0, 0, -4, 0, 0, 0, 0, 2.5, tc.origin, 0, 0, 0, 1}
			requireAffine(t, report.IndexToRAS[:], want[:], 1e-6)
			if voxels := output.Bytes()[352:]; !bytes.Equal(voxels, tc.voxels) {
				t.Fatalf("voxels = %v, want %v", voxels, tc.voxels)
			}
			volume, err := nifti.Read(context.Background(), bytes.NewReader(output.Bytes()), nifti.ReadOptions{})
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			requireAffine(t, volume.Geometry.IndexToRAS[:], want[:], 1e-5)
		})
	}
}

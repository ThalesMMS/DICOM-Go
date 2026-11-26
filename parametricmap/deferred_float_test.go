package parametricmap

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestReadDeferredFloatMapOwnsPayloadAfterFileClose(t *testing.T) {
	for _, payload := range []PayloadKind{PayloadFloat32, PayloadFloat64} {
		for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
			t.Run(string(payload)+"/"+syntax.UID, func(t *testing.T) {
				file := testMapFile(t, payload, []float64{-2.25, 0, math.NaN(), 4}, 1, true, core.Tag{})
				file.Meta.Put(derivedio.UI(core.NewTag(0x0002, 0x0003), "2.25.1540.1"))
				for i, tag := range []core.Tag{derivedio.TagSOPInstanceUID, derivedio.TagStudyInstanceUID, derivedio.TagSeriesInstanceUID, derivedio.TagFrameOfReferenceUID} {
					file.Dataset.Put(derivedio.UI(tag, "2.25.1540."+string(rune('1'+i))))
				}
				file.Dataset.Put(derivedio.Seq(core.NewTag(0x0008, 0x2112), derivedio.DataSet(
					derivedio.UI(tagReferencedSOPClassUID, "1.2.840.10008.5.1.4.1.1.2"),
					derivedio.UI(tagReferencedSOPInstanceUID, "2.25.1540.9"),
				)))
				if syntax.UID != transfer.ExplicitVRLittleEndian.UID {
					var err error
					file.Dataset, _, err = pixeldata.TranscodeDataSet(context.Background(), file.Dataset, transfer.ExplicitVRLittleEndian, syntax, pixeldata.TranscodeOptions{})
					if err != nil {
						t.Fatal(err)
					}
				}
				file.TransferSyntax = syntax
				var encoded bytes.Buffer
				if err := object.WriteFile(&encoded, file); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "map.dcm")
				if err := os.WriteFile(path, encoded.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
				deferred, err := object.OpenFileWithOptions(path, object.ReadFileOptions{DeferPixelData: true})
				if err != nil {
					t.Fatal(err)
				}
				defer deferred.Close()
				tag := tagFloatPixelData
				if payload == PayloadFloat64 {
					tag = tagDoubleFloatPixelData
				}
				elem, ok := deferred.Dataset.Get(tag)
				if !ok || elem.Value != nil {
					t.Fatal("fixture did not defer the float payload")
				}
				metadata, err := ReadMetadata(deferred)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := metadata.FrameValues(0); !errors.Is(err, ErrPayloadUnavailable) {
					t.Fatal("metadata reader retained payload", err)
				}
				m, err := Read(deferred)
				if err != nil {
					t.Fatal(err)
				}
				if err := deferred.Close(); err != nil {
					t.Fatal(err)
				}
				// Neither later source edits nor closing its handle may affect the map.
				if err := os.WriteFile(path, []byte("replaced after read"), 0600); err != nil {
					t.Fatal(err)
				}
				values, err := m.FrameValues(0)
				if err != nil || len(values) != 4 || values[0] != -3.5 || values[1] != 1 || !math.IsNaN(values[2]) || values[3] != 9 {
					t.Fatalf("owned calibrated values = %v, %v", values, err)
				}
			})
		}
	}
}

func TestReadRejectsMalformedReferencedFrameNumbers(t *testing.T) {
	for _, elem := range []core.Element{
		derivedio.Raw(core.NewTag(0x0008, 0x1160), core.VRIS, nil),
		derivedio.Raw(core.NewTag(0x0008, 0x1160), core.VRIS, []byte("zero")),
		derivedio.Raw(core.NewTag(0x0008, 0x1160), core.VRIS, []byte("0")),
		derivedio.Raw(core.NewTag(0x0008, 0x1160), core.VROB, []byte{1, 2}),
	} {
		file := testMapFile(t, PayloadFloat32, []float64{1, 2, 3, 4}, 1, true, core.Tag{})
		file.Dataset.Put(derivedio.Seq(core.NewTag(0x0008, 0x2112), derivedio.DataSet(
			derivedio.UI(tagReferencedSOPClassUID, "1.2.840.10008.5.1.4.1.1.2"),
			derivedio.UI(tagReferencedSOPInstanceUID, "1.2.3.4"), elem,
		)))
		if _, err := Read(file); !errors.Is(err, ErrInvalidObject) {
			t.Fatalf("malformed reference %v accepted: %v", elem, err)
		}
	}
}

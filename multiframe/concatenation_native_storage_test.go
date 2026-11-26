package multiframe

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestSplitConcatenationRepackagesNativeFramesSharingStorageUnits(t *testing.T) {
	binaryFrames := [][]byte{{5}, {2}, {7}, {3}, {6}}
	binaryValues := [][]byte{{0xd5, 0x01}, {0x33, 0}}
	byteFrames := [][]byte{{10, 11, 12}, {13, 14, 15}, {16, 17, 18}, {20, 21, 22}, {23, 24, 25}}
	byteValues := [][]byte{{10, 11, 12, 13, 14, 15, 16, 17, 18, 0}, {20, 21, 22, 23, 24, 25}}
	for _, test := range []struct {
		name       string
		syntax     transfer.Syntax
		bits       uint16
		sourceVR   core.VR
		sourceWire []byte
		wantFrames [][]byte
		wantValues [][]byte
	}{
		{"LE binary OB", transfer.ExplicitVRLittleEndian, 1, core.VROB, []byte{0xd5, 0x67}, binaryFrames, binaryValues},
		{"BE binary OW", transfer.ExplicitVRBigEndian, 1, core.VROW, []byte{0x67, 0xd5}, binaryFrames, binaryValues},
		{"BE odd 8-bit OW", transfer.ExplicitVRBigEndian, 8, core.VROW, []byte{11, 10, 13, 12, 15, 14, 17, 16, 20, 18, 22, 21, 24, 23, 0, 25}, byteFrames, byteValues},
	} {
		t.Run(test.name, func(t *testing.T) {
			const rows, columns, frames = 1, 3, 5
			order := test.syntax.ByteOrder
			dataset := derivedio.Object(
				derivedio.UI(tagSOPClassUID, "1.2.840.10008.5.1.4.1.1.7.1"),
				derivedio.UI(tagSOPInstanceUID, concatSource),
				derivedio.UI(tagStudyInstanceUID, concatSource+".2"),
				derivedio.UI(tagSeriesInstanceUID, concatSource+".3"),
				derivedio.CS(tagPhotometricInterpretation, "MONOCHROME2"),
				derivedio.IS(tagNumberOfFrames, frames),
				object.USElement(tagRows, order, rows),
				object.USElement(tagColumns, order, columns),
				object.USElement(tagSamplesPerPixel, order, 1),
				object.USElement(tagBitsAllocated, order, test.bits),
				object.USElement(tagBitsStored, order, test.bits),
				object.USElement(tagHighBit, order, test.bits-1),
				object.USElement(tagPixelRepresentation, order, 0),
				derivedio.Seq(tagSharedFunctionalGroupsSequence, core.DataSet{}),
				core.NewRawElement(tagPixelDataForTest, test.sourceVR, test.sourceWire),
			)
			dataset.SetValueByteOrder(order)
			path := filepath.Join(t.TempDir(), "source.dcm")
			writeObjectFile(t, path, &object.File{Dataset: dataset, TransferSyntax: test.syntax})
			ctx := context.Background()
			reader, err := pixeldata.OpenFrameReader(ctx, path, pixeldata.FrameReaderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reader.Close() })

			cap := int64(3 * len(test.wantFrames[0]))
			parts, files := splitToMemory(t, reader, SplitOptions{MaxPixelBytes: cap})
			if len(parts) != 2 || parts[0].Frames != 3 || parts[1].FirstFrame != 3 || parts[1].Frames != 2 {
				t.Fatalf("parts = %+v", parts)
			}
			concat, err := NewConcatenation(files)
			if err != nil || concat.NumberOfFrames() != frames {
				t.Fatalf("read back concatenation: %+v, %v", concat, err)
			}
			for i, file := range files {
				pixel, ok := file.Dataset.Get(tagPixelDataForTest)
				raw, _ := pixel.RawBytes()
				if !ok || pixel.VR() != core.VROB || !bytes.Equal(raw, test.wantValues[i]) || file.TransferSyntax.UID != test.syntax.UID {
					t.Fatalf("part %d Pixel Data: %v %x, syntax %q; want OB %x in %q",
						i, pixel.VR(), raw, file.TransferSyntax.UID, test.wantValues[i], test.syntax.UID)
				}
				decoded, err := pixeldata.NewFrameReader(ctx, file, nil, pixeldata.FrameReaderOptions{})
				if err != nil {
					t.Fatal(err)
				}
				for j := 0; j < parts[i].Frames; j++ {
					frame, _, err := decoded.Frame(ctx, j)
					if want := test.wantFrames[parts[i].FirstFrame+j]; err != nil || !bytes.Equal(frame, want) {
						t.Fatalf("part %d frame %d: got %x, %v; want %x", i, j, frame, err, want)
					}
				}
			}
		})
	}
}

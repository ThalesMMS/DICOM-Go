package dimse

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestVerifiedStorageRejectsPixelDataEncodingMismatch(t *testing.T) {
	class := dicomtest.TestSOPClassUID
	instance := dicomtest.TestSOPInstanceUID
	dataset := object.FromElements(dicomtest.DatasetWithPixelData(), nil)
	native := transfer.ExplicitVRLittleEndian
	compressed := transfer.JPEGBaseline
	encode := func(syntax transfer.Syntax) []byte {
		t.Helper()
		var encoded bytes.Buffer
		if err := object.WriteDataSet(&encoded, dataset, syntax); err != nil {
			t.Fatal(err)
		}
		return encoded.Bytes()
	}
	nativeBytes := encode(native)
	// These are valid uint8 samples on the wire, even though the first four
	// bytes match an encapsulated Item tag in Little Endian.
	pixels, _ := dataset.GetRaw(core.TagPixelData)
	if len(pixels) < 4 || !bytes.HasSuffix(nativeBytes, pixels) {
		t.Fatal("fixture does not end with native Pixel Data")
	}
	copy(nativeBytes[len(nativeBytes)-len(pixels):], []byte{0xfe, 0xff, 0x00, 0xe0})
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, image.NewGray(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	dataset.Put(core.Element{
		Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB},
		Value:  core.FragmentSequence{Fragments: [][]byte{jpegBytes.Bytes()}},
	})
	compressedBytes := encode(compressed)

	for _, tc := range []struct {
		syntax transfer.Syntax
		wrong  []byte
		valid  []byte
	}{
		{native, compressedBytes, nativeBytes},
		{compressed, nativeBytes, compressedBytes},
	} {
		t.Run(tc.syntax.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, server := testPipeAssociations(t, []ul.AcceptedContext{
				{ID: 1, AbstractSyntaxUID: class, TransferSyntaxUID: tc.syntax.UID},
			})
			dir := t.TempDir()
			done := make(chan error, 1)
			go func() {
				defer server.Close()
				done <- ServeStorageAssociation(ctx, server, StorageSCPOptions{StreamSink: FileStreamSink{Dir: dir, Verify: true}})
			}()
			store := NewStoreClient(client)
			send := func(data []byte, want uint16) {
				t.Helper()
				result, err := store.StoreEncodedWithOptions(ctx, func(_ context.Context, dest io.Writer, _ transfer.Syntax) error {
					_, err := dest.Write(data)
					return err
				}, CStoreOptions{AffectedSOPClassUID: class, AffectedSOPInstanceUID: instance})
				if result.Response == nil || result.Response.Status != want {
					t.Errorf("C-STORE returned %+v, error %v; want 0x%04X", result.Response, err, want)
				}
			}
			send(tc.wrong, StatusCStoreCannotUnderstand)
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Errorf("pixel mismatch left entries %v, error %v", entries, err)
			}
			send(tc.valid, StatusSuccess)
			fileBytes, err := os.ReadFile(filepath.Join(dir, instance+".dcm"))
			if err != nil || !bytes.HasSuffix(fileBytes, tc.valid) {
				t.Errorf("corrected retry did not publish the valid encoding under the original UID path, error %v", err)
			}
			if err := client.Release(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatalf("SCP: %v", err)
			}
		})
	}
}

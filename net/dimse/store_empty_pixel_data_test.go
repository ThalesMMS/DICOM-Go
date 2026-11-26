package dimse

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/encapdoc"
	"github.com/ThalesMMS/dicom-go/internal/netstore"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// The receiver must refuse a declared CT image with empty Pixel Data before
// publication. The sender can then retry the same SOP Instance UID with pixels.
func TestCStoreRejectsDeclaredImageWithEmptyPixelData(t *testing.T) {
	const ctClass = "1.2.840.10008.5.1.4.1.1.2"
	const validInstance = "1.2.826.0.1.3680043.10.2300.30"
	const retryInstance = "1.2.826.0.1.3680043.10.2300.31"
	const pdfInstance = "1.2.826.0.1.3680043.10.2300.32"
	const pixelBytes = 64 * 32 * 2
	syntax := transfer.ExplicitVRLittleEndian
	valid := storeAdmissionDataSet(t, ctClass, validInstance, pixelBytes)
	corrected := storeAdmissionDataSet(t, ctClass, retryInstance, pixelBytes)
	emptyImage, err := object.ReadDataSet(bytes.NewReader(corrected), syntax)
	if err != nil {
		t.Fatal(err)
	}
	emptyImage.Put(core.NewRawElement(core.TagPixelData, core.VROW, nil))
	var emptyBytes bytes.Buffer
	if err := object.WriteDataSet(&emptyBytes, emptyImage, syntax); err != nil {
		t.Fatal(err)
	}
	// Check that the stimulus still contains the explicit zero-length OW.
	if !bytes.HasSuffix(emptyBytes.Bytes(), []byte{0xe0, 0x7f, 0x10, 0x00, 'O', 'W', 0, 0, 0, 0, 0, 0}) {
		t.Fatal("empty CT fixture does not end with zero-length (7FE0,0010) OW")
	}
	emptyImage.Remove(core.TagPixelData)
	var absentBytes bytes.Buffer
	if err := object.WriteDataSet(&absentBytes, emptyImage, syntax); err != nil {
		t.Fatal(err)
	}
	document, err := encapdoc.NewPDF(encapdoc.PDFOptions{
		SOPInstanceUID: pdfInstance, Data: []byte("%PDF-1.4\n%%EOF\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var documentBytes bytes.Buffer
	if err := object.WriteDataSet(&documentBytes, document.Dataset, syntax); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"decoded", "verified-stream"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			peer, local := testPipeAssociations(t, []ul.AcceptedContext{
				{ID: 1, AbstractSyntaxUID: ctClass, TransferSyntaxUID: syntax.UID},
				{ID: 3, AbstractSyntaxUID: encapdoc.EncapsulatedPDFStorage, TransferSyntaxUID: syntax.UID},
			})
			dir := t.TempDir()
			opts := StorageSCPOptions{}
			if mode == "verified-stream" {
				opts.StreamSink = FileStreamSink{Dir: dir, Verify: true}
			} else {
				opts.StoreHandler = CStoreHandlerFunc(func(ctx context.Context, request CStoreRequestContext) (uint16, error) {
					_, err := netstore.SavePart10WithContext(ctx, dir, request.DataSet, request.DataSetSyntax)
					return StatusSuccess, err
				})
			}
			done := make(chan error, 1)
			go func() {
				defer local.Close()
				done <- ServeStorageAssociation(ctx, local, opts)
			}()
			client := NewStoreClient(peer)
			store := func(t *testing.T, class, instance string, dataset []byte, want uint16) {
				t.Helper()
				result, err := client.StoreEncodedWithOptions(ctx, func(_ context.Context, w io.Writer, _ transfer.Syntax) error {
					_, err := io.Copy(w, bytes.NewReader(dataset))
					return err
				}, CStoreOptions{AffectedSOPClassUID: class, AffectedSOPInstanceUID: instance})
				if result.Response == nil {
					t.Fatalf("C-STORE returned no DIMSE response: %v", err)
				}
				if result.Response.Status != want {
					t.Errorf("C-STORE status = 0x%04X, want 0x%04X", result.Response.Status, want)
					return
				}
				var statusErr *CStoreStatusError
				if want == StatusSuccess && err != nil || want != StatusSuccess && !errors.As(err, &statusErr) {
					t.Errorf("C-STORE error = %v for status 0x%04X", err, want)
				}
			}
			readPublished := func(t *testing.T, instance string) *object.File {
				t.Helper()
				file, err := object.OpenFile(filepath.Join(dir, instance+".dcm"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := file.Close(); err != nil {
						t.Error(err)
					}
				})
				return file
			}
			t.Run("complete-ct-image", func(t *testing.T) {
				store(t, ctClass, validInstance, valid, StatusSuccess)
				pixels, ok := readPublished(t, validInstance).Dataset.GetRaw(core.TagPixelData)
				if !ok || !bytes.Equal(pixels, valid[len(valid)-pixelBytes:]) {
					t.Fatalf("published CT lacks the %d transmitted pixel bytes", pixelBytes)
				}
			})
			t.Run("pdf-without-pixel-data", func(t *testing.T) {
				store(t, encapdoc.EncapsulatedPDFStorage, pdfInstance, documentBytes.Bytes(), StatusSuccess)
				file := readPublished(t, pdfInstance)
				if _, present := file.Dataset.Get(core.TagPixelData); present {
					t.Fatal("stored non-image object gained a Pixel Data element")
				}
				if class, ok := file.Dataset.GetUID(core.NewTag(0x0008, 0x0016)); !ok || class != encapdoc.EncapsulatedPDFStorage {
					t.Fatalf("stored non-image SOP class = %q", class)
				}
			})
			t.Run("empty-ct-rejected-before-publication", func(t *testing.T) {
				store(t, ctClass, retryInstance, emptyBytes.Bytes(), StatusCStoreDataSetDoesNotMatch)
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 2 {
					t.Errorf("after empty CT, directory contains %v, error %v; want only both controls", entries, err)
				}
			})
			t.Run("missing-ct-pixel-data-rejected-before-publication", func(t *testing.T) {
				store(t, ctClass, retryInstance, absentBytes.Bytes(), StatusCStoreDataSetDoesNotMatch)
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 2 {
					t.Errorf("after CT without Pixel Data, directory contains %v, error %v; want only both controls", entries, err)
				}
			})
			t.Run("same-uid-retry-publishes-complete-image-once", func(t *testing.T) {
				store(t, ctClass, retryInstance, corrected, StatusSuccess)
				pixels, ok := readPublished(t, retryInstance).Dataset.GetRaw(core.TagPixelData)
				if !ok || !bytes.Equal(pixels, corrected[len(corrected)-pixelBytes:]) {
					t.Errorf("retried CT at the original UID path has %d pixel bytes, want the corrected %d bytes", len(pixels), pixelBytes)
				}
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 3 {
					t.Errorf("after retry, directory contains %v, error %v; want both controls and one corrected image", entries, err)
				}
			})
			if err := peer.Release(ctx); err != nil {
				t.Fatalf("release: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("ServeStorageAssociation: %v", err)
			}
		})
	}
}

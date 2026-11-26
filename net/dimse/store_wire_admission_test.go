package dimse

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/internal/netstore"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Exercise the production receiver with a peer that sends command and dataset
// identities independently. A refused, fragmented request must not publish
// a Part 10 file or leave bytes to be consumed as the next request.
func TestCStoreWireAdmissionAndPublication(t *testing.T) {
	const otherClass = "1.2.840.10008.5.1.4.1.1.4"
	const otherInstance = "1.2.826.0.1.3680043.10.2300.2"
	const maxDataSetBytes = 12 << 10
	const validPixelBytes = 9 << 10
	class := dicomtest.TestSOPClassUID
	instance := dicomtest.TestSOPInstanceUID
	valid := storeAdmissionDataSet(t, class, instance, validPixelBytes)
	oversized := storeAdmissionDataSet(t, class, instance, 14<<10)

	for _, mode := range []string{"decoded", "verified-stream"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			root := t.TempDir()
			outDir := filepath.Join(root, "incoming")
			opts := StorageSCPOptions{MaxDataSetBytes: maxDataSetBytes}
			handled := make(chan uint16, 16)
			if mode == "verified-stream" {
				opts.StreamSink = FileStreamSink{Dir: outDir, Verify: true}
			} else {
				opts.StoreHandler = CStoreHandlerFunc(func(ctx context.Context, req CStoreRequestContext) (uint16, error) {
					handled <- req.Request.MessageID
					if _, err := netstore.SavePart10WithContext(ctx, outDir, req.DataSet, req.DataSetSyntax); err != nil {
						return StatusCStoreOutOfResources, NewCStoreSCPError(StatusCStoreOutOfResources, "publication failed", err)
					}
					return StatusSuccess, nil
				})
			}
			addr, done := startStreamSCP(t, ctx, opts)
			assoc := dialStoreSCU(t, ctx, addr)
			defer func() { _ = assoc.Close() }()
			pc, err := AcceptedContextForSOPClass(assoc, class)
			if err != nil {
				t.Fatal(err)
			}

			t.Run("missing-publication-directory", func(t *testing.T) {
				assertStoreAdmissionResponse(t, ctx, assoc, pc.ID, 10, class, instance, valid, StatusCStoreOutOfResources)
				if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
					t.Fatalf("publication failure left entries %v, error %v", entries, err)
				}
			})
			if err := os.Mkdir(outDir, 0o700); err != nil {
				t.Fatal(err)
			}
			refusals := []struct {
				name         string
				commandClass string
				data         []byte
				status       uint16
			}{
				{"command-class-vs-context", otherClass, storeAdmissionDataSet(t, otherClass, instance, validPixelBytes), StatusCStoreDataSetDoesNotMatch},
				{"dataset-class-vs-command", class, storeAdmissionDataSet(t, otherClass, instance, validPixelBytes), StatusCStoreDataSetDoesNotMatch},
				{"dataset-instance-vs-command", class, storeAdmissionDataSet(t, class, otherInstance, validPixelBytes), StatusCStoreDataSetDoesNotMatch},
				{"missing-dataset-class", class, storeAdmissionDataSet(t, "", instance, validPixelBytes), StatusCStoreDataSetDoesNotMatch},
				{"missing-dataset-instance", class, storeAdmissionDataSet(t, class, "", validPixelBytes), StatusCStoreDataSetDoesNotMatch},
				{"truncated-dataset-value", class, valid[:len(valid)-2], StatusCStoreCannotUnderstand},
				{"dataset-budget-exceeded", class, oversized, StatusCStoreOutOfResources},
			}
			for index, refusal := range refusals {
				t.Run(refusal.name, func(t *testing.T) {
					assertStoreAdmissionResponse(t, ctx, assoc, pc.ID, uint16(11+index), refusal.commandClass, instance, refusal.data, refusal.status)
					if entries, err := os.ReadDir(outDir); err != nil || len(entries) != 0 {
						t.Fatalf("refused request left entries %v, error %v", entries, err)
					}
				})
			}
			t.Run("accepts-next-instance-on-same-association", func(t *testing.T) {
				assertStoreAdmissionResponse(t, ctx, assoc, pc.ID, 40, class, instance, valid, StatusSuccess)
				entries, err := os.ReadDir(outDir)
				if err != nil || len(entries) != 1 || entries[0].Name() != instance+".dcm" {
					t.Fatalf("successful request published entries %v, error %v", entries, err)
				}
				file, err := object.OpenFile(filepath.Join(outDir, instance+".dcm"))
				if err != nil {
					t.Fatal(err)
				}
				for tag, want := range map[core.Tag]string{
					core.NewTag(0x0008, 0x0016): class,
					core.NewTag(0x0008, 0x0018): instance,
				} {
					if got, ok := file.Dataset.GetUID(tag); !ok || got != want {
						t.Fatalf("published %s = %q, ok = %t, want %q", tag, got, ok, want)
					}
				}
				pixels, _ := file.Dataset.Get(core.TagPixelData)
				value, ok := pixels.Value.(core.RawValue)
				if !ok || !bytes.Equal(value, valid[len(valid)-validPixelBytes:]) {
					t.Fatalf("published Pixel Data has %d bytes, want the original %d bytes", len(value), validPixelBytes)
				}
			})
			if err := assoc.Release(ctx); err != nil {
				t.Fatalf("Release: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("ServeStorageAssociation: %v", err)
			}
			close(handled)
			var handledIDs []uint16
			for id := range handled {
				handledIDs = append(handledIDs, id)
			}
			if mode == "decoded" && !slices.Equal(handledIDs, []uint16{10, 40}) {
				t.Fatalf("storage handler received message IDs %v, want [10 40]", handledIDs)
			}
		})
	}
}

func storeAdmissionDataSet(t *testing.T, class, instance string, pixelBytes int) []byte {
	t.Helper()
	dataset := readIntegrationFixture(t).Dataset
	for tag, value := range map[core.Tag]string{
		core.NewTag(0x0008, 0x0016): class,
		core.NewTag(0x0008, 0x0018): instance,
	} {
		if value == "" {
			dataset.Remove(tag)
			continue
		}
		if err := dataset.SetStrings(tag, core.VRUI, value); err != nil {
			t.Fatal(err)
		}
	}
	dataset.SetUS(core.NewTag(0x0028, 0x0010), 64)
	dataset.SetUS(core.NewTag(0x0028, 0x0011), uint16(pixelBytes/(64*2)))
	dataset.Put(core.NewRawElement(core.TagPixelData, core.VROW, bytes.Repeat([]byte{0x1a, 0x06}, pixelBytes/2)))
	var data bytes.Buffer
	if err := object.WriteDataSet(&data, dataset, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func assertStoreAdmissionResponse(t *testing.T, ctx context.Context, assoc *ul.Association, pcID byte, messageID uint16, class, instance string, data []byte, status uint16) {
	t.Helper()
	if err := SendCStoreRequestWithContext(ctx, assoc, pcID, CStoreRequest{
		AffectedSOPClassUID: class, AffectedSOPInstanceUID: instance, MessageID: messageID,
	}); err != nil {
		t.Fatal(err)
	}
	writer := NewPDataWriterWithContext(ctx, assoc, pcID, false, peerMaxPDUWithHeader(assoc))
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Finish(); err != nil {
		t.Fatal(err)
	}
	response, err := ReceiveCStoreResponseWithContext(ctx, assoc, pcID)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != status || response.MessageIDBeingRespondedTo != messageID ||
		response.AffectedSOPClassUID != class || response.AffectedSOPInstanceUID != instance {
		t.Fatalf("C-STORE response = %+v, want status 0x%04X, message %d, class %s and instance %s", response, status, messageID, class, instance)
	}
}

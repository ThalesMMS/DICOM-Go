package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestParseArgsAttributeAssignments(t *testing.T) {
	for _, value := range []string{"PatientName=Synthetic^Test", "(0010,0020)=Test", "SOPInstanceUID=1.2.3.987", "PatientName=", "PatientName=One=Two"} {
		var stderr bytes.Buffer
		opts, err := parseArgs([]string{"-s", value, "127.0.0.1:104", "test.dcm"}, &stderr)
		if err != nil || len(opts.attributes) != 1 {
			t.Fatalf("valid assignment failed: %v %s", err, stderr.String())
		}
	}
	for _, value := range []string{"SECRET", "SecretKeyword=SECRET", "Rows=2", "PixelData=SECRET", "NumberOfFrames=2", "FileMetaInformationVersion=SECRET", "SOPInstanceUID=SECRET", "PatientName=One\\Two", "PatientAge=12345Y", "ReferencedSeriesSequence=SECRET"} {
		var stderr bytes.Buffer
		_, err := parseArgs([]string{"-s", value, "127.0.0.1:104", "test.dcm"}, &stderr)
		if !errors.Is(err, errUsage) || strings.Contains(stderr.String(), value) || strings.Contains(stderr.String(), "SECRET") {
			t.Fatalf("invalid assignment diagnostic: %v %q", err, stderr.String())
		}
	}
}

func TestRunAttributeCoercionLoopbackAndTranscodeReport(t *testing.T) {
	for _, mode := range []string{"native", "rle", "lossy"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			data, err := dicomtest.ExplicitVRFile()
			if err != nil {
				t.Fatal(err)
			}
			if mode != "native" {
				dataset := object.FromElements(dicomtest.DatasetWithPixelData(), nil)
				// JPEG Baseline takes unsigned 8-bit samples; use the same image
				// for RLE to exercise report composition consistently.
				for _, tag := range []core.Tag{core.NewTag(0x0028, 0x0100), core.NewTag(0x0028, 0x0101)} {
					dataset.Put(core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRUS}, Value: core.Uint16Value{8}})
				}
				for _, tag := range []core.Tag{core.NewTag(0x0028, 0x0010), core.NewTag(0x0028, 0x0011)} {
					dataset.Put(core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRUS}, Value: core.Uint16Value{2}})
				}
				dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0102), VR: core.VRUS}, Value: core.Uint16Value{7}})
				dataset.Put(core.NewRawElement(core.TagPixelData, core.VROB, []byte{0, 64, 128, 255}))
				var buffer bytes.Buffer
				if err := object.WriteFile(&buffer, &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
					t.Fatal(err)
				}
				data = buffer.Bytes()
			}
			path := tempDICOMFile(t, data)
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			var stored dimse.CStoreRequestContext
			done := make(chan error, 1)
			go func() {
				assoc, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "STORESCP", Context: ctx, AcceptAnyAbstractSyntax: true, SupportedTransferSyntaxes: []string{transfer.RLELossless.UID, transfer.JPEGBaseline.UID, transfer.ExplicitVRLittleEndian.UID}})
				if err != nil {
					done <- err
					return
				}
				defer assoc.Close()
				done <- dimse.ServeStorageAssociation(ctx, assoc, dimse.StorageSCPOptions{StoreHandler: dimse.CStoreHandlerFunc(func(_ context.Context, req dimse.CStoreRequestContext) (uint16, error) {
					stored = req
					return dimse.StatusSuccess, nil
				})})
			}()
			args := []string{"-s", "PatientName=First^Synthetic", "-s", "PatientName=Final^Synthetic", "-s", "SOPInstanceUID=1.2.3.987", "-called", "STORESCP"}
			if mode == "rle" {
				args = append(args, "-transcode-to", transfer.RLELossless.UID)
			}
			if mode == "lossy" {
				args = append(args, "-transcode-to", transfer.JPEGBaseline.UID, "-allow-lossy", "-max-pixel-error", "255")
			}
			args = append(args, listener.Addr().String(), path)
			var stdout, stderr bytes.Buffer
			if code := runWithContext(ctx, args, &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if name, _ := stored.DataSet.GetString(core.NewTag(0x0010, 0x0010)); name != "Final^Synthetic" {
				t.Fatalf("wrong coerced name %q", name)
			}
			if uid, _ := stored.DataSet.GetUID(core.NewTag(0x0008, 0x0018)); uid != "1.2.3.987" || stored.Request.AffectedSOPInstanceUID != uid {
				t.Fatal("SOP command identity mismatch")
			}
			parsed, err := object.ReadFile(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if uid, _ := parsed.Dataset.GetUID(core.NewTag(0x0008, 0x0018)); uid != dicomtest.TestSOPInstanceUID {
				t.Fatal("source changed")
			}
			storedBytes, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(storedBytes, data) {
				t.Fatal("source file changed")
			}
			if mode != "native" && !strings.Contains(stdout.String(), "C-STORE representation source=1") {
				t.Fatal("lost transcode report")
			}
			if mode == "lossy" && !strings.Contains(stdout.String(), "C-STORE lossy verification source=1") {
				t.Fatal("lost selected verification report")
			}
			if strings.Contains(stdout.String(), "Final^Synthetic") || strings.Contains(stdout.String(), "1.2.3.987") || strings.Contains(stderr.String(), path) {
				t.Fatal("unsafe diagnostics")
			}
		})
	}
}

func TestRunAttributeCoercionRefusesBeforeConnection(t *testing.T) {
	data, err := dicomtest.ExplicitVRFile()
	if err != nil {
		t.Fatal(err)
	}
	path := tempDICOMFile(t, data)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-s", "PatientName=SECRET", "-coerce-max-bytes", "1", "127.0.0.1:1", path}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stdout.String(), "outcome=failure") || strings.Contains(stderr.String(), "dial") || strings.Contains(stderr.String(), "SECRET") || strings.Contains(stderr.String(), path) {
		t.Fatalf("wrong refusal: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

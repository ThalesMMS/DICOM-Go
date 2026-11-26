package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
	j2kenc "github.com/ThalesMMS/dicom-go/pixeldata/jpeg2000"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type compressedStoreCase struct {
	name string
	file *object.File
}

func compressedStoreCases(t *testing.T) []compressedStoreCase {
	t.Helper()
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := jpegls.RegisterEncoder(encoders); err != nil {
		t.Fatal(err)
	}
	if err := j2kenc.RegisterEncoders(encoders); err != nil {
		t.Fatal(err)
	}
	cases := []compressedStoreCase{
		{"JPEG Lossless", codecfixture.JPEGLosslessSV1RGB8Interleaved().File()},
		{"RLE", codecfixture.RLELosslessSmall().File()},
	}
	for _, syntax := range []transfer.Syntax{transfer.JPEGLSLossless, transfer.JPEG2000LosslessOnly} {
		dataset, _, err := pixeldata.TranscodeDataSet(context.Background(), codecfixture.NativeSmall().Object(), transfer.ExplicitVRLittleEndian, syntax, pixeldata.TranscodeOptions{EncoderRegistry: encoders})
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, compressedStoreCase{syntax.Name, &object.File{Dataset: dataset, TransferSyntax: syntax}})
	}
	for i := range cases {
		file := cases[i].file
		file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{compressedTestSOPClass}})
		file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{fmt.Sprintf("1.2.826.0.1.3680043.10.1284.%d", i+1)}})
		// Read the authored Part 10 envelope once so expected fragments include
		// DICOM even-length padding, without invoking any pixel decoder.
		var encoded bytes.Buffer
		if err := object.WriteFile(&encoded, file); err != nil {
			t.Fatal(err)
		}
		canonical, err := object.ReadFile(bytes.NewReader(encoded.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		cases[i].file = canonical
	}
	return cases
}

func startCompressedStoreSCP(t *testing.T, ctx context.Context, opts options, outDir string) (*ul.Listener, <-chan error) {
	t.Helper()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	done := make(chan error, 1)
	go func() {
		accept := storescpAcceptOptions(opts)
		accept.Context = ctx
		assoc, err := listener.AcceptAssociation(accept)
		if err != nil {
			done <- err
			return
		}
		done <- handleAssociationWithLimits(assoc, outDir, io.Discard, opts.limits)
	}()
	return listener, done
}

func assertCompressedStorePreserved(t *testing.T, path string, source *object.File, bitPreserving bool) {
	t.Helper()
	saved, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer saved.Close()
	if saved.TransferSyntax.UID != source.TransferSyntax.UID {
		t.Fatal("stored transfer syntax changed")
	}
	for _, tag := range []core.Tag{core.NewTag(0x0008, 0x0016), core.NewTag(0x0008, 0x0018)} {
		want, _ := source.Dataset.GetString(tag)
		got, _ := saved.Dataset.GetString(tag)
		if got != want {
			t.Fatal("stored SOP identity changed")
		}
	}
	want, _ := source.Dataset.Get(core.TagPixelData)
	got, ok := saved.Dataset.Get(core.TagPixelData)
	if !ok || !reflect.DeepEqual(got.Value, want.Value) {
		t.Fatal("compressed Pixel Data (BOT/fragments) changed")
	}
	if bitPreserving {
		var expected bytes.Buffer
		if err := object.WriteDataSet(&expected, source.Dataset, source.TransferSyntax); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		header, err := object.ReadPart10Header(bytes.NewReader(data), object.ReadFileOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data[header.DataSetOffset:], expected.Bytes()) {
			t.Fatal("bit-preserving path changed received dataset bytes")
		}
	}
}

func TestStorescpStoresCompressedEnvelopes(t *testing.T) {
	for _, tc := range compressedStoreCases(t) {
		for _, bitPreserving := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/bit-preserving=%t", tc.name, bitPreserving), func(t *testing.T) {
				opts, err := parseArgs(nil, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				opts.limits.bitPreserving = bitPreserving
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				dir := t.TempDir()
				listener, done := startCompressedStoreSCP(t, ctx, opts, dir)
				assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{CalledAETitle: "STORESCP", CallingAETitle: "SYNTHETIC", Contexts: []ul.PresentationContext{{AbstractSyntaxUID: compressedTestSOPClass, TransferSyntaxUIDs: []string{tc.file.TransferSyntax.UID}}}})
				if err != nil {
					t.Fatal(err)
				}
				defer assoc.Close()
				pc, err := dimse.AcceptedContextForSOPClass(assoc, compressedTestSOPClass)
				if err != nil {
					t.Fatal(err)
				}
				uid, _ := tc.file.Dataset.GetString(core.NewTag(0x0008, 0x0018))
				if err := dimse.SendCStoreRequest(assoc, pc.ID, dimse.CStoreRequest{AffectedSOPClassUID: compressedTestSOPClass, AffectedSOPInstanceUID: uid, MessageID: 1}); err != nil {
					t.Fatal(err)
				}
				if err := dimse.SendDataSet(assoc, pc.ID, tc.file.Dataset, tc.file.TransferSyntax); err != nil {
					t.Fatal(err)
				}
				rsp, err := dimse.ReceiveCStoreResponse(assoc, pc.ID)
				if err != nil || rsp.Status != dimse.StatusSuccess {
					t.Fatalf("compressed C-STORE status=%+v err=%v", rsp, err)
				}
				if err := assoc.Release(ctx); err != nil {
					t.Fatal(err)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				assertCompressedStorePreserved(t, filepath.Join(dir, uid+".dcm"), tc.file, bitPreserving)
			})
		}
	}
}

func TestStorescpPromiscuousCompressedStillChecksIdentityAndLimits(t *testing.T) {
	for _, bitPreserving := range []bool{false, true} {
		for _, failure := range []string{"identity", "pixel-limit", "valid"} {
			t.Run(fmt.Sprintf("%s/bit-preserving=%t", failure, bitPreserving), func(t *testing.T) {
				opts, err := parseArgs([]string{"-promiscuous"}, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				opts.limits.bitPreserving = bitPreserving
				wantStatus := uint16(statusDataSetDoesNotMatch)
				if failure == "valid" {
					wantStatus = dimse.StatusSuccess
				}
				if failure == "pixel-limit" {
					opts.limits.maxPixelDataBytes = 1
					wantStatus = statusOutOfResources
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				dir := t.TempDir()
				listener, done := startCompressedStoreSCP(t, ctx, opts, dir)
				file := compressedStoreCases(t)[1].file
				const unknownSOP = "1.2.826.0.1.3680043.10.1284.999"
				file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{unknownSOP}})
				uid, _ := file.Dataset.GetString(core.NewTag(0x0008, 0x0018))
				commandUID := uid
				if failure == "identity" {
					commandUID += ".99"
				}
				assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{CalledAETitle: "STORESCP", CallingAETitle: "SYNTHETIC", Contexts: []ul.PresentationContext{{AbstractSyntaxUID: unknownSOP, TransferSyntaxUIDs: []string{file.TransferSyntax.UID}}}})
				if err != nil {
					t.Fatal(err)
				}
				defer assoc.Close()
				pc := assoc.AcceptedContexts[0]
				if err := dimse.SendCStoreRequest(assoc, pc.ID, dimse.CStoreRequest{AffectedSOPClassUID: unknownSOP, AffectedSOPInstanceUID: commandUID, MessageID: 1}); err != nil {
					t.Fatal(err)
				}
				if err := dimse.SendDataSet(assoc, pc.ID, file.Dataset, file.TransferSyntax); err != nil {
					t.Fatal(err)
				}
				rsp, err := dimse.ReceiveCStoreResponse(assoc, pc.ID)
				if err != nil || rsp.Status != wantStatus {
					t.Fatalf("refusal status=%+v err=%v", rsp, err)
				}
				if failure != "pixel-limit" || bitPreserving {
					if err := assoc.Release(ctx); err != nil {
						t.Fatal(err)
					}
				} else {
					assoc.Close()
				}
				serverErr := <-done
				if serverErr != nil && !errors.Is(serverErr, parser.ErrMaxPixelDataBytesExceeded) {
					t.Fatalf("server error=%v", serverErr)
				}
				wantFiles := 0
				if failure == "valid" {
					wantFiles = 1
					assertCompressedStorePreserved(t, filepath.Join(dir, uid+".dcm"), file, bitPreserving)
				}
				if entries, err := os.ReadDir(dir); err != nil || len(entries) != wantFiles {
					t.Fatal("unexpected files after compressed store")
				}
			})
		}
	}
}

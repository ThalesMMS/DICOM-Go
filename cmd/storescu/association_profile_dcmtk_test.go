package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestStorescuAssociationProfileDCMTKExplicitTranscodeSocket(t *testing.T) {
	tool := dcmtkoracle.Tool(t, "storescp")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	data, err := dicomtest.NewFileBuilder().AddElements(dicomtest.DatasetWithPixelData()...).Build()
	if err != nil {
		t.Fatal(err)
	}
	file, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	ctUID := "1.2.840.10008.5.1.4.1.1.2"
	for _, target := range []struct {
		obj *object.Object
		tag core.Tag
	}{{file.Dataset, core.NewTag(0x0008, 0x0016)}, {file.Meta, core.NewTag(0x0002, 0x0002)}} {
		target.obj.Put(core.Element{Header: core.ElementHeader{Tag: target.tag, VR: core.VRUI}, Value: core.StringValue{ctUID}})
	}
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "synthetic.dcm")
	if err := os.WriteFile(sourcePath, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	profile := strings.Replace(requestorProfileFixture, "PresentationContext1 = CTImageStorage\\R\nPresentationContext2 = CTImageStorage\\N", "PresentationContext1 = CTImageStorage\\R", 1)
	configPath := writeRequestorProfile(t, profile)
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserve.Addr().String()
	_, port, _ := net.SplitHostPort(address)
	reserve.Close()
	outDir := t.TempDir()
	cmd := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, tool, "-aet", "PROFILE", "-xf", configPath, "P", "-od", outDir, port)
	var oracleOutput bytes.Buffer
	cmd.Stdout = &oracleOutput
	cmd.Stderr = &oracleOutput
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
		if t.Failed() {
			t.Log("DCMTK synthetic store peer:", oracleOutput.String())
		}
	})
	ready := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			ready = true
			break
		}
		select {
		case <-done:
			t.Fatal("DCMTK profile SCP exited", oracleOutput.String())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("oracle readiness")
	}
	opts, err := parseArgs([]string{"-config-file", configPath, "-profile", "P", "-called", "PROFILE", "-transcode-to", transfer.RLELossless.UID, address, sourcePath}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runStoreWithContext(ctx, opts, &output, io.Discard); err != nil {
		t.Fatal("explicit representation preparation failed", err, output.String())
	}
	if !strings.Contains(output.String(), "selected="+transfer.RLELossless.UID) {
		t.Fatal("RLE-only profile selected a hidden fallback")
	}
	var storedPath string
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(outDir)
		if err == nil && len(entries) == 1 {
			storedPath = filepath.Join(outDir, entries[0].Name())
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if storedPath == "" {
		t.Fatal("remote synthetic store missing")
	}
	stored, err := object.OpenFile(storedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer stored.Close()
	if stored.TransferSyntax.UID != transfer.RLELossless.UID {
		t.Fatal("stored transfer syntax")
	}
	transcodeOptions, err := storeTranscodeOptions(opts)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := pixeldata.NewFrameReader(ctx, stored, nil, pixeldata.FrameReaderOptions{Registry: transcodeOptions.Transcode.DecoderRegistry})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	frame, _, err := reader.Frame(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	original, err := pixeldata.NewFrameReader(ctx, file, nil, pixeldata.FrameReaderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	want, _, err := original.Frame(ctx, 0)
	if err != nil || !bytes.Equal(frame, want) {
		t.Fatal("lossless decoded samples", err)
	}
}

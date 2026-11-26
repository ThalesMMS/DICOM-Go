package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func extractedTestIdentifier() *object.Object {
	return object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: core.VRPN}, Value: core.StringValue{"SYNTHETIC^FIND"}}, {Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0010), VR: core.VRUS}, Value: core.Uint16Value{42}}}, std.Dictionary)
}
func TestExtractRawByteEquivalentWithoutSOPIdentity(t *testing.T) {
	dir := t.TempDir()
	x, err := newFindExtractor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	id := extractedTestIdentifier()
	if err := x.Write(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "rsp000001.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := object.WriteDataSet(&want, id, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatal("extract changed Identifier bytes")
	}
	read, err := object.ReadDataSet(bytes.NewReader(got), transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	if read.Has(core.NewTag(0x0008, 0x0018)) || read.Has(core.NewTag(0x0002, 0x0002)) {
		t.Fatal("fabricated SOP identity or File Meta")
	}
	if n, err := read.GetInt(core.NewTag(0x0028, 0x0010)); err != nil || n != 42 {
		t.Fatalf("numeric readback=%d/%v", n, err)
	}
}
func TestExtractCollisionCancellationLimitsLeaveNoTemporary(t *testing.T) {
	for _, mode := range []string{"collision", "cancel", "limit"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			x, err := newFindExtractor(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer x.Close()
			ctx := context.Background()
			if mode == "collision" {
				if err := os.WriteFile(filepath.Join(dir, "rsp000001.dcm"), []byte("preserved"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "cancel" {
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			if mode == "limit" {
				x.bytes = maxExtractTotalBytes - 1
			}
			if err := x.Write(ctx, extractedTestIdentifier()); err == nil {
				t.Fatal("expected extraction failure")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if mode == "collision" {
				want = 1
				data, _ := os.ReadFile(filepath.Join(dir, "rsp000001.dcm"))
				if string(data) != "preserved" {
					t.Fatal("overwrote collision")
				}
			}
			if len(entries) != want {
				t.Fatalf("temporary/published files after error=%v", entries)
			}
		})
	}
	writer := &extractWriter{ctx: context.Background(), dst: shortExtractWriter{}, limit: 1024}
	if _, err := writer.Write([]byte("abcd")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write=%v", err)
	}
}

type shortExtractWriter struct{}

func (shortExtractWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

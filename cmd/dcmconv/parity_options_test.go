package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestConvParityRemovesPrivateTagsRealFile(t *testing.T) {
	input, pixels := writeImage(t)
	file, err := object.OpenFile(input)
	if err != nil {
		t.Fatal(err)
	}
	creator := core.NewTag(0x0029, 0x0010)
	private := core.NewTag(0x0029, 0x1010)
	file.Dataset.Put(core.NewRawElement(creator, core.VRLO, []byte("SYNTHETIC ")))
	file.Dataset.Put(core.NewRawElement(private, core.VROB, []byte{11, 22, 33, 44}))
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if err := os.WriteFile(input, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "filtered.dcm")
	code, _, stderr := runConv(t, "-ts", "explicit-le", "-remove-private-tags", "-o", output, input)
	if code != 0 {
		t.Fatalf("private flag refused real synthetic file: exit=%d stderr=%s", code, stderr)
	}
	result, err := object.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	if _, found := result.Dataset.Get(creator); found {
		t.Fatal("private creator retained")
	}
	if _, found := result.Dataset.Get(private); found {
		t.Fatal("private value retained")
	}
	got, _ := result.GetRaw(core.TagPixelData)
	if !bytes.Equal(got, pixels) {
		t.Fatal("unselected pixel bytes changed")
	}
	after, err := os.ReadFile(input)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("original source modified")
	}
}

func TestConvParityGeneratesIconRealFile(t *testing.T) {
	input, pixels := writeImage(t)
	output := filepath.Join(t.TempDir(), "icon.dcm")
	code, _, stderr := runConv(t, "-ts", "explicit-le", "-generate-icon", "-o", output, input)
	if code != 0 {
		t.Fatalf("icon flag refused real synthetic file: exit=%d stderr=%s", code, stderr)
	}
	file, err := object.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	items, ok := file.Dataset.GetSequence(core.NewTag(0x0088, 0x0200))
	if !ok || len(items) != 1 {
		t.Fatalf("generated icon items=%d found=%v", len(items), ok)
	}
	if rows, ok := items[0].GetRaw(core.NewTag(0x0028, 0x0010)); !ok || len(rows) != 2 || binary.LittleEndian.Uint16(rows) != 64 {
		t.Fatalf("icon rows=%v found=%v", rows, ok)
	}
	if columns, ok := items[0].GetRaw(core.NewTag(0x0028, 0x0011)); !ok || len(columns) != 2 || binary.LittleEndian.Uint16(columns) != 64 {
		t.Fatalf("icon columns=%v found=%v", columns, ok)
	}
	icon, ok := items[0].GetRaw(core.TagPixelData)
	if !ok || len(icon) != 64*64 {
		t.Fatalf("icon bytes=%d found=%v", len(icon), ok)
	}
	got, _ := file.GetRaw(core.TagPixelData)
	if !bytes.Equal(got, pixels) {
		t.Fatal("main stored pixels changed during icon generation")
	}
}

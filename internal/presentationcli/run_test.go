package presentationcli

import (
	"bytes"
	"context"
	"encoding/binary"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/wrappercli"
	"github.com/ThalesMMS/dicom-go/object"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestGSPSCreationAndActualPGMSamples(t *testing.T) {
	dir := t.TempDir()
	input, gs, pgm := filepath.Join(dir, "image.dcm"), filepath.Join(dir, "state.dcm"), filepath.Join(dir, "image.pgm")
	text := func(tag core.Tag, vr core.VR, value string) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{value}}
	}
	elements := []core.Element{text(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.2"), text(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.3.4"), text(core.NewTag(0x0020, 0x000D), core.VRUI, "1.2.3"), text(core.NewTag(0x0020, 0x000E), core.VRUI, "1.2.3.5"), text(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"), object.USElement(core.NewTag(0x0028, 0x0010), binary.LittleEndian, 2), object.USElement(core.NewTag(0x0028, 0x0011), binary.LittleEndian, 2), object.USElement(core.NewTag(0x0028, 0x0002), binary.LittleEndian, 1), object.USElement(core.NewTag(0x0028, 0x0100), binary.LittleEndian, 8), object.USElement(core.NewTag(0x0028, 0x0101), binary.LittleEndian, 8), object.USElement(core.NewTag(0x0028, 0x0102), binary.LittleEndian, 7), object.USElement(core.NewTag(0x0028, 0x0103), binary.LittleEndian, 0), text(core.NewTag(0x0028, 0x1050), core.VRDS, "128"), text(core.NewTag(0x0028, 0x1051), core.VRDS, "256"), core.NewRawElement(core.TagPixelData, core.VROB, []byte{0, 85, 170, 255})}
	file, err := wrappercli.DatasetFile(object.FromElements(elements, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err = wrappercli.WriteFile(input, file); err != nil {
		t.Fatal(err)
	}
	if err = Execute(context.Background(), "dcmpsmk", []string{input, gs}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err = Execute(context.Background(), "dcmp2pgm", []string{"-presentation-state", gs, input, pgm}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(pgm)
	want := append([]byte("P5\n2 2\n255\n"), 0, 85, 170, 255)
	if !bytes.Equal(got, want) {
		t.Fatalf("PGM samples %v want %v", got, want)
	}
	if err = Execute(context.Background(), "dcmp2pgm", []string{"-frame", "2", input, filepath.Join(dir, "invalid")}, io.Discard, io.Discard); err == nil {
		t.Fatal("missing frame accepted")
	}
	if _, err = os.Stat(filepath.Join(dir, "invalid")); !os.IsNotExist(err) {
		t.Fatal("partial output published")
	}
}

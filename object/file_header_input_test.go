package object

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Independent Part10 wire construction: neither object.WriteFile nor the
// parser's encoder is used as the cursor/value-location oracle.
func headerInputPart10Fixture(syntax transfer.Syntax) ([]byte, []byte, int64) {
	encode := func(order binary.ByteOrder, tag core.Tag, vr string, value []byte) []byte {
		long := vr == "OB"
		length := 8
		if long {
			length = 12
		}
		wire := make([]byte, length)
		order.PutUint16(wire, tag.Group)
		order.PutUint16(wire[2:], tag.Element)
		copy(wire[4:], vr)
		if long {
			order.PutUint32(wire[8:], uint32(len(value)))
		} else {
			order.PutUint16(wire[6:], uint16(len(value)))
		}
		return append(wire, value...)
	}
	uid := []byte(syntax.UID)
	if len(uid)%2 != 0 {
		uid = append(uid, 0)
	}
	meta := encode(binary.LittleEndian, core.NewTag(0x0002, 0x0010), "UI", uid)
	group := make([]byte, 4)
	binary.LittleEndian.PutUint32(group, uint32(len(meta)))
	p10 := append(make([]byte, 128), 'D', 'I', 'C', 'M')
	p10 = append(p10, encode(binary.LittleEndian, core.NewTag(0x0002, 0x0000), "UL", group)...)
	p10 = append(p10, meta...)
	offset := int64(len(p10))
	p10 = append(p10, encode(syntax.ByteOrder, core.NewTag(0x0008, 0x103e), "LO", []byte("AB"))...)
	payload := bytes.Repeat([]byte{0x6d}, 4096)
	p10 = append(p10, encode(syntax.ByteOrder, core.TagPixelData, "OB", payload)...)
	p10 = append(p10, encode(syntax.ByteOrder, core.NewTag(0xfffa, 0x0010), "LO", []byte("CD"))...)
	return p10, payload, offset
}

func TestPart10HeaderInputFileReplayWithNonzeroSourceBase(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		wire, payload, offset := headerInputPart10Fixture(syntax)
		const prefix = 29
		wire = append(make([]byte, prefix), wire...)
		path := filepath.Join(t.TempDir(), "nonphi.dcm")
		if err := os.WriteFile(path, wire, 0600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.Seek(prefix, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		header, err := ReadPart10Header(f, ReadFileOptions{})
		pos, _ := f.Seek(0, io.SeekCurrent)
		if err != nil || header.DataSetOffset != prefix+offset || pos != prefix+offset {
			t.Fatalf("Part10 header cursor=%d offset=%d err=%v", pos, header.DataSetOffset, err)
		}
		if _, err := f.Seek(prefix, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		file, err := ReadFileWithOptions(f, ReadFileOptions{DeferPixelData: true})
		if err != nil {
			t.Fatal(err)
		}
		locations := file.ValueLocations(core.TagPixelData)
		if len(locations) != 1 || locations[0].ValueOffset != offset+22 || locations[0].Length != int64(len(payload)) {
			t.Fatalf("locations=%+v", locations)
		}
		before, _ := f.Seek(0, io.SeekCurrent)
		var copied bytes.Buffer
		n, err := file.Dataset.CopyValueTo(core.TagPixelData, &copied)
		after, _ := f.Seek(0, io.SeekCurrent)
		if err != nil || n != int64(len(payload)) || !bytes.Equal(copied.Bytes(), payload) || before != after || after != int64(len(wire)) {
			t.Fatalf("replay n=%d error=%v cursor=%d/%d", n, err, before, after)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Seek(0, io.SeekCurrent); err != nil {
			t.Fatalf("ReadFile closed caller source: %v", err)
		}
	}
}

func TestPart10HeaderInputOwnedFileCloseAndBudget(t *testing.T) {
	wire, payload, _ := headerInputPart10Fixture(transfer.ExplicitVRLittleEndian)
	path := filepath.Join(t.TempDir(), "nonphi.dcm")
	if err := os.WriteFile(path, wire, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := OpenFileWithOptions(path, ReadFileOptions{DeferPixelData: true})
	if err != nil {
		t.Fatal(err)
	}
	if file.source == nil {
		t.Fatal("deferred owned source not retained")
	}
	var out bytes.Buffer
	if _, err := file.Dataset.CopyValueTo(core.TagPixelData, &out); err != nil || !bytes.Equal(out.Bytes(), payload) {
		t.Fatalf("replay=%v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Dataset.CopyValueTo(core.TagPixelData, io.Discard); err == nil {
		t.Fatal("replay succeeded after closing owned source")
	}
	_, err = OpenFileWithOptions(path, ReadFileOptions{SkipPixelData: true, MaxTotalBytes: int64(len(wire) - 1)})
	if err == nil {
		t.Fatal("late total budget accepted")
	}
	if !errors.Is(err, parser.ErrMaxTotalBytesExceeded) {
		t.Fatalf("late budget identity changed: %v", err)
	}
}

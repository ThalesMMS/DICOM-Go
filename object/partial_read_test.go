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
	"github.com/ThalesMMS/dicom-go/transfer"
)

func explicitElementBytes(g, e uint16, vr string, value []byte) []byte {
	long := vr == "OB" || vr == "OW" || vr == "SQ" || vr == "UN" || vr == "UT"
	var b []byte
	b = binary.LittleEndian.AppendUint16(b, g)
	b = binary.LittleEndian.AppendUint16(b, e)
	b = append(b, vr...)
	if long {
		b = append(b, 0, 0)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(value)))
	} else {
		b = binary.LittleEndian.AppendUint16(b, uint16(len(value)))
	}
	return append(b, value...)
}

// partialFixture returns a complete Explicit VR Little Endian Part 10 file
// with two data set elements and its length.
func partialFixture(t *testing.T) ([]byte, int) {
	t.Helper()
	obj := New(nil)
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}})
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.3.4"}})
	var buf bytes.Buffer
	if err := WriteFile(&buf, &File{Dataset: obj, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), buf.Len()
}

func TestPartialReadAtEachElementKind(t *testing.T) {
	base, _ := partialFixture(t)
	item := explicitElementBytes(0x0008, 0x1150, "UI", []byte("1.2\x00"))
	cases := []struct {
		name string
		tail []byte
	}{
		{"short value", explicitElementBytes(0x0010, 0x0010, "PN", []byte("ABCD"))[:10]},
		{"long value", explicitElementBytes(0x0009, 0x1001, "OB", []byte{1, 2, 3, 4, 5, 6})[:14]},
		{"header", []byte{0x10, 0x00, 0x20}},
		{"open sequence", append(append(explicitElementBytes(0x0008, 0x1140, "SQ", nil)[:8], 0xff, 0xff, 0xff, 0xff), append([]byte{0xfe, 0xff, 0x00, 0xe0, 0xff, 0xff, 0xff, 0xff}, item...)...)},
		{"pixel data fragment", append(append(explicitElementBytes(0x7fe0, 0x0010, "OB", nil)[:8], 0xff, 0xff, 0xff, 0xff),
			0xfe, 0xff, 0x00, 0xe0, 0, 0, 0, 0, 0xfe, 0xff, 0x00, 0xe0, 16, 0, 0, 0, 1, 2)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := append(append([]byte(nil), base...), c.tail...)
			if _, err := ReadFileWithOptions(bytes.NewReader(data), ReadFileOptions{}); err == nil || errors.Is(err, ErrPartialRead) {
				t.Fatalf("strict error = %v", err)
			}
			for _, src := range []io.Reader{bytes.NewReader(data), io.MultiReader(bytes.NewReader(data))} {
				f, err := ReadFileWithOptions(src, ReadFileOptions{AllowPartialRead: true})
				var pe *PartialReadError
				if !errors.Is(err, ErrPartialRead) || !errors.Is(err, ErrDataSet) || !errors.As(err, &pe) || f == nil {
					t.Fatalf("partial error = %v, file %v", err, f != nil)
				}
				read, ok := f.PartialRead()
				if !ok || read.Elements != 2 || read.Err == nil {
					t.Fatalf("partial read = %+v", read)
				}
				wantRetained := 2
				if c.name == "open sequence" {
					wantRetained = 3
					if len(read.Containers) != 2 || !read.Containers[0].Retained || !read.Containers[1].Retained {
						t.Fatalf("open sequence prefixes not marked: %+v", read.Containers)
					}
				}
				if read.RetainedElements != wantRetained || len(f.Dataset.Elements()) != wantRetained {
					t.Fatalf("kept %d elements", len(f.Dataset.Elements()))
				}
				if err := WriteFile(io.Discard, f); !errors.Is(err, ErrPartialFile) {
					t.Fatalf("write error = %v", err)
				}
				if err := WriteFileWithOptions(io.Discard, f, WriteFileOptions{AllowPartial: true}); err != nil {
					t.Fatalf("confirmed write: %v", err)
				}
			}
		})
	}
	// A complete file is not partial with the option on.
	f, err := ReadFileWithOptions(bytes.NewReader(base), ReadFileOptions{AllowPartialRead: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, partial := f.PartialRead(); partial {
		t.Fatal("complete file reported as partial")
	}
}

// A partial File opened from a path keeps its source for deferred values.
func TestOpenFilePartialKeepsDeferredValues(t *testing.T) {
	base, _ := partialFixture(t)
	want := bytes.Repeat([]byte{0xab}, 128)
	data := append(append([]byte(nil), base...), explicitElementBytes(0x0009, 0x1001, "OB", want)...)
	data = append(data, explicitElementBytes(0x0010, 0x0010, "PN", []byte("ABCD"))[:10]...)
	path := filepath.Join(t.TempDir(), "partial.dcm")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := OpenFileWithOptions(path, ReadFileOptions{AllowPartialRead: true, InlineValueBytesThreshold: 64})
	if !errors.Is(err, ErrPartialRead) || f == nil {
		t.Fatalf("partial open: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	var got bytes.Buffer
	if _, err := f.Dataset.CopyValueTo(core.NewTag(0x0009, 0x1001), &got); err != nil || !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("deferred value: %v % X", err, got.Bytes())
	}
}

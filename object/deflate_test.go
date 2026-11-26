package object

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"context"
	"encoding/binary"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

// zlibDeflatedFixture writes a Deflated Part 10 file and returns it, the
// offset of the data set and the same file with the data set rewrapped in a
// zlib stream.
func zlibDeflatedFixture(t *testing.T, name string) (raw []byte, offset int, wrapped []byte) {
	t.Helper()
	obj := New(nil)
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}})
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.3.4"}})
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: core.VRPN}, Value: core.StringValue{name}})
	var buf bytes.Buffer
	if err := WriteFile(&buf, &File{Dataset: obj, TransferSyntax: transfer.DeflatedExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	raw = buf.Bytes()
	offset = 132 + 12 + int(binary.LittleEndian.Uint32(raw[140:144]))
	plain, err := io.ReadAll(flate.NewReader(bytes.NewReader(raw[offset:])))
	if err != nil {
		t.Fatal(err)
	}
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	_, _ = zw.Write(plain)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	wrapped = append(append([]byte(nil), raw[:offset]...), z.Bytes()...)
	return raw, offset, wrapped
}

func TestZlibDeflatedDataSet(t *testing.T) {
	raw, offset, wrapped := zlibDeflatedFixture(t, "ZLIB^TEST")
	if _, err := ReadFileWithOptions(bytes.NewReader(wrapped), ReadFileOptions{}); err == nil {
		t.Fatal("strict read accepted a zlib-wrapped data set")
	}
	for _, src := range []io.Reader{bytes.NewReader(wrapped), io.MultiReader(bytes.NewReader(wrapped))} {
		f, err := ReadFileWithOptions(src, ReadFileOptions{AllowZlibDeflate: true})
		if err != nil {
			t.Fatal(err)
		}
		if name, _ := f.Dataset.GetString(core.NewTag(0x0010, 0x0010)); name != "ZLIB^TEST" {
			t.Fatalf("patient name %q", name)
		}
		recs, _ := f.Dataset.BoundaryRecoveries()
		if len(recs) != 1 || recs[0].Reason != parser.BoundaryZlibDeflate || recs[0].Offset != int64(offset) {
			t.Fatalf("recoveries %+v", recs)
		}
		// Writing produces raw deflate again.
		var out bytes.Buffer
		if err := WriteFile(&out, f); err != nil {
			t.Fatal(err)
		}
		if isZlibHeader(out.Bytes()[offset:]) {
			t.Fatal("writer produced a zlib header")
		}
	}
	// Raw deflate with the option on is read without a report.
	f, err := ReadFileWithOptions(bytes.NewReader(raw), ReadFileOptions{AllowZlibDeflate: true})
	if err != nil {
		t.Fatal(err)
	}
	if recs, _ := f.Dataset.BoundaryRecoveries(); len(recs) != 0 {
		t.Fatalf("raw deflate reported %+v", recs)
	}
	// A corrupt Adler-32 trailer is an error.
	bad := append([]byte(nil), wrapped...)
	bad[len(bad)-1] ^= 0xff
	if _, err := ReadFileWithOptions(bytes.NewReader(bad), ReadFileOptions{AllowZlibDeflate: true}); err == nil {
		t.Fatal("corrupt Adler-32 accepted")
	}
	// Data set level readers honor the option too.
	obj, err := ReadDataSetWithOptions(bytes.NewReader(wrapped[offset:]), transfer.DeflatedExplicitVRLittleEndian, ReadFileOptions{AllowZlibDeflate: true})
	if recs, _ := obj.BoundaryRecoveries(); err != nil || len(recs) != 1 {
		t.Fatalf("ReadDataSetWithOptions: %v", err)
	}
	obj, _, err = ReadDataSetWithValidation(context.Background(), bytes.NewReader(wrapped[offset:]), transfer.DeflatedExplicitVRLittleEndian, ReadFileOptions{AllowZlibDeflate: true}, validation.Options{})
	if err != nil {
		t.Fatalf("ReadDataSetWithValidation: %v", err)
	}
	if recs, _ := obj.BoundaryRecoveries(); len(recs) != 1 {
		t.Fatalf("ReadDataSetWithValidation recoveries %+v", recs)
	}
}

func TestRawDeflatedReadsRemainIndependentAfterCorruption(t *testing.T) {
	const firstName, secondName = "FIRST^TEST", "OTHER^TEST"
	firstRaw, offset, _ := zlibDeflatedFixture(t, firstName)
	secondRaw, _, _ := zlibDeflatedFixture(t, secondName)
	// Final block with the reserved block type 3.
	corrupt := append(bytes.Clone(firstRaw[:offset]), 0x07)
	for name, reader := range map[string]func([]byte) io.Reader{
		"seekable": func(wire []byte) io.Reader { return bytes.NewReader(wire) },
		"stream":   func(wire []byte) io.Reader { return io.MultiReader(bytes.NewReader(wire)) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			first, err := ReadFile(reader(firstRaw))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ReadFile(reader(corrupt)); err == nil {
				t.Fatal("corrupt raw Deflate stream accepted")
			}
			second, err := ReadFile(reader(secondRaw))
			if err != nil {
				t.Fatal(err)
			}
			for _, result := range []struct {
				file *File
				name string
			}{{first, firstName}, {second, secondName}} {
				if name, ok := result.file.Dataset.GetString(core.NewTag(0x0010, 0x0010)); !ok || name != result.name {
					t.Fatalf("patient name %q, present = %v, want %q", name, ok, result.name)
				}
			}
		})
	}
}

func TestIsZlibHeader(t *testing.T) {
	for _, c := range []struct {
		head []byte
		want bool
	}{
		{[]byte{0x78, 0x9c}, true},
		{[]byte{0x78, 0x01}, true},
		{[]byte{0x78, 0xda}, true},
		{[]byte{0x78, 0xbb}, false}, // FDICT set
		{[]byte{0x78, 0x9d}, false}, // bad FCHECK
		{[]byte{0x88, 0x98}, false}, // window above 32 KiB
		{[]byte{0x79, 0x9c}, false}, // not deflate
		{[]byte{0x78}, false},
	} {
		if got := isZlibHeader(c.head); got != c.want {
			t.Errorf("% X: %v, want %v", c.head, got, c.want)
		}
	}
}

func FuzzZlibDeflatedDataSet(f *testing.F) {
	f.Add([]byte{0x78, 0x9c, 0x03, 0x00, 0x00, 0x00, 0x00, 0x01})
	f.Add([]byte{0x78, 0x9c, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ReadDataSetWithOptions(bytes.NewReader(data), transfer.DeflatedExplicitVRLittleEndian, ReadFileOptions{AllowZlibDeflate: true, MaxElements: 1024, MaxTotalBytes: 1 << 20})
	})
}

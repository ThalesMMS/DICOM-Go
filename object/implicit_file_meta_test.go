package object

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/parser"
)

func implicitElement(group, element uint16, value []byte) []byte {
	if len(value)%2 != 0 {
		value = append(append([]byte(nil), value...), 0)
	}
	b := make([]byte, 8, 8+len(value))
	binary.LittleEndian.PutUint16(b[0:], group)
	binary.LittleEndian.PutUint16(b[2:], element)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(value)))
	return append(b, value...)
}

// implicitMetaFile writes a Part 10 file whose group 0002 is Implicit VR
// Little Endian, followed by an Implicit VR Little Endian dataset.
func implicitMetaFile(preamble bool) []byte {
	var meta []byte
	meta = append(meta, implicitElement(0x0002, 0x0001, []byte{0, 1})...)
	meta = append(meta, implicitElement(0x0002, 0x0002, []byte("1.2.840.10008.5.1.4.1.1.7"))...)
	meta = append(meta, implicitElement(0x0002, 0x0003, []byte("1.2.3.4"))...)
	meta = append(meta, implicitElement(0x0002, 0x0010, []byte("1.2.840.10008.1.2"))...)
	length := make([]byte, 4)
	binary.LittleEndian.PutUint32(length, uint32(len(meta)))
	meta = append(implicitElement(0x0002, 0x0000, length), meta...)
	var out []byte
	if preamble {
		out = append(make([]byte, 128), "DICM"...)
	}
	out = append(out, meta...)
	out = append(out, implicitElement(0x0008, 0x0016, []byte("1.2.840.10008.5.1.4.1.1.7"))...)
	out = append(out, implicitElement(0x0010, 0x0020, []byte("ID12"))...)
	return out
}

func TestImplicitVRFileMetaIsOptIn(t *testing.T) {
	data := implicitMetaFile(true)
	if _, err := ReadFileWithOptions(bytes.NewReader(data), ReadFileOptions{}); !errors.Is(err, ErrFileMeta) {
		t.Fatalf("strict error = %v, want ErrFileMeta", err)
	}
	for name, src := range map[string]io.Reader{
		"seekable":     bytes.NewReader(data),
		"non-seekable": io.MultiReader(bytes.NewReader(data)),
	} {
		f, err := ReadFileWithOptions(src, ReadFileOptions{AllowImplicitVRFileMeta: true})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if f.TransferSyntax.UID != "1.2.840.10008.1.2" {
			t.Fatalf("%s: syntax %s", name, f.TransferSyntax.UID)
		}
		if v, _ := f.Dataset.GetString(core.NewTag(0x0010, 0x0020)); v != "ID12" {
			t.Fatalf("%s: dataset value %q", name, v)
		}
		recs, _ := f.Meta.BoundaryRecoveries()
		if len(recs) != 1 || recs[0].Reason != parser.BoundaryImplicitVRFileMeta || recs[0].Offset != 132 {
			t.Fatalf("%s: recoveries = %+v", name, recs)
		}
		// Rewriting produces conforming Explicit VR File Meta Information.
		var buf bytes.Buffer
		if err := WriteFile(&buf, f); err != nil {
			t.Fatalf("%s: write: %v", name, err)
		}
		again, err := ReadFileWithOptions(bytes.NewReader(buf.Bytes()), ReadFileOptions{})
		if err != nil {
			t.Fatalf("%s: strict re-read: %v", name, err)
		}
		if recs, _ := again.Meta.BoundaryRecoveries(); len(recs) != 0 {
			t.Fatalf("%s: rewritten file still reports %+v", name, recs)
		}
	}
	// An explicit File Meta is never reported as a recovery.
	f, err := ReadFileWithOptions(bytes.NewReader(explicitMetaCopy(t, data)), ReadFileOptions{AllowImplicitVRFileMeta: true})
	if err != nil {
		t.Fatal(err)
	}
	if recs, _ := f.Meta.BoundaryRecoveries(); len(recs) != 0 {
		t.Fatalf("explicit meta reported %+v", recs)
	}
}

func explicitMetaCopy(t *testing.T, implicit []byte) []byte {
	t.Helper()
	f, err := ReadFileWithOptions(bytes.NewReader(implicit), ReadFileOptions{AllowImplicitVRFileMeta: true})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteFile(&buf, f); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImplicitVRFileMetaWithoutPreamble(t *testing.T) {
	data := implicitMetaFile(false)
	f, res, err := ReadFileWithTransferSyntaxRecovery(bytes.NewReader(data),
		ReadFileOptions{AllowImplicitVRFileMeta: true}, TransferSyntaxRecoveryOptions{AllowMissingPreamble: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != TransferSyntaxSourceDeclaredMissingPreamble {
		t.Fatalf("resolution = %s", res.Source)
	}
	if recs, _ := f.Meta.BoundaryRecoveries(); len(recs) != 1 {
		t.Fatalf("recoveries = %+v", recs)
	}
}

// FuzzImplicitVRFileMeta mixes Implicit and Explicit VR group 0002 headers:
// reading never panics, and a recovered read always reports the recovery.
func FuzzImplicitVRFileMeta(f *testing.F) {
	f.Add(implicitMetaFile(true), uint8(0))
	f.Add(implicitMetaFile(true), uint8(40))
	f.Add(explicitMetaCopyForFuzz(), uint8(7))
	f.Fuzz(func(t *testing.T, data []byte, flip uint8) {
		if len(data) > 4096 {
			data = data[:4096]
		}
		data = append([]byte(nil), data...)
		if len(data) > 140 {
			i := 132 + int(flip)%(len(data)-132)
			data[i] ^= 0x41 // turn VR letters into length bytes and back
		}
		f, err := ReadFileWithOptions(bytes.NewReader(data), ReadFileOptions{AllowImplicitVRFileMeta: true, MaxTotalBytes: 1 << 20})
		if err != nil || f.Meta == nil {
			return
		}
		recs, _ := f.Meta.BoundaryRecoveries()
		if len(data) >= 138 && implicitVRFileMetaHead(data[132:138]) && len(recs) == 0 {
			t.Fatal("implicit File Meta read without a reported recovery")
		}
	})
}

func explicitMetaCopyForFuzz() []byte {
	f, err := ReadFileWithOptions(bytes.NewReader(implicitMetaFile(true)), ReadFileOptions{AllowImplicitVRFileMeta: true})
	if err != nil {
		panic(err)
	}
	var buf bytes.Buffer
	if err := WriteFile(&buf, f); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

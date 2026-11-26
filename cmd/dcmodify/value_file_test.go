package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestDcmodifyValueFileRealByteOrderAndCharset(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		t.Run(syntax.UID, func(t *testing.T) {
			dir := t.TempDir()
			input := dc1353Input(t, dir, "in.dcm", syntax, core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0005), VR: core.VRCS}, Value: core.StringValue{"ISO_IR 100"}})
			rows := filepath.Join(dir, "rows.raw")
			pn := filepath.Join(dir, "pn.raw")
			fl := filepath.Join(dir, "fl.raw")
			os.WriteFile(rows, []byte{0x34, 0x12}, 0o600)
			os.WriteFile(pn, []byte{'A', 0xe9}, 0o600)
			floats := make([]byte, 8)
			binary.LittleEndian.PutUint32(floats, math.Float32bits(1.25))
			binary.LittleEndian.PutUint32(floats[4:], math.Float32bits(2.5))
			os.WriteFile(fl, floats, 0o600)
			if c := dc1353Run(t, "-if", "Rows="+rows, "-mf", "PatientName="+pn, "-if", "ScanningSpotSize="+fl, "-in-place", input); c != 0 {
				t.Fatal(c)
			}
			f := dc1353Read(t, input)
			v, e := f.Dataset.GetInts(core.NewTag(0x0028, 0x0010))
			if e != nil || !reflect.DeepEqual(v, []int64{0x1234}) {
				t.Fatalf("%v %v", v, e)
			}
			name, e := f.Dataset.LookupString(core.NewTag(0x0010, 0x0010))
			if e != nil || name != "Aé" {
				t.Fatalf("%q %v", name, e)
			}
			values, e := f.Dataset.GetFloats(core.NewTag(0x300a, 0x0398))
			if e != nil || !reflect.DeepEqual(values, []float64{1.25, 2.5}) {
				t.Fatalf("%v %v", values, e)
			}
		})
	}
}
func TestDcmodifyValueFileCapsOddAndMissingFile(t *testing.T) {
	dir := t.TempDir()
	input := dc1353Input(t, dir, "in.dcm", transfer.ExplicitVRLittleEndian)
	before, _ := os.ReadFile(input)
	odd := filepath.Join(dir, "odd.raw")
	os.WriteFile(odd, []byte{1}, 0o600)
	big := filepath.Join(dir, "big.raw")
	os.WriteFile(big, []byte{1, 0, 2, 0}, 0o600)
	for _, args := range [][]string{{"-if", "Rows=" + odd, "-in-place", input}, {"-max-value-file-bytes", "2", "-if", "Rows=" + big, "-in-place", input}, {"-max-edit-value-bytes", "2", "-if", "Rows=" + big, "-in-place", input}, {"-imt", "-mf", "PatientID=" + filepath.Join(dir, "absent.raw"), "-in-place", input}} {
		if c := dc1353Run(t, args...); c != 1 {
			t.Fatal(c)
		}
	}
	after, _ := os.ReadFile(input)
	if !bytes.Equal(before, after) {
		t.Fatal("value failure modified source")
	}
	dc1353NoTemps(t, dir)
}
func TestDcmodifyValueSnapshotsDetachedAndCumulative(t *testing.T) {
	dir := t.TempDir()
	input := dc1353Input(t, dir, "in.dcm", transfer.ExplicitVRLittleEndian)
	source := filepath.Join(dir, "value.raw")
	os.WriteFile(source, []byte{'A', ' '}, 0o600)
	opts, e := parseArgs([]string{"-mf", "PatientName=" + source, "-in-place", input}, &bytes.Buffer{})
	if e != nil {
		t.Fatal(e)
	}
	jobs, conditions, e := compileActions(opts, std.Dictionary)
	if e != nil {
		t.Fatal(e)
	}
	values, e := snapshotValues(context.Background(), opts, jobs)
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(source, []byte{'B', ' '}, 0o600)
	if _, _, e := executeFile(context.Background(), opts, jobs, conditions, values, std.Dictionary, &bytes.Buffer{}, func() (string, error) { return "1.2.3", nil }); e != nil {
		t.Fatal(e)
	}
	f := dc1353Read(t, input)
	v, _ := f.Dataset.GetString(core.NewTag(0x0010, 0x0010))
	if v != "A" {
		t.Fatal("snapshot was not detached")
	}
	other := filepath.Join(dir, "other.raw")
	os.WriteFile(other, []byte{1, 0}, 0o600)
	if c := dc1353Run(t, "-max-value-source-bytes", "3", "-mf", "PatientName="+source, "-if", "Rows="+other, "-in-place", input); c != 1 {
		t.Fatal(c)
	}
}

func TestDcmodifyValueFileOpaqueWordsFDATAndUTF8(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		t.Run(syntax.UID, func(t *testing.T) {
			dir := t.TempDir()
			input := dc1353Input(t, dir, "in.dcm", syntax, core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0005), VR: core.VRCS}, Value: core.StringValue{"ISO_IR 192"}})
			fd := make([]byte, 16)
			binary.LittleEndian.PutUint64(fd, math.Float64bits(1.25))
			binary.LittleEndian.PutUint64(fd[8:], math.Float64bits(-2.5))
			payloads := []struct {
				path string
				data []byte
			}{{"TimeRange", fd}, {"FrameIncrementPointer", []byte{0x28, 0, 0x10, 0}}, {"EncapsulatedDocument", []byte{0, 0xff, 0x80, 0x20}}, {"DarkCurrentCounts", []byte{0x34, 0x12, 0xcd, 0xab}}, {"PatientName", []byte{'A', 0xc3, 0xa9, ' '}}}
			args := []string{}
			for i, p := range payloads {
				source := filepath.Join(dir, string(rune('a'+i))+".raw")
				if err := os.WriteFile(source, p.data, 0o600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "-if", p.path+"="+source)
			}
			args = append(args, "-in-place", input)
			if code := dc1353Run(t, args...); code != 0 {
				t.Fatal(code)
			}
			f := dc1353Read(t, input)
			if values, err := f.Dataset.GetFloats(core.NewTag(0x0008, 0x1163)); err != nil || !reflect.DeepEqual(values, []float64{1.25, -2.5}) {
				t.Fatalf("FD %v %v", values, err)
			}
			at, _ := f.Dataset.Get(core.NewTag(0x0028, 0x0009))
			raw, ok := at.Value.(core.RawValue)
			if !ok || syntax.ByteOrder.Uint16(raw) != 0x0028 || syntax.ByteOrder.Uint16(raw[2:]) != 0x0010 {
				t.Fatal("AT unit order incorrect")
			}
			ob, _ := f.Dataset.Get(core.NewTag(0x0042, 0x0011))
			if raw, ok := ob.Value.(core.RawValue); !ok || !bytes.Equal(raw, payloads[2].data) {
				t.Fatal("opaque bytes reinterpreted")
			}
			ow, _ := f.Dataset.Get(core.NewTag(0x0014, 0x3050))
			raw, ok = ow.Value.(core.RawValue)
			if !ok || syntax.ByteOrder.Uint16(raw) != 0x1234 || syntax.ByteOrder.Uint16(raw[2:]) != 0xabcd {
				t.Fatal("OW order incorrect")
			}
			if name, err := f.Dataset.LookupString(core.NewTag(0x0010, 0x0010)); err != nil || name != "Aé" {
				t.Fatalf("UTF8 %q %v", name, err)
			}
		})
	}
}

func TestDcmodifyValueFileNativePixelAndEncapsulatedRefusal(t *testing.T) {
	dir := t.TempDir()
	value := filepath.Join(dir, "pixel.raw")
	if err := os.WriteFile(value, []byte{0x34, 0x12}, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		input := dc1353Input(t, dir, "native-"+syntax.UID+".dcm", syntax,
			core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0100), VR: core.VRUS}, Value: core.Uint16Value{16}},
			core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROW}, Value: core.RawValue{0, 0}},
		)
		if code := dc1353Run(t, "-if", "PixelData="+value, "-in-place", input); code != 0 {
			t.Fatal(code)
		}
		f := dc1353Read(t, input)
		el, _ := f.Dataset.Get(core.TagPixelData)
		raw, ok := el.Value.(core.RawValue)
		if !ok || len(raw) != 2 || syntax.ByteOrder.Uint16(raw) != 0x1234 {
			t.Fatal("native pixel units incorrect")
		}
	}
	// A syntax-qualified metadata-only fixture proves the CLI refuses the
	// encapsulated contract before inserting bytes; it makes no image/IOD claim.
	compressed := dc1353Input(t, dir, "encapsulated.dcm", transfer.JPEGBaseline)
	before, _ := os.ReadFile(compressed)
	if code := dc1353Run(t, "-if", "PixelData="+value, "-in-place", compressed); code != 1 {
		t.Fatal("encapsulated pixel insertion accepted")
	}
	after, _ := os.ReadFile(compressed)
	if !bytes.Equal(before, after) {
		t.Fatal("encapsulated refusal published")
	}
	dc1353NoTemps(t, dir)
}

func TestDcmodifyValueFileChargesEveryWildcardTarget(t *testing.T) {
	dir := t.TempDir()
	input := dc1353Input(t, dir, "in.dcm", transfer.ExplicitVRLittleEndian,
		core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1110), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{
			{Elements: []core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: core.VRPN}, Value: core.StringValue{"OLD"}}}},
			{Elements: []core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: core.VRPN}, Value: core.StringValue{"OLD"}}}},
		}}},
	)
	value := filepath.Join(dir, "value.raw")
	if err := os.WriteFile(value, []byte{'A', ' '}, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(input)
	if code := dc1353Run(t, "-mf", "ReferencedStudySequence[*].PatientName="+value, "-max-edit-value-bytes", "3", "-in-place", input); code != 1 {
		t.Fatal("copies counted only once")
	}
	after, _ := os.ReadFile(input)
	if !bytes.Equal(before, after) {
		t.Fatal("partial wildcard edit published")
	}
	if code := dc1353Run(t, "-mf", "ReferencedStudySequence[*].PatientName="+value, "-max-edit-value-bytes", "4", "-in-place", input); code != 0 {
		t.Fatal("exact cumulative cap refused")
	}
	f := dc1353Read(t, input)
	items, _ := f.Dataset.GetSequence(core.NewTag(0x0008, 0x1110))
	if len(items) != 2 {
		t.Fatal("items missing")
	}
	for _, item := range items {
		if v, _ := item.GetString(core.NewTag(0x0010, 0x0010)); v != "A" {
			t.Fatal("wildcard target not edited")
		}
	}
	dc1353NoTemps(t, dir)
}

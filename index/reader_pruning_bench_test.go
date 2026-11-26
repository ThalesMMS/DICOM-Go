package index

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"reflect"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// This file is a self-contained A/B harness that also compiles on the archived
// baseline. Reflection sets opt-in fields only when that binary supports them;
// baseline labels all run its existing traversal. It never fabricates a gain
// by changing fixture bytes, selectors, profiles, limits or source adapters.
func pruningBenchMode(opts Options, mode string) Options {
	v := reflect.ValueOf(&opts).Elem()
	if mode == "Sorted" || mode == "Both" {
		if field := v.FieldByName("AssumeSortedTags"); field.IsValid() {
			field.SetBool(true)
		}
	}
	if mode == "Opaque" || mode == "Both" {
		if field := v.FieldByName("SequencePruning"); field.IsValid() {
			field.SetUint(1)
		}
	}
	return opts
}

func pruningBenchWire(tag core.Tag, vr string, value []byte) []byte {
	length := len(value)
	b := make([]byte, 4)
	binary.LittleEndian.PutUint16(b, tag.Group)
	binary.LittleEndian.PutUint16(b[2:], tag.Element)
	if vr != "" {
		b = append(b, vr...)
		if vr == "SQ" || vr == "OB" || vr == "UN" {
			b = append(b, 0, 0)
		} else {
			at := len(b)
			b = append(b, 0, 0)
			binary.LittleEndian.PutUint16(b[at:], uint16(length))
			return append(b, value...)
		}
	}
	at := len(b)
	b = append(b, 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(b[at:], uint32(length))
	return append(b, value...)
}

func pruningBenchText(tag core.Tag, vr, text string) []byte {
	raw := []byte(text)
	if len(raw)%2 != 0 {
		pad := byte(' ')
		if vr == "UI" {
			pad = 0
		}
		raw = append(raw, pad)
	}
	return pruningBenchWire(tag, vr, raw)
}
func pruningBenchItem(value []byte) []byte {
	return pruningBenchWire(core.NewTag(0xfffe, 0xe000), "", value)
}

func pruningBenchPart10(dataset []byte, directory bool) []byte {
	class := "1.2.840.10008.5.1.4.1.1.2.1"
	if directory {
		class = "1.2.840.10008.1.3.10"
	}
	meta := pruningBenchText(core.NewTag(2, 2), "UI", class)
	meta = append(meta, pruningBenchText(core.NewTag(2, 3), "UI", "1.2.3.8")...)
	meta = append(meta, pruningBenchText(core.NewTag(2, 0x10), "UI", transfer.ExplicitVRLittleEndian.UID)...)
	length := make([]byte, 4)
	binary.LittleEndian.PutUint32(length, uint32(len(meta)))
	meta = append(pruningBenchWire(core.NewTag(2, 0), "UL", length), meta...)
	wire := append(make([]byte, 128), []byte("DICM")...)
	wire = append(wire, meta...)
	return append(wire, dataset...)
}

func pruningBenchCore(frames int) []byte {
	wire := pruningBenchText(tags.SOPClassUID, "UI", "1.2.840.10008.5.1.4.1.1.2.1")
	wire = append(wire, pruningBenchText(tags.SOPInstanceUID, "UI", "1.2.3.8")...)
	wire = append(wire, pruningBenchText(tags.Modality, "CS", "CT")...)
	for _, v := range []struct {
		tag      core.Tag
		vr, text string
	}{{tags.StudyInstanceUID, "UI", "1.2.3"}, {tags.SeriesInstanceUID, "UI", "1.2.3.4"}, {tags.SeriesNumber, "IS", "1"}, {tags.InstanceNumber, "IS", "1"}, {tags.NumberOfFrames, "IS", strconv.Itoa(frames)}} {
		wire = append(wire, pruningBenchText(v.tag, v.vr, v.text)...)
	}
	return wire
}

func pruningBenchFixture(name string, count int) ([]byte, Options) {
	opts := Options{Profile: ProfileCore}
	wire := pruningBenchCore(count)
	if name == "CSAPrimitive" {
		// Real CSA header tags contain private OB, not SQ. Already-cheap skips
		// are retained as a negative control, irrespective of measured gain.
		for _, tag := range []core.Tag{core.NewTag(0x29, 0x1010), core.NewTag(0x29, 0x1020)} {
			wire = append(wire, pruningBenchWire(tag, "OB", bytes.Repeat([]byte{0x58}, count*4096))...)
		}
		return pruningBenchPart10(wire, false), opts
	}
	if name == "DICOMDIRIcons" {
		directory := core.NewTag(4, 0x1220)
		opts = Options{Profile: ProfileNone, Selectors: []Selector{{ID: "file", Path: []core.Tag{directory}, Tag: core.NewTag(4, 0x1500), Occurrence: OccurrenceAll}, {ID: "uid", Path: []core.Tag{directory}, Tag: core.NewTag(4, 0x1511), Occurrence: OccurrenceAll}}}
		body := []byte{}
		for i := 0; i < count; i++ {
			item := pruningBenchText(core.NewTag(4, 0x1500), "CS", fmt.Sprintf("FILE%04d", i))
			item = append(item, pruningBenchText(core.NewTag(4, 0x1511), "UI", fmt.Sprintf("1.2.3.%d", i+1))...)
			icon := []byte{}
			for _, tag := range []core.Tag{tags.SamplesPerPixel, tags.Rows, tags.Columns, tags.BitsAllocated, tags.BitsStored, tags.HighBit, tags.PixelRepresentation} {
				icon = append(icon, pruningBenchWire(tag, "US", []byte{1, 0})...)
			}
			icon = append(icon, pruningBenchWire(tags.PixelData, "OB", make([]byte, 4096))...)
			item = append(item, pruningBenchWire(core.NewTag(0x88, 0x200), "SQ", pruningBenchItem(icon))...)
			body = append(body, pruningBenchItem(item)...)
		}
		wire = pruningBenchWire(directory, "SQ", body)
		wire = append(wire, pruningBenchText(tagSpecificCharacterSet, "CS", "ISO_IR 100")...)
		return pruningBenchPart10(wire, true), opts
	}
	seq := core.NewTag(0x5200, 0x9230)
	if name == "PrivateSQTail" {
		seq = core.NewTag(0x29, 0x2000)
	}
	if name == "EnhancedSelected" {
		opts.Selectors = []Selector{{ID: "position", Path: []core.Tag{seq, core.NewTag(0x20, 0x9113)}, Tag: tags.ImagePositionPatient, Occurrence: OccurrenceAll}}
	}
	body := []byte{}
	for i := 0; i < count; i++ {
		item := []byte{}
		if name == "EnhancedSelected" {
			pos := pruningBenchText(tags.ImagePositionPatient, "DS", "1\\2\\3")
			item = append(item, pruningBenchWire(core.NewTag(0x20, 0x9113), "SQ", pruningBenchItem(pos))...)
		}
		hidden := []byte{}
		for j := 0; j < 32; j++ {
			hidden = append(hidden, pruningBenchWire(core.NewTag(0x28, uint16(0x100+j)), "US", []byte{1, 0})...)
		}
		item = append(item, pruningBenchWire(core.NewTag(0x28, 0x9132), "SQ", pruningBenchItem(hidden))...)
		body = append(body, pruningBenchItem(item)...)
	}
	wire = append(wire, pruningBenchWire(seq, "SQ", body)...)
	return pruningBenchPart10(wire, false), opts
}

func BenchmarkHeaderPruning(b *testing.B) {
	for _, name := range []string{"EnhancedUnselected", "EnhancedSelected", "CSAPrimitive", "DICOMDIRIcons", "PrivateSQTail"} {
		wire, opts := pruningBenchFixture(name, 2048)
		for _, mode := range []string{"Legacy", "Sorted", "Opaque", "Both"} {
			b.Run(name+"/"+mode, func(b *testing.B) {
				options := pruningBenchMode(opts, mode)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := ReadSeeker(context.Background(), name, bytes.NewReader(wire), int64(len(wire)), options); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func TestHeaderPruningBenchmarkFixturesEquivalent(t *testing.T) {
	for _, name := range []string{"EnhancedUnselected", "EnhancedSelected", "CSAPrimitive", "DICOMDIRIcons", "PrivateSQTail"} {
		t.Run(name, func(t *testing.T) {
			wire, opts := pruningBenchFixture(name, 8)
			before := append([]byte(nil), wire...)
			legacy, err := ReadSeeker(context.Background(), name, bytes.NewReader(wire), int64(len(wire)), opts)
			if err != nil {
				t.Fatal(err)
			}
			if name != "DICOMDIRIcons" && legacy.Record.Instance.SOPInstanceUID != "1.2.3.8" {
				t.Fatalf("independent root UID %q", legacy.Record.Instance.SOPInstanceUID)
			}
			if name == "DICOMDIRIcons" {
				if len(legacy.Record.Selected["file"]) != 8 || len(legacy.Record.Selected["uid"]) != 8 {
					t.Fatal("fixture lost records")
				}
				if raw := legacy.Record.Selected["file"][7].Element.Value.(core.RawValue); string(raw) != "FILE0007" {
					t.Fatalf("independent file ID %q", raw)
				}
			}
			if name == "EnhancedSelected" {
				if len(legacy.Record.Selected["position"]) != 8 {
					t.Fatal("fixture lost frames")
				}
				if raw := legacy.Record.Selected["position"][7].Element.Value.(core.RawValue); string(raw) != "1\\2\\3 " {
					t.Fatalf("independent position %q", raw)
				}
			}
			for _, mode := range []string{"Sorted", "Opaque", "Both"} {
				got, err := ReadSeeker(context.Background(), name, bytes.NewReader(wire), int64(len(wire)), pruningBenchMode(opts, mode))
				if err != nil || !reflect.DeepEqual(legacy.Record, got.Record) {
					t.Fatalf("mode %s extraction differs: %v", mode, err)
				}
			}
			if !bytes.Equal(before, wire) {
				t.Fatal("fixture source mutated")
			}
		})
	}
}

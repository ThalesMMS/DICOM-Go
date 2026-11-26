package main

import (
	"bytes"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestConvOutputOptionParsing(t *testing.T) {
	args := []string{"-ts", "jpeg-ls-lossless", "-fragment-bytes", "32", "-offset-table", "empty", "-uid-policy", "always", "-compress-icons", "-o", "out.dcm", "in.dcm"}
	opts, err := parseArgs(args, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	_, api, err := transcodeOptions(opts)
	if err != nil {
		t.Fatal(err)
	}
	if api.MaxFragmentBytes != 32 || api.OffsetTable != object.OffsetTableEmpty || api.UIDPolicy != pixeldata.TranscodeUIDAlways || !api.CompressIcons {
		t.Fatalf("options=%+v", api)
	}
	for _, flags := range [][]string{{"-fragment-bytes", "3"}, {"-fragment-bytes", "-2"}, {"-offset-table", "wrong"}, {"-planar-output", "wrong"}, {"-uid-policy", "wrong"}, {"-secondary-capture", "-uid-policy", "preserve"}} {
		args := append([]string{"-ts", "explicit-le", "-o", "out.dcm"}, flags...)
		args = append(args, "in.dcm")
		if _, err := parseArgs(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %v", flags)
		}
	}
	for value, policy := range map[string]pixeldata.PlanarOutputPolicy{"default": pixeldata.PlanarOutputDefault, "iod": pixeldata.PlanarOutputIOD, "interleaved": pixeldata.PlanarOutputInterleaved, "planar": pixeldata.PlanarOutputPlanar} {
		opts, err := parseArgs([]string{"-ts", "explicit-le", "-planar-output", value, "-o", "out.dcm", "in.dcm"}, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		_, api, err := transcodeOptions(opts)
		if err != nil || api.PlanarOutput != policy {
			t.Fatalf("planar=%s %v", value, err)
		}
	}
}

func TestConvWritesFragmentedAndSecondaryCaptureOutputs(t *testing.T) {
	input, _ := writeImage(t)
	dir := t.TempDir()
	encoded := filepath.Join(dir, "encoded.dcm")
	if code, _, stderr := runConv(t, "-ts", "jpeg-ls-lossless", "-fragment-bytes", "2", "-offset-table", "empty", "-uid-policy", "always", "-o", encoded, input); code != 0 {
		t.Fatalf("encode=%s", stderr)
	}
	read, err := object.OpenFile(encoded)
	if err != nil {
		t.Fatal(err)
	}
	pixel, err := pixeldata.ExtractView(read.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if len(pixel.Sequence.Fragments) < 2 || len(pixel.Sequence.OffsetTable) != 0 {
		t.Fatal("fragment/BOT option not written")
	}
	read.Close()
	// Provide the study/series identity required by the public SC option.
	file, err := object.OpenFile(input)
	if err != nil {
		t.Fatal(err)
	}
	file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.2"}})
	for tag, value := range map[core.Tag]string{core.NewTag(0x0020, 0x000d): "1.2.826.0.1.3680043.10.1.1323.1", core.NewTag(0x0020, 0x000e): "1.2.826.0.1.3680043.10.1.1323.2"} {
		file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRUI}, Value: core.StringValue{value}})
	}
	var data bytes.Buffer
	if err := object.WriteFile(&data, file); err != nil {
		t.Fatal(err)
	}
	file.Close()
	prepared := filepath.Join(dir, "prepared.dcm")
	if err := os.WriteFile(prepared, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	sc := filepath.Join(dir, "sc.dcm")
	if code, _, stderr := runConv(t, "-ts", "explicit-le", "-secondary-capture", "-o", sc, prepared); code != 0 {
		t.Fatalf("SC=%s", stderr)
	}
	read, err = object.OpenFile(sc)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	class, _ := read.Dataset.GetString(core.NewTag(0x0008, 0x0016))
	meta, _ := read.Meta.GetString(core.NewTag(0x0002, 0x0002))
	if class != "1.2.840.10008.5.1.4.1.1.7" || class != meta {
		t.Fatal("SC class/meta mismatch")
	}
}

func TestConvSplitAliasReadsBackBothTableModes(t *testing.T) {
	input, want := writeImage(t)
	for _, policy := range []string{"basic", "empty"} {
		encoded := filepath.Join(t.TempDir(), "encoded.dcm")
		code, _, stderr := runConv(t, "-ts", "jpeg-ls-lossless", "-split", "3", "-offset-table", policy, "-o", encoded, input)
		if code != 0 {
			t.Fatalf("split/table encode=%s", stderr)
		}
		file, err := object.OpenFile(encoded)
		if err != nil {
			t.Fatal(err)
		}
		pixel, err := pixeldata.Extract(file.Dataset)
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
		if len(pixel.Sequence.Fragments) < 2 {
			t.Fatal("split alias did not fragment")
		}
		for _, fragment := range pixel.Sequence.Fragments {
			if len(fragment) != 2 {
				t.Fatal("odd split was not rounded down to even cap")
			}
		}
		if policy == "basic" && len(pixel.Sequence.OffsetTable) != 4 || policy == "empty" && len(pixel.Sequence.OffsetTable) != 0 {
			t.Fatal("table policy was ignored")
		}
		native := filepath.Join(t.TempDir(), "native.dcm")
		code, _, stderr = runConv(t, "-ts", "explicit-le", "-o", native, encoded)
		if code != 0 {
			t.Fatalf("split/table decode=%s", stderr)
		}
		read, err := object.OpenFile(native)
		if err != nil {
			t.Fatal(err)
		}
		defer read.Close()
		got, _ := read.Dataset.GetRaw(core.TagPixelData)
		if !bytes.Equal(got, want) {
			t.Fatal("split changed decoded samples")
		}
	}
}

func TestConvRootUIDIsPerCallAndPreservesRelatedIdentity(t *testing.T) {
	input, _ := writeImage(t)
	source, err := object.OpenFile(input)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	sop, _ := source.Dataset.GetString(core.NewTag(0x0008, 0x0018))
	for _, root := range []string{"1.2.826.0.1.3680043.10.1352", "1.2.826.0.1.3680043.10.1353"} {
		output := filepath.Join(t.TempDir(), "derived.dcm")
		code, _, stderr := runConv(t, "-ts", "explicit-le", "-uid-policy", "always", "-root-uid", root, "-o", output, input)
		if code != 0 {
			t.Fatalf("root encode=%s", stderr)
		}
		file, err := object.OpenFile(output)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		got, _ := file.Dataset.GetString(core.NewTag(0x0008, 0x0018))
		meta, _ := file.Meta.GetString(core.NewTag(0x0002, 0x0003))
		if !strings.HasPrefix(got, root+".") || got == sop || !core.IsValidUID(got) || got != meta {
			t.Fatal("root not applied consistently to SOP/meta")
		}
		refs, ok := file.Dataset.GetSequence(core.NewTag(0x0008, 0x2112))
		if !ok || len(refs) == 0 {
			t.Fatal("predecessor provenance absent")
		}
		ref, _ := refs[0].GetString(core.NewTag(0x0008, 0x1155))
		if ref != sop {
			t.Fatal("source reference was remapped")
		}
		for _, tag := range []core.Tag{core.NewTag(0x0020, 0x000d), core.NewTag(0x0020, 0x000e)} {
			old, ok := source.Dataset.Get(tag)
			fresh, found := file.Dataset.Get(tag)
			if ok != found || !reflect.DeepEqual(old, fresh) {
				t.Fatal("unrequested related identity changed")
			}
		}
	}
}

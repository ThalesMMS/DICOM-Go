package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"io"
	"strings"
	"testing"
	"time"
)

func layoutData() *object.Object {
	return object.FromElements([]core.Element{core.NewRawElement(core.NewTag(0x0008, 0x0016), core.VRUI, []byte("1.2.3")), core.NewRawElement(core.NewTag(0x0008, 0x0018), core.VRUI, []byte("1.2.3.4")), core.NewRawElement(core.NewTag(0x0020, 0x000D), core.VRUI, []byte("1.2.3.5")), core.NewRawElement(core.NewTag(0x0020, 0x000E), core.VRUI, []byte("1.2.3.6")), core.NewRawElement(core.NewTag(0x0010, 0x0020), core.VRLO, []byte("synthetic "))}, std.Dictionary)
}
func layoutPolicy(t *testing.T, args ...string) *storageLayoutPolicy {
	t.Helper()
	opts, err := parseArgs(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.storagePolicy == nil {
		t.Fatal("missing feature policy")
	}
	return opts.storagePolicy
}
func TestStorageLayoutDefaultsFlagsAndConflicts(t *testing.T) {
	opts, err := parseArgs(nil, io.Discard)
	if err != nil || opts.storagePolicy != nil || opts.limits.storageRuntime != nil {
		t.Fatalf("default changed %v", err)
	}
	for _, args := range [][]string{{"-path-template", ""}, {"-path-template", "../{SOPInstanceUID}"}, {"-path-template", "{SOPInstanceUID}", "-sort-on-study-uid"}, {"-path-template", "{SOPInstanceUID}", "-filename-extension", ".x"}, {"-unique-filenames", "-timenames"}, {"-filename-extension", "exe"}, {"-filename-extension", ".x/"}, {"-filename-extension", ".x."}, {"-hook-timeout", "1s"}, {"-eostudy-timeout", "1s"}, {"-exec-on-reception", "[]"}, {"-single", "-exec-on-eostudy", storageHookJSON([]string{"/trusted/helper"})}, {"-exec-on-reception", storageHookJSON([]string{"/trusted/helper"}), "-hook-queue-depth", "0"}, {"-exec-on-eostudy", storageHookJSON([]string{"/trusted/helper"}), "-max-active-studies", "256"}} {
		if _, err := parseArgs(args, io.Discard); !errors.Is(err, errUsage) {
			t.Fatalf("accepted %v/%v", args, err)
		}
	}
}
func TestStorageLayoutTemplateModesAndMissingUID(t *testing.T) {
	src := layoutData()
	now := time.Date(2026, 10, 9, 12, 13, 14, 123456789, time.FixedZone("synthetic", 3600))
	for _, tc := range []struct {
		args []string
		want string
	}{{[]string{"-path-template", "{StudyInstanceUID}/{SeriesInstanceUID}/{SOPInstanceUID}.dcm"}, "1.2.3.5/1.2.3.6/1.2.3.4.dcm"}, {[]string{"-sort-on-study-uid", "-filename-extension", ".data"}, "1.2.3.5/1.2.3.4.data"}, {[]string{"-filename-extension", ""}, "1.2.3.4"}, {[]string{"-timenames"}, "20261009T111314.123456789Z.dcm"}, {[]string{"-path-template", "{PatientID,hash}.dcm"}, "b3cc0475bb"}} {
		p := layoutPolicy(t, tc.args...)
		got, err := p.format(context.Background(), src, "1.2.3.4", "1.2.3.5", now)
		if len(tc.args) > 1 && strings.Contains(tc.args[1], "hash") {
			if err != nil || len(got) != 68 {
				t.Fatalf("hash %q/%v", got, err)
			}
		} else if err != nil || got != tc.want {
			t.Fatalf("format %q/%v want %q", got, err, tc.want)
		}
	}
	p := layoutPolicy(t, "-unique-filenames")
	p.random = bytes.NewReader(make([]byte, 16))
	got, err := p.format(context.Background(), src, "1.2.3.4", "", now)
	if err != nil || got != strings.Repeat("0", 32)+".dcm" {
		t.Fatal(got, err)
	}
	p.random = bytes.NewReader(nil)
	if _, err = p.format(context.Background(), src, "1.2.3.4", "", now); err == nil {
		t.Fatal("random failure accepted")
	}
	for _, src := range []*object.Object{nil, object.New(std.Dictionary), object.FromElements([]core.Element{core.NewRawElement(core.NewTag(0x0020, 0x000D), core.VRUI, []byte("1.2\\1.3"))}, std.Dictionary)} {
		if _, err := storageStudyUID(src); !errors.Is(err, errStorageLayout) {
			t.Fatal("invalid study admitted")
		}
	}
	p = layoutPolicy(t, "-path-template", "{PatientName}.dcm")
	if _, err = p.format(context.Background(), src, "1.2.3.4", "", now); !errors.Is(err, errStorageLayout) {
		t.Fatal("missing attribute fell back")
	}
}
func TestStorageLayoutCharsetBigEndianDateAndVM(t *testing.T) {
	src := layoutData()
	src.Put(core.NewRawElement(core.NewTag(0x0008, 0x0005), core.VRCS, []byte("ISO_IR 100")))
	src.Put(core.NewRawElement(core.NewTag(0x0010, 0x0010), core.VRPN, []byte{'J', 0xf6, 'r', 'g'}))
	p := layoutPolicy(t, "-path-template", "{PatientName}.dcm")
	got, err := p.format(context.Background(), src, "1.2.3.4", "", time.Now())
	if err != nil || got != "Jörg.dcm" {
		t.Fatalf("charset %q/%v", got, err)
	}
	src.Put(core.NewRawElement(core.NewTag(0x0028, 0x0010), core.VRUS, []byte{0, 7}))
	src.SetValueByteOrder(binary.BigEndian)
	p = layoutPolicy(t, "-path-template", "{Rows}.dcm")
	got, err = p.format(context.Background(), src, "1.2.3.4", "", time.Now())
	if err != nil || got != "7.dcm" {
		t.Fatalf("BE %q/%v", got, err)
	}
	src.Put(core.NewRawElement(core.NewTag(0x0008, 0x0020), core.VRDA, []byte("20240228")))
	src.Put(core.NewRawElement(core.NewTag(0x0008, 0x0008), core.VRCS, []byte("ORIGINAL\\PRIMARY")))
	p = layoutPolicy(t, "-path-template", "{StudyDate,date,+1d}/{ImageType[1]}.dcm")
	got, err = p.format(context.Background(), src, "1.2.3.4", "", time.Now())
	if err != nil || got != "20240229/PRIMARY.dcm" {
		t.Fatalf("date/index %q/%v", got, err)
	}
	p = layoutPolicy(t, "-path-template", "{ImageType}.dcm")
	if _, err = p.format(context.Background(), src, "1.2.3.4", "", time.Now()); !errors.Is(err, errStorageLayout) {
		t.Fatal("ambiguous VM")
	}
	// Formatting leaves raw backing bytes intact, including their source order.
	e, _ := src.Get(core.NewTag(0x0028, 0x0010))
	raw, _ := e.RawBytes()
	if !bytes.Equal(raw, []byte{0, 7}) || src.ValueByteOrder() != binary.BigEndian {
		t.Fatal("template mutated source")
	}
}

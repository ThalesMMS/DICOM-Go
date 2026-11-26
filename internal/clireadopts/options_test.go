package clireadopts

import (
	"flag"
	"io"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
)

func crOptions(t *testing.T, file bool, args ...string) Options {
	t.Helper()
	var o Options
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o.Register(fs, file)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestReadOptionsDefaultsAndCallerOwnership(t *testing.T) {
	o := crOptions(t, true)
	if o.Enabled() || o.Read.StrictReservedBytes || o.Apply(object.ReadFileOptions{}).OddLengthPolicy != parser.RejectOddLength {
		t.Fatal("default parser policy changed")
	}
	base := object.ReadFileOptions{Dictionary: std.Dictionary, DeferPixelData: true, MaxTotalBytes: 41, MaxElements: 17,
		BulkDataDescriptor: &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{{Threshold: 12}}}}
	got := crOptions(t, false, "-read-replace-wrong-delimiter").Apply(base)
	if !reflect.DeepEqual(got.Dictionary, base.Dictionary) || !got.DeferPixelData || got.BulkDataDescriptor != base.BulkDataDescriptor || got.MaxTotalBytes != 41 || got.MaxElements != 17 || !got.SequenceRecovery.ReplaceWrongDelimiter {
		t.Fatal("caller dictionary, replay or server resource policy was replaced")
	}
	if base.SequenceRecovery.ReplaceWrongDelimiter {
		t.Fatal("caller options mutated")
	}
	if !reflect.DeepEqual((Options{}).Apply(base), base) {
		t.Fatal("zero overlay changed caller options")
	}
	strict := crOptions(t, true, "-read-strict-reserved-bytes")
	if strict.Enabled() || !strict.Apply(object.ReadFileOptions{}).StrictReservedBytes {
		t.Fatal("strictness was treated as recovery")
	}
}

func TestReadOptionsDomainsBeforeIO(t *testing.T) {
	for _, args := range [][]string{
		{"-read-odd-length", "pad"}, {"-read-max-boundary-recoveries", "0"},
		{"-read-max-implicit-vr-elements", "-1"}, {"-read-max-total-bytes", "0"},
		{"-read-max-element-bytes", "-1"}, {"-read-max-elements", "0"},
		{"-read-max-sequence-depth", "-1"}, {"-read-max-fragments", "0"},
		{"-read-max-pixel-data-bytes", "0"}, {"-read-prefer-dictionary-length-field-size"},
	} {
		var o Options
		fs := flag.NewFlagSet("read", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		o.Register(fs, true)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		if err := o.Validate(); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	var o Options
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o.Register(fs, false)
	if err := fs.Parse([]string{"-read-allow-implicit-file-meta"}); err == nil {
		t.Fatal("raw server exposed file meta recovery")
	}
}

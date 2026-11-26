package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
)

func TestDcmconvWriteOptions(t *testing.T) {
	var stderr bytes.Buffer
	opts, err := parseArgs([]string{"-ts", "deflated", "-compression-level", "0", "-padding", "create", "-pad-file", "512", "-pad-item", "32", "-max-padding-bytes", "4096", "-o", "out.dcm", "in.dcm"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	_, transcode, err := transcodeOptions(opts)
	if err != nil {
		t.Fatal(err)
	}
	write := transcode.WriteOptions
	if write.DeflateLevel == nil || *write.DeflateLevel != 0 || write.Padding.Policy != object.PaddingCreate || write.Padding.Alignment != 512 || write.Padding.ItemAlignment != 32 || write.Padding.MaxAddedBytes != 4096 {
		t.Fatalf("options=%+v", write)
	}
	for _, flags := range [][]string{{"-compression-level", "10"}, {"-compression-level", "-2"}, {"-padding", "create", "-pad-file", "3"}, {"-padding", "remove", "-pad-item", "32"}, {"-pad-file", "4294967296"}, {"-max-padding-bytes", "-1"}, {"-padding", "unknown"}} {
		args := append(append([]string{"-ts", "deflated", "-o", "out.dcm"}, flags...), "in.dcm")
		if _, err := parseArgs(args, &stderr); err == nil {
			t.Fatalf("accepted %v", flags)
		}
	}
	opts, err = parseArgs([]string{"-ts", "explicit-le", "-compression-level", "1", "-o", "out.dcm", "in.dcm"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := transcodeOptions(opts); err == nil {
		t.Fatal("native target accepted Deflate level")
	}
	opts, err = parseArgs([]string{"-ts", "explicit-le", "-o", "out.dcm", "in.dcm"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	_, transcode, err = transcodeOptions(opts)
	if err != nil || transcode.WriteOptions.DeflateLevel != nil || transcode.WriteOptions.Padding.Policy != object.PaddingPreserve {
		t.Fatal("defaults changed")
	}
}

func TestDcmconvWriteOptionsHelpDomain(t *testing.T) {
	var stderr bytes.Buffer
	if _, err := parseArgs([]string{"-h"}, &stderr); err != flag.ErrHelp {
		t.Fatal(err)
	}
	for _, text := range []string{"before compression", "not physical compressed size", "excluding Item Header", "does not control pixel codecs", "16 MiB"} {
		if !strings.Contains(stderr.String(), text) {
			t.Fatalf("missing help: %s", text)
		}
	}
}

func TestDcmconvAdditionalWriteDomains(t *testing.T) {
	for _, flags := range [][]string{{"-group-length", "unknown"}, {"-sequence-length", "unknown"}, {"-dataset-only", "-padding", "create", "-pad-file", "512"}} {
		args := append(append([]string{"-ts", "explicit-le", "-o", "out.dcm"}, flags...), "in.dcm")
		if _, err := parseArgs(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %v", flags)
		}
	}
	var help bytes.Buffer
	_, _ = parseArgs([]string{"-h"}, &help)
	for _, text := range []string{"-group-length", "-sequence-length", "-dataset-only", "destination must be absent", "read-strict-reserved-bytes"} {
		if !strings.Contains(help.String(), text) {
			t.Fatalf("missing help %s", text)
		}
	}
}

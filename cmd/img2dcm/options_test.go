package main

import (
	"bytes"
	"github.com/ThalesMMS/dicom-go/core"
	"os"
	"path/filepath"
	"testing"
)

func TestImg2DCMUsageRejectsContradictoryOrMissingRawOptions(t *testing.T) {
	for _, args := range [][]string{
		{}, {"-new-sc", "-vlp", "a", "b"}, {"-series-from", "a", "-study-from", "b", "in", "out"}, {"-format", "raw", "in", "out"}, {"-rows", "1", "in", "out"}, {"-format", "video", "-new-sc", "in", "out"}, {"-format", "png", "-j2k-lossless", "in", "out"}, {"-max-frames", "100001", "in", "out"}, {"-max-input-bytes", "0", "in", "out"}, {"-oph-photo", "in", "out"},
	} {
		var out, diag bytes.Buffer
		if code := run(args, &out, &diag); code != 2 {
			t.Fatalf("exit %d for %v", code, args)
		}
	}
	var out, diag bytes.Buffer
	if run([]string{"-h"}, &out, &diag) != 0 {
		t.Fatal("help")
	}
}
func TestImg2DCMSeriesFromPreservesSeriesAndGeneratesNewInstance(t *testing.T) {
	input := writeInput(t, "own.png", cliPNG(t))
	sourcePath := filepath.Join(t.TempDir(), "source.dcm")
	output := filepath.Join(t.TempDir(), "derived.dcm")
	var out, diag bytes.Buffer
	if code := run([]string{"-patient-name", "Synthetic^Person", "-study-uid", "1.2.3", "-series-uid", "1.2.3.4", input, sourcePath}, &out, &diag); code != 0 {
		t.Fatalf("source: %s", diag.String())
	}
	source := cliRead(t, sourcePath)
	if code := run([]string{"-series-from", sourcePath, input, output}, &out, &diag); code != 0 {
		t.Fatalf("derived: %s", diag.String())
	}
	f := cliRead(t, output)
	for _, tag := range []core.Tag{core.NewTag(0x0010, 0x0010), core.NewTag(0x0020, 0x000d), core.NewTag(0x0020, 0x000e)} {
		a, _ := source.Dataset.GetString(tag)
		b, _ := f.Dataset.GetString(tag)
		if a != b {
			t.Fatalf("inherit %s", tag)
		}
	}
	a, _ := source.Dataset.GetString(core.NewTag(0x0008, 0x0018))
	b, _ := f.Dataset.GetString(core.NewTag(0x0008, 0x0018))
	if a == b {
		t.Fatal("source SOP reused")
	}
	before, _ := os.ReadFile(sourcePath)
	if run([]string{"-series-from", sourcePath, input, sourcePath}, &out, &diag) != 1 {
		t.Fatal("source overwritten")
	}
	after, _ := os.ReadFile(sourcePath)
	if !bytes.Equal(before, after) {
		t.Fatal("source changed")
	}
}

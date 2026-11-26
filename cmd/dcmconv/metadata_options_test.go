package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/iconimage"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestMetadataOptionAliasesAndRefusalsBeforeInput(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		cap   int64
		group string
	}{
		{[]string{"-remove-gl"}, 0, "remove"},
		{[]string{"-remove-gl", "-group-length", "remove"}, 0, "remove"},
		{[]string{"-split", "1025"}, 1024, "preserve"},
		{[]string{"-split", "4294967295"}, 4294967294, "preserve"},
		{[]string{"-split", "1025", "-fragment-bytes", "1024"}, 1024, "preserve"},
	} {
		args := append([]string{"-ts", "jpeg-ls-lossless", "-o", "out.dcm"}, tc.args...)
		args = append(args, "missing.dcm")
		opts, err := parseArgs(args, &bytes.Buffer{})
		if err != nil || opts.fragmentBytes != tc.cap || opts.groupLength != tc.group {
			t.Fatalf("aliases=%v got=%+v error=%v", tc.args, opts, err)
		}
	}
	for _, args := range [][]string{
		{"-remove-gl", "-group-length", "create"}, {"-split", "1"}, {"-split", "-1"}, {"-split", "4294967296"}, {"-split", "1025", "-fragment-bytes", "1026"},
		{"-icon-minmax", "0,1"}, {"-generate-icon", "-icon-minmax", "NaN,1"}, {"-generate-icon", "-icon-minmax", "1,1"}, {"-generate-icon", "-icon-minmax", "0,1", "-icon-auto-minmax"}, {"-icon-auto-minmax"},
		{"-root-uid", "2.25"}, {"-root-uid", "1.2.840.10008.9"}, {"-root-uid", "1.02"}, {"-root-uid", "1.2.3", "-uid-policy", "preserve"},
		{"-j2k-backend", "openjpeg", "-j2k-rate", strings.Repeat("1,", 5000)},
		{"-generate-icon", "-icon-minmax", strings.Repeat("1", 5000) + ",2"},
		{"-j2k-backend", "other"}, {"-j2k-rate", "10"}, {"-j2k-backend", "openjpeg", "-j2k-rate", "10,20"}, {"-j2k-backend", "openjpeg", "-j2k-rate", "Inf"}, {"-j2k-backend", "openjpeg", "-j2k-tile", "64,0"}, {"-j2k-backend", "openjpeg", "-j2k-resolutions", "34"},
	} {
		output := filepath.Join(t.TempDir(), "out.dcm")
		argv := append([]string{"-ts", "explicit-le", "-o", output}, args...)
		argv = append(argv, filepath.Join(t.TempDir(), "missing-input.dcm"))
		code, _, stderr := runConv(t, argv...)
		if code != 2 || strings.Contains(stderr, "no such file") {
			t.Fatalf("options not rejected before input: args=%v code=%d stderr=%s", args, code, stderr)
		}
		if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("rejected option created output")
		}
	}
	for _, args := range [][]string{{"-split", "1025"}, {"-j2k-backend", "openjpeg"}, {"-j2k-backend", "openjpeg", "-j2k-rate", "10"}} {
		argv := append([]string{"-ts", "explicit-le", "-o", "out.dcm"}, args...)
		argv = append(argv, "missing.dcm")
		code, _, stderr := runConv(t, argv...)
		if code != 2 || strings.Contains(stderr, "no such file") {
			t.Fatalf("native-target controls not rejected early: %v %d %s", args, code, stderr)
		}
	}
}

func TestMetadataCheckReportsOnlyCodesAndRebuildsIdentities(t *testing.T) {
	input, _ := writeImage(t)
	file, err := object.OpenFile(input)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, tag := range []core.Tag{core.NewTag(2, 2), core.NewTag(2, 3), core.NewTag(2, 0x10)} {
		file.Meta.Put(core.NewRawElement(tag, core.VRUI, []byte("9.9.9.SYNTHETIC_SECRET")))
	}
	report, err := file.ValidatePart10Identity()
	if err != nil || len(report.Findings) != 3 {
		t.Fatalf("invalid fixture: %v %+v", err, report)
	}
	opts := options{metadata: metadataOptions{checkMeta: true}}
	lines, err := applyMetadata(context.Background(), opts, file, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(lines, "\n")
	if strings.Contains(text, "SYNTHETIC_SECRET") || !strings.Contains(text, "file_meta.mismatch (0002,0002)") || !strings.Contains(text, "rebuilt identities verified") {
		t.Fatalf("unsafe/incomplete metadata report: %s", text)
	}
	fixed, err := file.ValidatePart10Identity()
	if err != nil || len(fixed.Findings) != 0 {
		t.Fatalf("identities not rebuilt: %+v %v", fixed, err)
	}
}

func TestMetadataPrecursorPreservesSourceAndRefusesAliases(t *testing.T) {
	input, _ := writeImage(t)
	for _, alias := range []string{input, filepath.Join(t.TempDir(), "hardlink.dcm"), filepath.Join(t.TempDir(), "symlink.dcm")} {
		if alias != input {
			var err error
			if strings.Contains(alias, "hardlink") {
				err = os.Link(input, alias)
			} else {
				err = os.Symlink(input, alias)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		before, err := os.ReadFile(input)
		if err != nil {
			t.Fatal(err)
		}
		code, _, _ := runConv(t, "-ts", "explicit-le", "-remove-private-tags", "-o", alias, input)
		after, err := os.ReadFile(input)
		if code != 1 || err != nil || !bytes.Equal(before, after) {
			t.Fatal("metadata precursor bypassed source/output alias guard")
		}
	}
	source, err := os.Open(input)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	digest, err := preparationDigest(context.Background(), source, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	changed[len(changed)-1] ^= 1
	if err := os.WriteFile(input, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyPreparationSource(context.Background(), source, input, digest, 1<<20); err == nil {
		t.Fatal("source change accepted")
	}
	if _, err := preparationDigest(context.Background(), source, 1); err == nil {
		t.Fatal("source byte admission escaped")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := preparationDigest(ctx, source, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("digest cancellation=%v", err)
	}
}

func TestMetadataIconWindowOptions(t *testing.T) {
	for _, tc := range []struct {
		args []string
		mode iconimage.WindowMode
	}{
		{[]string{"-generate-icon"}, iconimage.WindowFromSource},
		{[]string{"-generate-icon", "-icon-minmax", "-10,10"}, iconimage.WindowMinMax},
		{[]string{"-generate-icon", "-icon-auto-minmax"}, iconimage.WindowAutomatic},
	} {
		input, _ := writeImage(t)
		output := filepath.Join(t.TempDir(), "icon.dcm")
		argv := append([]string{"-ts", "explicit-le", "-o", output}, tc.args...)
		argv = append(argv, input)
		code, _, stderr := runConv(t, argv...)
		if code != 0 {
			t.Fatalf("window=%v: %s", tc.args, stderr)
		}
		opts, err := parseArgs(argv, &bytes.Buffer{})
		if err != nil || opts.metadata.iconWindow != tc.mode {
			t.Fatalf("window mode=%v error=%v", opts.metadata.iconWindow, err)
		}
		source, err := object.OpenFile(input)
		if err != nil {
			t.Fatal(err)
		}
		defer source.Close()
		oracle, err := iconimage.Generate(context.Background(), source, iconimage.GenerateOptions{Window: tc.mode, Min: opts.metadata.iconMin, Max: opts.metadata.iconMax})
		if err != nil {
			t.Fatal(err)
		}
		want := oracle.Value.(core.SequenceValue).Items[0].Elements
		result, err := object.OpenFile(output)
		if err != nil {
			t.Fatal(err)
		}
		defer result.Close()
		icons, ok := result.Dataset.GetSequence(core.NewTag(0x0088, 0x0200))
		if !ok || len(icons) != 1 {
			t.Fatal("icon not generated")
		}
		for _, element := range want {
			expected, raw := element.RawBytes()
			if !raw {
				continue
			}
			got, ok := icons[0].GetRaw(element.Tag())
			if !ok || !bytes.Equal(got, expected) {
				t.Fatalf("icon oracle mismatch tag=%s", element.Tag())
			}
		}
	}
}

func TestMetadataWithReadRecoveryAndDatasetOnly(t *testing.T) {
	for _, datasetOnly := range []bool{false, true} {
		input, pixels := writeImage(t)
		output := filepath.Join(t.TempDir(), "converted.dcm")
		argv := []string{"-ts", "explicit-le", "-read-recover", "-generate-icon", "-check-meta", "-remove-private-tags", "-o", output}
		if datasetOnly {
			argv = append(argv, "-dataset-only")
		}
		argv = append(argv, input)
		code, text, stderr := runConv(t, argv...)
		if code != 0 || !strings.Contains(text, "generated native icon") || !strings.Contains(text, "rebuilt identities verified") {
			t.Fatalf("metadata recovery route=%d %s %s", code, text, stderr)
		}
		var result *object.Object
		if datasetOnly {
			body, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			result, err = object.ReadDataSet(bytes.NewReader(body), transfer.ExplicitVRLittleEndian)
			if err != nil {
				t.Fatal(err)
			}
		} else {
			file, err := object.OpenFile(output)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			result = file.Dataset
		}
		defer result.Close()
		got, _ := result.GetRaw(core.TagPixelData)
		icons, ok := result.GetSequence(core.NewTag(0x0088, 0x0200))
		if !bytes.Equal(got, pixels) || !ok || len(icons) != 1 {
			t.Fatal("recovery/dataset-only ignored metadata or changed main pixels")
		}
	}
}

func TestMetadataCheckRealMismatchedMetaAndFailureCleanup(t *testing.T) {
	input, pixels := writeImage(t)
	file, err := object.OpenFile(input)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := file.Meta.GetRaw(core.NewTag(2, 3))
	file.Close()
	original, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.Index(original, instance)
	if at < 0 {
		t.Fatal("fixture meta UID missing")
	}
	last := len(bytes.TrimRight(instance, "\x00 ")) - 1
	if last < 0 {
		t.Fatal("empty fixture UID")
	}
	changed := append([]byte(nil), original...)
	changed[at+last] = '9'
	if original[at+last] == '9' {
		changed[at+last] = '8'
	}
	if err := os.WriteFile(input, changed, 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "checked.dcm")
	code, text, stderr := runConv(t, "-ts", "explicit-le", "-check-meta", "-o", output, input)
	if code != 0 || !strings.Contains(text, "file_meta.mismatch (0002,0003)") {
		t.Fatalf("real meta check=%d %s %s", code, text, stderr)
	}
	result, err := object.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	report, err := result.ValidatePart10Identity()
	if err != nil || len(report.Findings) != 0 {
		t.Fatal("output meta remained inconsistent")
	}
	got, _ := result.GetRaw(core.TagPixelData)
	after, _ := os.ReadFile(input)
	if !bytes.Equal(got, pixels) || !bytes.Equal(after, changed) {
		t.Fatal("meta check changed pixels/source")
	}
	bad, err := object.OpenFile(input)
	if err != nil {
		t.Fatal(err)
	}
	bad.Dataset.Put(core.NewRawElement(core.NewTag(0x0028, 0x0100), core.VRUS, binary.LittleEndian.AppendUint16(nil, 32)))
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, bad); err != nil {
		t.Fatal(err)
	}
	bad.Close()
	if err := os.WriteFile(input, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	sentinel := []byte("existing-destination")
	if err := os.WriteFile(output, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	code, _, _ = runConv(t, "-ts", "explicit-le", "-generate-icon", "-o", output, input)
	preserved, _ := os.ReadFile(output)
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(output), ".dcmconv-*.dcm"))
	if code != 1 || !bytes.Equal(preserved, sentinel) || err != nil || len(temps) != 0 {
		t.Fatal("unsupported icon published, replaced output or leaked precursor")
	}
}

func TestMetadataExplicitJPEG2000CodingRequestsReencode(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		force bool
	}{
		{[]string{"-j2k-backend", "openjpeg"}, false},
		{[]string{"-j2k-backend", "openjpeg", "-j2k-executable", "runtime"}, false},
		{[]string{"-j2k-backend", "openjpeg", "-j2k-tile", "64,64"}, true},
		{[]string{"-j2k-backend", "openjpeg", "-j2k-resolutions", "0"}, true},
		{[]string{"-j2k-backend", "openjpeg", "-j2k-rate", "10,3", "-allow-lossy"}, true},
	} {
		args := append([]string{"-ts", "1.2.840.10008.1.2.4.91", "-o", "output.dcm"}, tc.args...)
		args = append(args, "missing.dcm")
		opts, err := parseArgs(args, &bytes.Buffer{})
		if err != nil || opts.force != tc.force {
			t.Fatalf("explicit coding force=%v wanted=%v error=%v", opts.force, tc.force, err)
		}
	}
}

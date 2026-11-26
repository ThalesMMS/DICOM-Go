package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/encapimage"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

func writeInput(t testing.TB, name string, b []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if e := os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	return path
}
func cliPNG(t testing.TB) []byte {
	t.Helper()
	g := image.NewGray(image.Rect(0, 0, 2, 1))
	copy(g.Pix, []byte{9, 255})
	var b bytes.Buffer
	if e := png.Encode(&b, g); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func cliRead(t *testing.T, path string) *object.File {
	t.Helper()
	f, e := object.OpenFile(path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = f.Close() })
	r, e := iod.Validate(f.Dataset, iod.Options{CheckValues: true})
	if e != nil || len(r.Errors()) != 0 {
		t.Fatalf("IOD: %v %+v", e, r.Errors())
	}
	return f
}
func TestImg2DCMAtomicOutputPixelAndSummary(t *testing.T) {
	input := writeInput(t, "own.png", cliPNG(t))
	output := filepath.Join(t.TempDir(), "output.dcm")
	var out, diag bytes.Buffer
	if code := run([]string{input, output}, &out, &diag); code != 0 {
		t.Fatalf("exit %d: %s", code, diag.String())
	}
	f := cliRead(t, output)
	raw, _ := f.Dataset.GetRaw(core.NewTag(0x7fe0, 0x0010))
	if !bytes.Equal(raw, []byte{9, 255}) || !bytes.Contains(out.Bytes(), []byte("decoded=true")) {
		t.Fatal("pixels/summary differ")
	}
	uid, _ := f.Dataset.GetString(core.NewTag(0x0008, 0x0018))
	if !core.IsValidUID(uid) {
		t.Fatal("missing valid SOP UID")
	}
}
func TestImg2DCMFailureLeavesTargetAndNoTemporary(t *testing.T) {
	input := writeInput(t, "invalid.png", []byte("bad input"))
	dir := t.TempDir()
	output := filepath.Join(dir, "out.dcm")
	original := []byte("existing output")
	os.WriteFile(output, original, 0600)
	var out, diag bytes.Buffer
	if run([]string{input, output}, &out, &diag) != 1 {
		t.Fatal("bad input accepted")
	}
	after, _ := os.ReadFile(output)
	entries, _ := os.ReadDir(dir)
	if !bytes.Equal(after, original) || len(entries) != 1 || out.Len() != 0 {
		t.Fatal("failure changed/published output")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if runContext(ctx, []string{input, output}, &out, &diag) != 1 {
		t.Fatal("cancel accepted")
	}
	after, _ = os.ReadFile(output)
	if !bytes.Equal(after, original) {
		t.Fatal("cancel changed output")
	}
}
func TestImg2DCMRefusesPathAndHardlinkAlias(t *testing.T) {
	input := writeInput(t, "own.png", cliPNG(t))
	before, _ := os.ReadFile(input)
	alias := filepath.Join(filepath.Dir(input), "alias")
	if e := os.Link(input, alias); e != nil {
		t.Fatal(e)
	}
	for _, output := range []string{input, alias} {
		var out, diag bytes.Buffer
		if run([]string{input, output}, &out, &diag) != 1 {
			t.Fatal("alias accepted")
		}
		after, _ := os.ReadFile(input)
		if !bytes.Equal(before, after) {
			t.Fatal("input replaced")
		}
	}
}

type cancelOnWrite struct {
	cancel context.CancelFunc
	data   bytes.Buffer
	calls  int
}

func (w *cancelOnWrite) Write(p []byte) (int, error) {
	w.calls++
	n, e := w.data.Write(p)
	w.cancel()
	return n, e
}
func TestImg2DCMWriterCancellationStopsBeforeNextWrite(t *testing.T) {
	f, _, e := encapimage.FromImages(context.Background(), [][]byte{cliPNG(t)}, encapimage.ImageOptions{})
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &cancelOnWrite{cancel: cancel}
	if e := writePart10Context(ctx, w, f); !errors.Is(e, context.Canceled) || w.calls != 1 {
		t.Fatalf("cancelled writes=%d error=%v", w.calls, e)
	}
}

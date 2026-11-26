package srcli

import (
	"bytes"
	"context"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/wrappercli"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/sr"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func syntheticInput(t *testing.T, dir string) string {
	t.Helper()
	ds := object.FromElements([]core.Element{textElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.2"), textElement(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.3.4"), textElement(core.NewTag(0x0020, 0x000D), core.VRUI, "1.2.3"), textElement(core.NewTag(0x0020, 0x000E), core.VRUI, "1.2.3.5")}, nil)
	file, err := wrappercli.DatasetFile(ds)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "image.dcm")
	if err = wrappercli.WriteFile(path, file); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestKOSTitlesAndSRRenderers(t *testing.T) {
	for _, title := range []string{"113000", "113001", "113037", "113038", "113039"} {
		t.Run(title, func(t *testing.T) {
			dir := t.TempDir()
			input := syntheticInput(t, dir)
			output := filepath.Join(dir, "kos.dcm")
			args := []string{"-o", output, "-title", title}
			if title == "113001" {
				args = append(args, "-quality-reason", "111210|DCM|Motion blur")
			}
			args = append(args, input)
			if err := Execute(context.Background(), "mkkos", args, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			file, err := wrappercli.ReadDICOM(output, true)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := sr.ReadDocument(file.Dataset)
			if err != nil || doc.Title.CodeValue != title {
				t.Fatalf("title = %v, err %v", doc, err)
			}
			if title != "113000" {
				if _, err = sr.ReadKOSRejection(context.Background(), file.Dataset, sr.KOSRejectionLimits{}); err != nil {
					t.Fatal(err)
				}
			}
			for _, tool := range []string{"dsrdump", "dsr2html"} {
				var b bytes.Buffer
				if err := Execute(context.Background(), tool, []string{output}, &b, io.Discard); err != nil {
					t.Fatal(err)
				}
				if b.Len() == 0 || tool == "dsr2html" && !strings.Contains(b.String(), "<html") {
					t.Fatal("missing rendered document")
				}
			}
			file.Close()
		})
	}
}
func TestKOSUsageRefusals(t *testing.T) {
	dir := t.TempDir()
	in := syntheticInput(t, dir)
	for _, args := range [][]string{{"-o", filepath.Join(dir, "unknown"), "-title", "invalid", in}, {"-o", filepath.Join(dir, "quality"), "-title", "113001", in}} {
		if err := Execute(context.Background(), "mkkos", args, io.Discard, io.Discard); err == nil {
			t.Fatal("invalid KOS accepted")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "quality")); !os.IsNotExist(err) {
		t.Fatal("partial output")
	}
}

// -title 113030 selects the KOS document title (113030, DCM, "Manifest")
// of CID 7010 (DCMTK CODE_DCM_Manifest) for an ordinary key object selection.
func TestKOSManifestTitle(t *testing.T) {
	dir := t.TempDir()
	input := syntheticInput(t, dir)
	output := filepath.Join(dir, "manifest.dcm")
	if err := Execute(context.Background(), "mkkos", []string{"-o", output, "-title", "113030", input}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	file, err := wrappercli.ReadDICOM(output, true)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	doc, err := sr.ReadDocument(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Title != sr.KOSTitleManifest || doc.SOPClassUID != sr.KeyObjectSelectionDocumentStorage {
		t.Fatalf("title %+v, SOP Class %s", doc.Title, doc.SOPClassUID)
	}
	if refs := sr.KeyObjectSelectionImages(doc); len(refs) != 1 || refs[0].SOPInstanceUID != "1.2.3.4" {
		t.Fatalf("references = %+v", refs)
	}
	for _, args := range [][]string{
		{"-o", filepath.Join(dir, "reason"), "-title", "113030", "-quality-reason", "111210|DCM|Motion blur", input},
		{"-o", filepath.Join(dir, "composite"), "-title", "113030", "-reference-type", "COMPOSITE", input},
	} {
		if err := Execute(context.Background(), "mkkos", args, io.Discard, io.Discard); err == nil {
			t.Fatalf("manifest accepted %v", args)
		}
	}
}

func textElement(tag core.Tag, vr core.VR, value string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{value}}
}

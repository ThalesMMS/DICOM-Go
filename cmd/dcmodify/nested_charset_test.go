package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Metadata rewrites need to encode an edited element in its containing
// dataset's repertoire, including explicit overrides on sequence items.
func TestDcmodifyNestedCharsetEditsPersistOrRefuseWithoutPublication(t *testing.T) {
	charsetTag := core.NewTag(0x0008, 0x0005)
	sequenceTag := core.NewTag(0x0008, 0x1110)
	nameTag := core.NewTag(0x0010, 0x0010)
	documentTag := core.NewTag(0x0042, 0x0011)
	document := []byte{0x10, 0xff, 0, 7}
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			dir := t.TempDir()
			input := dc1353Input(t, dir, "input.dcm", syntax,
				dicomtest.NewStringElement(charsetTag, core.VRCS, "ISO_IR 100"),
				dicomtest.NewSequenceElement(sequenceTag,
					core.DataSet{Elements: []core.Element{dicomtest.NewPNElement(nameTag, "FIRST^ORIGINAL")}},
					core.DataSet{Elements: []core.Element{
						dicomtest.NewStringElement(charsetTag, core.VRCS, "ISO_IR 192"),
						dicomtest.NewPNElement(nameTag, "SECOND^ORIGINAL"),
					}},
				),
				core.NewRawElement(documentTag, core.VROB, document),
			)
			original, err := os.ReadFile(input)
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name       string
				inPlace    bool
				firstEdit  string
				secondEdit string
				wantCode   int
			}{
				{"both scopes can represent edits", false, "(0008,1110)[0].PatientName=Noël^Ana", "(0008,1110)[1].PatientName=林^晴", 0},
				{"unrepresentable inherited scope before compatible edit", false, "(0008,1110)[0].PatientName=林^晴", "(0008,1110)[1].PatientName=Noël^Ana", 1},
				{"unrepresentable inherited scope after compatible edit", true, "(0008,1110)[1].PatientName=Noël^Ana", "(0008,1110)[0].PatientName=林^晴", 1},
			} {
				t.Run(tc.name, func(t *testing.T) {
					output := filepath.Join(dir, tc.name+".dcm")
					args := []string{"-m", tc.firstEdit, "-m", tc.secondEdit}
					if tc.inPlace {
						args = append(args, "-in-place")
					} else {
						args = append(args, "-o", output)
					}
					if code := dc1353Run(t, append(args, input)...); code != tc.wantCode {
						t.Fatalf("exit = %d, want %d", code, tc.wantCode)
					}
					current, err := os.ReadFile(input)
					if err != nil || !bytes.Equal(current, original) {
						t.Fatalf("source changed, read error %v", err)
					}
					dc1353NoTemps(t, dir)
					if tc.wantCode != 0 {
						if _, err := os.Stat(output); !os.IsNotExist(err) {
							t.Fatalf("failed edit published output: %v", err)
						}
						return
					}
					reopened := dc1353Read(t, output)
					if value, ok := reopened.GetString(nameTag); !ok || value != "SYNTHETIC^OLD" {
						t.Fatalf("root Patient Name = %q (%v)", value, ok)
					}
					raw, found := reopened.GetRaw(documentTag)
					if !found || !bytes.Equal(raw, document) || reopened.TransferSyntax.UID != syntax.UID {
						t.Fatalf("opaque data or transfer syntax changed: %x, %s", raw, reopened.TransferSyntax.UID)
					}
					items, ok := reopened.Dataset.GetSequence(sequenceTag)
					if !ok || len(items) != 2 {
						t.Fatalf("target sequence = %v, found %v", items, ok)
					}
					for i, want := range []struct {
						text string
						wire []byte
					}{
						{"Noël^Ana", []byte{'N', 'o', 0xeb, 'l', '^', 'A', 'n', 'a'}},
						{"林^晴", []byte("林^晴 ")},
					} {
						value, err := items[i].LookupString(nameTag)
						raw, ok := items[i].GetRaw(nameTag)
						if err != nil || value != want.text || !ok || !bytes.Equal(raw, want.wire) {
							t.Errorf("item %d Patient Name = %q (%x), %v", i, value, raw, err)
						}
					}
				})
			}
		})
	}
}

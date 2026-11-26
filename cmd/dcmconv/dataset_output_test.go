package main

import (
	"bytes"
	"compress/flate"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestConvContainerAndSerializationMatrix(t *testing.T) {
	elems := append([]core.Element{core.NewRawElement(core.NewTag(0x0008, 0), core.VRUL, []byte{0, 0, 0, 0}),
		dicomtest.NewSequenceElement(core.NewTag(0x0008, 0x1140), core.DataSet{Elements: []core.Element{dicomtest.NewUIElement(core.NewTag(0x0008, 0x1150), "1.2.3")}})}, dicomtest.DatasetWithPixelData()...)
	inputBytes, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian, elems...)
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "in.dcm")
	if err := os.WriteFile(input, inputBytes, 0600); err != nil {
		t.Fatal(err)
	}
	for _, ts := range []string{"implicit-le", "explicit-le", "explicit-be", "deflated"} {
		target, _ := resolveSyntax(ts)
		for _, group := range []string{"preserve", "recalculate", "create", "remove"} {
			for _, seq := range []string{"undefined", "defined", "preserve"} {
				var part10Dataset []byte
				for _, datasetOnly := range []bool{false, true} {
					name := ts + "/" + group + "/" + seq
					if datasetOnly {
						name += "/dataset"
					}
					t.Run(name, func(t *testing.T) {
						output := filepath.Join(t.TempDir(), "out.dcm")
						args := []string{"-ts", ts, "-group-length", group, "-sequence-length", seq, "-o", output}
						if datasetOnly {
							args = append(args, "-dataset-only")
						}
						args = append(args, input)
						if code, _, stderr := runConv(t, args...); code != 0 {
							t.Fatalf("code=%d stderr=%s", code, stderr)
						}
						data, err := os.ReadFile(output)
						if err != nil {
							t.Fatal(err)
						}
						encodedDataset := data
						if !datasetOnly {
							header, err := object.ReadPart10Header(bytes.NewReader(data), object.ReadFileOptions{})
							if err != nil {
								t.Fatal(err)
							}
							encodedDataset = data[header.DataSetOffset:]
							part10Dataset = append([]byte(nil), encodedDataset...)
						} else if !bytes.Equal(encodedDataset, part10Dataset) {
							t.Fatal("dataset extraction changed verified Part 10 body bytes")
						}
						var obj *object.Object
						if datasetOnly {
							if len(data) > 132 && string(data[128:132]) == "DICM" {
								t.Fatal("dataset-only wrote Part 10")
							}
							obj, err = object.OpenDataSet(output, target)
						} else {
							var f *object.File
							f, err = object.OpenFile(output)
							if err == nil {
								defer f.Close()
								obj = f.Dataset
							}
						}
						if err != nil {
							t.Fatal(err)
						}
						if datasetOnly {
							defer obj.Close()
						}
						_, present := obj.Get(core.NewTag(0x0008, 0))
						if present != (group != "remove") {
							t.Fatal("group length presence wrong")
						}
						if group == "preserve" {
							n, ok := obj.GetRaw(core.NewTag(0x0008, 0))
							if !ok || !bytes.Equal(n, []byte{0, 0, 0, 0}) {
								t.Fatal("group value was not preserved")
							}
						}
						sq, ok := obj.Get(core.NewTag(0x0008, 0x1140))
						if !ok || sq.EncodedLength().IsUndefined() != (seq != "defined") {
							t.Fatal("sequence length policy wrong")
						}
						if ts == "deflated" {
							if len(encodedDataset)%2 != 0 {
								t.Fatal("Deflate dataset has odd encoded length")
							}
							// ReadByte avoids consuming the optional A.5 NULL.
							reader := bytes.NewReader(encodedDataset)
							inflated := flate.NewReader(reader)
							_, copyErr := io.Copy(io.Discard, inflated)
							closeErr := inflated.Close()
							if copyErr != nil || closeErr != nil {
								t.Fatal("raw Deflate stream failed")
							}
							tail := encodedDataset[len(encodedDataset)-reader.Len():]
							if len(tail) > 1 || len(tail) == 1 && tail[0] != 0 {
								t.Fatal("Deflate stream has invalid trailing bytes")
							}
						}
					})
				}
			}
		}
	}
	after, _ := os.ReadFile(input)
	if !bytes.Equal(after, inputBytes) {
		t.Fatal("serialization changed source")
	}
}
func TestConvDatasetOnlyPublicationAndDomains(t *testing.T) {
	input, _ := writeImage(t)
	dir := t.TempDir()
	output := filepath.Join(dir, "out.dcm")
	if code, _, _ := runConv(t, "-dataset-only", "-padding", "create", "-pad-file", "512", "-o", output, input); code != 2 {
		t.Fatal("Part 10 alignment accepted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("invalid option wrote output")
	}
	if err := os.WriteFile(output, []byte("KEEP"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runConv(t, "-ts", "explicit-le", "-dataset-only", "-o", output, input); code != 1 {
		t.Fatal("dataset-only overwrote destination")
	}
	if b, _ := os.ReadFile(output); string(b) != "KEEP" {
		t.Fatal("destination changed")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runConv(t, "-ts", "explicit-le", "-dataset-only", "-o", filepath.Join(link, "new.dcm"), input); code != 1 {
		t.Fatal("symlink parent accepted")
	}
	atomic := filepath.Join(t.TempDir(), "atomic.dcm")
	if code, out, err := runConv(t, "-ts", "explicit-le", "-dataset-only", "-durability", "atomic", "-o", atomic, input); code != 0 || !strings.Contains(out, "file-synced=false directory-synced=false") {
		t.Fatalf("%d %s %s", code, out, err)
	}
	opts := pixeldata.TranscodeOptions{Limits: pixeldata.TranscodeLimits{MaxOutputBytes: 1}}
	short := filepath.Join(t.TempDir(), "short.dcm")
	if _, err := transcodeDatasetOnly(context.Background(), input, short, transfer.ExplicitVRLittleEndian, opts); err == nil {
		t.Fatal("output limit ignored")
	}
	if _, err := os.Stat(short); !os.IsNotExist(err) {
		t.Fatal("bounded failure published")
	}
	if _, _, err := transcodeOptions(options{syntax: "explicit-le", groupLength: "bad"}); err == nil {
		t.Fatal("invalid group policy accepted")
	}
}

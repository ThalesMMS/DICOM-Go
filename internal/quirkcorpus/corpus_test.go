package quirkcorpus

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestGDCMQuirkCorpusStageMatrix(t *testing.T) {
	fixtures, e := Fixtures()
	if e != nil {
		t.Fatal(e)
	}
	if len(fixtures) != 29 {
		t.Fatal("named quirk coverage", len(fixtures))
	}
	for _, f := range fixtures {
		t.Run(f.ID, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), f.ID+".dcm")
			if e := os.WriteFile(path, f.Wire, 0600); e != nil {
				t.Fatal(e)
			}
			for _, recover := range []bool{false, true} {
				got, e := Scan(context.Background(), path, f, recover)
				if e != nil {
					t.Error(got.Mode, e)
					continue
				}
				want := f.Strict
				if recover {
					want = f.Recover
				}
				t.Logf("%s: %+v, recovery=%v, diagnostic=%s", got.Mode, got.Stages, got.Recovery, got.Diagnostic)
				if got.Stages != want {
					t.Errorf("%s stages=%+v want=%+v", got.Mode, got.Stages, want)
				}
				retained, e := os.ReadFile(path)
				if e != nil || !bytes.Equal(retained, f.Wire) || got.SourceSHA256 != Hash(retained) {
					t.Fatalf("source changed or hash mismatch: %v", e)
				}
				if got.Stages.Parse == "ok" && !got.MetadataChecked {
					t.Fatal("successful parse without independent metadata check")
				}
				if got.Stages.Decode == "reject" && bytes.Contains([]byte(got.Diagnostic), []byte("no pixel data codec registered")) {
					t.Fatal("rejection must exercise a registered decoder")
				}
			}
		})
	}
}

func fixtureByID(t *testing.T, id string) Fixture {
	t.Helper()
	fixtures, e := Fixtures()
	if e != nil {
		t.Fatal(e)
	}
	for _, f := range fixtures {
		if f.ID == id {
			return f
		}
	}
	t.Fatal("unknown fixture", id)
	return Fixture{}
}

func TestGDCMQuirkMetadataCanariesKeepPixelHash(t *testing.T) {
	f := fixtureByID(t, "FLAIR-wrong-group-length")
	for _, id := range []string{"dimensions", "orientation", "spacing", "position", "photometric", "bit-depth", "signedness", "missing-signedness", "meta-sop"} {
		t.Run(id, func(t *testing.T) {
			file, e := object.ReadFileWithOptions(bytes.NewReader(f.Wire), ReadOptions())
			if e != nil {
				t.Fatal(e)
			}
			defer file.Close()
			if e := CheckMetadata(file, f.Oracle); e != nil {
				t.Fatal(e)
			}
			before, e := semanticSnapshot(file.Dataset, binary.LittleEndian)
			if e != nil {
				t.Fatal(e)
			}
			pixel, _ := file.Dataset.Get(core.TagPixelData)
			raw, ok := pixel.RawBytes()
			if !ok {
				t.Fatal("missing raw pixels")
			}
			hash := Hash(raw)
			switch id {
			case "dimensions":
				file.Dataset.Put(us(0x0028, 0x0010, 1))
				file.Dataset.Put(us(0x0028, 0x0011, 4))
			case "orientation":
				file.Dataset.Put(text(0x0020, 0x0037, core.VRDS, "0\\1\\0\\1\\0\\0"))
			case "spacing":
				file.Dataset.Put(text(0x0028, 0x0030, core.VRDS, "2\\1.5"))
			case "position":
				file.Dataset.Put(text(0x0020, 0x0032, core.VRDS, "5\\7\\10"))
			case "photometric":
				file.Dataset.Put(text(0x0028, 0x0004, core.VRCS, "MONOCHROME1"))
			case "bit-depth":
				file.Dataset.Put(us(0x0028, 0x0101, 7))
				file.Dataset.Put(us(0x0028, 0x0102, 6))
			case "signedness":
				file.Dataset.Put(us(0x0028, 0x0103, 1))
			case "missing-signedness":
				file.Dataset.Remove(tag(0x0028, 0x0103))
			case "meta-sop":
				file.Meta.Put(text(0x0002, 0x0002, core.VRUI, "1.2.840.10008.5.1.4.1.1.2"))
			}
			pixel, _ = file.Dataset.Get(core.TagPixelData)
			raw, ok = pixel.RawBytes()
			if !ok || Hash(raw) != hash {
				t.Fatal("canary changed Pixel Data")
			}
			if e := CheckMetadata(file, f.Oracle); e == nil {
				t.Fatal("pixel hash hid a metadata regression")
			}
			after, e := semanticSnapshot(file.Dataset, binary.LittleEndian)
			if e != nil || (id != "meta-sop" && reflect.DeepEqual(before, after)) {
				t.Fatal("preservation snapshot missed mutation", e)
			}
		})
	}
}

func TestGDCMQuirkJPEGControlBeforeTrailingInformation(t *testing.T) {
	f := fixtureByID(t, "ACUSON-8-YBR_FULL-JPEG-TrailingInfo")
	file, e := object.ReadFileWithOptions(bytes.NewReader(f.Wire), ReadOptions())
	if e != nil {
		t.Fatal(e)
	}
	defer file.Close()
	element, _ := file.Dataset.Get(core.TagPixelData)
	sequence, ok := element.Value.(core.FragmentSequence)
	if !ok || len(sequence.Fragments) != 1 {
		t.Fatal("expected single authored JPEG fragment")
	}
	payload := sequence.Fragments[0]
	tail := bytes.Index(payload, []byte("OWN-TRAILER"))
	if tail < 2 {
		t.Fatal("missing own trailer")
	}
	payload = append([]byte{}, payload[:tail]...)
	if len(payload)%2 != 0 {
		payload = append(payload, 0)
	}
	file.Dataset.Put(dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, payload))
	f.Wire = part10Meta(file.Meta.Elements(), encoded(transfer.ExplicitVRLittleEndian, file.Dataset.Elements()...))
	path := filepath.Join(t.TempDir(), "control.dcm")
	if e = os.WriteFile(path, f.Wire, 0600); e != nil {
		t.Fatal(e)
	}
	got, e := Scan(context.Background(), path, f, false)
	if e != nil || got.Stages != good() {
		t.Fatal("valid JPEG control did not decode/render independently authored samples", got, e)
	}
}

func TestGDCMQuirkAdmissionCancellationAndPartialWrites(t *testing.T) {
	f := fixtureByID(t, "PMS-IncompletePixelData")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := Scan(ctx, "not-opened", f, true); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "fixture.dcm")
	for _, wire := range [][]byte{append(append([]byte{}, f.Wire...), 0), make([]byte, MaxFixtureBytes+1)} {
		if e := os.WriteFile(path, wire, 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := Scan(context.Background(), path, f, true); e == nil {
			t.Fatal("foreign/oversize fixture admitted")
		}
	}
	opts := ReadOptions()
	opts.AllowIncompletePixelData = true
	file, e := object.ReadFileWithOptions(bytes.NewReader(f.Wire), opts)
	if file == nil || !errors.Is(e, object.ErrPartialRead) {
		t.Fatal("missing partial error", e)
	}
	defer file.Close()
	var out bytes.Buffer
	if e := object.WriteFile(&out, file); !errors.Is(e, object.ErrPartialFile) || out.Len() != 0 {
		t.Fatal("partial write escaped", e, out.Len())
	}
	oversize := fixtureByID(t, "i32.XADC.7.215MegaBytes")
	file, e = object.ReadFileWithOptions(bytes.NewReader(oversize.Wire), ReadOptions())
	if e == nil || file != nil {
		t.Fatal("declared byte budget did not reject before publication", e)
	}
}

func TestGDCMQuirkCommittedManifest(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "quirks")
	data, e := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if e != nil {
		t.Fatal(e)
	}
	var got Manifest
	if e := json.Unmarshal(data, &got); e != nil {
		t.Fatal(e)
	}
	fixtures, e := Fixtures()
	if e != nil {
		t.Fatal(e)
	}
	want := NewManifest(fixtures)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("committed manifest diverges from authored expectations")
	}
	for _, f := range fixtures {
		wire, e := os.ReadFile(filepath.Join(dir, f.ID+".dcm"))
		if e != nil || !bytes.Equal(wire, f.Wire) {
			t.Fatal(f.ID, "committed fixture changed", e)
		}
	}
}

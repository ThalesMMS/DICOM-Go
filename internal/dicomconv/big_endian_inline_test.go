package dicomconv

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// bigEndianInlineTags are small OW, OF, OD, OL and OV values, a palette
// among them, that paletteFile adds to richFile.
var bigEndianInlineTags = []core.Tag{
	core.NewTag(0x0028, 0x1201), // Red Palette Color Lookup Table Data, OW
	core.NewTag(0x0066, 0x0016), // Point Coordinates Data, OF
	core.NewTag(0x0066, 0x0022), // Double Point Coordinates Data, OD
	core.NewTag(0x0066, 0x0040), // Long Primitive Point Index List, OL
	core.NewTag(0x0072, 0x0081), // Selector OV Value, OV
	core.TagPixelData,           // OW, 16 bits allocated
}

// paletteFile returns richFile with the values of bigEndianInlineTags,
// written and read again in syntax: the writer encodes every value in the
// byte order of syntax.
func paletteFile(t *testing.T, syntax transfer.Syntax) *object.File {
	t.Helper()
	file := richFile(t)
	vrs := []core.VR{core.VROW, core.VROF, core.VROD, core.VROL, core.VROV}
	for i, vr := range vrs {
		value := make([]byte, 16)
		for j := range value {
			value[j] = byte(i*16 + j + 1)
		}
		file.Dataset.Put(raw(bigEndianInlineTags[i], vr, value))
	}
	file.TransferSyntax = syntax
	return reread(t, file)
}

func jsonInlineBinaries(t *testing.T, encoded []byte) map[core.Tag][]byte {
	t.Helper()
	var document map[string]struct {
		InlineBinary string
	}
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	out := map[core.Tag][]byte{}
	for _, tag := range bigEndianInlineTags {
		element, ok := document[tag.HexString()]
		data, err := base64.StdEncoding.DecodeString(element.InlineBinary)
		if !ok || element.InlineBinary == "" || err != nil {
			t.Fatalf("%s InlineBinary %q: %v", tag, element.InlineBinary, err)
		}
		out[tag] = data
	}
	return out
}

// dcm2json of an Explicit VR Big Endian object writes the Little Endian
// InlineBinary of the Explicit VR Little Endian object with the same values,
// and json2dcm restores the Big Endian object byte for byte, with the binary
// values inline or in bulk data files (#1982).
func TestBigEndianJSONHasLittleEndianInlineBinaryAndRoundTrips(t *testing.T) {
	ctx := context.Background()
	little := paletteFile(t, transfer.ExplicitVRLittleEndian)
	big := paletteFile(t, transfer.ExplicitVRBigEndian)
	for _, withMeta := range []bool{false, true} {
		opts := EncodeOptions{WithMeta: withMeta, KeepCharacterSet: true}
		reference, err := Encode(ctx, little, JSON, opts)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := Encode(ctx, big, JSON, opts)
		if err != nil {
			t.Fatal(err)
		}
		want, got := jsonInlineBinaries(t, reference), jsonInlineBinaries(t, encoded)
		for _, tag := range bigEndianInlineTags {
			if !bytes.Equal(got[tag], want[tag]) {
				t.Errorf("WithMeta=%t: %s InlineBinary % X, want the Little Endian % X", withMeta, tag, got[tag], want[tag])
			}
		}
	}
	for _, bulk := range []bool{false, true} {
		dir := t.TempDir()
		opts := EncodeOptions{WithMeta: true, KeepCharacterSet: true}
		if bulk {
			opts.BulkDir, opts.BulkThreshold, opts.BaseDir = filepath.Join(dir, "bulk"), 4, dir
		}
		encoded, err := Encode(ctx, big, JSON, opts)
		if err != nil {
			t.Fatal(err)
		}
		back, err := Decode(ctx, encoded, JSON, DecodeOptions{BaseDir: dir})
		if err != nil {
			t.Fatalf("bulk=%t: decode: %v", bulk, err)
		}
		if back.TransferSyntax.UID != transfer.ExplicitVRBigEndian.UID {
			t.Fatalf("bulk=%t: transfer syntax %s", bulk, back.TransferSyntax.UID)
		}
		target := filepath.Join(dir, "out.dcm")
		if err := WritePart10(target, back); err != nil {
			t.Fatal(err)
		}
		written, err := ReadPart10(target)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(datasetBytes(t, written), datasetBytes(t, big)) {
			t.Fatalf("bulk=%t: the Big Endian data set changed through JSON", bulk)
		}
		_ = written.Close()
	}
}

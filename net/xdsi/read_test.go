package xdsi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestRetrievedIdentityRejectsDuplicateWireTags(t *testing.T) {
	data := imageBytes(t, testImage)
	tag := []byte{0x20, 0, 0x0d, 0, 'U', 'I'}
	start := bytes.Index(data, tag)
	if start < 0 {
		t.Fatal("fixture study tag")
	}
	end := start + 8 + int(binary.LittleEndian.Uint16(data[start+6:start+8]))
	duplicate := append([]byte(nil), data[:end]...)
	duplicate = append(duplicate, data[start:end]...)
	duplicate = append(duplicate, data[end:]...)
	limits, _ := normalizeLimits(Limits{})
	if _, err := validateInstance(context.Background(), duplicate, mustManifest(t).refs[0], testMetadata().SourcePatient,
		[]string{transfer.ExplicitVRLittleEndian.UID}, limits); !errors.Is(err, ErrIdentity) {
		t.Fatalf("duplicate identity: %v", err)
	}
}

func TestRetrievedDeflatedImageHonorsMetadataBudget(t *testing.T) {
	ds := testImageObject(testImage)
	ds.Put(derivedio.Str(core.NewTag(0x0010, 0x4000), core.VRLT, string(bytes.Repeat([]byte{'x'}, 10000))))
	file, err := derivedio.File(testCT, testImage, ds)
	if err != nil {
		t.Fatal(err)
	}
	file.TransferSyntax = transfer.DeflatedExplicitVRLittleEndian
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	limits, _ := normalizeLimits(Limits{})
	limits.MetadataBytes = 4096
	if _, err := validateInstance(context.Background(), encoded.Bytes(), mustManifest(t).refs[0], testMetadata().SourcePatient,
		[]string{transfer.DeflatedExplicitVRLittleEndian.UID}, limits); !errors.Is(err, ErrIdentity) {
		t.Fatal(err)
	}
	limits.MetadataBytes = 20000
	if syntax, err := validateInstance(context.Background(), encoded.Bytes(), mustManifest(t).refs[0], testMetadata().SourcePatient,
		[]string{transfer.DeflatedExplicitVRLittleEndian.UID}, limits); err != nil || syntax != transfer.DeflatedExplicitVRLittleEndian.UID {
		t.Fatalf("within budget: %s %v", syntax, err)
	}
}

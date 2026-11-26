package dicomconv

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestDCM2JSONBulkFileWritesResolvableFileURIs(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "in.dcm")
	file := richFile(t)
	if err := WritePart10(source, file); err != nil {
		t.Fatal(err)
	}
	document := filepath.Join(dir, "doc.json")
	bulkFile := filepath.Join(dir, "bulk.bin")
	var stdout, stderr bytes.Buffer
	args := []string{"-keep-charset", "-bulk-file", bulkFile, "-bulk-threshold", "8", source, document}
	if code := Run(DCM2JSON, args, &stdout, &stderr); code != 0 {
		t.Fatalf("dcm2json: exit %d: %s", code, stderr.String())
	}
	encoded, err := os.ReadFile(document)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"BulkDataURI": "file:///`)) || !bytes.Contains(encoded, []byte("offset=")) {
		t.Fatalf("no file: BulkDataURI with offset written:\n%s", encoded)
	}
	if info, err := os.Stat(bulkFile); err != nil || info.Size() == 0 {
		t.Fatalf("bulk file = %v, %v", info, err)
	}

	jsonOpts := dicomjson.DefaultUnmarshalOptions()
	jsonOpts.TransferSyntax = transfer.ExplicitVRLittleEndian
	decoded, err := dicomjson.UnmarshalContext(context.Background(), encoded, std.Dictionary, jsonOpts)
	if err != nil {
		t.Fatal(err)
	}
	if err := object.ResolveBulkData(context.Background(), decoded, object.FileBulkDataResolver{Root: dir}, object.ResolveBulkDataOptions{}); err != nil {
		t.Fatalf("ResolveBulkData: %v", err)
	}
	var resolved bytes.Buffer
	if err := object.WriteDataSet(&resolved, decoded, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(resolved.Bytes(), datasetBytes(t, file)) {
		t.Fatal("resolved file: references changed the data set")
	}

	if code := Run(DCM2JSON, []string{"-bulk-file", bulkFile, "-bulk-dir", dir, source, document}, &stdout, &stderr); code != 1 {
		t.Fatalf("-bulk-file with -bulk-dir: exit %d, want 1", code)
	}
}

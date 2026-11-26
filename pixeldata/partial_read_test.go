package pixeldata_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestDecompressRefusesPartialFile(t *testing.T) {
	obj := object.New(nil)
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}})
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.3.4"}})
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, &object.File{Dataset: obj, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	data := append(buf.Bytes(), 0x10, 0x00, 0x10, 0x00, 'P', 'N', 8, 0, 'A') // truncated value
	f, err := object.ReadFileWithOptions(bytes.NewReader(data), object.ReadFileOptions{AllowPartialRead: true})
	if !errors.Is(err, object.ErrPartialRead) || f == nil {
		t.Fatalf("partial read: %v", err)
	}
	if _, err := pixeldata.DecompressFileContext(context.Background(), f, pixeldata.DecompressOptions{}); !errors.Is(err, object.ErrPartialFile) {
		t.Fatalf("decompress error = %v", err)
	}
}

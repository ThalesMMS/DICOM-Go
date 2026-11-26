package object

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestWriteFileFramesInheritsPaddingAndLevel(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.DeflatedExplicitVRLittleEndian} {
		frames := &memoryFrames{frames: [][]byte{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}}}
		header := FromElements(frameTestDataSet(3, 8), nil)
		whole := FromElements(append(frameTestDataSet(3, 8), core.NewRawElement(core.TagPixelData, core.VROB, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 0})), nil)
		opts := WriteFileOptions{Padding: PaddingOptions{Policy: PaddingCreate, Alignment: 512}, GroupLength: GroupLengthCreate}
		if syntax.Deflated {
			level := 1
			opts.DeflateLevel = &level
		}
		var want, got bytes.Buffer
		if err := WriteFileWithOptions(&want, &File{Dataset: whole, TransferSyntax: syntax}, opts); err != nil {
			t.Fatal(err)
		}
		if err := WriteFileFrames(&got, &File{Dataset: header, TransferSyntax: syntax}, frames, FrameWriteOptions{WriteFileOptions: opts}); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want.Bytes(), got.Bytes()) {
			t.Fatal("frame writer options differ")
		}
		if !reflect.DeepEqual(frames.reads, []int{0, 1, 2}) {
			t.Fatalf("extra frame replay: %v", frames.reads)
		}
	}
}

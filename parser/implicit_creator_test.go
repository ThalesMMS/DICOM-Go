package parser

import (
	"bytes"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// TestImplicitPrivateCreatorVR reads Private Creators as LO in Implicit VR
// without a private dictionary (PS3.5 7.8.1), in every block and inside
// items, while odd groups below 0009 and an undefined-length creator, which
// only the sequence grammar can read, stay UN.
func TestImplicitPrivateCreatorVR(t *testing.T) {
	syntax := transfer.ImplicitVRLittleEndian
	undefined := definedElementBytes(syntax, core.NewTag(0x0013, 0x0010), core.VRUN, 0xffffffff, nil)
	undefined = append(undefined, 0xfe, 0xff, 0x00, 0xe0, 0, 0, 0, 0, 0xfe, 0xff, 0xdd, 0xe0, 0, 0, 0, 0)
	stream := bytes.Join([][]byte{
		definedElementBytes(syntax, core.NewTag(0x0003, 0x0010), core.VRUN, 2, []byte("AB")),
		definedElementBytes(syntax, core.NewTag(0x0009, 0x0010), core.VRLO, 4, []byte("ACME")),
		definedElementBytes(syntax, core.NewTag(0x0009, 0x00ff), core.VRLO, 4, []byte("LAST")),
		definedElementBytes(syntax, core.NewTag(0x0009, 0x1001), core.VRUN, 2, []byte{1, 2}),
		undefined,
	}, nil)
	want := map[core.Tag]core.VR{
		core.NewTag(0x0003, 0x0010): core.VRUN,
		core.NewTag(0x0009, 0x0010): core.VRLO,
		core.NewTag(0x0009, 0x00ff): core.VRLO,
		core.NewTag(0x0009, 0x1001): core.VRUN,
		core.NewTag(0x0013, 0x0010): core.VRUN,
	}
	reader := NewReader(bytes.NewReader(stream), syntax, ReaderOptions{Dictionary: std.Dictionary})
	seen := map[core.Tag]bool{}
	for {
		tok, err := reader.Next()
		if err != nil {
			break
		}
		tag, vr := tok.Element.Tag(), tok.Element.VR()
		if tok.Kind != TokenElement {
			tag, vr = tok.Header.Tag, tok.Header.VR
		}
		if expected, ok := want[tag]; ok && !seen[tag] {
			seen[tag] = true
			if vr != expected {
				t.Fatalf("%s: VR %s, want %s", tag, vr, expected)
			}
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("read %d of %d elements", len(seen), len(want))
	}
}

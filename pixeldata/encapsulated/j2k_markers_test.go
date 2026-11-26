package encapsulated_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
)

func j2kFixtureFrames(t *testing.T) [][]byte {
	t.Helper()
	file, err := object.OpenFile(filepath.Join("..", "codecfixture", "testdata", "codecfull", "pydicom", "emri_small_jpeg_2k_lossless.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	element, _ := file.Dataset.Get(core.TagPixelData)
	sequence := element.Value.(core.FragmentSequence)
	if len(sequence.Fragments) != 10 || len(sequence.OffsetTable) != 0 {
		t.Fatalf("fixture layout %d items / BOT %d", len(sequence.Fragments), len(sequence.OffsetTable))
	}
	return sequence.Fragments
}

// withComment inserts a COM segment carrying FF D9 FF 4F after SIZ, so a scanner
// searching for markers instead of honouring lengths would end the frame early.
func withComment(t *testing.T, frame []byte) []byte {
	t.Helper()
	if !bytes.HasPrefix(frame, []byte{0xFF, 0x4F, 0xFF, 0x51}) {
		t.Fatal("frame does not start with SOC SIZ")
	}
	sizEnd := 4 + int(binary.BigEndian.Uint16(frame[4:]))
	comment := []byte{0xFF, 0x64, 0x00, 0x08, 0x00, 0x01, 0xFF, 0xD9, 0xFF, 0x4F}
	out := append(append(append([]byte(nil), frame[:sizEnd]...), comment...), frame[sizEnd:]...)
	// Psot of the first SOT counts only its own tile-part, so it is unchanged.
	return out
}

// lastTilePartToEOC rewrites the single tile-part's Psot to 0, which the
// standard allows for the last tile-part (the tile-part then runs to EOC).
func lastTilePartToEOC(t *testing.T, frame []byte) []byte {
	t.Helper()
	at := bytes.Index(frame, []byte{0xFF, 0x90, 0x00, 0x0A})
	if at < 0 {
		t.Fatal("no SOT")
	}
	out := append([]byte(nil), frame...)
	binary.BigEndian.PutUint32(out[at+6:], 0)
	return out
}

func plan(t *testing.T, sequence core.FragmentSequence, frames int) ([][]byte, error) {
	t.Helper()
	p, err := encapsulated.FromFragments(context.Background(), sequence, nil, frames, encapsulated.JPEG2000, encapsulated.Limits{})
	if err != nil {
		return nil, err
	}
	out := make([][]byte, p.Len())
	for i := range out {
		view, err := p.Frame(context.Background(), i)
		if err != nil {
			return nil, err
		}
		out[i] = append([]byte(nil), view.Data...)
	}
	return out, nil
}

func TestJPEG2000FramingInfersBoundariesStructurally(t *testing.T) {
	frames := j2kFixtureFrames(t)
	for i, frame := range frames {
		if err := encapsulated.ValidateFrame(context.Background(), frame, encapsulated.JPEG2000); err != nil {
			t.Fatalf("fixture frame %d: %v", i, err)
		}
	}
	special := []([]byte){withComment(t, frames[0]), lastTilePartToEOC(t, frames[1]), frames[2]}
	for i, frame := range special[:2] {
		if err := encapsulated.ValidateFrame(context.Background(), frame, encapsulated.JPEG2000); err != nil {
			t.Fatalf("special frame %d: %v", i, err)
		}
	}
	// Each frame split across two even-length Items, empty BOT: boundaries
	// come from the codestream structure alone.
	var fragments [][]byte
	for _, frame := range special {
		split := len(frame) / 2 &^ 1
		fragments = append(fragments, frame[:split], frame[split:])
	}
	got, err := plan(t, core.FragmentSequence{Fragments: fragments}, len(special))
	if err != nil {
		t.Fatal(err)
	}
	for i := range special {
		if !bytes.Equal(got[i], special[i]) {
			t.Fatalf("frame %d reassembled wrongly", i)
		}
	}
	// One Item per frame with the fixture's own layout.
	got, err = plan(t, core.FragmentSequence{Fragments: frames}, len(frames))
	if err != nil || len(got) != len(frames) {
		t.Fatalf("one Item per frame: %d frames, %v", len(got), err)
	}
}

func TestJPEG2000FramingRejectsAmbiguousOrDamagedStreams(t *testing.T) {
	frame := j2kFixtureFrames(t)[0]
	for name, data := range map[string][]byte{
		"missing EOC":      frame[:len(frame)-2],
		"trailing bytes":   append(append([]byte(nil), frame...), 0x01, 0x02),
		"not a codestream": append([]byte{0xFF, 0xD8}, frame[2:]...),
		"JP2 container":    append([]byte{0, 0, 0, 12, 'j', 'P', ' ', ' ', 0x0D, 0x0A, 0x87, 0x0A}, frame...),
		"marker in bitstream": func() []byte {
			d := lastTilePartToEOC(t, frame)
			sod := bytes.Index(d, []byte{0xFF, 0x93})
			d[sod+4], d[sod+5] = 0xFF, 0xA0
			return d
		}(),
	} {
		if err := encapsulated.ValidateFrame(context.Background(), data, encapsulated.JPEG2000); !errors.Is(err, encapsulated.ErrLayout) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Two frames in one Item cannot be split without an offset table.
	joined := append(append([]byte(nil), frame...), frame...)
	if _, err := plan(t, core.FragmentSequence{Fragments: [][]byte{joined}}, 2); err == nil {
		t.Fatal("two frames in one Item accepted")
	}
	// A single Item for a single frame needs no scan to plan; the frame's own
	// validation then refuses the bytes after EOC.
	got, err := plan(t, core.FragmentSequence{Fragments: [][]byte{append(append([]byte(nil), frame...), 0x00, 0x01)}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := encapsulated.ValidateFrame(context.Background(), got[0], encapsulated.JPEG2000); !errors.Is(err, encapsulated.ErrLayout) {
		t.Fatalf("bytes after EOC accepted: %v", err)
	}
}

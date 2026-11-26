package encapsulated_test

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
)

func TestParseJ2KLayoutLocatesTileParts(t *testing.T) {
	for i, frame := range j2kFixtureFrames(t) {
		for name, data := range map[string][]byte{
			"as stored":          frame,
			"comment with FF D9": withComment(t, frame),
			"Psot zero":          lastTilePartToEOC(t, frame),
			"padded":             append(append([]byte(nil), frame...), 0),
		} {
			if name == "padded" && len(frame)%2 == 0 {
				continue
			}
			layout, err := encapsulated.ParseJ2KLayout(data)
			if err != nil {
				t.Fatalf("frame %d %s: %v", i, name, err)
			}
			if binary.BigEndian.Uint16(data[layout.MainHeaderEnd:]) != 0xff90 || len(layout.TileParts) == 0 {
				t.Fatalf("frame %d %s: main header ends at %d with %d tile-parts", i, name, layout.MainHeaderEnd, len(layout.TileParts))
			}
			at := layout.MainHeaderEnd
			for _, part := range layout.TileParts {
				if part.Start != at || binary.BigEndian.Uint16(data[part.HeaderEnd-2:]) != 0xff93 || part.End < part.HeaderEnd {
					t.Fatalf("frame %d %s: tile-part %+v", i, name, part)
				}
				at = part.End
			}
			if at != layout.EOC || binary.BigEndian.Uint16(data[layout.EOC:]) != 0xffd9 {
				t.Fatalf("frame %d %s: EOC at %d, tile-parts end at %d", i, name, layout.EOC, at)
			}
		}
	}
}

func TestParseJ2KLayoutRejectsBrokenFraming(t *testing.T) {
	frame := j2kFixtureFrames(t)[0]
	for _, data := range [][]byte{nil, frame[:2], frame[:len(frame)/2], append(append([]byte(nil), frame...), 1, 2)} {
		if _, err := encapsulated.ParseJ2KLayout(data); !errors.Is(err, encapsulated.ErrLayout) {
			t.Errorf("%d bytes: error = %v, want ErrLayout", len(data), err)
		}
	}
}

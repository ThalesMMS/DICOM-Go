package jpeg2000

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/examples/codec-adapters/jpeg2000/internal/j2kcore"
)

// OpenJPEG's encoder writes no packed packet headers and mishandles POC in
// tile-part headers, so those codestreams are made here from lossless
// OpenJPEG ones: the core's tier-2 locates every packet, and the packets are
// reordered (POC) or their headers moved to PPM or PPT segments. The result is
// then checked against OpenJPEG's decoder, which must return the source
// samples: a packet order or packing the standard does not describe would not
// decode.

// rawTilePart is one tile-part of a codestream split at its markers.
type rawTilePart struct {
	tile          int
	index, parts  byte
	header        []byte // tile-part header segments between SOT and SOD
	data          []byte
	mainHeaderEnd int
}

// splitCodestream returns the main header (SOC up to the first SOT) and the
// tile-parts.
func splitCodestream(t *testing.T, data []byte) ([]byte, []rawTilePart) {
	t.Helper()
	pos := 2
	for be16(data[pos:]) != 0xFF90 {
		pos += 2 + be16(data[pos+2:])
	}
	main := data[:pos]
	var parts []rawTilePart
	for pos+2 <= len(data) && be16(data[pos:]) == 0xFF90 {
		psot := int(binary.BigEndian.Uint32(data[pos+6:]))
		end := pos + psot
		if psot == 0 {
			end = len(data) - 2
		}
		part := rawTilePart{tile: be16(data[pos+4:]), index: data[pos+10], parts: data[pos+11]}
		cursor := pos + 12
		for be16(data[cursor:]) != 0xFF93 {
			cursor += 2 + be16(data[cursor+2:])
		}
		part.header = data[pos+12 : cursor]
		part.data = data[cursor+2 : end]
		parts = append(parts, part)
		pos = end
	}
	if be16(data[pos:]) != 0xFFD9 {
		t.Fatalf("codestream does not end with EOC after its tile-parts")
	}
	return main, parts
}

func be16(b []byte) int { return int(binary.BigEndian.Uint16(b)) }

func marker(code int, payload []byte) []byte {
	out := binary.BigEndian.AppendUint16(nil, uint16(code))
	out = binary.BigEndian.AppendUint16(out, uint16(2+len(payload)))
	return append(out, payload...)
}

// packedSegments splits packed header data into PPM or PPT segments with
// increasing indexes, each within the 65535-byte segment length.
func packedSegments(code int, data []byte) []byte {
	var out []byte
	for index := 0; len(data) > 0 || index == 0; index++ {
		n := min(len(data), 65535-3)
		out = append(out, marker(code, append([]byte{byte(index)}, data[:n]...))...)
		data = data[n:]
	}
	return out
}

func pocSegment(changes []j2kcore.ProgressionChange) []byte {
	var payload []byte
	for _, c := range changes {
		payload = append(payload, byte(c.ResolutionStart), byte(c.ComponentStart))
		payload = binary.BigEndian.AppendUint16(payload, uint16(c.LayerEnd))
		payload = append(payload, byte(c.ResolutionEnd), byte(c.ComponentEnd), byte(c.Order))
	}
	return marker(0xFF5F, payload)
}

// rewriteCodestream rebuilds a codestream with one tile-part per tile: "poc"
// reorders every tile's packets as changes say and puts the POC in the main
// header, "tile-poc" puts it in the first tile's header only, "ppm" and "ppt"
// move the packet headers to PPM or PPT segments.
func rewriteCodestream(t *testing.T, data []byte, mode string, changes []j2kcore.ProgressionChange) []byte {
	t.Helper()
	stream, err := j2kcore.ParseCodestream(data)
	if err != nil {
		t.Fatal(err)
	}
	main, parts := splitCodestream(t, data)
	var ppm []byte
	out := append([]byte(nil), main...)
	var tileParts []byte
	for _, part := range parts {
		if part.parts != 1 {
			t.Fatalf("tile %d has %d tile-parts", part.tile, part.parts)
		}
		spans, err := stream.PacketSpans(part.tile)
		if err != nil {
			t.Fatal(err)
		}
		header := append([]byte(nil), part.header...)
		var body, headers []byte
		switch mode {
		case "poc", "tile-poc":
			if mode == "poc" || part.tile == 0 {
				reordered := *stream
				reordered.POC = changes
				reordered.TilePOC = nil
				order, err := reordered.PacketOrder(part.tile)
				if err != nil {
					t.Fatal(err)
				}
				byKey := map[[4]int]j2kcore.PacketSpan{}
				for _, span := range spans {
					byKey[[4]int{span.Layer, span.Resolution, span.Component, span.Precinct}] = span
				}
				if len(order) != len(spans) {
					t.Fatalf("tile %d: %d packets reordered, %d read", part.tile, len(order), len(spans))
				}
				spans = spans[:0]
				for _, key := range order {
					spans = append(spans, byKey[key])
				}
				if mode == "tile-poc" {
					header = append(header, pocSegment(changes)...)
				}
			}
			for _, span := range spans {
				body = append(append(append(body, span.SOP...), span.Header...), span.Body...)
			}
		case "ppm", "ppt":
			for _, span := range spans {
				headers = append(headers, span.Header...)
				body = append(append(body, span.SOP...), span.Body...)
			}
			if mode == "ppm" {
				ppm = binary.BigEndian.AppendUint32(ppm, uint32(len(headers)))
				ppm = append(ppm, headers...)
			} else {
				header = append(header, packedSegments(0xFF61, headers)...)
			}
		}
		sot := []byte{0xFF, 0x90, 0, 10}
		sot = binary.BigEndian.AppendUint16(sot, uint16(part.tile))
		sot = binary.BigEndian.AppendUint32(sot, uint32(12+len(header)+2+len(body)))
		sot = append(sot, part.index, part.parts)
		tileParts = append(append(append(append(tileParts, sot...), header...), 0xFF, 0x93), body...)
	}
	switch mode {
	case "poc":
		out = append(out, pocSegment(changes)...)
	case "ppm":
		out = append(out, packedSegments(0xFF60, ppm)...)
	}
	out = append(out, tileParts...)
	return append(out, 0xFF, 0xD9)
}

// TestCorePackedHeadersAndPOCAgainstOpenJPEG builds POC, tile POC, PPM and
// PPT codestreams from lossless OpenJPEG ones and requires both OpenJPEG and
// the core to decode them to the source samples. Enable as
// TestCoreFeaturesAgainstOpenJPEG.
func TestCorePackedHeadersAndPOCAgainstOpenJPEG(t *testing.T) {
	compress, decompress := os.Getenv("DICOM_GO_OPENJPEG_COMPRESS"), os.Getenv("DICOM_GO_OPENJPEG_DECOMPRESS")
	if compress == "" || decompress == "" {
		t.Skip("opt-in: set DICOM_GO_OPENJPEG_COMPRESS and DICOM_GO_OPENJPEG_DECOMPRESS")
	}
	pocChanges := []j2kcore.ProgressionChange{
		{ResolutionEnd: 3, ComponentEnd: 2, LayerEnd: 1, Order: j2kcore.ProgressionCPRL},
		{ResolutionEnd: 33, ComponentEnd: 3, LayerEnd: 2, Order: j2kcore.ProgressionRPCL},
		{ResolutionStart: 1, ResolutionEnd: 33, ComponentStart: 1, ComponentEnd: 3, LayerEnd: 3, Order: j2kcore.ProgressionPCRL},
		{ResolutionEnd: 33, ComponentEnd: 3, LayerEnd: 3, Order: j2kcore.ProgressionLRCP},
	}
	base := openJPEGCase{columns: 61, rows: 47, components: 3, bits: 8, args: []string{"-n", "4", "-r", "30,10,1", "-t", "32,24", "-c", "[32,32],[16,16]"}}
	dir := t.TempDir()
	for _, tc := range []struct {
		mode    string
		extra   []string
		changes []j2kcore.ProgressionChange
		// corpusID names the qualification corpus fixture the codestream
		// becomes (written with DICOM_GO_J2K_REWRITE_OUT).
		corpusID string
	}{
		{mode: "poc", changes: pocChanges, corpusID: "jpeg2000-core-rgb8-poc"},
		{mode: "tile-poc", changes: pocChanges, corpusID: "jpeg2000-core-rgb8-tile-poc"},
		{mode: "ppm"},
		{mode: "ppm", extra: []string{"-EPH", "-SOP"}, corpusID: "jpeg2000-core-rgb8-ppm-sop-eph"},
		{mode: "ppt"},
		{mode: "ppt", extra: []string{"-EPH", "-p", "RPCL", "-M", "5"}, corpusID: "jpeg2000-core-rgb8-ppt-rpcl-styles"},
	} {
		name := fmt.Sprintf("%s-%d", tc.mode, len(tc.extra))
		t.Run(name, func(t *testing.T) {
			source := openJPEGSource(base)
			input := filepath.Join(dir, name+".rawl")
			if err := os.WriteFile(input, planarBytes(source, base.bits), 0o600); err != nil {
				t.Fatal(err)
			}
			original := filepath.Join(dir, name+".j2k")
			runOpenJPEG(t, compress, append(append([]string{"-i", input, "-o", original, "-F", fmt.Sprintf("%d,%d,%d,%d,u", base.columns, base.rows, base.components, base.bits)}, base.args...), tc.extra...)...)
			data, err := os.ReadFile(original)
			if err != nil {
				t.Fatal(err)
			}
			rewritten := rewriteCodestream(t, data, tc.mode, tc.changes)
			if bytes.Equal(rewritten, data) {
				t.Fatal("the rewrite changed nothing")
			}
			path := filepath.Join(dir, name+".rewritten.j2k")
			if err := os.WriteFile(path, rewritten, 0o600); err != nil {
				t.Fatal(err)
			}
			decoded := filepath.Join(dir, name+".rawl")
			runOpenJPEG(t, decompress, "-i", path, "-o", decoded)
			raw, err := os.ReadFile(decoded)
			if err != nil {
				t.Fatal(err)
			}
			if want := planarBytes(source, base.bits); !bytes.Equal(raw, want) {
				t.Fatalf("OpenJPEG does not decode the rewritten codestream to the source")
			}
			stream, err := j2kcore.ParseCodestream(rewritten)
			if err != nil {
				t.Fatal(err)
			}
			image, err := j2kcore.DecodeContext(context.Background(), stream, j2kcore.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			for c := range source {
				for i, v := range source[c] {
					if image.Components[c][i] != v {
						t.Fatalf("component %d sample %d is %d, source %d", c, i, image.Components[c][i], v)
					}
				}
			}
			if dirOut := os.Getenv("DICOM_GO_J2K_REWRITE_OUT"); dirOut != "" && tc.corpusID != "" {
				// The corpus layout: interleaved samples. The codestream is
				// lossless and OpenJPEG returned the source, so the source
				// is also OpenJPEG's reconstruction.
				interleaved := make([]byte, 0, len(raw))
				for i := range source[0] {
					for c := range source {
						interleaved = append(interleaved, byte(source[c][i]))
					}
				}
				for suffix, content := range map[string][]byte{".j2k": rewritten, ".source.raw": interleaved, ".openjpeg.raw": interleaved} {
					if err := os.WriteFile(filepath.Join(dirOut, tc.corpusID+suffix), content, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

package jpip

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const coreFixtureDir = "../pixeldata/codecfixture/testdata/codecfull/jpeg2000/core"

// codestreamFixtures returns the bare JPEG 2000 codestreams of the codec
// corpus: OpenJPEG output covering every progression order, POC, tiles,
// precincts, layers, sub-sampling, SOP/EPH, PPM/PPT and the bypass and
// termination modes, plus the HTJ2K and Part 2 samples.
func codestreamFixtures(t testing.TB) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, pattern := range []string{
		filepath.Join(coreFixtureDir, "*.j2k"),
		filepath.Join(coreFixtureDir, "..", "*.j2c"),
		filepath.Join(coreFixtureDir, "..", "*.j2k"),
	} {
		paths, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			out[strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))] = data
		}
	}
	if len(out) < 20 {
		t.Fatalf("found %d codestream fixtures", len(out))
	}
	return out
}

func sortedNames(m map[string][]byte) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// withoutPrecincts lists fixtures served only as tiles, and why.
var withoutPrecincts = map[string]string{
	"htj2k-rpcl-lossless":                "HTJ2K without PLT",
	"jpeg2000-core-rgb8-ppm-sop-eph":     "PPM",
	"jpeg2000-core-rgb8-ppt-rpcl-styles": "PPT",
}

func TestIndexCodestreamLocatesEveryPacket(t *testing.T) {
	fixtures := codestreamFixtures(t)
	for _, name := range sortedNames(fixtures) {
		t.Run(name, func(t *testing.T) {
			data := fixtures[name]
			index, err := IndexCodestream(data, IndexLimits{})
			if err != nil {
				t.Fatal(err)
			}
			if _, skip := withoutPrecincts[name]; skip {
				if index.Precincts() {
					t.Fatal("precinct data-bins offered for a codestream that cannot provide them")
				}
				return
			}
			if !index.Precincts() {
				t.Fatalf("no precinct data-bins: %v", index.precinctErr)
			}
			ids := map[uint64]bool{}
			for ti, tile := range index.tiles {
				var spans []byteSpan
				for _, p := range tile.precincts {
					if ids[p.id] {
						t.Fatalf("duplicate precinct data-bin ID %d", p.id)
					}
					ids[p.id] = true
					if len(p.packets) != tile.geometry.layers {
						t.Fatalf("tile %d precinct %d has %d packets, want %d layers", ti, p.id, len(p.packets), tile.geometry.layers)
					}
					spans = append(spans, p.packets...)
				}
				// The packets tile the packet data of every tile-part exactly.
				sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
				at := 0
				for _, part := range tile.parts {
					pos := part.HeaderEnd
					for at < len(spans) && spans[at].start < part.End {
						if spans[at].start != pos || spans[at].end > part.End {
							t.Fatalf("tile %d: packet [%d,%d) does not continue at %d", ti, spans[at].start, spans[at].end, pos)
						}
						pos = spans[at].end
						at++
					}
					if pos != part.End {
						t.Fatalf("tile %d: tile-part data ends at %d, packets at %d", ti, part.End, pos)
					}
				}
			}
		})
	}
}

// packetBytes maps each precinct data-bin ID to its packets, without SOP
// marker segments.
func packetBytes(index *CodestreamIndex) map[uint64][][]byte {
	out := map[uint64][][]byte{}
	for _, tile := range index.tiles {
		for _, p := range tile.precincts {
			for _, span := range p.packets {
				packet := index.data[span.start:span.end]
				if len(packet) >= 6 && packet[0] == 0xff && packet[1] == 0x91 {
					packet = packet[6:]
				}
				out[p.id] = append(out[p.id], packet)
			}
		}
	}
	return out
}

func equalPackets(t *testing.T, got, want map[uint64][][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d precinct data-bins, want %d", len(got), len(want))
	}
	for id, packets := range want {
		if len(got[id]) != len(packets) {
			t.Fatalf("precinct %d: %d packets, want %d", id, len(got[id]), len(packets))
		}
		for l := range packets {
			if !bytes.Equal(got[id][l], packets[l]) {
				t.Fatalf("precinct %d layer %d differs", id, l)
			}
		}
	}
}

// withPLT rewrites a codestream so every tile-part header carries PLT
// segments listing its packet lengths (and drops TLM, whose tile-part
// lengths change).
func withPLT(t *testing.T, index *CodestreamIndex) []byte {
	t.Helper()
	data := index.data
	var out []byte
	err := walkSegments(data[:index.layout.MainHeaderEnd], func(s segment) error {
		if s.marker != markerTLM {
			out = append(out, data[s.start:s.end]...)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var spans []byteSpan
	for _, tile := range index.tiles {
		for _, p := range tile.precincts {
			spans = append(spans, p.packets...)
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	for _, part := range index.layout.TileParts {
		var lengths []byte
		for _, span := range spans {
			if span.start >= part.HeaderEnd && span.start < part.End {
				lengths = appendVBAS(lengths, uint64(span.len()))
			}
		}
		var plt []byte
		for z := 0; len(lengths) > 0; z++ {
			n := min(len(lengths), 60000)
			// Do not split a VBAS value between segments.
			for n < len(lengths) && lengths[n-1]&0x80 != 0 {
				n--
			}
			plt = binary.BigEndian.AppendUint16(plt, markerPLT)
			plt = binary.BigEndian.AppendUint16(plt, uint16(3+n))
			plt = append(plt, byte(z))
			plt = append(plt, lengths[:n]...)
			lengths = lengths[n:]
		}
		header := append([]byte(nil), data[part.Start:part.Start+12]...)
		err := walkSegments(data[part.Start+12:part.HeaderEnd-2], func(s segment) error {
			if s.marker != markerPLT {
				header = append(header, data[part.Start+12+s.start:part.Start+12+s.end]...)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		header = append(header, plt...)
		header = append(header, 0xff, 0x93)
		if binary.BigEndian.Uint32(data[part.Start+6:]) != 0 {
			binary.BigEndian.PutUint32(header[6:], uint32(len(header)+part.End-part.HeaderEnd))
		}
		out = append(out, header...)
		out = append(out, data[part.HeaderEnd:part.End]...)
	}
	return append(out, 0xff, 0xd9)
}

func TestIndexCodestreamPLTMatchesPacketHeaders(t *testing.T) {
	fixtures := codestreamFixtures(t)
	for _, name := range sortedNames(fixtures) {
		if _, skip := withoutPrecincts[name]; skip {
			continue
		}
		t.Run(name, func(t *testing.T) {
			original, err := IndexCodestream(fixtures[name], IndexLimits{})
			if err != nil {
				t.Fatal(err)
			}
			if original.usedPLT {
				t.Fatal("fixture unexpectedly carries PLT")
			}
			tagged := withPLT(t, original)
			index, err := IndexCodestream(tagged, IndexLimits{})
			if err != nil {
				t.Fatal(err)
			}
			if !index.usedPLT || !index.Precincts() {
				t.Fatalf("PLT not used: %v", index.precinctErr)
			}
			equalPackets(t, packetBytes(index), packetBytes(original))
			headers, err := indexCodestream(tagged, IndexLimits{}, false)
			if err != nil || headers.usedPLT {
				t.Fatalf("packet-header index: %v (PLT %v)", err, headers.usedPLT)
			}
			equalPackets(t, packetBytes(headers), packetBytes(original))
		})
	}
}

func TestIndexCodestreamRejectsInconsistentPLT(t *testing.T) {
	fixtures := codestreamFixtures(t)
	original, err := IndexCodestream(fixtures["jpeg2000-core-u8-odd-layers"], IndexLimits{})
	if err != nil {
		t.Fatal(err)
	}
	tagged := withPLT(t, original)
	at := bytes.Index(tagged, []byte{0xff, 0x58})
	tagged[at+5]++ // first packet length
	index, err := IndexCodestream(tagged, IndexLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if index.usedPLT || !index.Precincts() {
		t.Fatalf("inconsistent PLT: used=%v precincts=%v", index.usedPLT, index.Precincts())
	}
	equalPackets(t, packetBytes(index), packetBytes(original))
}

func TestIndexCodestreamFailsClosed(t *testing.T) {
	data := codestreamFixtures(t)["jpeg2000-core-rgb8-rpcl-precincts"]
	for _, n := range []int{0, 1, 2, 20, len(data) / 2, len(data) - 1} {
		if _, err := IndexCodestream(data[:n], IndexLimits{}); !errors.Is(err, ErrMalformedCodestream) {
			t.Errorf("prefix %d: error = %v, want ErrMalformedCodestream", n, err)
		}
	}
	if _, err := IndexCodestream(data, IndexLimits{MaxCodestreamBytes: len(data) - 1}); !errors.Is(err, ErrIndexLimit) {
		t.Errorf("size limit: %v", err)
	}
	index, err := IndexCodestream(data, IndexLimits{MaxPackets: 10})
	if err != nil {
		t.Fatal(err)
	}
	if index.Precincts() || !errors.Is(index.precinctErr, ErrIndexLimit) {
		t.Fatalf("packet limit: precincts=%v err=%v", index.Precincts(), index.precinctErr)
	}
}

// fullStream serializes every data-bin of a codestream as one response.
func fullStream(t *testing.T, index *CodestreamIndex, kind string) []byte {
	t.Helper()
	plan := newPlan(index, 1, kind, viewWindow{layers: -1, length: -1})
	body := plan.write(map[dataBinKey]int64{}, -1, 1<<30)
	if body.eor != EORImageDone {
		t.Fatalf("EOR = %d, want image done", body.eor)
	}
	return body.data
}

func rebuild(t *testing.T, body []byte) Reconstruction {
	t.Helper()
	stream, err := ParseStream(body)
	if err != nil {
		t.Fatal(err)
	}
	set := NewDataBinSet(0)
	if err := set.AddStream(stream); err != nil {
		t.Fatal(err)
	}
	rec, err := set.Codestream(1)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestPrecinctStreamRebuildsEveryPacket(t *testing.T) {
	fixtures := codestreamFixtures(t)
	for _, name := range sortedNames(fixtures) {
		if _, skip := withoutPrecincts[name]; skip {
			continue
		}
		t.Run(name, func(t *testing.T) {
			original, err := IndexCodestream(fixtures[name], IndexLimits{})
			if err != nil {
				t.Fatal(err)
			}
			rec := rebuild(t, fullStream(t, original, "jpp-stream"))
			if !rec.Complete {
				t.Fatal("complete stream rebuilt as partial")
			}
			rebuilt, err := indexCodestream(rec.Codestream, IndexLimits{}, false)
			if err != nil {
				t.Fatal(err)
			}
			if !rebuilt.Precincts() {
				t.Fatalf("rebuilt codestream: %v", rebuilt.precinctErr)
			}
			for _, tile := range rebuilt.tiles {
				if tile.geometry.progression != progressionRPCL || len(tile.geometry.poc) != 0 {
					t.Fatalf("rebuilt progression %d with %d POC entries", tile.geometry.progression, len(tile.geometry.poc))
				}
			}
			equalPackets(t, packetBytes(rebuilt), packetBytes(original))
		})
	}
}

func TestTileStreamRebuildsTheCodestream(t *testing.T) {
	fixtures := codestreamFixtures(t)
	for _, name := range sortedNames(fixtures) {
		t.Run(name, func(t *testing.T) {
			data := fixtures[name]
			index, err := IndexCodestream(data, IndexLimits{})
			if err != nil {
				t.Fatal(err)
			}
			rec := rebuild(t, fullStream(t, index, "jpt-stream"))
			if !rec.Complete {
				t.Fatal("complete stream rebuilt as partial")
			}
			// Tiles come back in tile order with TLM and PLM dropped.
			want, err := filteredHeader(data[:index.layout.MainHeaderEnd], false)
			if err != nil {
				t.Fatal(err)
			}
			for _, tile := range index.tiles {
				for _, part := range tile.parts {
					want = append(want, data[part.Start:part.End]...)
					if binary.BigEndian.Uint32(data[part.Start+6:]) == 0 {
						binary.BigEndian.PutUint32(want[len(want)-(part.End-part.Start)+6:], uint32(part.End-part.Start))
					}
				}
			}
			want = append(want, 0xff, 0xd9)
			if !bytes.Equal(rec.Codestream, want) {
				t.Fatal("rebuilt codestream differs from the tile-parts")
			}
		})
	}
}

func TestPrecinctStreamFillsMissingLayersWithEmptyPackets(t *testing.T) {
	data := codestreamFixtures(t)["jpeg2000-core-rgb8-rpcl-precincts"]
	index, err := IndexCodestream(data, IndexLimits{})
	if err != nil {
		t.Fatal(err)
	}
	plan := newPlan(index, 1, "jpp-stream", viewWindow{layers: 1, length: -1})
	body := plan.write(map[dataBinKey]int64{}, -1, 1<<30)
	if body.eor != EORWindowDone {
		t.Fatalf("EOR = %d, want window done", body.eor)
	}
	rec := rebuild(t, body.data)
	if rec.Complete {
		t.Fatal("one-layer stream rebuilt as complete")
	}
	rebuilt, err := indexCodestream(rec.Codestream, IndexLimits{}, false)
	if err != nil {
		t.Fatal(err)
	}
	got, want := packetBytes(rebuilt), packetBytes(index)
	for id, packets := range want {
		if !bytes.Equal(got[id][0], packets[0]) {
			t.Fatalf("precinct %d first layer differs", id)
		}
		for l := 1; l < len(packets); l++ {
			if !bytes.Equal(got[id][l], []byte{0}) {
				t.Fatalf("precinct %d layer %d = % x, want an empty packet", id, l, got[id][l])
			}
		}
	}
}

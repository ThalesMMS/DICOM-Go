package jpip

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// ErrDataBinLimit reports data-bins that would exceed a DataBinSet's budget.
var ErrDataBinLimit = errors.New("dicom: JPIP data-bin cache limit exceeded")

// ErrIncompleteDataBins reports data-bins that cannot form a codestream: the
// main header, or the tile headers of a precinct stream, are incomplete.
var ErrIncompleteDataBins = errors.New("dicom: JPIP data-bins are insufficient for a codestream")

type dataBinKey struct {
	stream uint64
	class  DataBinClass
	id     uint64
}

type dataBin struct {
	data     []byte
	total    int64 // -1 until a message ends the bin
	segments []pendingSegment
}

type pendingSegment struct {
	offset uint64
	data   []byte
}

func (b *dataBin) complete() bool { return b.total >= 0 && int64(len(b.data)) == b.total }

// DataBinSet is a client-side JPIP cache: it merges the messages of one or
// more JPP/JPT responses into data-bins and rebuilds a JPEG 2000 codestream
// from them. It is not safe for concurrent use.
type DataBinSet struct {
	maxBytes int64
	bytes    int64
	bins     map[dataBinKey]*dataBin
}

// NewDataBinSet returns an empty set holding at most maxBytes of data-bin
// content (default 256 MiB).
func NewDataBinSet(maxBytes int64) *DataBinSet {
	if maxBytes <= 0 {
		maxBytes = defaultMaxResponseBytes
	}
	return &DataBinSet{maxBytes: maxBytes, bins: map[dataBinKey]*dataBin{}}
}

// AddStream merges every message of a parsed response.
func (s *DataBinSet) AddStream(stream Stream) error {
	for _, m := range stream.Messages {
		if err := s.Add(m); err != nil {
			return err
		}
	}
	return nil
}

// Add merges one message. Repeated ranges are ignored; ranges that start
// past the received prefix are held until the gap is filled. Extended
// classes are stored under their base class.
func (s *DataBinSet) Add(m Message) error {
	class := m.Class
	switch class {
	case ClassExtendedPrecinct:
		class = ClassPrecinct
	case ClassExtendedTile:
		class = ClassTile
	}
	key := dataBinKey{stream: m.Stream, class: class, id: m.ID}
	bin := s.bins[key]
	if bin == nil {
		bin = &dataBin{total: -1}
		s.bins[key] = bin
	}
	end := m.Offset + uint64(len(m.Data))
	if end < m.Offset || end > uint64(s.maxBytes) {
		return ErrDataBinLimit
	}
	if m.Complete {
		if bin.total >= 0 && uint64(bin.total) != end {
			return fmt.Errorf("%w: conflicting data-bin lengths", ErrMalformedStream)
		}
		bin.total = int64(end)
	}
	if bin.total >= 0 && end > uint64(bin.total) {
		return fmt.Errorf("%w: data past the end of a data-bin", ErrMalformedStream)
	}
	if len(m.Data) == 0 {
		return nil
	}
	if m.Offset > uint64(len(bin.data)) {
		if s.bytes+int64(len(m.Data)) > s.maxBytes {
			return ErrDataBinLimit
		}
		s.bytes += int64(len(m.Data))
		bin.segments = append(bin.segments, pendingSegment{offset: m.Offset, data: append([]byte(nil), m.Data...)})
		return nil
	}
	if err := s.extend(bin, m.Offset, m.Data); err != nil {
		return err
	}
	// Absorb held segments that now touch the prefix.
	sort.Slice(bin.segments, func(i, j int) bool { return bin.segments[i].offset < bin.segments[j].offset })
	kept := bin.segments[:0]
	for _, seg := range bin.segments {
		if seg.offset <= uint64(len(bin.data)) {
			s.bytes -= int64(len(seg.data))
			if err := s.extend(bin, seg.offset, seg.data); err != nil {
				return err
			}
			continue
		}
		kept = append(kept, seg)
	}
	bin.segments = kept
	return nil
}

func (s *DataBinSet) extend(bin *dataBin, offset uint64, data []byte) error {
	skip := uint64(len(bin.data)) - offset
	if skip >= uint64(len(data)) {
		return nil
	}
	tail := data[skip:]
	if s.bytes+int64(len(tail)) > s.maxBytes {
		return ErrDataBinLimit
	}
	s.bytes += int64(len(tail))
	bin.data = append(bin.data, tail...)
	return nil
}

// Bytes returns the data-bin content held, including out-of-order ranges.
func (s *DataBinSet) Bytes() int64 { return s.bytes }

func (s *DataBinSet) bin(stream uint64, class DataBinClass, id uint64) *dataBin {
	return s.bins[dataBinKey{stream: stream, class: class, id: id}]
}

// Reconstruction is a codestream rebuilt from data-bins.
type Reconstruction struct {
	Codestream []byte
	// Complete reports that every data-bin of the image was present and
	// complete, so the codestream decodes to the full-quality image. When
	// false the codestream is valid but missing precincts or layers are coded
	// as empty packets (or missing tiles are absent).
	Complete bool
}

// Codestream rebuilds a JPEG 2000 codestream for one codestream identifier.
// Tile data-bins (JPT-stream) are concatenated; precinct data-bins
// (JPP-stream) are written as one tile-part per tile in RPCL order, with the
// main and tile headers' COD set to RPCL, POC removed and absent packets
// coded as empty packets, so a Part 1 decoder reads the precincts received.
func (s *DataBinSet) Codestream(stream uint64) (Reconstruction, error) {
	main := s.bin(stream, ClassMainHeader, 0)
	if main == nil || !main.complete() {
		return Reconstruction{}, fmt.Errorf("%w: main header", ErrIncompleteDataBins)
	}
	params, size, err := parseHeader(main.data, nil, true)
	if err != nil {
		return Reconstruction{}, err
	}
	if params.cod == nil {
		return Reconstruction{}, malformed("missing COD")
	}
	for key := range s.bins {
		if key.stream == stream && key.class == ClassTile {
			return s.tileCodestream(stream, main.data, size)
		}
	}
	return s.precinctCodestream(stream, main.data, params, size)
}

// filteredHeader copies marker segments, dropping those that describe the
// original packet or tile-part layout and setting COD progression to RPCL.
func filteredHeader(data []byte, rpcl bool) ([]byte, error) {
	out := make([]byte, 0, len(data))
	err := walkSegments(data, func(seg segment) error {
		switch seg.marker {
		case markerTLM, markerPLM, markerPLT:
			return nil
		case markerPOC:
			if rpcl {
				return nil
			}
		case markerCOD:
			if rpcl {
				start := len(out)
				out = append(out, data[seg.start:seg.end]...)
				out[start+5] = progressionRPCL
				return nil
			}
		}
		out = append(out, data[seg.start:seg.end]...)
		return nil
	})
	return out, err
}

func (s *DataBinSet) tileCodestream(stream uint64, main []byte, size imageSize) (Reconstruction, error) {
	header, err := filteredHeader(main, false)
	if err != nil {
		return Reconstruction{}, err
	}
	out := header
	complete := true
	tiles := 0
	for t := 0; t < size.numTiles(); t++ {
		bin := s.bin(stream, ClassTile, uint64(t))
		if bin == nil || !bin.complete() {
			complete = false
		}
		if bin == nil {
			continue
		}
		// Keep whole tile-parts only.
		data := bin.data
		for at := 0; at+12 <= len(data); {
			if binary.BigEndian.Uint16(data[at:]) != markerSOT {
				return Reconstruction{}, malformed("tile data-bin does not hold tile-parts")
			}
			psot := int(binary.BigEndian.Uint32(data[at+6:]))
			if psot == 0 && bin.complete() {
				psot = len(data) - at
			}
			if psot < 14 || at+psot > len(data) {
				break
			}
			part := data[at : at+psot]
			if psot == len(data)-at && binary.BigEndian.Uint32(part[6:]) == 0 {
				// A Psot of zero ran to EOC in the source; write the length.
				part = append([]byte(nil), part...)
				binary.BigEndian.PutUint32(part[6:], uint32(psot))
			}
			out = append(out, part...)
			at += psot
			tiles++
		}
	}
	if tiles == 0 {
		return Reconstruction{}, fmt.Errorf("%w: no tile-part", ErrIncompleteDataBins)
	}
	out = append(out, 0xff, 0xd9)
	return Reconstruction{Codestream: out, Complete: complete}, nil
}

func (s *DataBinSet) precinctCodestream(stream uint64, main []byte, mainParams headerParameters, size imageSize) (Reconstruction, error) {
	if mainParams.packed || mainParams.extended {
		return Reconstruction{}, malformed("precinct data-bins with relocated packet headers or Part 2 geometry")
	}
	out, err := filteredHeader(main, true)
	if err != nil {
		return Reconstruction{}, err
	}
	complete := true
	tiles := 0
	numTiles := size.numTiles()
	for t := 0; t < numTiles; t++ {
		headerBin := s.bin(stream, ClassTileHeader, uint64(t))
		if headerBin == nil || !headerBin.complete() {
			complete = false
			continue
		}
		params, _, err := parseHeader(headerBin.data, &size, false)
		if err != nil {
			return Reconstruction{}, err
		}
		if params.packed || params.extended {
			return Reconstruction{}, malformed("precinct data-bins with relocated packet headers or Part 2 geometry")
		}
		style, comps, _, err := codingParameters(size, mainParams, params)
		if err != nil {
			return Reconstruction{}, err
		}
		g, err := newTileGeometry(size, t, style, comps, nil)
		if err != nil {
			return Reconstruction{}, err
		}
		tileHeader, err := filteredHeader(headerBin.data, true)
		if err != nil {
			return Reconstruction{}, err
		}
		emptyPacket := []byte{0}
		if g.scod&scodEPH != 0 {
			emptyPacket = append(emptyPacket, 0xff, 0x92)
		}
		// Split each precinct data-bin into its packets.
		packets := make([][][]byte, g.precinctCount)
		for c := range g.components {
			comp := &g.components[c]
			for r := range comp.resolutions {
				res := &comp.resolutions[r]
				for k := 0; k < res.precincts(); k++ {
					id := precinctBinID(t, c, res.precinctBase+k, len(g.components), numTiles)
					bin := s.bin(stream, ClassPrecinct, id)
					i := g.precinctIndex(c, r, k)
					if bin == nil {
						complete = false
						continue
					}
					split := splitPrecinct(&g, c, r, k, bin)
					if len(split) < g.layers {
						complete = false
					}
					packets[i] = split
				}
			}
		}
		body := make([]byte, 0)
		err = g.forEachPacket(progressionRPCL, nil, func(l, r, c, k int) error {
			split := packets[g.precinctIndex(c, r, k)]
			if l < len(split) {
				body = append(body, split[l]...)
			} else {
				body = append(body, emptyPacket...)
			}
			return nil
		})
		if err != nil {
			return Reconstruction{}, err
		}
		psot := 12 + len(tileHeader) + 2 + len(body)
		if uint64(psot) > 1<<32-1 {
			return Reconstruction{}, fmt.Errorf("%w: tile-part length", ErrIndexLimit)
		}
		sot := make([]byte, 12)
		binary.BigEndian.PutUint16(sot, markerSOT)
		binary.BigEndian.PutUint16(sot[2:], 10)
		binary.BigEndian.PutUint16(sot[4:], uint16(t))
		binary.BigEndian.PutUint32(sot[6:], uint32(psot))
		sot[10], sot[11] = 0, 1
		out = append(out, sot...)
		out = append(out, tileHeader...)
		out = append(out, 0xff, 0x93)
		out = append(out, body...)
		tiles++
	}
	if tiles == 0 {
		return Reconstruction{}, fmt.Errorf("%w: no tile header", ErrIncompleteDataBins)
	}
	out = append(out, 0xff, 0xd9)
	return Reconstruction{Codestream: out, Complete: complete}, nil
}

// splitPrecinct returns the complete packets a precinct data-bin holds, in
// layer order. Part 1 packets are measured from their headers, and their SOP
// marker segments, optional and now out of sequence, are dropped. A complete
// HTJ2K data-bin cannot be split without decoding, so it is returned whole as
// the first layer with empty slots for the others: in RPCL order a
// precinct's layers are contiguous. An incomplete HTJ2K data-bin contributes
// nothing.
func splitPrecinct(g *tileGeometry, c, r, k int, bin *dataBin) [][]byte {
	if g.ht {
		if !bin.complete() {
			return nil
		}
		out := make([][]byte, g.layers)
		out[0] = bin.data
		for l := 1; l < g.layers; l++ {
			out[l] = []byte{}
		}
		return out
	}
	state := newPrecinctState(g.codeBlockGrids(c, r, k))
	var out [][]byte
	pos := 0
	for l := 0; l < g.layers && pos < len(bin.data); l++ {
		end, err := measurePacket(bin.data, pos, state, l, g.scod, g.components[c].coding.cmodes)
		if err != nil {
			break
		}
		packet := bin.data[pos:end]
		if g.scod&scodSOP != 0 && len(packet) >= 6 && packet[0] == 0xff && packet[1] == 0x91 {
			packet = packet[6:]
		}
		out = append(out, packet)
		pos = end
	}
	return out
}

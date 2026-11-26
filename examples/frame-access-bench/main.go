package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const (
	rows, columns = 256, 256
	frameCount    = 64
)

var fixtureMetadata = pixeldata.Metadata{
	Rows: rows, Columns: columns, SamplesPerPixel: 1,
	BitsAllocated: 16, BitsStored: 16, HighBit: 15, PixelRepresentation: 1,
	NumberOfFrames: frameCount, PhotometricInterpretation: "MONOCHROME2",
}

func fixtureFrame(frame int) []byte {
	data := make([]byte, 0, rows*columns*2)
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			value := ((x*7 + y*13 + frame*131) % 4096) - 2048
			data = binary.LittleEndian.AppendUint16(data, uint16(int16(value)))
		}
	}
	return data
}

func prepare(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for _, c := range []struct {
		name   string
		syntax transfer.Syntax
		codec  pixeldata.FrameEncoder
		split  bool
	}{
		{"rle-bot", transfer.RLELossless, rle.NewEncoder(), false},
		{"rle-empty", transfer.RLELossless, rle.NewEncoder(), false},
		{"jls-bot", transfer.JPEGLSLossless, jpegls.NewEncoder(), false},
		{"jls-split-bot", transfer.JPEGLSLossless, jpegls.NewEncoder(), true},
		{"jls-split-empty", transfer.JPEGLSLossless, jpegls.NewEncoder(), true},
	} {
		seq := core.FragmentSequence{}
		var offset uint32
		for i := 0; i < frameCount; i++ {
			if c.name != "rle-empty" && c.name != "jls-split-empty" {
				seq.OffsetTable = binary.LittleEndian.AppendUint32(seq.OffsetTable, offset)
			}
			encoded, err := c.codec.EncodeFrame(context.Background(), fixtureFrame(i), fixtureMetadata)
			if err != nil {
				return fmt.Errorf("encode %s frame %d: %w", c.name, i, err)
			}
			data := encoded.Data
			if len(data)%2 != 0 {
				data = append(data, 0)
			}
			fragments := [][]byte{data}
			if c.split {
				mid := (len(data) / 4) * 2
				fragments = [][]byte{data[:mid], data[mid:]}
			}
			for _, fragment := range fragments {
				seq.Fragments = append(seq.Fragments, fragment)
				offset += uint32(8 + len(fragment))
			}
		}
		if err := writeFixture(filepath.Join(dir, c.name+".dcm"), c.syntax, seq); err != nil {
			return err
		}
	}
	return nil
}

func writeFixture(path string, syntax transfer.Syntax, seq core.FragmentSequence) error {
	us := func(element, value uint16) core.Element {
		return core.NewRawElement(core.NewTag(0x0028, element), core.VRUS, binary.LittleEndian.AppendUint16(nil, value))
	}
	text := func(group, element uint16, vr core.VR, values ...string) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: core.NewTag(group, element), VR: vr}, Value: core.StringValue(values)}
	}
	sequence := func(group, element uint16, items ...core.DataSet) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: core.NewTag(group, element), VR: core.VRSQ}, Value: core.SequenceValue{Items: items}}
	}
	item := func(elements ...core.Element) core.DataSet { return core.DataSet{Elements: elements} }
	m := fixtureMetadata
	positions := make([]core.DataSet, m.NumberOfFrames)
	for i := range positions {
		positions[i] = item(sequence(0x0020, 0x9113, item(text(0x0020, 0x0032, core.VRDS, "0", "0", strconv.Itoa(i)))))
	}
	shared := item(
		sequence(0x0020, 0x9116, item(text(0x0020, 0x0037, core.VRDS, "1", "0", "0", "0", "1", "0"))),
		sequence(0x0028, 0x9110, item(
			text(0x0018, 0x0050, core.VRDS, "1"),
			text(0x0028, 0x0030, core.VRDS, "1", "1"),
		)),
		sequence(0x0028, 0x9145, item(
			text(0x0028, 0x1052, core.VRDS, "0"),
			text(0x0028, 0x1053, core.VRDS, "1"),
			text(0x0028, 0x1054, core.VRLO, "HU"),
		)),
	)
	ds := object.FromElements([]core.Element{
		text(0x0008, 0x0016, core.VRUI, "1.2.840.10008.5.1.4.1.1.2.1"),
		text(0x0008, 0x0018, core.VRUI, "2.25.1678.1"),
		text(0x0008, 0x0060, core.VRCS, "CT"),
		text(0x0010, 0x0010, core.VRPN, "SYNTHETIC^FRAMEACCESS"),
		text(0x0010, 0x0020, core.VRLO, "SYN1678"),
		text(0x0020, 0x000d, core.VRUI, "2.25.1678.2"),
		text(0x0020, 0x000e, core.VRUI, "2.25.1678.3"),
		text(0x0020, 0x0052, core.VRUI, "2.25.1678.4"),
		text(0x0028, 0x1050, core.VRDS, "0"),
		text(0x0028, 0x1051, core.VRDS, "4096"),
		sequence(0x5200, 0x9229, shared),
		sequence(0x5200, 0x9230, positions...),
		us(0x0002, m.SamplesPerPixel),
		text(0x0028, 0x0004, core.VRCS, m.PhotometricInterpretation),
		text(0x0028, 0x0008, core.VRIS, strconv.Itoa(m.NumberOfFrames)),
		us(0x0010, m.Rows), us(0x0011, m.Columns), us(0x0100, m.BitsAllocated),
		us(0x0101, m.BitsStored), us(0x0102, m.HighBit), us(0x0103, m.PixelRepresentation),
		{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength}, Value: seq},
	}, nil)
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	err = object.WriteFile(out, &object.File{Dataset: ds, TransferSyntax: syntax})
	return errors.Join(err, out.Close())
}

type countedSource struct {
	*os.File
	calls int64
	bytes int64
}

func (s *countedSource) ReadAt(dst []byte, offset int64) (int, error) {
	n, err := s.File.ReadAt(dst, offset)
	s.calls++
	s.bytes += int64(n)
	return n, err
}

func newReader(ctx context.Context, source *countedSource, deferred bool) (*pixeldata.FrameReader, error) {
	ds, err := object.ReadFileWithOptions(source, object.ReadFileOptions{DeferPixelData: deferred})
	if err != nil {
		return nil, err
	}
	registry := pixeldata.NewMemoryRegistry()
	if err = rle.Register(registry); err != nil {
		return nil, err
	}
	if err = jpegls.Register(registry); err != nil {
		return nil, err
	}
	return pixeldata.NewFrameReader(ctx, ds, source, pixeldata.FrameReaderOptions{Registry: registry})
}

func run(path, storage, api string, iterations int, profilePath string) error {
	ctx := context.Background()
	if storage != "resident" && storage != "deferred" {
		return fmt.Errorf("unknown storage %q", storage)
	}
	now := highResolutionClock()
	start := now()
	input, err := object.OpenSourceFile(path)
	if err != nil {
		return err
	}
	defer input.Close()
	source := &countedSource{File: input}
	reader, err := newReader(ctx, source, storage == "deferred")
	if err != nil {
		return err
	}
	defer reader.Close()
	setup := now() - start
	setupCalls, setupBytes := source.calls, source.bytes
	if reader.Metadata() != fixtureMetadata {
		return fmt.Errorf("unexpected stored metadata %+v", reader.Metadata())
	}
	m := fixtureMetadata
	m.NumberOfFrames = 1
	dst := make([]byte, m.FrameSize())
	type result struct {
		data     []byte
		metadata pixeldata.Metadata
		mode     pixeldata.BufferMode
	}
	var decode func(int) (result, error)
	switch api {
	case "frame":
		decode = func(i int) (result, error) {
			data, metadata, err := reader.Frame(ctx, i)
			return result{data: data, metadata: metadata}, err
		}
	case "into":
		decode = func(i int) (result, error) {
			got, err := reader.FrameInto(ctx, i, dst)
			return result{data: dst[:got.Written], metadata: got.Layout.Metadata, mode: got.Layout.Mode}, err
		}
	case "raw":
		decode = func(i int) (result, error) {
			data, err := reader.RawFrame(ctx, i)
			return result{data: data}, err
		}
	case "locate":
		decode = func(i int) (result, error) {
			n, err := reader.RawFrameLength(i)
			if n <= 0 && err == nil {
				err = fmt.Errorf("frame %d has no encoded bytes", i)
			}
			return result{}, err
		}
	default:
		return fmt.Errorf("unknown API %q", api)
	}

	indices := []int{0, 1, frameCount - 1, 0}
	if iterations == 0 {
		log.Printf("setup_ns=%d ReaderAt_calls=%d ReaderAt_bytes=%d", setup, setupCalls, setupBytes)
		if api != "frame" && api != "into" {
			return fmt.Errorf("verification requires frame or into")
		}
		for _, i := range indices {
			beforeCalls, beforeBytes := source.calls, source.bytes
			start := now()
			got, err := decode(i)
			elapsed := now() - start
			if err != nil {
				return err
			}
			if err := verifyFrame(got.data, got.metadata, m, i); err != nil {
				return err
			}
			gotBytes := source.bytes - beforeBytes
			if storage == "deferred" {
				n, err := reader.RawFrameLength(i)
				if err != nil {
					return err
				}
				if gotBytes != n {
					return fmt.Errorf("frame %d read %d bytes, encoded length %d", i, gotBytes, n)
				}
			}
			log.Printf("frame=%d ns=%d ReaderAt_calls=%d ReaderAt_bytes=%d mode=%s", i, elapsed, source.calls-beforeCalls, gotBytes, got.mode)
			fmt.Printf("%d,%x,%d,%d,%d,%d,%d,%d,%s\n", i, sha256.Sum256(got.data), m.Rows, m.Columns, m.SamplesPerPixel, m.BitsStored, m.HighBit, m.PixelRepresentation, m.PhotometricInterpretation)
		}
		return nil
	}
	for _, i := range indices {
		if _, err := decode(i); err != nil {
			return err
		}
	}
	if profilePath != "" {
		profile, err := os.Create(profilePath)
		if err != nil {
			return err
		}
		defer profile.Close()
		if err := pprof.StartCPUProfile(profile); err != nil {
			return err
		}
		defer pprof.StopCPUProfile()
	}
	beforeCalls, beforeBytes := source.calls, source.bytes
	var phaseNS [4]int64
	for turn := 0; turn < iterations; turn++ {
		for phase, i := range indices {
			begin := now()
			if _, err := decode(i); err != nil {
				return err
			}
			phaseNS[phase] += now() - begin
		}
	}
	readCalls, readBytes := source.calls-beforeCalls, source.bytes-beforeBytes
	// Measure allocations separately so the per-operation Windows counter
	// syscall's own allocations do not inflate the library observations.
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for turn := 0; turn < iterations; turn++ {
		for _, i := range indices {
			if _, err := decode(i); err != nil {
				return err
			}
		}
	}
	runtime.ReadMemStats(&after)
	elapsed := int64(0)
	for _, ns := range phaseNS {
		elapsed += ns
	}
	count := float64(iterations * len(indices))
	fmt.Printf("%.3f,%.3f,%.3f,%.3f,%.3f,%d,%d,%d",
		float64(elapsed)/count, float64(after.TotalAlloc-before.TotalAlloc)/count,
		float64(after.Mallocs-before.Mallocs)/count, float64(readCalls)/count,
		float64(readBytes)/count, setup, setupCalls, setupBytes)
	for _, ns := range phaseNS {
		fmt.Printf(",%.3f", float64(ns)/float64(iterations))
	}
	fmt.Println()
	return nil
}

func verifyFrame(data []byte, metadata, expected pixeldata.Metadata, frame int) error {
	if sha256.Sum256(data) != sha256.Sum256(fixtureFrame(frame)) || metadata != expected {
		return fmt.Errorf("frame %d mismatch: %x, %+v", frame, sha256.Sum256(data), metadata)
	}
	return nil
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "prepare" {
		if err := prepare(os.Args[2]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) < 5 || len(os.Args) > 6 {
		log.Fatal("usage: frame-access-bench prepare DIRECTORY or frame-access-bench FILE {resident|deferred} {frame|into|raw|locate} ITERATIONS [CPU_PROFILE]")
	}
	iterations, err := strconv.Atoi(os.Args[4])
	if err != nil || iterations < 0 {
		log.Fatalf("invalid iterations %q", os.Args[4])
	}
	profile := ""
	if len(os.Args) == 6 {
		profile = os.Args[5]
	}
	if err := run(os.Args[1], os.Args[2], os.Args[3], iterations, profile); err != nil {
		log.Fatal(err)
	}
}

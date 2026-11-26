package jpeg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func progressive12Fixtures(t testing.TB) []progressive12Fixture {
	t.Helper()
	data, err := os.ReadFile("testdata/progressive12/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var r progressive12Receipt
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if !r.Successful || !r.NoPHI || r.License != "Apache-2.0" || len(r.Cases) != 16 {
		t.Fatal("incomplete qualification receipt")
	}
	return r.Cases
}

func progressive12FixtureBytes(t testing.TB, name, hash string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata/progressive12", name))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != hash {
		t.Fatal("fixture hash mismatch", name)
	}
	return raw
}

func TestProgressive12IndependentFixturesAndCallers(t *testing.T) {
	for _, f := range progressive12Fixtures(t) {
		t.Run(f.Name, func(t *testing.T) {
			var streams, reference [][]byte
			for i, name := range f.Streams {
				streams = append(streams, progressive12FixtureBytes(t, name, f.StreamHashes[i]))
				reference = append(reference, progressive12FixtureBytes(t, f.References[i], f.ReferenceHashes[i]))
			}
			obj := progressive12CompressedObject(f.Metadata, streams)
			pixel, err := pixeldata.Extract(obj)
			if err != nil {
				t.Fatal(err)
			}
			registry := progressiveRegistry(t)
			got, err := registry.DecodeFrames(f.UID, pixel, obj)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Data) != 2 || got.Rows != 37 || got.Columns != 41 {
				t.Fatal("frame metadata", got)
			}
			for phase := range got.Data {
				a, b := extendedProfileSamples(got.Data[phase], f.Metadata), extendedProfileSamples(reference[phase], f.Metadata)
				if len(a) != len(b) {
					t.Fatal("sample count")
				}
				for i := range a {
					if abs(a[i]-b[i]) > 2 {
						t.Fatalf("sample %d got%d want%d", i, a[i], b[i])
					}
				}
			}
			// Monochrome1 changes presentation, never stored samples.
			if f.Metadata.SamplesPerPixel == 1 {
				obj.Put(dicomtest.NewStringElement(tagPhotometricInterpretation, core.VRCS, "MONOCHROME1"))
				mono1, err := registry.DecodeFrames(f.UID, pixel, obj)
				if err != nil || !bytes.Equal(mono1.Data[0], got.Data[0]) {
					t.Fatal("MONOCHROME1 samples", err)
				}
			}
			for _, never := range []bool{false, true} {
				ctx := context.Background()
				if never {
					ctx = pixeldata.WithJPEGColorConversion(ctx, pixeldata.JPEGColorNever)
				}
				decoded, err := registry.DecodeFramesContext(ctx, f.UID, pixel, obj)
				if err != nil || !bytes.Equal(decoded.Data[0], got.Data[0]) {
					t.Fatal("color policy", err)
				}
			}
			syntax := transfer.JPEGFullProgression
			if f.UID == UIDSpectralSelection {
				syntax = transfer.JPEGSpectralSelection
			}
			native, target, err := pixeldata.DecompressDataSet(obj, syntax, pixeldata.DecompressOptions{Registry: registry})
			if err != nil {
				t.Fatal(err)
			}
			if target.Encapsulated {
				t.Fatal("decompress syntax")
			}
			transcoded, _, err := pixeldata.TranscodeDataSet(context.Background(), obj, syntax, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{DecoderRegistry: registry})
			if err != nil {
				t.Fatal(err)
			}
			for _, out := range []*object.Object{native, transcoded} {
				frames, err := pixeldata.ExtractNativeFrames(out)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(frames.Data[0], got.Data[0]) || !bytes.Equal(frames.Data[1], got.Data[1]) {
					t.Fatal("caller samples differ")
				}
				format := display.PixelFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 11}
				if f.Metadata.SamplesPerPixel == 1 {
					img, err := display.RenderGray(display.Frame{Rows: 37, Columns: 41, Pixels: frames.Data[0], Format: format})
					if err != nil || img.Bounds().Dx() != 41 {
						t.Fatal("gray display", err)
					}
				} else {
					if frames.Metadata.PhotometricInterpretation != "RGB" || frames.Metadata.PlanarConfiguration != 0 {
						t.Fatal("RGB metadata", frames.Metadata)
					}
					img, err := display.RenderColor16(display.ColorFrame{Rows: 37, Columns: 41, Pixels: frames.Data[0], Format: format, SamplesPerPixel: 3, Photometric: "RGB"})
					if err != nil || img.Bounds().Dy() != 37 {
						t.Fatal("color display", err)
					}
				}
			}
		})
	}
}

type progressiveSection struct {
	marker                    byte
	start, body, entropy, end int
}

func progressiveSections(t testing.TB, stream []byte) []progressiveSection {
	t.Helper()
	d := extendedProcess4Decoder{data: stream, pos: 2}
	var sections []progressiveSection
	for d.pos < len(stream) {
		s := progressiveSection{start: d.pos}
		marker, err := d.nextMarker()
		if err != nil {
			t.Fatal(err)
		}
		s.marker = marker
		if marker == 0xd9 {
			s.end = d.pos
			sections = append(sections, s)
			break
		}
		segment, err := d.readSegment()
		if err != nil {
			t.Fatal(err)
		}
		s.body = d.pos - len(segment)
		if marker == 0xda {
			s.entropy = d.pos
			_, end, err := d.entropySegment()
			if err != nil {
				t.Fatal(err)
			}
			d.pos = end
		}
		s.end = d.pos
		sections = append(sections, s)
	}
	return sections
}

func TestProgressive12RejectsInvalidSequences(t *testing.T) {
	fixtures := progressive12Fixtures(t)
	for _, f := range fixtures {
		if f.Name != "rgb-ep-gradient" && f.Name != "mono-es-flat" && f.Name != "mono-ep-restart" {
			continue
		}
		stream := progressive12FixtureBytes(t, f.Streams[0], f.StreamHashes[0])
		m := f.Metadata
		m.NumberOfFrames = 1
		sections := progressiveSections(t, stream)
		var sof, dqt progressiveSection
		var scans []progressiveSection
		for _, s := range sections {
			switch s.marker {
			case 0xc2:
				sof = s
			case 0xdb:
				dqt = s
			case 0xda:
				scans = append(scans, s)
			}
		}
		check := func(name string, bad []byte) {
			t.Helper()
			t.Run(f.Name+"/"+name, func(t *testing.T) {
				obj := progressive12CompressedObject(m, [][]byte{bad})
				pixel, err := pixeldata.Extract(obj)
				if err != nil {
					t.Fatal(err)
				}
				codec := codecFor(f.UID)
				for _, recover := range []bool{false, true} {
					ctx := pixeldata.DecodeContextWithRecovery(context.Background(), pixeldata.DecodeRecovery{JPEGSequentialScanQuirks: recover})
					got, err := codec.DecodeContext(ctx, pixel, obj)
					if err == nil || len(got.Data) != 0 {
						t.Fatal("published invalid progression", err)
					}
					if !errors.Is(err, ErrInvalidFragment) && !errors.Is(err, ErrUnsupportedBitsStored) {
						t.Fatal("untyped refusal", err)
					}
				}
			})
		}
		mutate := func(name string, offset int, value byte) {
			bad := bytes.Clone(stream)
			bad[offset] = value
			check(name, bad)
		}
		mutate("precision", sof.body, 11)
		mutate("sampling", sof.body+7, 0x21)
		mutate("arithmetic SOF", sof.start+1, 0xca)
		mutate("unknown component", scans[0].body+1, 254)
		tail := scans[0].body + 1 + 2*int(stream[scans[0].body])
		mutate("DC mixed with AC", tail+1, 1)
		mutate("approximation 14", tail+2, 14)
		mutate("refinement before initial", tail+2, 0x10)
		mutate("skipped refinement", tail+2, 0x30)
		bad := append(bytes.Clone(stream[:scans[0].start]), stream[scans[0].end:]...)
		check("missing initial DC", bad)
		bad = append(bytes.Clone(stream[:scans[len(scans)-1].start]), stream[scans[len(scans)-1].end:]...)
		check("missing final band", bad)
		bad = append(bytes.Clone(stream[:scans[0].end]), stream[scans[0].start:scans[0].end]...)
		bad = append(bad, stream[scans[0].end:]...)
		check("duplicate initial band", bad)
		bad = append(bytes.Clone(stream[:scans[0].end]), 0)
		bad = append(bad, stream[scans[0].end:]...)
		check("extra entropy byte", bad)
		bad = append(bytes.Clone(stream[:scans[0].end]), stream[dqt.start:dqt.end]...)
		bad[scans[0].end+dqt.body-dqt.start+1] += 1
		bad = append(bad, stream[scans[0].end:]...)
		check("DQT altered during progression", bad)
		check("truncated EOI", stream[:len(stream)-2])
		if strings.Contains(f.Name, "restart") {
			bad = bytes.Clone(stream)
			for i := scans[0].entropy; i+1 < scans[0].end; i++ {
				if bad[i] == 255 && bad[i+1] == 0xd0 {
					bad[i+1] = 0xd3
					break
				}
			}
			check("wrong restart number", bad)
		}
		if f.Metadata.SamplesPerPixel == 3 {
			mutate("duplicate frame component", sof.body+9, stream[sof.body+6])
			mutate("duplicate scan component", scans[0].body+3, stream[scans[0].body+1])
			mutate("AC interleaved", tail, 1)
			obj := progressive12CompressedObject(m, [][]byte{stream})
			pixel, _ := pixeldata.Extract(obj)
			if _, err := codecFor(UIDSpectralSelection).Decode(pixel, obj); !errors.Is(err, ErrInvalidFragment) {
				t.Fatal(".53 accepted refinement", err)
			}
		}
	}
}

func TestProgressive12MetadataAndResources(t *testing.T) {
	f := progressive12Fixtures(t)[6] // qualified RGB; select independently below.
	for _, fixture := range progressive12Fixtures(t) {
		if fixture.Name == "rgb-ep-gradient" {
			f = fixture
		}
	}
	stream := progressive12FixtureBytes(t, f.Streams[0], f.StreamHashes[0])
	m := f.Metadata
	m.NumberOfFrames = 1
	for _, tc := range []struct {
		name  string
		tag   core.Tag
		value uint16
		want  error
	}{
		{"stored11", tagBitsStored, 11, ErrUnsupportedBitsStored}, {"highbit", tagHighBit, 10, ErrUnsupportedHighBit}, {"signed", tagPixelRepresentation, 1, pixeldata.ErrUnsupportedPixelRepresentation}, {"planar", tagPlanarConfiguration, 1, pixeldata.ErrUnsupportedPlanarConfiguration}, {"allocated12", tagBitsAllocated, 12, ErrUnsupportedBitsAllocated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := progressive12CompressedObject(m, [][]byte{stream})
			obj.Put(dicomtest.Uint16Element(tc.tag, core.VRUS, nil, tc.value))
			pixel, _ := pixeldata.Extract(obj)
			got, err := codecFor(f.UID).Decode(pixel, obj)
			if !errors.Is(err, tc.want) || len(got.Data) != 0 {
				t.Fatal(err)
			}
		})
	}
	missingPC := m
	missingPC.PlanarConfigurationPresent = false
	objMissing := progressive12CompressedObject(missingPC, [][]byte{stream})
	pxMissing, _ := pixeldata.Extract(objMissing)
	if _, err := codecFor(f.UID).Decode(pxMissing, objMissing); !errors.Is(err, pixeldata.ErrUnsupportedPlanarConfiguration) {
		t.Fatal("missing RGB planar attribute", err)
	}
	monoPC := m
	monoPC.SamplesPerPixel = 1
	monoPC.PhotometricInterpretation = "MONOCHROME2"
	objMono := progressive12CompressedObject(monoPC, [][]byte{stream})
	pxMono, _ := pixeldata.Extract(objMono)
	if _, err := codecFor(f.UID).Decode(pxMono, objMono); !errors.Is(err, pixeldata.ErrUnsupportedPlanarConfiguration) {
		t.Fatal("mono planar attribute", err)
	}
	huge := m
	huge.Rows = 65535
	huge.Columns = 65535
	objHuge := progressive12CompressedObject(huge, [][]byte{stream})
	pxHuge, _ := pixeldata.Extract(objHuge)
	if out, err := codecFor(f.UID).Decode(pxHuge, objHuge); !errors.Is(err, ErrProgressiveResourceLimit) || len(out.Data) != 0 {
		t.Fatal("public oversized request", err)
	}
	obj := progressive12CompressedObject(m, [][]byte{stream})
	pixel, _ := pixeldata.Extract(obj)
	ctx := pixeldata.WithJPEGColorConversion(context.Background(), pixeldata.JPEGColorAlways)
	if _, err := codecFor(f.UID).DecodeContext(ctx, pixel, obj); !errors.Is(err, pixeldata.ErrUnsupportedPhotometricInterpretation) {
		t.Fatal("unqualified transform", err)
	}
	obj.Put(dicomtest.NewStringElement(tagPhotometricInterpretation, core.VRCS, "YBR_FULL"))
	if _, err := codecFor(f.UID).Decode(pixel, obj); !errors.Is(err, pixeldata.ErrUnsupportedPhotometricInterpretation) {
		t.Fatal("YBR accepted", err)
	}
	for _, modify := range []func(*pixeldata.Metadata){
		func(m *pixeldata.Metadata) { m.Rows = 65535; m.Columns = 65535 },
		func(m *pixeldata.Metadata) { m.NumberOfFrames = int(^uint(0) >> 1) },
	} {
		bad := m
		modify(&bad)
		if _, err := progressive12RequestBytes(bad, 0, 0); !errors.Is(err, ErrProgressiveResourceLimit) {
			t.Fatal("request overflow admitted", err)
		}
	}
	if _, err := progressive12RequestBytes(m, ^uint64(0), 0); !errors.Is(err, ErrProgressiveResourceLimit) {
		t.Fatal("input overflow admitted", err)
	}
	// Explicit work gates execute after valid headers, before entropy or loops.
	for _, which := range []string{"scans", "work"} {
		d := progressive12Decoder{extendedProcess4Decoder: extendedProcess4Decoder{data: stream, pos: 2}, ctx: context.Background()}
		for i := range d.approx {
			for k := range d.approx[i] {
				d.approx[i][k] = -1
			}
		}
		for {
			marker, _ := d.nextMarker()
			segment, err := d.readSegment()
			if err != nil {
				t.Fatal(err)
			}
			switch marker {
			case 0xc2:
				err = d.parseSOF2(segment, m)
			case 0xdb:
				err = d.parseDQT(segment)
			case 0xc4:
				err = d.parseDHT(segment)
			case 0xda:
				if which == "scans" {
					d.scans = progressive12MaxScans
				} else {
					d.work = &progressive12WorkBudget{remaining: 0}
				}
				err = d.decodeProgressiveScan(segment)
				if !errors.Is(err, ErrProgressiveResourceLimit) {
					t.Fatal(which, err)
				}
			}
			if marker == 0xda {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

type progressiveCancelContext struct {
	context.Context
	cancel    context.CancelFunc
	calls, at int
}

func (c *progressiveCancelContext) Err() error {
	c.calls++
	if c.at > 0 && c.calls == c.at {
		c.cancel()
	}
	return c.Context.Err()
}

func TestProgressive12CancellationAndConcurrentUse(t *testing.T) {
	f := progressive12Fixtures(t)[0]
	for _, fixture := range progressive12Fixtures(t) {
		if fixture.Name == "rgb-ep-restart" {
			f = fixture
		}
	}
	stream := progressive12FixtureBytes(t, f.Streams[0], f.StreamHashes[0])
	m := f.Metadata
	m.NumberOfFrames = 1
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	counter := &progressiveCancelContext{Context: base, cancel: cancel}
	want, err := decodeProgressive12(counter, stream, m, false)
	if err != nil {
		t.Fatal(err)
	}
	checks := counter.calls
	// Deterministic real cancellation at every observed checkpoint spans header
	// admission, allocation, entropy/MCU loops, IDCT, and output row assembly.
	for at := 1; at <= checks; at++ {
		base, cancel := context.WithCancel(context.Background())
		ctx := &progressiveCancelContext{Context: base, cancel: cancel, at: at}
		out, err := decodeProgressive12(ctx, stream, m, false)
		cancel()
		if !errors.Is(err, context.Canceled) || out != nil {
			t.Fatalf("checkpoint%d: bytes%d err%v", at, len(out), err)
		}
	}
	obj := progressive12CompressedObject(m, [][]byte{stream})
	pixel, _ := pixeldata.Extract(obj)
	codec := codecFor(f.UID)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				got, err := codec.Decode(pixel, obj)
				if err != nil || len(got.Data) != 1 || !bytes.Equal(got.Data[0], want) {
					t.Error("concurrent result", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func FuzzProgressive12(f *testing.F) {
	for _, fixture := range progressive12Fixtures(f) {
		f.Add(progressive12FixtureBytes(f, fixture.Streams[0], fixture.StreamHashes[0]), fixture.Metadata.SamplesPerPixel, fixture.UID == UIDSpectralSelection)
	}
	f.Fuzz(func(t *testing.T, data []byte, count uint16, spectral bool) {
		if len(data) > 1<<20 {
			return
		}
		if count != 1 && count != 3 {
			return
		}
		photo := "MONOCHROME2"
		if count == 3 {
			photo = "RGB"
		}
		m := extendedProfileMetadata(12, photo)
		m.NumberOfFrames = 1
		out, err := decodeProgressive12(context.Background(), data, m, spectral)
		if err == nil && len(out) != int(m.FrameSize()) {
			t.Fatal(fmt.Sprintf("output size%d", len(out)))
		}
	})
}

func TestProgressive12RequestWorkAndMarkerCancellation(t *testing.T) {
	f := progressive12Fixtures(t)[0]
	for _, fixture := range progressive12Fixtures(t) {
		if fixture.Name == "rgb-ep-restart" {
			f = fixture
		}
	}
	stream := progressive12FixtureBytes(t, f.Streams[0], f.StreamHashes[0])
	m := f.Metadata
	m.NumberOfFrames = 1
	calibration := &progressive12WorkBudget{remaining: progressive12MaxBlockVisits}
	want, err := decodeProgressive12WithBudget(context.Background(), stream, m, false, calibration)
	if err != nil {
		t.Fatal(err)
	}
	visits := progressive12MaxBlockVisits - calibration.remaining
	// Two individually admissible frames share a budget. The second frame must
	// causally fail even though its own work and both frames' bytes fit the limits.
	shared := &progressive12WorkBudget{remaining: visits*2 - 1}
	first, err := decodeProgressive12WithBudget(context.Background(), stream, m, false, shared)
	if err != nil || !bytes.Equal(first, want) {
		t.Fatal(err)
	}
	second, err := decodeProgressive12WithBudget(context.Background(), stream, m, false, shared)
	if !errors.Is(err, ErrProgressiveResourceLimit) || second != nil {
		t.Fatal("multiframe work admitted", err)
	}
	for _, restart := range []bool{false, true} {
		base, cancel := context.WithCancel(context.Background())
		ctx := &progressiveCancelContext{Context: base, cancel: cancel, at: 2}
		fill := bytes.Repeat([]byte{255}, 8192)
		fill = append(fill, 0xd0)
		d := progressive12Decoder{extendedProcess4Decoder: extendedProcess4Decoder{data: fill}, ctx: ctx}
		if restart {
			r := extendedBitReader{data: fill}
			err = d.consumeProgressiveRestart(&r, 0xd0)
			if r.pos >= len(fill) {
				t.Fatal("restart fill consumed after cancellation")
			}
		} else {
			_, err = d.nextProgressiveMarker()
			if d.pos >= len(fill) {
				t.Fatal("marker fill consumed after cancellation")
			}
		}
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatal("marker loop did not cancel", restart, err)
		}
	}
}

func TestProgressive12IDCTClampsBeforeHostConversion(t *testing.T) {
	for _, value := range []float64{1 << 40, -(1 << 40)} {
		maximum := 4095
		want := uint16(0)
		if value > 0 {
			want = 4095
		}
		if got := roundExtendedIDCTSample(value, 2048, maximum); got != want {
			t.Fatal("host conversion", value, got, want)
		}
		d := extendedProcess4Decoder{precision: 12}
		c := extendedComponent{blocksPerLine: 1, blocksPerColumn: 1, samples: make([]uint16, 64)}
		var coefficients [64]float64
		coefficients[0] = value * 8
		d.writeIDCTBlock(&c, 0, 0, coefficients)
		for _, got := range c.samples {
			if got != want {
				t.Fatal("DC clamp", value, got, want)
			}
		}
	}
}

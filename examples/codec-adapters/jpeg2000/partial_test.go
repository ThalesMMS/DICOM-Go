package jpeg2000

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	j2kenc "github.com/ThalesMMS/dicom-go/pixeldata/jpeg2000"
	"github.com/ThalesMMS/dicom-go/transfer"

	"github.com/ThalesMMS/dicom-go/examples/codec-adapters/jpeg2000/internal/j2kcore"
)

// coreMetadata describes a corpus codestream as one DICOM frame.
func coreMetadata(t testing.TB, codestream []byte) pixeldata.Metadata {
	t.Helper()
	stream, err := j2kcore.ParseCodestream(codestream)
	if err != nil {
		t.Fatal(err)
	}
	w, h := stream.OutputSize(0)
	depth := stream.Components[0].Depth
	m := pixeldata.Metadata{
		Rows: uint16(h), Columns: uint16(w), SamplesPerPixel: uint16(len(stream.Components)),
		BitsAllocated: 8, BitsStored: uint16(depth), HighBit: uint16(depth - 1),
		NumberOfFrames: 1, PhotometricInterpretation: "MONOCHROME2",
	}
	if depth > 8 {
		m.BitsAllocated = 16
	}
	if stream.Components[0].Signed {
		m.PixelRepresentation = 1
	}
	if len(stream.Components) == 3 {
		m.PhotometricInterpretation = "RGB"
		if stream.Coding.MCT {
			m.PhotometricInterpretation = "YBR_RCT"
			if stream.Coding.Wavelet != j2kcore.Wavelet53Reversible {
				m.PhotometricInterpretation = "YBR_ICT"
			}
		}
	}
	return m
}

// cropInterleaved cuts r out of an interleaved frame of columns pixels.
func cropInterleaved(frame []byte, columns, pixelBytes int, r image.Rectangle) []byte {
	var out []byte
	for y := r.Min.Y; y < r.Max.Y; y++ {
		out = append(out, frame[(y*columns+r.Min.X)*pixelBytes:(y*columns+r.Max.X)*pixelBytes]...)
	}
	return out
}

func partial(t testing.TB, codec *Codec, codestream []byte, m pixeldata.Metadata, request pixeldata.PartialDecodeRequest) []byte {
	t.Helper()
	frames, err := codec.DecodeFramePartial(context.Background(), codestream, m, request)
	if err != nil {
		t.Fatalf("%+v: %v", request, err)
	}
	region := request.Region
	if region == (image.Rectangle{}) {
		region = image.Rect(0, 0, int(m.Columns), int(m.Rows))
	}
	out := pixeldata.ReducedRegion(region, request.ReductionLevel)
	if frames.Rows != out.Dy() || frames.Columns != out.Dx() || len(frames.Data) != 1 {
		t.Fatalf("%+v: %d frames of %dx%d, want %v", request, len(frames.Data), frames.Columns, frames.Rows, out)
	}
	return frames.Data[0]
}

// TestCorePartialDecodeIsConsistent checks, on corpus codestreams (tiled,
// subsampled, colour, irreversible, layered), that a partial decode is the
// crop of the decode it is part of, bit for bit: a full-resolution region
// of the full decode, a reduced region of the reduced frame (the
// codestream's own lower resolution, which DecodeCoreReduced checks against
// OpenJPEG), and a region of the first layers of the frame of those layers;
// all layers are the full decode.
func TestCorePartialDecodeIsConsistent(t *testing.T) {
	codec := NewCore(CoreOptions{})
	for _, fixture := range []string{
		"jpeg2000-core-u12-tiles", "jpeg2000-core-rgb8-rpcl-precincts", "jpeg2000-core-u8-subsampled",
		"jpeg2000-core-u8-vsc-reset-irreversible", "jpeg2000-core-u8-odd-layers", "jpeg2000-core-s12",
	} {
		t.Run(fixture, func(t *testing.T) {
			codestream := corpusCodestream(t, fixture)
			m := coreMetadata(t, codestream)
			pixelBytes := int(m.SamplesPerPixel) * int(m.BitsAllocated/8)
			capabilities, err := codec.PartialDecodeCapabilities(codestream, m)
			if err != nil || !capabilities.Region || capabilities.ReductionLevels == 0 || capabilities.QualityLayers == 0 {
				t.Fatalf("capabilities %+v, %v", capabilities, err)
			}
			full, err := codec.decoder.(ContextDecoder).DecodeFrameContext(context.Background(), codestream, m)
			if err != nil {
				t.Fatal(err)
			}
			for layers := 0; layers <= capabilities.QualityLayers; layers++ {
				for reduce := 0; reduce <= capabilities.ReductionLevels; reduce++ {
					whole := partial(t, codec, codestream, m, pixeldata.PartialDecodeRequest{ReductionLevel: reduce, QualityLayers: layers})
					if (layers == 0 || layers == capabilities.QualityLayers) && reduce == 0 && !bytes.Equal(whole, full) {
						t.Fatalf("%d layers: the whole frame differs from the full decode", layers)
					}
					if reduce > 0 && layers == 0 {
						reduced, err := DecodeCoreReduced(context.Background(), codestream, reduce, CoreOptions{})
						if err != nil {
							t.Fatal(err)
						}
						for i, v := range reduced.Components[0] {
							var got int32
							if m.BitsAllocated == 8 {
								got = int32(whole[i*pixelBytes])
							} else {
								got = int32(binary.LittleEndian.Uint16(whole[i*pixelBytes:]))
							}
							if m.PixelRepresentation == 1 {
								got = int32(int16(uint16(got)<<(16-m.BitsStored))) >> (16 - m.BitsStored)
							}
							if got != v {
								t.Fatalf("reduce %d: sample %d is %d, DecodeCoreReduced %d", reduce, i, got, v)
							}
						}
					}
					reducedColumns := pixeldata.ReducedRegion(image.Rect(0, 0, int(m.Columns), int(m.Rows)), reduce).Dx()
					w, h := int(m.Columns), int(m.Rows)
					for _, region := range []image.Rectangle{
						image.Rect(0, 0, 1, 1), image.Rect(w/3, h/4, w/3+w/2, h/4+h/3), image.Rect(w-5, h-3, w, h), image.Rect(w/2-1, 0, w/2+1, h),
					} {
						got := partial(t, codec, codestream, m, pixeldata.PartialDecodeRequest{Region: region, ReductionLevel: reduce, QualityLayers: layers})
						if want := cropInterleaved(whole, reducedColumns, pixelBytes, pixeldata.ReducedRegion(region, reduce)); !bytes.Equal(got, want) {
							t.Fatalf("%v at reduce %d, %d layers differs from the crop of the frame", region, reduce, layers)
						}
					}
				}
			}
		})
	}
}

// layerCases are corpus codestreams decoded from their first layers, at
// full or reduced resolution. The references in testdata/layers are
// opj_decompress -l (and -r) output (planar, little-endian), regenerated
// with DICOM_GO_J2K_UPDATE_REDUCED=1 and DICOM_GO_OPENJPEG_DECOMPRESS set.
var layerCases = []struct {
	fixture        string
	layers, reduce int
}{
	{"jpeg2000-core-u8-odd-layers", 1, 0},
	{"jpeg2000-core-u8-odd-layers", 2, 0},
	{"jpeg2000-core-u8-odd-layers", 1, 1},
	{"jpeg2000-core-u8-odd-layers", 2, 2},
}

// TestCoreQualityLayersMatchOpenJPEG compares the decode of the first
// layers with OpenJPEG's, and checks that the error against the decode of
// every layer shrinks as layers are added.
func TestCoreQualityLayersMatchOpenJPEG(t *testing.T) {
	update := os.Getenv("DICOM_GO_J2K_UPDATE_REDUCED") == "1"
	decompress := os.Getenv("DICOM_GO_OPENJPEG_DECOMPRESS")
	if update && decompress == "" {
		t.Fatal("DICOM_GO_J2K_UPDATE_REDUCED needs DICOM_GO_OPENJPEG_DECOMPRESS")
	}
	codec := NewCore(CoreOptions{})
	previous := map[string]int64{}
	for _, tc := range layerCases {
		name := fmt.Sprintf("%s-l%d", tc.fixture, tc.layers)
		if tc.reduce > 0 {
			name += fmt.Sprintf("-r%d", tc.reduce)
		}
		t.Run(name, func(t *testing.T) {
			codestream := corpusCodestream(t, tc.fixture)
			m := coreMetadata(t, codestream)
			reference := filepath.Join("testdata", "layers", name+".rawl")
			if update {
				path := filepath.Join(t.TempDir(), tc.fixture+".j2k")
				if err := os.WriteFile(path, codestream, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(reference), 0o755); err != nil {
					t.Fatal(err)
				}
				runOpenJPEG(t, decompress, "-i", path, "-o", reference, "-l", strconv.Itoa(tc.layers), "-r", strconv.Itoa(tc.reduce))
			}
			raw, err := os.ReadFile(reference)
			if err != nil {
				t.Fatal(err)
			}
			out := pixeldata.ReducedRegion(image.Rect(0, 0, int(m.Columns), int(m.Rows)), tc.reduce)
			want, rest, err := readPlane(raw, out.Dx()*out.Dy(), int(m.BitsStored), m.PixelRepresentation == 1)
			if err != nil || len(rest) != 0 || m.SamplesPerPixel != 1 || m.BitsAllocated != 8 {
				t.Fatalf("reference: %v, %d bytes left", err, len(rest))
			}
			got := partial(t, codec, codestream, m, pixeldata.PartialDecodeRequest{QualityLayers: tc.layers, ReductionLevel: tc.reduce})
			all := partial(t, codec, codestream, m, pixeldata.PartialDecodeRequest{ReductionLevel: tc.reduce})
			var distance int64
			for i, v := range want {
				if int32(got[i]) != v {
					t.Fatalf("sample %d is %d, OpenJPEG -l %d -r %d gives %d", i, got[i], tc.layers, tc.reduce, v)
				}
				d := int64(got[i]) - int64(all[i])
				distance += d * d
			}
			if distance == 0 {
				t.Fatalf("%d of the layers decode as all of them", tc.layers)
			}
			if tc.reduce == 0 {
				if last, ok := previous[tc.fixture]; ok && distance > last {
					t.Fatalf("%d layers are further from the full decode (%d) than fewer (%d)", tc.layers, distance, last)
				}
				previous[tc.fixture] = distance
			}
		})
	}
}

func TestCorePartialDecodeRejectsInvalidRequests(t *testing.T) {
	codec := NewCore(CoreOptions{})
	codestream := corpusCodestream(t, "jpeg2000-core-u8-odd-layers")
	m := coreMetadata(t, codestream)
	capabilities, err := codec.PartialDecodeCapabilities(codestream, m)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		request pixeldata.PartialDecodeRequest
		want    []error
	}{
		{"reduction beyond the levels", pixeldata.PartialDecodeRequest{ReductionLevel: capabilities.ReductionLevels + 1}, []error{pixeldata.ErrInvalidReduction, ErrInvalidReduction}},
		{"negative reduction", pixeldata.PartialDecodeRequest{ReductionLevel: -1}, []error{pixeldata.ErrInvalidReduction}},
		{"layers beyond the codestream's", pixeldata.PartialDecodeRequest{QualityLayers: capabilities.QualityLayers + 1}, []error{pixeldata.ErrInvalidQualityLayers}},
		{"region outside", pixeldata.PartialDecodeRequest{Region: image.Rect(0, 0, int(m.Columns)+1, 1)}, []error{pixeldata.ErrInvalidRegion}},
	} {
		_, err := codec.DecodeFramePartial(context.Background(), codestream, m, c.request)
		for _, want := range c.want {
			if !errors.Is(err, want) {
				t.Errorf("%s: %v, want %v", c.name, err, want)
			}
		}
	}
	// An image origin off the reduced grid is not reduced natively: the
	// capabilities say so, and the request asks for the fallback.
	offsets := corpusCodestream(t, "jpeg2000-core-u12-pcrl-offsets")
	om := coreMetadata(t, offsets)
	if got, err := codec.PartialDecodeCapabilities(offsets, om); err != nil || got.ReductionLevels != 0 || !got.Region {
		t.Errorf("offset image capabilities %+v, %v", got, err)
	}
	if _, err := codec.DecodeFramePartial(context.Background(), offsets, om, pixeldata.PartialDecodeRequest{ReductionLevel: 1}); !errors.Is(err, pixeldata.ErrPartialDecodeUnsupported) {
		t.Errorf("offset image reduced: %v", err)
	}
	// Other backends serve nothing partially.
	other := New()
	if got, err := other.PartialDecodeCapabilities(codestream, m); err != nil || got != (pixeldata.PartialDecodeCapabilities{}) {
		t.Errorf("pure-Go backend capabilities %+v, %v", got, err)
	}
	if _, err := other.DecodeFramePartial(context.Background(), codestream, m, pixeldata.PartialDecodeRequest{}); !errors.Is(err, pixeldata.ErrPartialDecodeUnsupported) {
		t.Errorf("pure-Go backend: %v", err)
	}
}

// partialTestFile writes a synthetic frame (a smooth ramp with a little
// texture) encoded by the base module's pure-Go encoder to syntax, and
// returns its path.
func partialTestFile(t testing.TB, dir string, rows, columns, levels int, syntax transfer.Syntax) string {
	t.Helper()
	us := func(tag uint16, v int) core.Element {
		return core.NewRawElement(core.NewTag(0x0028, tag), core.VRUS, binary.LittleEndian.AppendUint16(nil, uint16(v)))
	}
	pixels := make([]byte, rows*columns*2)
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			v := x + y/2 + (x*y)%5
			binary.LittleEndian.PutUint16(pixels[2*(y*columns+x):], uint16(v))
		}
	}
	dataset := object.FromElements([]core.Element{
		core.NewRawElement(core.NewTag(0x0008, 0x0016), core.VRUI, []byte("1.2.840.10008.5.1.4.1.1.7\x00")),
		core.NewRawElement(core.NewTag(0x0008, 0x0018), core.VRUI, []byte("1.2.826.0.1.3680043.10.1.1151.1\x00")),
		us(0x0002, 1), core.NewRawElement(core.NewTag(0x0028, 0x0004), core.VRCS, []byte("MONOCHROME2 ")),
		us(0x0010, rows), us(0x0011, columns), us(0x0100, 16), us(0x0101, 12), us(0x0102, 11), us(0x0103, 0),
		core.NewRawElement(core.TagPixelData, core.VROW, pixels),
	}, nil)
	encoders := pixeldata.NewMemoryEncoderRegistry()
	encoder, err := j2kenc.NewEncoder(j2kenc.Options{TransferSyntaxUID: syntax.UID, Levels: levels})
	if err != nil {
		t.Fatal(err)
	}
	if err := encoders.RegisterEncoder(syntax.UID, encoder); err != nil {
		t.Fatal(err)
	}
	encoded, _, err := pixeldata.TranscodeFile(context.Background(), &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}, syntax,
		pixeldata.TranscodeOptions{EncoderRegistry: encoders, Limits: pixeldata.TranscodeLimits{MaxPixels: 1 << 26}})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := object.WriteFile(&b, encoded); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, syntax.UID+".dcm")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// wholeFrameOnly hides a codec's partial and region decoders, so that
// FrameReader.DecodePartial takes its full-decode fallback.
type wholeFrameOnly struct{ pixeldata.Codec }

// TestFrameReaderDecodePartialWithCoreCodec reads regions at reduced
// resolution of Part 1 and HTJ2K frames through pixeldata.FrameReader: the
// core codec serves them itself, a region at full resolution is the crop of
// the frame, and a reduction stays within a few levels of the mean of the
// full decode's cells (the fallback's reduction) on this smooth image.
func TestFrameReaderDecodePartialWithCoreCodec(t *testing.T) {
	dir := t.TempDir()
	for _, syntax := range []transfer.Syntax{transfer.JPEG2000LosslessOnly, transfer.HTJ2KLossless} {
		t.Run(syntax.UID, func(t *testing.T) {
			path := partialTestFile(t, dir, 96, 160, 4, syntax)
			core := coreDecoders(t)
			codec, _ := core.GetCodec(syntax.UID)
			fallback := pixeldata.NewMemoryRegistry()
			if err := fallback.RegisterCodec(syntax.UID, wholeFrameOnly{codec}); err != nil {
				t.Fatal(err)
			}
			reader, err := pixeldata.OpenFrameReader(context.Background(), path, pixeldata.FrameReaderOptions{Registry: core})
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			averaged, err := pixeldata.OpenFrameReader(context.Background(), path, pixeldata.FrameReaderOptions{Registry: fallback})
			if err != nil {
				t.Fatal(err)
			}
			defer averaged.Close()
			capabilities, err := reader.PartialDecodeCapabilities(context.Background(), 0)
			if err != nil || capabilities != (pixeldata.PartialDecodeCapabilities{Region: true, ReductionLevels: 4, QualityLayers: 1}) {
				t.Fatalf("capabilities %+v, %v", capabilities, err)
			}
			frame, metadata, err := reader.Frame(context.Background(), 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, region := range []image.Rectangle{{}, image.Rect(17, 9, 130, 70), image.Rect(159, 95, 160, 96)} {
				for reduce := 0; reduce <= 4; reduce++ {
					request := pixeldata.PartialDecodeRequest{Region: region, ReductionLevel: reduce, NoFallback: true}
					got, err := reader.DecodePartial(context.Background(), request)
					if err != nil {
						t.Fatalf("%v reduce %d: %v", region, reduce, err)
					}
					if got.Path != pixeldata.PartialDecodeCodec || got.Fallback || got.AveragedReduction {
						t.Fatalf("%v reduce %d: path %v, fallback %v", region, reduce, got.Path, got.Fallback)
					}
					request.NoFallback = false
					want, err := averaged.DecodePartial(context.Background(), request)
					if err != nil {
						t.Fatal(err)
					}
					if want.Path != pixeldata.PartialDecodeFullFrame || want.Fallback != (reduce > 0 || region != image.Rectangle{}) {
						t.Fatalf("fallback path %v, fallback %v", want.Path, want.Fallback)
					}
					if got.Region != want.Region || len(got.Data) != len(want.Data) {
						t.Fatalf("%v reduce %d: %v and %v", region, reduce, got.Region, want.Region)
					}
					if reduce == 0 {
						r := region
						if r == (image.Rectangle{}) {
							r = image.Rect(0, 0, int(metadata.Columns), int(metadata.Rows))
						}
						if !bytes.Equal(got.Data, cropInterleaved(frame, int(metadata.Columns), 2, r)) || !bytes.Equal(got.Data, want.Data) {
							t.Fatalf("%v differs from the crop of the frame", region)
						}
						continue
					}
					// The wavelet low band is centred on the cell's first sample
					// and the mean on the cell, (2^n-1)/2 pixels apart: on this
					// ramp (1 + 1/2 a pixel) 3/4 (2^n-1) levels, plus the texture.
					worst := 0
					for i := 0; i < len(got.Data); i += 2 {
						d := int(binary.LittleEndian.Uint16(got.Data[i:])) - int(binary.LittleEndian.Uint16(want.Data[i:]))
						worst = max(worst, d, -d)
					}
					if worst > 4+(3<<reduce)/4 {
						t.Fatalf("%v reduce %d: %d from the averaged reduction", region, reduce, worst)
					}
				}
			}
			if _, err := reader.DecodePartial(context.Background(), pixeldata.PartialDecodeRequest{ReductionLevel: 5}); !errors.Is(err, pixeldata.ErrInvalidReduction) {
				t.Fatalf("reduce 5 of 4 levels: %v", err)
			}
			if _, err := reader.DecodePartial(context.Background(), pixeldata.PartialDecodeRequest{QualityLayers: 2}); !errors.Is(err, pixeldata.ErrInvalidQualityLayers) {
				t.Fatalf("2 layers of 1: %v", err)
			}
		})
	}
}

// BenchmarkCorePartialDecode compares, on a 2048x2048 12-bit frame with
// five decomposition levels, a 1/8 thumbnail and a 512x512 region at 1/2
// decoded by the core codec with the fallback that decodes the whole frame
// and reduces it.
func BenchmarkCorePartialDecode(b *testing.B) {
	path := partialTestFile(b, b.TempDir(), 2048, 2048, 5, transfer.JPEG2000LosslessOnly)
	core := coreDecoders(b)
	codec, _ := core.GetCodec(transfer.JPEG2000LosslessOnly.UID)
	fallback := pixeldata.NewMemoryRegistry()
	if err := fallback.RegisterCodec(transfer.JPEG2000LosslessOnly.UID, wholeFrameOnly{codec}); err != nil {
		b.Fatal(err)
	}
	for _, c := range []struct {
		name     string
		registry pixeldata.Registry
		request  pixeldata.PartialDecodeRequest
	}{
		{"thumbnail-1/8/codec", core, pixeldata.PartialDecodeRequest{ReductionLevel: 3}},
		{"thumbnail-1/8/full-decode+average", fallback, pixeldata.PartialDecodeRequest{ReductionLevel: 3}},
		{"region-512-at-1/2/codec", core, pixeldata.PartialDecodeRequest{Region: image.Rect(512, 512, 1536, 1536), ReductionLevel: 1}},
		{"region-512-at-1/2/full-decode+average", fallback, pixeldata.PartialDecodeRequest{Region: image.Rect(512, 512, 1536, 1536), ReductionLevel: 1}},
	} {
		b.Run(c.name, func(b *testing.B) {
			reader, err := pixeldata.OpenFrameReader(context.Background(), path, pixeldata.FrameReaderOptions{Registry: c.registry})
			if err != nil {
				b.Fatal(err)
			}
			defer reader.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := reader.DecodePartial(context.Background(), c.request); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

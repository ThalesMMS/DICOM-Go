package jpeg

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func extendedProfileMetadata(bits uint16, photometric string) pixeldata.Metadata {
	m := extendedEncoderMetadata(37, 41, bits)
	if bits == 8 {
		m.BitsAllocated = 8
	}
	m.PhotometricInterpretation = photometric
	if photometric == "RGB" || photometric == "YBR_FULL" {
		m.SamplesPerPixel, m.PlanarConfigurationPresent = 3, true
	}
	return m
}

// extendedProfileFrame is an own synthetic raster: smooth independent
// component gradients/texture and a sharp colored insert, with odd borders.
// Components are supplied directly, without a color transform or vendor data.
func extendedProfileFrame(m pixeldata.Metadata, phase int) []byte {
	if m.SamplesPerPixel == 1 {
		raw := extendedPhantomFrame(int(m.Rows), int(m.Columns), m.BitsStored, uint32(1457+phase))
		if m.BitsAllocated == 8 {
			out := make([]byte, len(raw)/2)
			for i := range out {
				out[i] = raw[2*i]
			}
			return out
		}
		for i := 0; i < len(raw); i += 2 {
			binary.LittleEndian.PutUint16(raw[i:], binary.LittleEndian.Uint16(raw[i:])&((1<<m.BitsStored)-1))
		}
		return raw
	}
	out := make([]byte, int(m.FrameSize()))
	peak := float64((uint32(1) << m.BitsStored) - 1)
	for y := 0; y < int(m.Rows); y++ {
		for x := 0; x < int(m.Columns); x++ {
			xx, yy := float64(x)/float64(m.Columns-1), float64(y)/float64(m.Rows-1)
			v := [3]float64{.12 + .6*xx + .12*math.Sin(float64(y+phase)*.3), .17 + .55*yy + .1*math.Cos(float64(x+phase)*.2), .7 - .4*xx + .1*math.Sin(float64(x+y+phase)*.4)}
			if x > int(m.Columns)/3 && x < int(m.Columns)/2 && y > int(m.Rows)/3 && y < int(m.Rows)/2 {
				v = [3]float64{.85, .2, .05}
			}
			if m.PhotometricInterpretation == "YBR_FULL" {
				v[1], v[2] = .4+.2*v[1], .4+.2*v[2]
			}
			for c := 0; c < 3; c++ {
				value := uint16(math.Round(max(.02, min(.95, v[c])) * peak))
				i := ((y*int(m.Columns)+x)*3 + c) * m.BytesPerSample()
				if m.BitsAllocated == 8 {
					out[i] = byte(value)
				} else {
					binary.LittleEndian.PutUint16(out[i:], value)
				}
			}
		}
	}
	return out
}

func extendedProfileObject(m pixeldata.Metadata, raw []byte) *object.Object {
	out := nativeExtendedObject(m.Rows, m.Columns, m.BitsStored, m.NumberOfFrames, raw)
	for tag, value := range map[core.Tag]uint16{tagSamplesPerPixel: m.SamplesPerPixel, tagBitsAllocated: m.BitsAllocated, tagBitsStored: m.BitsStored, tagHighBit: m.HighBit} {
		out.Put(dicomtest.Uint16Element(tag, core.VRUS, nil, value))
	}
	out.Put(dicomtest.NewStringElement(tagPhotometricInterpretation, core.VRCS, m.PhotometricInterpretation))
	out.Put(dicomtest.NewStringElement(tagNumberOfFrames, core.VRIS, strconv.Itoa(m.NumberOfFrames)))
	if m.PlanarConfigurationPresent {
		out.Put(dicomtest.Uint16Element(tagPlanarConfiguration, core.VRUS, nil, m.PlanarConfiguration))
	}
	vr := core.VROW
	if m.BitsAllocated == 8 {
		vr = core.VROB
	}
	out.Put(core.NewRawElement(core.TagPixelData, vr, raw))
	return out
}

func extendedProfileSamples(raw []byte, m pixeldata.Metadata) []int {
	if m.BitsAllocated == 16 {
		return extendedSamples(raw, m.BitsStored)
	}
	out := make([]int, len(raw))
	for i, value := range raw {
		out[i] = int(value)
	}
	return out
}

func TestExtendedEncoderStrictAndCompatibilityProfiles(t *testing.T) {
	strict, err := NewExtendedEncoderWithOptions(ExtendedEncoderOptions{Quality: 75})
	if err != nil {
		t.Fatal(err)
	}
	caps := strict.Capabilities()
	if len(caps.CompatibilityProfiles) != 0 || slices.Contains(caps.PhotometricInterpretations, "RGB") || len(caps.SamplesPerPixel) != 1 {
		t.Fatalf("strict capabilities=%+v", caps)
	}
	for _, bits := range []uint16{8, 12} {
		m := extendedProfileMetadata(bits, "MONOCHROME2")
		if got, err := strict.EncodeFrame(context.Background(), extendedProfileFrame(m, 0), m); err != nil || got.CompatibilityProfile != "" {
			t.Fatalf("strict mono%d=%+v/%v", bits, got.CompatibilityProfile, err)
		}
	}
	for _, bits := range []uint16{9, 10, 11} {
		m := extendedProfileMetadata(bits, "MONOCHROME1")
		raw := extendedProfileFrame(m, 0)
		if _, err := strict.EncodeFrame(context.Background(), raw, m); !errors.Is(err, pixeldata.ErrUnsupportedEncoderMetadata) {
			t.Fatal("strict accepted reduced precision", bits, err)
		}
		old, _ := NewExtendedEncoder(75)
		got, err := old.EncodeFrame(context.Background(), raw, m)
		if err != nil || got.CompatibilityProfile != pixeldata.EncoderCompatibilityJPEGExtendedReducedPrecision {
			t.Fatal("historical profile missing", bits, err)
		}
	}
	m := extendedProfileMetadata(12, "RGB")
	if _, err := strict.EncodeFrame(context.Background(), extendedProfileFrame(m, 0), m); !errors.Is(err, pixeldata.ErrUnsupportedEncoderMetadata) {
		t.Fatal("implicit color", err)
	}
	if _, err := NewExtendedEncoderWithOptions(ExtendedEncoderOptions{Quality: 75, MaxWorkingBytes: -1}); !errors.Is(err, ErrInvalidExtendedEncoderLimits) {
		t.Fatal(err)
	}
	if _, err := NewExtendedEncoderWithOptions(ExtendedEncoderOptions{Quality: 75, MaxEncodedFrameBytes: -1}); !errors.Is(err, ErrInvalidExtendedEncoderLimits) {
		t.Fatal(err)
	}
}

func TestExtendedEncoderNewProfilesSOF1AndRoundTrip(t *testing.T) {
	for _, profile := range []struct {
		bits uint16
		pi   string
	}{{8, "MONOCHROME1"}, {12, "MONOCHROME2"}, {8, "RGB"}, {12, "RGB"}, {12, "YBR_FULL"}} {
		for _, quality := range []int{25, 50, 75, 90, 95, 100} {
			t.Run(fmt.Sprintf("%s%d-q%d", profile.pi, profile.bits, quality), func(t *testing.T) {
				m := extendedProfileMetadata(profile.bits, profile.pi)
				frame := extendedProfileFrame(m, 0)
				encoder, _ := NewExtendedEncoderWithOptions(ExtendedEncoderOptions{Quality: quality, AllowColorCompatibility: m.SamplesPerPixel == 3})
				encoded, err := encoder.EncodeFrame(context.Background(), frame, m)
				if err != nil {
					t.Fatal(err)
				}
				sof, err := jpegFrameSOFMarker(context.Background(), encoded.Data)
				if err != nil || sof != 0xc1 {
					t.Fatal("not SOF1", sof, err)
				}
				// Inspect header before entropy: 1x1 components, explicit precision,
				// and 8-bit quantization even at quality25 for Process2.
				for p := 2; p < len(encoded.Data); {
					marker := encoded.Data[p+1]
					length := int(binary.BigEndian.Uint16(encoded.Data[p+2:]))
					payload := encoded.Data[p+4 : p+2+length]
					if marker == 0xdb && profile.bits == 8 && payload[0]&0xf0 != 0 {
						t.Fatal("16-bit DQT at precision8")
					}
					if marker == 0xc1 {
						if payload[0] != byte(profile.bits) || payload[5] != byte(m.SamplesPerPixel) {
							t.Fatal("SOF precision/components", payload)
						}
						for c := 0; c < int(m.SamplesPerPixel); c++ {
							if payload[7+c*3] != 0x11 {
								t.Fatal("implicit subsampling")
							}
						}
					}
					if marker == 0xda {
						break
					}
					p += 2 + length
				}
				ctx := pixeldata.WithJPEGColorConversion(context.Background(), pixeldata.JPEGColorNever)
				dataset := extendedProfileObject(m, frame)
				dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true}, Value: core.FragmentSequence{Fragments: [][]byte{encoded.Data}}})
				decoded, err := New().DecodeContext(ctx, pixeldata.PixelData{Encapsulated: true, Sequence: core.FragmentSequence{Fragments: [][]byte{encoded.Data}}}, dataset)
				if err != nil {
					t.Fatal(err)
				}
				psnr, worst := extendedErrors(extendedProfileSamples(decoded.Data[0], m), extendedProfileSamples(frame, m), profile.bits)
				t.Logf("PSNR %.3f max %d", psnr, worst)
				floor := map[int]float64{25: 24, 50: 27, 75: 30, 90: 36, 95: 42, 100: 56}[quality]
				if profile.bits == 12 {
					floor = map[int]float64{25: 38, 50: 44, 75: 49, 90: 56, 95: 60, 100: 78}[quality]
				}
				if psnr < floor || quality == 100 && worst > 2 {
					t.Fatalf("PSNR %.3f/max%d", psnr, worst)
				}
				if m.SamplesPerPixel == 3 && (encoded.CompatibilityProfile != pixeldata.EncoderCompatibilityJPEGExtendedColor || encoded.PlanarConfiguration == nil || *encoded.PlanarConfiguration != 0 || decoded.PhotometricInterpretation != profile.pi) {
					t.Fatal("color contract", encoded.CompatibilityProfile, decoded.PhotometricInterpretation)
				}
			})
		}
	}
}

func TestExtendedEncoderColorRejectsInvalidMetadata(t *testing.T) {
	encoder, _ := NewExtendedEncoderWithOptions(ExtendedEncoderOptions{Quality: 75, AllowColorCompatibility: true, AllowLegacyReducedPrecision: true})
	for _, mutate := range []func(*pixeldata.Metadata){
		func(m *pixeldata.Metadata) { m.PlanarConfiguration = 1 }, func(m *pixeldata.Metadata) { m.PlanarConfigurationPresent = false },
		func(m *pixeldata.Metadata) { m.PixelRepresentation = 1 }, func(m *pixeldata.Metadata) { m.HighBit = 12 },
		func(m *pixeldata.Metadata) { m.BitsStored, m.HighBit = 10, 9 }, func(m *pixeldata.Metadata) { m.BitsAllocated = 32 },
		func(m *pixeldata.Metadata) { m.SamplesPerPixel = 4 }, func(m *pixeldata.Metadata) { m.PhotometricInterpretation = "YBR_FULL_422" },
		func(m *pixeldata.Metadata) { m.PhotometricInterpretation = "HSV" }, func(m *pixeldata.Metadata) { m.NumberOfFrames = 0 },
	} {
		m := extendedProfileMetadata(12, "RGB")
		mutate(&m)
		if _, err := encoder.EncodeFrame(context.Background(), nil, m); !errors.Is(err, pixeldata.ErrUnsupportedEncoderMetadata) {
			t.Fatal("invalid profile admitted", m, err)
		}
	}
	m := extendedProfileMetadata(8, "YBR_FULL")
	if _, err := encoder.EncodeFrame(context.Background(), nil, m); !errors.Is(err, pixeldata.ErrUnsupportedEncoderMetadata) {
		t.Fatal("YBR8 admitted", err)
	}
	m = extendedProfileMetadata(12, "RGB")
	if _, err := encoder.EncodeFrame(context.Background(), []byte{0}, m); !errors.Is(err, pixeldata.ErrPixelDataSizeMismatch) {
		t.Fatal("bad length", err)
	}
}

type extendedCancelChecks struct {
	context.Context
	left int
}

func (c *extendedCancelChecks) Err() error {
	c.left--
	if c.left <= 0 {
		return context.Canceled
	}
	return nil
}

func TestExtendedEncoderFiniteBudgetsAndCancellation(t *testing.T) {
	m := extendedProfileMetadata(12, "RGB")
	frame := extendedProfileFrame(m, 0)
	before := bytes.Clone(frame)
	for _, options := range []ExtendedEncoderOptions{{Quality: 75, AllowColorCompatibility: true, MaxWorkingBytes: 1}, {Quality: 75, AllowColorCompatibility: true, MaxEncodedFrameBytes: 1}} {
		encoder, _ := NewExtendedEncoderWithOptions(options)
		got, err := encoder.EncodeFrame(context.Background(), frame, m)
		if !errors.Is(err, ErrExtendedEncoderResourceLimit) || len(got.Data) != 0 {
			t.Fatal("budget bypassed", err)
		}
	}
	encoder, _ := NewExtendedEncoderWithOptions(ExtendedEncoderOptions{Quality: 75, AllowColorCompatibility: true})
	// These cancellation points span pre/post coefficient allocation, FDCT,
	// pre/post output allocation and entropy coding of this odd-sized raster.
	for _, checks := range []int{1, 2, 3, 4, 7, 8, 9, 10, 11, 12} {
		ctx := &extendedCancelChecks{Context: context.Background(), left: checks}
		got, err := encoder.EncodeFrame(ctx, frame, m)
		if !errors.Is(err, context.Canceled) || len(got.Data) != 0 {
			t.Fatalf("cancel check%d=%v", checks, err)
		}
	}
	if !bytes.Equal(before, frame) {
		t.Fatal("borrowed frame mutated")
	}
	// More than 1024 blocks reaches cancellation after entropy has already
	// emitted the first chunk, rather than only before entropy begins.
	m.Rows, m.Columns = 129, 193
	ctx := &extendedCancelChecks{Context: context.Background(), left: 24}
	if got, err := encoder.EncodeFrame(ctx, extendedProfileFrame(m, 0), m); !errors.Is(err, context.Canceled) || len(got.Data) != 0 {
		t.Fatal("mid-entropy cancellation", err)
	}
}

func TestExtendedEncoderTranscodeProfilesAndStoredYBRVerification(t *testing.T) {
	for _, pi := range []string{"RGB", "YBR_FULL"} {
		m := extendedProfileMetadata(12, pi)
		m.NumberOfFrames = 2
		raw := append(extendedProfileFrame(m, 0), extendedProfileFrame(m, 1)...)
		source := extendedProfileObject(m, raw)
		encoders := pixeldata.NewMemoryEncoderRegistry()
		if err := RegisterExtendedEncoderWithOptions(encoders, ExtendedEncoderOptions{Quality: 100, AllowColorCompatibility: true}); err != nil {
			t.Fatal(err)
		}
		decoders := pixeldata.NewMemoryRegistry()
		if err := Register(decoders); err != nil {
			t.Fatal(err)
		}
		options := pixeldata.TranscodeOptions{EncoderRegistry: encoders, DecoderRegistry: decoders}
		if out, _, err := pixeldata.TranscodeDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, transfer.JPEGExtended, options); out != nil || !errors.Is(err, pixeldata.ErrTranscodeLossyDisallowed) {
			t.Fatal("lossy gate", err)
		}
		options.AllowLossy = true
		limit := float64(2)
		options.MaxPixelError = &limit
		if pi == "YBR_FULL" {
			if out, _, err := pixeldata.TranscodeDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, transfer.JPEGExtended, options); out != nil || !errors.Is(err, pixeldata.ErrTranscodeComparisonUnsupported) {
				t.Fatal("Guess comparison must refuse changed PI", err)
			}
		}
		ctx := pixeldata.WithJPEGColorConversion(context.Background(), pixeldata.JPEGColorNever)
		out, report, err := pixeldata.TranscodeDataSet(ctx, source, transfer.ExplicitVRLittleEndian, transfer.JPEGExtended, options)
		if err != nil {
			t.Fatal(err)
		}
		if !report.Lossy || report.LossyVerification == nil || report.LossyVerification.Frames != 2 || !slices.Equal(report.EncodeCompatibilityProfiles, []pixeldata.EncoderCompatibilityProfile{pixeldata.EncoderCompatibilityJPEGExtendedColor}) {
			t.Fatal("report", report)
		}
		meta, err := pixeldata.ExtractMetadata(out)
		if err != nil || meta != m {
			t.Fatal("metadata changed", meta, m, err)
		}
		if lossy, _ := out.GetString(core.NewTag(0x0028, 0x2110)); lossy != "01" {
			t.Fatal("lossy attribute", lossy)
		}
		if method, _ := out.GetString(core.NewTag(0x0028, 0x2114)); method != jpegLossyMethod {
			t.Fatal("lossy method", method)
		}
	}
}

func TestExtendedEncoderStreamingCompatibilityAndYBRRefusal(t *testing.T) {
	for _, pi := range []string{"RGB", "YBR_FULL"} {
		t.Run(pi, func(t *testing.T) {
			m := extendedProfileMetadata(12, pi)
			m.NumberOfFrames = 2
			raw := append(extendedProfileFrame(m, 0), extendedProfileFrame(m, 1)...)
			dir := t.TempDir()
			input := filepath.Join(dir, "input.dcm")
			output := filepath.Join(dir, "output.dcm")
			writeExtendedTestFile(t, input, extendedProfileObject(m, raw), transfer.ExplicitVRLittleEndian)
			encoders := pixeldata.NewMemoryEncoderRegistry()
			if err := RegisterExtendedEncoderWithOptions(encoders, ExtendedEncoderOptions{Quality: 100, AllowColorCompatibility: true}); err != nil {
				t.Fatal(err)
			}
			decoders := pixeldata.NewMemoryRegistry()
			if err := Register(decoders); err != nil {
				t.Fatal(err)
			}
			limit := float64(2)
			options := pixeldata.TranscodeOptions{EncoderRegistry: encoders, DecoderRegistry: decoders, AllowLossy: true, MaxPixelError: &limit, Durability: pixeldata.TranscodeAtomic}
			if pi == "YBR_FULL" {
				if err := os.WriteFile(output, []byte("existing destination"), 0600); err != nil {
					t.Fatal(err)
				}
				_, err := pixeldata.TranscodePath(context.Background(), input, output, transfer.JPEGExtended, options)
				if !errors.Is(err, pixeldata.ErrTranscodeComparisonUnsupported) {
					t.Fatal("Guess must refuse", err)
				}
				got, err := os.ReadFile(output)
				if err != nil || string(got) != "existing destination" {
					t.Fatal("refusal changed destination", err)
				}
			}
			ctx := pixeldata.WithJPEGColorConversion(context.Background(), pixeldata.JPEGColorNever)
			report, err := pixeldata.TranscodePath(ctx, input, output, transfer.JPEGExtended, options)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(report.EncodeCompatibilityProfiles, []pixeldata.EncoderCompatibilityProfile{pixeldata.EncoderCompatibilityJPEGExtendedColor}) || report.LossyVerification == nil || report.LossyVerification.Frames != 2 {
				t.Fatal("stream report", report)
			}
			report.EncodeCompatibilityProfiles[0] = "caller changed"
			report, err = pixeldata.TranscodePath(ctx, input, output, transfer.JPEGExtended, options)
			if err != nil || report.EncodeCompatibilityProfiles[0] != pixeldata.EncoderCompatibilityJPEGExtendedColor {
				t.Fatal("report aliases encoder profile", err)
			}
			file, err := object.OpenFile(output)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			meta, err := pixeldata.ExtractMetadata(file.Dataset)
			if err != nil || meta != m || file.TransferSyntax.UID != transfer.JPEGExtended.UID {
				t.Fatal("stream metadata", meta, m, err)
			}
		})
	}
}

func TestExtendedEncoderExtremeSamplesAndAdmittedBudgets(t *testing.T) {
	for _, bits := range []uint16{8, 12} {
		for _, size := range [][2]uint16{{1, 1}, {1, 17}, {17, 1}, {17, 19}} {
			for _, pattern := range []string{"zero", "maximum", "alternating"} {
				m := extendedProfileMetadata(bits, "RGB")
				m.Rows, m.Columns = size[0], size[1]
				frame := make([]byte, int(m.Rows)*int(m.Columns)*3*m.BytesPerSample())
				peak := uint16((uint32(1) << bits) - 1)
				for sample := 0; sample < len(frame)/m.BytesPerSample(); sample++ {
					value := uint16(0)
					if pattern == "maximum" || pattern == "alternating" && sample%2 == 1 {
						value = peak
					}
					if bits == 8 {
						frame[sample] = byte(value)
					} else {
						binary.LittleEndian.PutUint16(frame[sample*2:], value)
					}
				}
				blocks := int64((m.Rows+7)/8) * int64((m.Columns+7)/8) * 3
				encodedBound := blocks*512 + 4096
				workingBound := int64(len(frame)) + blocks*128 + encodedBound + 65536
				for _, quality := range []int{1, 100} {
					encoder, err := NewExtendedEncoderWithOptions(ExtendedEncoderOptions{Quality: quality, AllowColorCompatibility: true, MaxWorkingBytes: workingBound, MaxEncodedFrameBytes: encodedBound})
					if err != nil {
						t.Fatal(err)
					}
					got, err := encoder.EncodeFrame(context.Background(), frame, m)
					if err != nil || int64(cap(got.Data)) > encodedBound {
						t.Fatal("admitted bound", bits, size, pattern, quality, err)
					}
					data := extendedProfileObject(m, frame)
					data.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true}, Value: core.FragmentSequence{Fragments: [][]byte{got.Data}}})
					ctx := pixeldata.WithJPEGColorConversion(context.Background(), pixeldata.JPEGColorNever)
					decoded, err := New().DecodeContext(ctx, pixeldata.PixelData{Encapsulated: true, Sequence: core.FragmentSequence{Fragments: [][]byte{got.Data}}}, data)
					if err != nil {
						t.Fatal(bits, size, pattern, quality, err)
					}
					if quality == 100 {
						_, worst := extendedErrors(extendedProfileSamples(decoded.Data[0], m), extendedProfileSamples(frame, m), bits)
						if worst > 2 {
							t.Fatal("extreme q100 error", worst)
						}
					}
				}
				for _, option := range []ExtendedEncoderOptions{{Quality: 75, AllowColorCompatibility: true, MaxWorkingBytes: workingBound - 1}, {Quality: 75, AllowColorCompatibility: true, MaxEncodedFrameBytes: encodedBound - 1}} {
					encoder, _ := NewExtendedEncoderWithOptions(option)
					if got, err := encoder.EncodeFrame(context.Background(), frame, m); !errors.Is(err, ErrExtendedEncoderResourceLimit) || len(got.Data) != 0 {
						t.Fatal("boundary budget", got, err)
					}
				}
			}
		}
	}
}

func TestExtendedEncoderConcurrentSelectedProfiles(t *testing.T) {
	encoder, err := NewExtendedEncoderWithOptions(ExtendedEncoderOptions{Quality: 90, AllowColorCompatibility: true, AllowLegacyReducedPrecision: true})
	if err != nil {
		t.Fatal(err)
	}
	type reference struct {
		meta    pixeldata.Metadata
		raw     []byte
		encoded pixeldata.EncodedFrame
	}
	var references []reference
	for _, profile := range []struct {
		bits uint16
		pi   string
	}{{8, "MONOCHROME2"}, {9, "MONOCHROME1"}, {12, "MONOCHROME2"}, {8, "RGB"}, {12, "RGB"}, {12, "YBR_FULL"}} {
		m := extendedProfileMetadata(profile.bits, profile.pi)
		raw := extendedProfileFrame(m, 0)
		got, err := encoder.EncodeFrame(context.Background(), raw, m)
		if err != nil {
			t.Fatal(err)
		}
		references = append(references, reference{m, raw, got})
	}
	var wait sync.WaitGroup
	for range 4 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 3 {
				for _, want := range references {
					got, err := encoder.EncodeFrame(context.Background(), want.raw, want.meta)
					if err != nil || got.CompatibilityProfile != want.encoded.CompatibilityProfile || got.PhotometricInterpretation != want.encoded.PhotometricInterpretation || !bytes.Equal(got.Data, want.encoded.Data) {
						t.Errorf("concurrent profile %s/%d: %v", want.meta.PhotometricInterpretation, want.meta.BitsStored, err)
					}
					caps := encoder.Capabilities()
					if len(caps.CompatibilityProfiles) > 0 {
						caps.CompatibilityProfiles[0] = "caller mutation"
					}
				}
			}
		}()
	}
	wait.Wait()
}

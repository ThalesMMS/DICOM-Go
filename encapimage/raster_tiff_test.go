package encapimage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
)

type ownTIFFPage struct {
	w, h, samples, bits, orientation, planar, photo int
	values                                          []uint16
	extra                                           map[uint16][]uint32
}

// This fixture writer encodes the public TIFF field contract directly. Expected
// samples below are independent finite rasters, not roundtrips of a decoder.
func ownTIFF(order binary.ByteOrder, pages ...ownTIFFPage) []byte {
	b := make([]byte, 8)
	copy(b, "II")
	if order == binary.BigEndian {
		copy(b, "MM")
	}
	order.PutUint16(b[2:], 42)
	link := 4
	for _, p := range pages {
		ifd := len(b)
		order.PutUint32(b[link:], uint32(ifd))
		tags := map[uint16][]uint32{256: {uint32(p.w)}, 257: {uint32(p.h)}, 259: {1}, 262: {uint32(p.photo)}, 274: {uint32(p.orientation)}, 277: {uint32(p.samples)}, 278: {1}, 284: {uint32(p.planar)}}
		for c := 0; c < p.samples; c++ {
			tags[258] = append(tags[258], uint32(p.bits))
		}
		strips := p.h
		components := p.samples
		if p.planar == 2 {
			strips *= p.samples
			components = 1
		}
		tags[273], tags[279] = make([]uint32, strips), make([]uint32, strips)
		for tag, v := range p.extra {
			tags[tag] = v
		}
		keys := make([]uint16, 0, len(tags))
		for tag := uint16(1); tag != 0; tag++ {
			if _, exists := tags[tag]; exists {
				keys = append(keys, tag)
			}
		}
		b = append(b, make([]byte, 2+12*len(keys)+4)...)
		order.PutUint16(b[ifd:], uint16(len(keys)))
		link = ifd + 2 + 12*len(keys)
		for strip := 0; strip < strips; strip++ {
			tags[273][strip] = uint32(len(b))
			tags[279][strip] = uint32(p.w * components * p.bits / 8)
			for x := 0; x < p.w; x++ {
				for c := 0; c < components; c++ {
					y, sourceC := strip, c
					if p.planar == 2 {
						y, sourceC = strip%p.h, strip/p.h
					}
					v := p.values[(y*p.w+x)*p.samples+sourceC]
					if p.bits == 8 {
						b = append(b, byte(v))
					} else {
						var sample [2]byte
						order.PutUint16(sample[:], v)
						b = append(b, sample[:]...)
					}
				}
			}
		}
		for i, tag := range keys {
			at := ifd + 2 + i*12
			values := tags[tag]
			order.PutUint16(b[at:], tag)
			typ, width := uint16(3), 2
			if tag == 254 || tag == 256 || tag == 257 || tag == 273 || tag == 278 || tag == 279 {
				typ, width = 4, 4
			}
			order.PutUint16(b[at+2:], typ)
			order.PutUint32(b[at+4:], uint32(len(values)))
			start := at + 8
			if len(values)*width > 4 {
				order.PutUint32(b[at+8:], uint32(len(b)))
				start = len(b)
				b = append(b, make([]byte, len(values)*width)...)
			}
			for j, v := range values {
				if width == 2 {
					order.PutUint16(b[start+j*width:], uint16(v))
				} else {
					order.PutUint32(b[start+j*width:], v)
				}
			}
		}
	}
	return b
}

func rasterSamples(page RasterPage) []uint16 {
	out := make([]uint16, len(page.Data)/(page.BitsStored/8))
	for i := range out {
		out[i] = uint16(page.Data[i])
		if page.BitsStored == 16 {
			out[i] = binary.LittleEndian.Uint16(page.Data[i*2:])
		}
	}
	return out
}

func TestRasterTIFFOrientationEndianPlanarAndExact16BitSC(t *testing.T) {
	wants := [][]uint16{{1, 2, 3, 4, 5, 6}, {2, 1, 4, 3, 6, 5}, {6, 5, 4, 3, 2, 1}, {5, 6, 3, 4, 1, 2}, {1, 3, 5, 2, 4, 6}, {5, 3, 1, 6, 4, 2}, {6, 4, 2, 5, 3, 1}, {2, 4, 6, 1, 3, 5}}
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		for orientation := 1; orientation <= 8; orientation++ {
			data := ownTIFF(order, ownTIFFPage{w: 2, h: 3, samples: 1, bits: 16, orientation: orientation, planar: 1, photo: 1, values: []uint16{1, 2, 3, 4, 5, 6}})
			page, err := DecodeRasterPage(context.Background(), data, 1, RasterAll, ImportLimits{})
			if err != nil || fmt.Sprint(rasterSamples(page)) != fmt.Sprint(wants[orientation-1]) {
				t.Fatalf("orientation %d/%T: %+v, %v", orientation, order, page, err)
			}
			wantRows, wantCols := 3, 2
			if orientation >= 5 {
				wantRows, wantCols = 2, 3
			}
			if page.Rows != wantRows || page.Columns != wantCols {
				t.Fatalf("rotated dimensions %+v", page)
			}
		}
		for _, planar := range []int{1, 2} {
			values := []uint16{0, 1, 65535, 32768, 12345, 255, 65534, 256, 42, 7, 4095, 8192}
			data := ownTIFF(order, ownTIFFPage{w: 2, h: 2, samples: 3, bits: 16, orientation: 1, planar: planar, photo: 2, values: values})
			page, err := DecodeRasterPage(context.Background(), data, 1, RasterAll, ImportLimits{})
			if err != nil || fmt.Sprint(rasterSamples(page)) != fmt.Sprint(values) {
				t.Fatalf("RGB16 %T planar%d: %v", order, planar, err)
			}
			f, report, err := BuildRasterPage(nil, page, ImageOptions{})
			if err != nil || report.BitsStored != 16 || getString(f.Dataset, tagSOPClassUID) != SecondaryCaptureImageStorage {
				t.Fatalf("RGB16 SC: %+v %v", report, err)
			}
			read := part10Roundtrip(t, f)
			if !bytes.Equal(nativeBytes(t, read), page.Data) || getUS(t, read.Dataset, tagBitsStored) != 16 {
				t.Fatal("SC readback changed exact samples")
			}
			if imageType, _ := read.Dataset.GetStrings(core.NewTag(0x0008, 0x0008)); fmt.Sprint(imageType) != "[DERIVED SECONDARY]" {
				t.Fatal("import claims original acquisition")
			}
			for _, channel := range []RasterChannel{RasterRed, RasterGreen, RasterBlue} {
				mono, err := DecodeRasterPage(nil, data, 1, channel, ImportLimits{})
				if err != nil {
					t.Fatal(err)
				}
				component := map[RasterChannel]int{RasterRed: 0, RasterGreen: 1, RasterBlue: 2}[channel]
				got := rasterSamples(mono)
				for i := range got {
					if got[i] != values[i*3+component] {
						t.Fatal("selected component changed")
					}
				}
			}
		}
	}
}

func TestRasterTIFFPagesWhiteIsZeroPreviewAndRefusals(t *testing.T) {
	first := ownTIFFPage{w: 2, h: 1, samples: 1, bits: 16, orientation: 1, planar: 1, photo: 0, values: []uint16{0, 65535}}
	second := ownTIFFPage{w: 1, h: 3, samples: 1, bits: 8, orientation: 1, planar: 1, photo: 1, values: []uint16{0, 128, 255}}
	data := ownTIFF(binary.BigEndian, first, second)
	info, err := InspectRaster(nil, data, ImportLimits{})
	if err != nil || len(info.Pages) != 2 || info.Pages[1].Rows != 3 || info.Pages[0].BitsStored != 16 {
		t.Fatalf("inspection %+v %v", info, err)
	}
	page, err := DecodeRasterPage(nil, data, 1, RasterAll, ImportLimits{})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := page.Preview(nil, 32)
	if err != nil {
		t.Fatal(err)
	}
	r, _, _, _ := preview.At(0, 0).RGBA()
	if r != 65535 {
		t.Fatal("white-is-zero preview was inverted incorrectly")
	}
	f, _, err := BuildRasterPage(nil, page, ImageOptions{})
	if err != nil || getString(f.Dataset, tagPhotometric) != "MONOCHROME1" || !bytes.Equal(nativeBytes(t, part10Roundtrip(t, f)), page.Data) {
		t.Fatalf("white-is-zero SC %v", err)
	}
	for _, tc := range []struct {
		tag   uint16
		value uint32
	}{{259, 5}, {317, 2}, {338, 2}, {34675, 99}, {324, 1}, {339, 3}, {262, 3}, {274, 9}} {
		input := first
		input.extra = map[uint16][]uint32{tc.tag: {tc.value}}
		if _, err := InspectRaster(nil, ownTIFF(binary.LittleEndian, input), ImportLimits{}); err == nil {
			t.Fatalf("unsupported/malformed tag %d accepted", tc.tag)
		}
	}
	for _, limits := range []ImportLimits{{MaxInputBytes: 8}, {MaxPixels: 1}, {MaxDecodedBytes: 1}, {MaxFrames: 1}} {
		if _, err := InspectRaster(nil, data, limits); !errors.Is(err, ErrImportLimit) {
			t.Fatalf("limit %+v: %v", limits, err)
		}
	}
	cycle := append([]byte(nil), data...)
	ifd := binary.BigEndian.Uint32(cycle[4:])
	count := binary.BigEndian.Uint16(cycle[ifd:])
	binary.BigEndian.PutUint32(cycle[int(ifd)+2+int(count)*12:], ifd)
	if _, err := InspectRaster(nil, cycle, ImportLimits{}); !errors.Is(err, ErrMalformedImage) {
		t.Fatalf("cyclic directory accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if page, err := DecodeRasterPage(ctx, data, 1, RasterAll, ImportLimits{}); !errors.Is(err, context.Canceled) || len(page.Data) != 0 {
		t.Fatalf("canceled decode published result: %+v %v", page, err)
	}
}

func TestRasterCancellationConcurrentImmutableAndPNGPrecision(t *testing.T) {
	data := ownTIFF(binary.BigEndian, ownTIFFPage{w: 2, h: 3, samples: 1, bits: 16, orientation: 1, planar: 1, photo: 1, values: []uint16{0, 1, 255, 256, 32768, 65535}})
	before := bytes.Clone(data)
	probe := &scriptedContext{cancelAt: 100000}
	if _, err := DecodeRasterPage(probe, data, 1, RasterAll, ImportLimits{}); err != nil {
		t.Fatal(err)
	}
	for checkpoint := 1; checkpoint <= probe.calls; checkpoint++ {
		ctx := &scriptedContext{cancelAt: checkpoint}
		page, err := DecodeRasterPage(ctx, data, 1, RasterAll, ImportLimits{})
		if !errors.Is(err, context.Canceled) || len(page.Data) != 0 || ctx.calls > checkpoint+1 {
			t.Fatalf("checkpoint%d result %+v err%v calls%d", checkpoint, page, err, ctx.calls)
		}
	}
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() { _, err := DecodeRasterPage(nil, data, 1, RasterAll, ImportLimits{}); done <- err }()
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(data, before) {
		t.Fatal("input mutated")
	}
	img := image.NewGray16(image.Rect(0, 0, 2, 1))
	img.SetGray16(0, 0, color.Gray16{Y: 1})
	img.SetGray16(1, 0, color.Gray16{Y: 65535})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	page, err := DecodeRasterPage(nil, encoded.Bytes(), 1, RasterAll, ImportLimits{})
	if err != nil || page.BitsStored != 16 || fmt.Sprint(rasterSamples(page)) != "[1 65535]" {
		t.Fatalf("PNG precision %+v %v", page, err)
	}
}

func TestRasterTIFFLibTIFFQualification(t *testing.T) {
	if os.Getenv("DICOMGO_TIFF_INTEROP") != "1" {
		t.Skip("set DICOMGO_TIFF_INTEROP=1 with tiffcp/tiffinfo on PATH")
	}
	root := os.Getenv("DICOMGO_TIFF_EVIDENCE_DIR")
	if root == "" {
		root = t.TempDir()
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, bits := range []int{8, 16} {
		for _, samples := range []int{1, 3} {
			for _, endian := range []string{"-L", "-B"} {
				for _, planar := range []string{"contig", "separate"} {
					name := fmt.Sprintf("samples%d-bits%d-%s-%s", samples, bits, endian[1:], planar)
					values := []uint16{0, 1, 255, 256, 32768, 65535}
					if bits == 8 {
						values = []uint16{0, 1, 127, 128, 254, 255}
					}
					var pixels []uint16
					for i := range values {
						for c := 0; c < samples; c++ {
							pixels = append(pixels, values[(i+c)%len(values)])
						}
					}
					photo := 1
					if samples == 3 {
						photo = 2
					}
					input := filepath.Join(root, name+"-own.tif")
					output := filepath.Join(root, name+"-libtiff.tif")
					inputPlanar := 1
					if planar == "separate" {
						inputPlanar = 2
					}
					// libtiff tiffcp's contig→separate converter refuses 16 bits;
					// preserve each input layout while independently rewriting it.
					page := ownTIFFPage{w: 2, h: 3, samples: samples, bits: bits, orientation: 1, planar: inputPlanar, photo: photo, values: pixels}
					if err := os.WriteFile(input, ownTIFF(binary.LittleEndian, page, page), 0600); err != nil {
						t.Fatal(err)
					}
					args := []string{"-s", "-c", "none", "-r", "1", endian, "-p", planar, input, output}
					log, err := exec.Command("tiffcp", args...).CombinedOutput()
					if err != nil {
						t.Fatalf("tiffcp %s: %v %s", name, err, log)
					}
					log, err = exec.Command("tiffinfo", "-D", "-d", "-w", output).CombinedOutput()
					if err != nil {
						t.Fatalf("independent tiffinfo %s: %v %s", name, err, log)
					}
					if err := os.WriteFile(filepath.Join(root, name+"-independent.log"), log, 0600); err != nil {
						t.Fatal(err)
					}
					checkIndependentTIFFSamples(t, log, pixels, samples, bits, inputPlanar)
					encoded, err := os.ReadFile(output)
					if err != nil {
						t.Fatal(err)
					}
					for page := 1; page <= 2; page++ {
						decoded, err := DecodeRasterPage(nil, encoded, page, RasterAll, ImportLimits{})
						if err != nil || fmt.Sprint(rasterSamples(decoded)) != fmt.Sprint(pixels) {
							t.Fatalf("libtiff written %s page%d samples %+v err%v", name, page, decoded, err)
						}
						f, _, err := BuildRasterPage(nil, decoded, ImageOptions{})
						if err != nil || !bytes.Equal(nativeBytes(t, part10Roundtrip(t, f))[:len(decoded.Data)], decoded.Data) {
							t.Fatalf("independent TIFF→SC readback %s: %v", name, err)
						}
					}
					t.Logf("%s: 2 libtiff-written pages, exact sample and IOD readback", name)
				}
			}
		}
	}
}

// tiffinfo prints TIFFReadEncodedStrip output after libtiff's native-order sample
// conversion. Compare those independently decoded bytes to the known raster,
// rather than treating successful TIFF parsing as a sample oracle.
func checkIndependentTIFFSamples(t *testing.T, log []byte, pixels []uint16, samples, bits, planar int) {
	t.Helper()
	directory, strip := -1, -1
	strips := map[[2]int][]byte{}
	scanner := bufio.NewScanner(bytes.NewReader(log))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "=== TIFF directory ") {
			if _, err := fmt.Sscanf(line, "=== TIFF directory %d ===", &directory); err != nil {
				t.Fatal(err)
			}
			strip = -1
		} else if strings.HasPrefix(line, "Strip ") {
			if _, err := fmt.Sscanf(line, "Strip %d:", &strip); err != nil {
				t.Fatal(err)
			}
		} else if strip >= 0 {
			for _, word := range strings.Fields(line) {
				if len(word) != 2 {
					t.Fatalf("unexpected independent sample line %q", line)
				}
				value, err := strconv.ParseUint(word, 16, 8)
				if err != nil {
					t.Fatal(err)
				}
				key := [2]int{directory, strip}
				strips[key] = append(strips[key], byte(value))
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	for directory := 0; directory < 2; directory++ {
		for y := 0; y < 3; y++ {
			planes, components := 1, samples
			if planar == 2 {
				planes, components = samples, 1
			}
			for plane := 0; plane < planes; plane++ {
				var want []byte
				for x := 0; x < 2; x++ {
					for c := 0; c < components; c++ {
						value := pixels[(y*2+x)*samples+plane+c]
						if bits == 8 {
							want = append(want, byte(value))
						} else {
							var native [2]byte
							binary.NativeEndian.PutUint16(native[:], value)
							want = append(want, native[:]...)
						}
					}
				}
				key := [2]int{directory, plane*3 + y}
				if !bytes.Equal(strips[key], want) {
					t.Fatalf("independent decoder samples directory%d strip%d =%x want%x", key[0], key[1], strips[key], want)
				}
				delete(strips, key)
			}
		}
	}
	if len(strips) != 0 {
		t.Fatal("independent decoder published extra strips")
	}
}

func FuzzRasterTIFFAdmission(f *testing.F) {
	f.Add(ownTIFF(binary.LittleEndian, ownTIFFPage{w: 2, h: 1, samples: 1, bits: 16, orientation: 1, planar: 1, photo: 1, values: []uint16{0, 65535}}))
	f.Add([]byte("II*\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		limits := ImportLimits{MaxInputBytes: 1 << 20, MaxPixels: 4096, MaxDecodedBytes: 32768, MaxFrames: 4}
		if !isTIFF(data) {
			return
		}
		info, err := InspectRaster(nil, data, limits)
		if err != nil {
			if len(info.Pages) != 0 {
				t.Fatal("failed admission publishes pages")
			}
			return
		}
		for _, page := range info.Pages {
			decoded, err := DecodeRasterPage(nil, data, page.Number, RasterAll, limits)
			if err != nil || len(decoded.Data) != decoded.Rows*decoded.Columns*decoded.Samples*decoded.BitsStored/8 {
				t.Fatalf("admitted TIFF cannot decode: %v", err)
			}
		}
	})
}

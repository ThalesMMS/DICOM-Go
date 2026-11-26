package pixeldata_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"reflect"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func intoFixture(t testing.TB, bits, samples, planar int, target transfer.Syntax, rows, cols, frames int) *pixeldata.FrameReader {
	t.Helper()
	options := streamTestRegistries(t)
	obj := streamTestSource(rows, cols, frames, samples, bits)
	// The RLE encoder accepts interleaved input only. Its component byte-plane
	// codestream can also be decoded from a compressed object declaring planar
	// configuration 1; the existing decoder still returns interleaved pixels.
	// Exercise that decoder contract without requiring a planar-input encoder.
	rlePlanarHeader := samples == 3 && planar == 1 && target.UID == transfer.RLELossless.UID
	encodedPlanar := planar
	if rlePlanarHeader {
		encodedPlanar = 0
	}
	if samples == 3 {
		obj.Put(core.NewRawElement(core.NewTag(0x0028, 0x0006), core.VRUS, []byte{byte(encodedPlanar), 0}))
	}
	file := &object.File{Dataset: obj, TransferSyntax: transfer.ExplicitVRLittleEndian}
	if target.UID != transfer.ExplicitVRLittleEndian.UID {
		var err error
		file, _, err = pixeldata.TranscodeFile(context.Background(), file, target, options)
		if err != nil {
			t.Fatal(err)
		}
	}
	if rlePlanarHeader {
		file.Dataset.Put(core.NewRawElement(core.NewTag(0x0028, 0x0006), core.VRUS, []byte{1, 0}))
	}
	r, err := pixeldata.NewFrameReader(context.Background(), file, nil, pixeldata.FrameReaderOptions{Registry: options.DecoderRegistry})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFrameReaderIntoMatchesAllocated(t *testing.T) {
	for _, bits := range []int{8, 16} {
		for _, shape := range []struct {
			name            string
			samples, planar int
		}{{"mono", 1, 0}, {"rgb", 3, 0}, {"planar", 3, 1}} {
			targets := []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.RLELossless}
			if shape.planar == 0 {
				targets = append(targets, transfer.JPEGLSLossless)
			}
			for _, target := range targets {
				t.Run(shape.name+"/"+target.Name+"/"+string(rune('A'+bits)), func(t *testing.T) {
					r := intoFixture(t, bits, shape.samples, shape.planar, target, 9, 13, 3)
					rlePlanarHeader := shape.planar == 1 && target.UID == transfer.RLELossless.UID
					if rlePlanarHeader && r.Metadata().PlanarConfiguration != 1 {
						t.Fatal("RLE decoder fixture lost planar-1 compressed header")
					}
					mode := pixeldata.BufferNativeDirect
					regionMode := mode
					switch target.UID {
					case transfer.RLELossless.UID:
						mode, regionMode = pixeldata.BufferCodecDirect, pixeldata.BufferCodecDirect
					case transfer.JPEGLSLossless.UID:
						mode, regionMode = pixeldata.BufferCodecDirect, pixeldata.BufferFrameRegionCopy
						if shape.samples == 1 {
							regionMode = pixeldata.BufferCodecDirect
						}
					}
					for i := 0; i < 3; i++ {
						want, m, err := r.Frame(context.Background(), i)
						if err != nil {
							t.Fatal(err)
						}
						if rlePlanarHeader && m.PlanarConfiguration != 0 {
							t.Fatal("RLE decoded output must preserve existing interleaved contract")
						}
						dst := bytes.Repeat([]byte{0xe7}, len(want)+7)
						got, err := r.FrameInto(context.Background(), i, dst)
						if err != nil || got.Written != len(want) || got.Layout.Mode != mode || !reflect.DeepEqual(got.Layout.Metadata, m) || !bytes.Equal(dst[:len(want)], want) || !bytes.Equal(dst[len(want):], bytes.Repeat([]byte{0xe7}, 7)) {
							t.Fatalf("frame %d: result=%+v error=%v", i, got, err)
						}
						layout, err := r.FrameBufferLayout(nil, i)
						if err != nil || layout != got.Layout {
							t.Fatalf("layout query %+v: %v", layout, err)
						}
						for _, rect := range []image.Rectangle{image.Rect(0, 0, 13, 9), image.Rect(1, 2, 5, 7), image.Rect(10, 8, 13, 9), image.Rect(0, 3, 13, 4)} {
							want, m, err := r.Region(context.Background(), i, rect)
							if err != nil {
								t.Fatal(err)
							}
							dst := bytes.Repeat([]byte{0xda}, len(want)+5)
							result, err := r.RegionInto(context.Background(), i, rect, dst)
							layout, queryErr := r.RegionBufferLayout(nil, i, rect)
							if regionMode == pixeldata.BufferFrameRegionCopy {
								if !errors.Is(queryErr, pixeldata.ErrBufferLayoutUnavailable) {
									t.Fatalf("fallback layout: %v", queryErr)
								}
							} else if queryErr != nil || layout != result.Layout {
								t.Fatalf("region requirements %+v: %v", layout, queryErr)
							}
							if err != nil || result.Written != len(want) || result.Layout.Mode != regionMode || result.Layout.Metadata != m || !bytes.Equal(dst[:len(want)], want) || !bytes.Equal(dst[len(want):], bytes.Repeat([]byte{0xda}, 5)) {
								t.Fatalf("region %v: %+v, %v", rect, result, err)
							}
						}
					}
				})
			}
		}
	}
}

func TestFrameReaderIntoPreflightPreservesDestination(t *testing.T) {
	for _, target := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.RLELossless, transfer.JPEGLSLossless} {
		r := intoFixture(t, 16, 1, 0, target, 9, 13, 2)
		size := int(r.Metadata().FrameSize())
		for _, tc := range []struct {
			name string
			run  func([]byte) (pixeldata.BufferResult, error)
			want error
		}{
			{"short", func(d []byte) (pixeldata.BufferResult, error) {
				return r.FrameInto(context.Background(), 0, d[:size-1])
			}, pixeldata.ErrBufferTooSmall},
			{"invalid-index", func(d []byte) (pixeldata.BufferResult, error) { return r.FrameInto(context.Background(), 2, d) }, pixeldata.ErrInvalidMetadata},
			{"invalid-region", func(d []byte) (pixeldata.BufferResult, error) {
				return r.RegionInto(context.Background(), 0, image.Rect(12, 8, 15, 9), d)
			}, pixeldata.ErrInvalidRegion},
			{"empty-region", func(d []byte) (pixeldata.BufferResult, error) {
				return r.RegionInto(context.Background(), 0, image.Rectangle{}, d)
			}, pixeldata.ErrInvalidRegion},
			{"cancel", func(d []byte) (pixeldata.BufferResult, error) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return r.FrameInto(ctx, 0, d)
			}, context.Canceled},
		} {
			t.Run(target.Name+"/"+tc.name, func(t *testing.T) {
				dst := bytes.Repeat([]byte{0xc3}, size+19)
				before := bytes.Clone(dst)
				got, err := tc.run(dst)
				if !errors.Is(err, tc.want) || got.Written != 0 || !bytes.Equal(dst, before) {
					t.Fatalf("%+v err=%v changed=%v", got, err, !bytes.Equal(dst, before))
				}
				if tc.name == "short" {
					var e *pixeldata.BufferSizeError
					if !errors.As(err, &e) || e.Required != int64(size) || e.Provided != int64(size-1) {
						t.Fatalf("size error %v", err)
					}
				}
			})
		}
	}
}

func TestFrameReaderIntoConcurrentOwnership(t *testing.T) {
	for _, target := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.RLELossless, transfer.JPEGLSLossless} {
		r := intoFixture(t, 16, 1, 0, target, 9, 13, 4)
		wants := make([][]byte, 4)
		for i := range wants {
			var err error
			wants[i], _, err = r.Frame(nil, i)
			if err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		for worker := 0; worker < 8; worker++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				dst := make([]byte, len(wants[0])+11)
				for turn := 0; turn < 20; turn++ {
					i := (worker + turn) % 4
					clear(dst)
					got, err := r.FrameInto(nil, i, dst)
					if err != nil || got.Written != len(wants[i]) || !bytes.Equal(dst[:got.Written], wants[i]) {
						t.Errorf("worker %d: %+v %v", worker, got, err)
						return
					}
					// Mutating the borrowed output cannot alter the source or another output.
					for at := range dst {
						dst[at] = 0xad
					}
				}
			}(worker)
		}
		wg.Wait()
		got, _, err := r.Frame(nil, 0)
		if err != nil || !bytes.Equal(got, wants[0]) {
			t.Fatalf("retained destination alias: %v", err)
		}
	}
}

func TestFrameReaderIntoRejectsSubsampledRegion(t *testing.T) {
	obj := streamTestSource(4, 6, 1, 3, 8)
	obj.Put(core.NewRawElement(core.NewTag(0x0028, 0x0004), core.VRCS, []byte("YBR_FULL_422")))
	// Native 4:2:2 stores two bytes per pixel; use the actual stored footprint.
	obj.Put(core.NewRawElement(core.TagPixelData, core.VROB, make([]byte, 4*6*2)))
	r, err := pixeldata.NewFrameReader(nil, &object.File{Dataset: obj, TransferSyntax: transfer.ExplicitVRLittleEndian}, nil, pixeldata.FrameReaderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dst := bytes.Repeat([]byte{3}, 64)
	before := bytes.Clone(dst)
	got, err := r.RegionInto(nil, 0, image.Rect(0, 0, 2, 2), dst)
	if !errors.Is(err, pixeldata.ErrFrameLayout) || got.Written != 0 || !bytes.Equal(dst, before) {
		t.Fatalf("%+v %v", got, err)
	}
	frame, _, err := r.Frame(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err = r.FrameInto(nil, 0, dst)
	if err != nil || got.Written != len(frame) || !bytes.Equal(dst[:len(frame)], frame) {
		t.Fatalf("whole 422: %+v %v", got, err)
	}
}

func TestFrameReaderIntoDeferredRegions(t *testing.T) {
	options := streamTestRegistries(t)
	for _, target := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.RLELossless, transfer.JPEGLSLossless} {
		indexed := intoFixture(t, 16, 1, 0, target, 9, 13, 3)
		path := writeStreamTestFile(t, t.TempDir(), "caller-buffer.dcm", indexed.File())
		deferred, err := pixeldata.OpenFrameReader(nil, path, pixeldata.FrameReaderOptions{Registry: options.DecoderRegistry})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			rect := image.Rect(10, 8, 13, 9)
			want, m, err := indexed.Region(nil, i, rect)
			if err != nil {
				t.Fatal(err)
			}
			dst := make([]byte, len(want))
			got, err := deferred.RegionInto(nil, i, rect, dst)
			if err != nil || got.Written != len(want) || got.Layout.Metadata != m || !bytes.Equal(dst, want) {
				t.Fatalf("deferred %s: %+v %v", target.Name, got, err)
			}
		}
		if err := deferred.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFrameReaderIntoRLEWholeFrameAdmission(t *testing.T) {
	options := streamTestRegistries(t)
	for _, tc := range []struct {
		name    string
		segment []byte
	}{{"repeat-overrun", []byte{0xfd, 7}}, {"literal-overrun", []byte{3, 7, 7, 7, 7}}, {"short-literal", []byte{0, 7}}} {
		obj := streamTestSource(1, 2, 1, 1, 8)
		header := make([]byte, 64)
		header[0] = 1
		header[4] = 64
		encoded := append(header, tc.segment...)
		obj.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true}, Value: core.FragmentSequence{Fragments: [][]byte{encoded}}})
		r, err := pixeldata.NewFrameReader(nil, &object.File{Dataset: obj, TransferSyntax: transfer.RLELossless}, nil, pixeldata.FrameReaderOptions{Registry: options.DecoderRegistry})
		if err != nil {
			t.Fatal(err)
		}
		for _, tolerant := range []bool{false, true} {
			ctx := pixeldata.DecodeContextWithRecovery(context.Background(), pixeldata.DecodeRecovery{RLESegmentOverrun: tolerant})
			old, m, oldErr := r.Frame(ctx, 0)
			dst := bytes.Repeat([]byte{0xcd}, 9)
			before := bytes.Clone(dst)
			result, newErr := r.FrameInto(ctx, 0, dst)
			if !tolerant {
				if oldErr == nil || newErr == nil || result.Written != 2 || !bytes.Equal(dst[2:], before[2:]) {
					t.Fatalf("%s strict full old%v new%v result%+v", tc.name, oldErr, newErr, result)
				}
			} else if oldErr != nil || newErr != nil || result.Written != 2 || result.Layout.Metadata != m || !bytes.Equal(dst[:2], old) || len(result.Diagnostics) != 1 || result.Diagnostics[0] != pixeldata.DiagnosticRLESegmentOverrun || !bytes.Equal(dst[2:], before[2:]) {
				t.Fatalf("%s recovered full old%v new%v result%+v", tc.name, oldErr, newErr, result)
			}
			for _, rect := range []image.Rectangle{image.Rect(0, 0, 2, 1), image.Rect(0, 0, 1, 1)} {
				old, m, oldErr := r.Region(ctx, 0, rect)
				dst := bytes.Repeat([]byte{0xcd}, 9)
				result, newErr := r.RegionInto(ctx, 0, rect, dst)
				if oldErr != nil {
					if newErr == nil {
						t.Fatalf("%s region%v weakened error", tc.name, rect)
					}
				} else if newErr != nil || result.Layout.Metadata != m || result.Written != len(old) || !bytes.Equal(dst[:len(old)], old) || !bytes.Equal(dst[len(old):], bytes.Repeat([]byte{0xcd}, 9-len(old))) {
					t.Fatalf("%s region%v old%v new%v result%+v", tc.name, rect, oldErr, newErr, result)
				}
				if tc.name != "short-literal" && len(result.Diagnostics) != 0 {
					t.Fatalf("region clipping introduced full-frame diagnostic: %+v", result)
				}
			}
		}
	}
}

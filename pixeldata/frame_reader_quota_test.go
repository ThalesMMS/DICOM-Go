package pixeldata

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type quotaReadAt struct {
	io.ReaderAt
	calls int
	bytes int
}

func (s *quotaReadAt) ReadAt(p []byte, offset int64) (int, error) {
	s.calls++
	s.bytes += len(p)
	return s.ReaderAt.ReadAt(p, offset)
}

func quotaFrameSource(t *testing.T, file *object.File, deferred bool) (*object.File, *quotaReadAt) {
	t.Helper()
	if !deferred {
		return file, nil
	}
	if err := file.Dataset.SetStrings(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"); err != nil {
		t.Fatal(err)
	}
	if err := file.Dataset.SetStrings(core.NewTag(0x0008, 0x0018), core.VRUI, "2.25.81231"); err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	if err := object.WriteFile(&wire, file); err != nil {
		t.Fatal(err)
	}
	source := bytes.NewReader(wire.Bytes())
	parsed, err := object.ReadFileWithOptions(source, object.ReadFileOptions{DeferPixelData: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parsed.Close() })
	return parsed, &quotaReadAt{ReaderAt: source}
}

func assertFrameQuota(t *testing.T, err error, name string, actual int64, maximum int64) {
	t.Helper()
	var limit *DecompressResourceLimitError
	if !errors.Is(err, ErrDecompressResourceLimit) || !errors.As(err, &limit) ||
		limit.Limit != name || limit.Actual != actual || limit.Maximum != maximum {
		t.Fatalf("error = %v; want %s (%d > %d)", err, name, actual, maximum)
	}
}

func TestFrameReaderNativeReadLimits(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%t", deferred), func(t *testing.T) {
			for _, tc := range []struct {
				name   string
				limits DecompressLimits
			}{
				{"MaxNativeBytes", DecompressLimits{MaxNativeBytes: 1}},
				{"MaxInputBytes", DecompressLimits{MaxInputBytes: 1}},
				{"MaxPixels", DecompressLimits{MaxPixels: 1}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					count := "2"
					dataset := object.FromElements(pixelMetadataElements(1, 3, 1, 8, 8, 7, 0, &count,
						core.NewRawElement(tagPhotometricInterpretation, core.VRCS, []byte("MONOCHROME2")),
						core.NewRawElement(core.TagPixelData, core.VROB, []byte{1, 2, 3, 4, 5, 6}),
					), nil)
					file, source := quotaFrameSource(t, &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}, deferred)
					reader, err := NewFrameReader(nil, file, source, FrameReaderOptions{Limits: tc.limits})
					if err != nil {
						t.Fatal(err)
					}
					if source == nil {
						source = &quotaReadAt{ReaderAt: reader.native}
						reader.native = source
					}

					if frame, _, err := reader.Frame(nil, 1); frame != nil {
						t.Fatalf("returned disallowed whole frame %x (%v)", frame, err)
					} else {
						assertFrameQuota(t, err, tc.name, 3, 1)
					}
					if _, err := reader.FrameBufferLayout(nil, 1); err == nil {
						t.Fatal("layout exposed disallowed whole-frame size")
					} else {
						assertFrameQuota(t, err, tc.name, 3, 1)
					}
					dst := bytes.Repeat([]byte{0xfd}, 5)
					before := bytes.Clone(dst)
					result, err := reader.FrameInto(nil, 1, dst)
					assertFrameQuota(t, err, tc.name, 3, 1)
					if result.Written != 0 || !bytes.Equal(dst, before) {
						t.Fatalf("failed whole-frame request touched destination: %+v %x", result, dst)
					}

					wide := image.Rect(1, 0, 3, 1)
					if frame, _, err := reader.Region(nil, 1, wide); frame != nil {
						t.Fatalf("returned disallowed region %x (%v)", frame, err)
					} else {
						assertFrameQuota(t, err, tc.name, 2, 1)
					}
					if _, err := reader.RegionBufferLayout(nil, 1, wide); err == nil {
						t.Fatal("layout exposed disallowed region size")
					} else {
						assertFrameQuota(t, err, tc.name, 2, 1)
					}
					result, err = reader.RegionInto(nil, 1, wide, dst)
					assertFrameQuota(t, err, tc.name, 2, 1)
					if source.calls != 0 || result.Written != 0 || !bytes.Equal(dst, before) {
						t.Fatalf("limit checks touched source or destination: reads=%d result=%+v dst=%x", source.calls, result, dst)
					}

					exact := image.Rect(1, 0, 2, 1)
					got, metadata, err := reader.Region(nil, 1, exact)
					if err != nil || !bytes.Equal(got, []byte{5}) || metadata.Rows != 1 || metadata.Columns != 1 {
						t.Fatalf("exact-boundary read: %x %+v %v", got, metadata, err)
					}
					result, err = reader.RegionInto(nil, 1, exact, dst)
					if err != nil || result.Written != 1 || dst[0] != 5 || !bytes.Equal(dst[1:], before[1:]) || source.bytes != 2 {
						t.Fatalf("exact-boundary destination: %+v %x, requested bytes=%d, error=%v", result, dst, source.bytes, err)
					}
					if raw, err := reader.RawFrame(nil, 1); err != nil || !bytes.Equal(raw, []byte{4, 5, 6}) {
						t.Fatalf("RawFrame independent of decoded limits: %x, %v", raw, err)
					}
				})
			}
		})
	}
}

func TestFrameReaderEncapsulatedIndexQuotaMatchesBackingStores(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.RLELossless, transfer.JPEGBaseline} {
		for _, deferred := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deferred=%t", syntax.UID, deferred), func(t *testing.T) {
				for _, tc := range []struct {
					name string
					opts FrameReaderOptions
				}{
					{"frames", FrameReaderOptions{MaxFrames: 1}},
					{"fragments", FrameReaderOptions{MaxFragments: 1}},
					{"invalid-frames", FrameReaderOptions{MaxFrames: -1}},
					{"invalid-fragments", FrameReaderOptions{MaxFragments: -1}},
					{"offset-table", FrameReaderOptions{MaxFrames: 2}},
					{"exact", FrameReaderOptions{MaxFrames: 2, MaxFragments: 2}},
				} {
					t.Run(tc.name, func(t *testing.T) {
						file := frameReaderDirectFile(2)
						file.TransferSyntax = syntax
						wire := []byte{0xff, 0xd8, 0xff, 0xd9}
						var basic []byte
						if tc.name == "offset-table" {
							basic = make([]byte, 12)
						}
						file.Dataset.Put(core.Element{
							Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true},
							Value:  core.FragmentSequence{OffsetTable: basic, Fragments: [][]byte{wire, wire}},
						})
						file, source := quotaFrameSource(t, file, deferred)
						reader, err := NewFrameReader(nil, file, source, tc.opts)
						if tc.name == "exact" {
							if err != nil {
								t.Fatal(err)
							}
							if frame, err := reader.RawFrame(nil, 1); err != nil || !bytes.Equal(frame, wire) {
								t.Fatalf("RawFrame at exact index quotas = %x, %v", frame, err)
							}
							return
						}
						if reader != nil || !errors.Is(err, ErrFrameLayout) || !errors.Is(err, encapsulated.ErrResourceLimit) {
							t.Fatalf("returned reader=%t, error=%v; want typed encapsulated index budget rejection", reader != nil, err)
						}
						if source != nil {
							if tc.name == "offset-table" && source.bytes != 8 {
								t.Fatalf("offset table preflight read %d bytes; want only its header", source.bytes)
							}
							if tc.name != "fragments" && tc.name != "offset-table" && source.calls != 0 {
								t.Fatalf("frame or option check issued %d input reads", source.calls)
							}
						}
					})
				}
			})
		}
	}
}

func TestFrameReaderNativeFrameIndexQuota(t *testing.T) {
	count := "2"
	dataset := object.FromElements(pixelMetadataElements(1, 1, 1, 8, 8, 7, 0, &count,
		core.NewRawElement(tagPhotometricInterpretation, core.VRCS, []byte("MONOCHROME2")),
		core.NewRawElement(core.TagPixelData, core.VROB, []byte{1, 2}),
	), nil)
	file := &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}
	got, err := NewFrameReader(nil, file, nil, FrameReaderOptions{MaxFrames: 1})
	assertFrameQuota(t, err, "MaxFrames", 2, 1)
	if got != nil || !errors.Is(err, ErrFrameLayout) {
		t.Fatalf("returned reader=%t, error=%v; want typed native index budget rejection", got != nil, err)
	}
}

func TestFrameReaderNativeQuotaChargesSharedWireUnits(t *testing.T) {
	for _, tc := range []struct {
		name     string
		syntax   transfer.Syntax
		vr       core.VR
		columns  uint16
		bits     uint16
		frames   string
		wire     []byte
		want     []byte
		readSize int64
	}{
		{"bit-seam", transfer.ExplicitVRLittleEndian, core.VROB, 7, 1, "3", []byte{0xd6, 0x39, 0x13, 0}, []byte{0x73}, 2},
		{"big-endian-word-seam", transfer.ExplicitVRBigEndian, core.VROW, 3, 8, "2", []byte{11, 10, 13, 12, 15, 14}, []byte{13, 14, 15}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataset := object.FromElements(pixelMetadataElementsWithOrder(tc.syntax.ByteOrder, 1, tc.columns, 1, tc.bits, tc.bits, tc.bits-1, 0, &tc.frames,
				core.NewRawElement(tagPhotometricInterpretation, core.VRCS, []byte("MONOCHROME2")),
				core.NewRawElement(core.TagPixelData, tc.vr, tc.wire),
			), nil)
			dataset.SetValueByteOrder(tc.syntax.ByteOrder)
			reader, err := NewFrameReader(nil, &object.File{Dataset: dataset, TransferSyntax: tc.syntax}, nil, FrameReaderOptions{
				Limits: DecompressLimits{MaxInputBytes: tc.readSize - 1, MaxNativeBytes: int64(len(tc.want))},
			})
			if err != nil {
				t.Fatal(err)
			}
			source := &quotaReadAt{ReaderAt: reader.native}
			reader.native = source
			out, _, err := reader.Frame(nil, 1)
			assertFrameQuota(t, err, "MaxInputBytes", tc.readSize, tc.readSize-1)
			if out != nil || source.calls != 0 {
				t.Fatalf("refused shared-unit frame returned %x after %d reads", out, source.calls)
			}

			reader, err = NewFrameReader(nil, &object.File{Dataset: dataset, TransferSyntax: tc.syntax}, nil, FrameReaderOptions{
				Limits: DecompressLimits{MaxInputBytes: tc.readSize, MaxNativeBytes: int64(len(tc.want))},
			})
			if err != nil {
				t.Fatal(err)
			}
			out, _, err = reader.Frame(nil, 1)
			if err != nil || !bytes.Equal(out, tc.want) {
				t.Fatalf("frame at exact encoded and decoded boundaries = %x, %v; want %x", out, err, tc.want)
			}
		})
	}
}

func TestFrameReaderRegionalCodecAdmissionPrecedesPayloadRead(t *testing.T) {
	registry := NewMemoryRegistry()
	codec := intoLegacyRegional{frameReaderDirectCodec: frameReaderDirectCodec(directCopyFrames)}
	if err := registry.RegisterCodec(transfer.RLELossless.UID, codec); err != nil {
		t.Fatal(err)
	}
	r := newDirectFrameReader(t, frameReaderDirectFile(2), FrameReaderOptions{
		Registry: registry,
		Limits:   DecompressLimits{MaxInputBytes: 1},
	})
	source := &directCountedSource{Source: r.fragments}
	r.fragments = source
	region := image.Rect(0, 0, 1, 2)
	got, _, err := r.Region(nil, 0, region)
	assertFrameQuota(t, err, "MaxInputBytes", 4, 1)
	if got != nil {
		t.Fatalf("regional codec returned %x after limits rejection", got)
	}
	dst := []byte{0xfd, 0xfd}
	result, err := r.RegionInto(nil, 0, region, dst)
	assertFrameQuota(t, err, "MaxInputBytes", 4, 1)
	part, err := r.DecodePartial(nil, PartialDecodeRequest{Frame: 0, Region: region})
	assertFrameQuota(t, err, "MaxInputBytes", 4, 1)
	if source.reads.Load() != 0 || result.Written != 0 || !bytes.Equal(dst, []byte{0xfd, 0xfd}) {
		t.Fatalf("regional codec touched buffers after limits rejection: reads=%d, result=%+v, dst=%x", source.reads.Load(), result, dst)
	}
	if part.Data != nil {
		t.Fatalf("regional partial decoder returned %x after limits rejection", part.Data)
	}
}

type quotaPartialCodec struct {
	frameReaderDirectCodec
}

func (quotaPartialCodec) PartialDecodeCapabilities([]byte, Metadata) (PartialDecodeCapabilities, error) {
	return PartialDecodeCapabilities{Region: true}, nil
}

func (quotaPartialCodec) DecodeFramePartial(context.Context, []byte, Metadata, PartialDecodeRequest) (Frames, error) {
	return Frames{Rows: 2, Columns: 1, Data: [][]byte{{1, 3}}}, nil
}

func TestFrameReaderPartialCodecAdmissionPrecedesPayloadRead(t *testing.T) {
	registry := NewMemoryRegistry()
	if err := registry.RegisterCodec(transfer.RLELossless.UID, quotaPartialCodec{}); err != nil {
		t.Fatal(err)
	}
	r := newDirectFrameReader(t, frameReaderDirectFile(2), FrameReaderOptions{
		Registry: registry,
		Limits:   DecompressLimits{MaxInputBytes: 1},
	})
	source := &directCountedSource{Source: r.fragments}
	r.fragments = source
	capabilities, err := r.PartialDecodeCapabilities(nil, 0)
	assertFrameQuota(t, err, "MaxInputBytes", 4, 1)
	request := PartialDecodeRequest{Frame: 0, Region: image.Rect(0, 0, 1, 2), NoFallback: true}
	result, err := r.DecodePartial(nil, request)
	assertFrameQuota(t, err, "MaxInputBytes", 4, 1)
	if source.reads.Load() != 0 || capabilities.Region || result.Data != nil {
		t.Fatalf("preadmission accessed source or published results: reads=%d capabilities=%+v result=%+v", source.reads.Load(), capabilities, result)
	}
}

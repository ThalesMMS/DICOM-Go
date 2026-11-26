package dicomjson

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var tagRedPalette = core.NewTag(0x0028, 0x1201)

// bigEndianPixelElements returns a Big Endian data set with a Red palette
// LUT and native OW Pixel Data of bits Bits Allocated, both holding the
// Little Endian bytes little once reversed by their unit.
func bigEndianPixelElements(bits uint16, little []byte) []core.Element {
	return []core.Element{
		core.NewRawElement(core.NewTag(0x0028, 0x0100), core.VRUS, binary.BigEndian.AppendUint16(nil, bits)),
		core.NewRawElement(tagRedPalette, core.VROW, valueencode.ReverseFixedWidth(little, 2)),
		core.NewRawElement(core.TagPixelData, core.VROW, valueencode.ReverseFixedWidth(little, int(bits/8))),
	}
}

// PixelDataBulkDataURIFunc streams native Pixel Data of a Big Endian object
// in Little Endian, at the sample width, like InlineBinary and
// BulkDataURIFunc, whether the value is held by the object, deferred on the
// source or replayed as a bulk data reference; SourceByteOrderInlineBinary
// keeps the object's bytes (#1982).
func TestPixelDataBulkDataURIFuncStreamsLittleEndian(t *testing.T) {
	little := make([]byte, 96<<10+8) // several streaming chunks
	for i := range little {
		little[i] = byte(i*7 + 1)
	}
	for _, bits := range []uint16{16, 32} {
		elements := bigEndianPixelElements(bits, little)
		wire := dicomtest.EncodeElements(transfer.ExplicitVRBigEndian, elements...)
		read := func(t *testing.T, opts object.ReadFileOptions) *object.Object {
			obj, err := object.ReadDataSetWithOptions(bytes.NewReader(wire), transfer.ExplicitVRBigEndian, opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = obj.Close() })
			return obj
		}
		sources := map[string]func(t *testing.T) *object.Object{
			"in memory": func(t *testing.T) *object.Object {
				obj := object.FromDataSet(core.DataSet{Elements: elements}, std.Dictionary)
				obj.SetValueByteOrder(binary.BigEndian)
				return obj
			},
			"deferred": func(t *testing.T) *object.Object {
				return read(t, object.ReadFileOptions{InlineValueBytesThreshold: 1})
			},
			"reference": func(t *testing.T) *object.Object {
				return read(t, object.ReadFileOptions{BulkDataDescriptor: &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{{}}}})
			},
		}
		for name, source := range sources {
			for _, keep := range []bool{false, true} {
				obj := source(t)
				var pixels, palette []byte
				_, err := Marshal(obj, Options{
					SourceByteOrderInlineBinary: keep,
					BulkDataReferenceURIFunc:    func(context.Context, object.BulkDataReference) (string, error) { return "", nil },
					BulkDataURIFunc: func(tag core.Tag, _ core.VR, data []byte) string {
						if tag == tagRedPalette {
							palette = bytes.Clone(data)
						}
						return "palette"
					},
					PixelDataBulkDataURIFunc: func(_ core.Tag, _ core.VR, open func() (io.ReadCloser, error)) (string, error) {
						reader, err := open()
						if err != nil {
							return "", err
						}
						defer reader.Close()
						pixels, err = io.ReadAll(reader)
						return "pixels", err
					},
				})
				if err != nil {
					t.Fatalf("%d bits %s keep=%t: %v", bits, name, keep, err)
				}
				wantPixels, wantPalette := little, little
				if keep {
					wantPixels, wantPalette = valueencode.ReverseFixedWidth(little, int(bits/8)), valueencode.ReverseFixedWidth(little, 2)
				}
				if !bytes.Equal(pixels, wantPixels) {
					t.Errorf("%d bits %s keep=%t: Pixel Data callback starts % X, want % X", bits, name, keep, pixels[:min(8, len(pixels))], wantPixels[:8])
				}
				if !bytes.Equal(palette, wantPalette) {
					t.Errorf("%d bits %s keep=%t: palette callback starts % X, want % X", bits, name, keep, palette[:min(8, len(palette))], wantPalette[:8])
				}
			}
		}
	}
}

func allocatedBytes(run func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	run()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// MaxInlineBinaryBytes refuses an oversized OW value of a Big Endian object
// before its Little Endian copy is made, the conversion never modifies the
// object's own buffers, and a value just read from the source is reversed in
// its own buffer instead of being copied again (#1982).
func TestBigEndianInlineConversionChecksLimitsAndOwnsBuffers(t *testing.T) {
	const length = 8 << 20
	little := bytes.Repeat([]byte{1, 2, 3, 4}, length/4)
	shared := valueencode.ReverseFixedWidth(little, 2)
	original := bytes.Clone(shared)
	obj := object.FromDataSet(core.DataSet{Elements: []core.Element{core.NewRawElement(tagRedPalette, core.VROW, shared)}}, std.Dictionary)
	obj.SetValueByteOrder(binary.BigEndian)

	var err error
	allocated := allocatedBytes(func() {
		_, err = Marshal(obj, Options{Limits: Limits{MaxInlineBinaryBytes: length - 1}})
	})
	if !errors.Is(err, ErrMaxInlineBinaryBytesExceeded) {
		t.Fatalf("oversized value: %v, want ErrMaxInlineBinaryBytesExceeded", err)
	}
	if allocated >= length/2 {
		t.Fatalf("refusing a %d-byte value allocated %d bytes", length, allocated)
	}

	for _, withBulk := range []bool{false, true} {
		opts := Options{}
		var received []byte
		if withBulk {
			opts.BulkDataURIFunc = func(_ core.Tag, _ core.VR, data []byte) string {
				received = bytes.Clone(data[:4])
				return "bulk"
			}
		}
		if _, err := Marshal(obj, opts); err != nil {
			t.Fatal(err)
		}
		if withBulk && !bytes.Equal(received, little[:4]) {
			t.Fatalf("BulkDataURIFunc received % X, want % X", received, little[:4])
		}
		held, _ := obj.Get(tagRedPalette)
		heldRaw, _ := held.RawBytes()
		if !bytes.Equal(shared, original) || !bytes.Equal(heldRaw, original) {
			t.Fatalf("withBulk=%t: Marshal modified the object's value", withBulk)
		}
	}

	// A bulk data reference is read into a buffer of its own and reversed
	// there: one value-sized allocation, not two.
	wire := dicomtest.EncodeElements(transfer.ExplicitVRBigEndian, core.NewRawElement(tagRedPalette, core.VROW, shared))
	deferred, err := object.ReadDataSetWithOptions(bytes.NewReader(wire), transfer.ExplicitVRBigEndian, object.ReadFileOptions{BulkDataDescriptor: &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{{}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer deferred.Close()
	for run := 0; run < 2; run++ {
		var first []byte
		allocated = allocatedBytes(func() {
			_, err = Marshal(deferred, Options{
				BulkDataReferenceURIFunc: func(context.Context, object.BulkDataReference) (string, error) { return "", nil },
				BulkDataURIFunc: func(_ core.Tag, _ core.VR, data []byte) string {
					first = bytes.Clone(data[:4])
					return "bulk"
				},
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, little[:4]) {
			t.Fatalf("run %d: deferred value callback received % X, want % X", run, first, little[:4])
		}
		if allocated >= length*3/2 {
			t.Fatalf("run %d: converting a %d-byte deferred value allocated %d bytes", run, length, allocated)
		}
	}
}

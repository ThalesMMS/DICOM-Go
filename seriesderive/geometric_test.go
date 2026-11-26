package seriesderive

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/derivation"
	"github.com/ThalesMMS/dicom-go/imagebuild"
	"github.com/ThalesMMS/dicom-go/internal/commontest"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

func geometricFixture(t *testing.T, kind imagebuild.IOD, syntax transfer.Syntax, signed bool, bits int, rows, cols int) (*object.File, []int64) {
	t.Helper()
	source := commontest.Source()
	modality := "CT"
	if kind == imagebuild.MR {
		modality = "MR"
	}
	source.Put(derivedio.CS(derivedio.TagModality, modality))
	source.Put(derivedio.DS(core.NewTag(0x0018, 0x0060), 120))
	if kind == imagebuild.MR {
		source.Put(derivedio.CS(core.NewTag(0x0018, 0x0023), "2D"))
	}
	stored := make([]int64, rows*cols)
	modalityValues := make([]float64, len(stored))
	for i := range stored {
		if signed {
			stored[i] = int64(-700 + i*37)
		} else {
			stored[i] = int64((i*137 + 11) % ((1 << bits) - 1))
			if bits == 1 {
				stored[i] = int64(i % 2)
			}
		}
		modalityValues[i] = float64(stored[i])
	}
	g := imagebuild.Geometry{Rows: rows, Columns: cols, PixelSpacing: [2]float64{.7, 1.3},
		RowDirection: [3]float64{.6, .8, 0}, ColumnDirection: [3]float64{-.48, .36, .8}, SliceThickness: 2.5}
	opts := imagebuild.Options{IOD: kind, Common: object.DerivedCommon{Source: source}, ContentTime: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
		Rescale: &imagebuild.Rescale{Slope: 1, Intercept: 0}}
	if kind == imagebuild.CT {
		opts.RescaleType = "HU"
	}
	files, err := imagebuild.Build(g, []imagebuild.Slice{{Values: modalityValues, Position: [3]float64{-19.25, 7.5, 33}}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	f := files[0]
	f.TransferSyntax = syntax
	order := syntax.ByteOrder
	f.Dataset.SetValueByteOrder(order)
	// Construct BE numeric metadata explicitly: changing the syntax alone is
	// not a conversion of raw numeric source payloads.
	for _, pair := range []struct {
		tag   core.Tag
		value uint16
	}{
		{derivedio.TagRows, uint16(rows)}, {derivedio.TagColumns, uint16(cols)},
		{derivedio.TagSamplesPerPixel, 1}, {derivedio.TagBitsAllocated, 16},
		{derivedio.TagBitsStored, uint16(bits)}, {derivedio.TagHighBit, uint16(bits - 1)},
		{derivedio.TagPixelRepresentation, 0},
	} {
		f.Dataset.Put(object.USElement(pair.tag, order, pair.value))
	}
	if signed {
		f.Dataset.Put(object.USElement(derivedio.TagPixelRepresentation, order, 1))
	}
	raw := make([]byte, len(stored)*2)
	for i, v := range stored {
		container := uint16(v) & uint16((uint32(1)<<uint(bits))-1)
		if bits <= 12 {
			container |= 0xa000
		} // ignored unused bits
		order.PutUint16(raw[i*2:], container)
	}
	f.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROW}, Value: core.RawValue(raw)})
	if err := f.RebuildFileMeta(); err != nil {
		t.Fatal(err)
	}
	assertGeometricValid(t, f)
	t.Cleanup(func() { _ = f.Close() })
	return f, stored
}

func assertGeometricValid(t *testing.T, f *object.File) {
	t.Helper()
	r, err := iod.Validate(f.Dataset, iod.Options{CheckValues: true})
	if err != nil || len(r.Errors()) != 0 || r.Truncated != 0 {
		t.Fatalf("IOD: %v, errors=%v", err, r.Errors())
	}
	v, err := validation.ValidateFile(context.Background(), core.DataSet{Elements: f.Meta.Elements()}, core.DataSet{Elements: f.Dataset.Elements()}, f.TransferSyntax,
		validation.Options{Mode: validation.ModeStrict, ByteOrder: f.Dataset.ValueByteOrder()})
	if err != nil || v.HasErrors() || v.Truncated {
		t.Fatalf("strict VR/VM/Part10: %v, findings=%v", err, v.Findings)
	}
}

func geometricBytes(t *testing.T, f *object.File) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := object.WriteFile(&b, f); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// This oracle inverts the documented stages as scalar operations, never
// calling the production affine/composition/geometry or sample helpers.
func oracleSourceCoordinate(column, row, resizeCols, resizeRows int, crop CropRegion, opts GeometricOptions) (float64, float64) {
	x, y := float64(column), float64(row)
	switch opts.RotateClockwise {
	case 90:
		x, y = y, float64(resizeRows-1)-x
	case 180:
		x, y = float64(resizeCols-1)-x, float64(resizeRows-1)-y
	case 270:
		x, y = float64(resizeCols-1)-y, x
	}
	if opts.FlipVertical {
		y = float64(resizeRows-1) - y
	}
	if opts.FlipHorizontal {
		x = float64(resizeCols-1) - x
	}
	x = float64(crop.Column) + (x+.5)*float64(crop.Width)/float64(resizeCols) - .5
	y = float64(crop.Row) + (y+.5)*float64(crop.Height)/float64(resizeRows) - .5
	return x, y
}

func oracleStored(stored []int64, sourceCols, column, row, resizeCols, resizeRows int, crop CropRegion, opts GeometricOptions) int64 {
	// Arbitrary-precision rational oracle for the documented pixel-centre
	// formula and kernel, independent of production's int64 weight arithmetic.
	x, y := column, row
	switch opts.RotateClockwise {
	case 90:
		x, y = row, resizeRows-1-column
	case 180:
		x, y = resizeCols-1-column, resizeRows-1-row
	case 270:
		x, y = resizeCols-1-row, column
	}
	if opts.FlipVertical {
		y = resizeRows - 1 - y
	}
	if opts.FlipHorizontal {
		x = resizeCols - 1 - x
	}
	coordinate := func(index, size, target, offset int) *big.Rat {
		r := big.NewRat(int64(2*index+1), 2)
		r.Mul(r, big.NewRat(int64(size), int64(target)))
		r.Add(r, big.NewRat(int64(2*offset-1), 2))
		if r.Cmp(big.NewRat(int64(offset), 1)) < 0 {
			r.SetInt64(int64(offset))
		}
		if r.Cmp(big.NewRat(int64(offset+size-1), 1)) > 0 {
			r.SetInt64(int64(offset + size - 1))
		}
		return r
	}
	xr, yr := coordinate(x, crop.Width, resizeCols, crop.Column), coordinate(y, crop.Height, resizeRows, crop.Row)
	integer := func(r *big.Rat) int { return int(new(big.Int).Quo(r.Num(), r.Denom()).Int64()) }
	if opts.Interpolation == Nearest {
		xr.Add(xr, big.NewRat(1, 2))
		yr.Add(yr, big.NewRat(1, 2))
		return stored[integer(yr)*sourceCols+integer(xr)]
	}
	x0, y0 := integer(xr), integer(yr)
	xs, ys := []int{x0, min(x0+1, crop.Column+crop.Width-1)}, []int{y0, min(y0+1, crop.Row+crop.Height-1)}
	fx, fy := new(big.Rat).Sub(xr, big.NewRat(int64(x0), 1)), new(big.Rat).Sub(yr, big.NewRat(int64(y0), 1))
	wx, wy := []*big.Rat{new(big.Rat).Sub(big.NewRat(1, 1), fx), fx}, []*big.Rat{new(big.Rat).Sub(big.NewRat(1, 1), fy), fy}
	sum := new(big.Rat)
	for j := 0; j < 2; j++ {
		for i := 0; i < 2; i++ {
			weight := new(big.Rat).Mul(wx[i], wy[j])
			sum.Add(sum, weight.Mul(weight, big.NewRat(stored[ys[j]*sourceCols+xs[i]], 1)))
		}
	}
	negative := sum.Sign() < 0
	if negative {
		sum.Neg(sum)
	}
	sum.Add(sum, big.NewRat(1, 2))
	n := int64(integer(sum))
	if negative {
		n = -n
	}
	return n
}

func checkGeometricOracle(t *testing.T, source, out *object.File, stored []int64, opts GeometricOptions, resizeRows, resizeCols int) {
	t.Helper()
	m, _ := pixeldata.ExtractMetadata(source.Dataset)
	n, _ := pixeldata.ExtractMetadata(out.Dataset)
	crop := CropRegion{Width: int(m.Columns), Height: int(m.Rows)}
	if opts.Crop != nil {
		crop = *opts.Crop
	}
	rows, cols := resizeRows, resizeCols
	if opts.RotateClockwise == 90 || opts.RotateClockwise == 270 {
		rows, cols = cols, rows
	}
	if int(n.Rows) != rows || int(n.Columns) != cols {
		t.Fatalf("dimensions %dx%d, want %dx%d", n.Rows, n.Columns, rows, cols)
	}
	ip, _ := source.Dataset.GetFloats(tagGeometricPosition)
	io, _ := source.Dataset.GetFloats(tagGeometricOrientation)
	ps, _ := source.Dataset.GetFloats(tagGeometricSpacing)
	op, _ := out.Dataset.GetFloats(tagGeometricPosition)
	oo, _ := out.Dataset.GetFloats(tagGeometricOrientation)
	os, _ := out.Dataset.GetFloats(tagGeometricSpacing)
	e, _ := out.Dataset.Get(core.TagPixelData)
	raw, _ := e.RawBytes()
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			x, y := oracleSourceCoordinate(col, row, resizeCols, resizeRows, crop, opts)
			for a := 0; a < 3; a++ {
				want := ip[a] + io[a]*ps[1]*x + io[a+3]*ps[0]*y
				got := op[a] + oo[a]*os[1]*float64(col) + oo[a+3]*os[0]*float64(row)
				if math.Abs(got-want) > 1e-8 {
					t.Fatalf("patient (%d,%d) axis%d %.12f want%.12f", col, row, a, got, want)
				}
			}
			want := oracleStored(stored, int(m.Columns), col, row, resizeCols, resizeRows, crop, opts)
			container := out.Dataset.ValueByteOrder().Uint16(raw[(row*cols+col)*2:])
			mask := uint16((uint32(1) << m.BitsStored) - 1)
			got := int64(container & mask)
			if m.PixelRepresentation == 1 && container&(1<<(m.BitsStored-1)) != 0 {
				got -= int64(1 << m.BitsStored)
			}
			if got != want {
				t.Fatalf("stored (%d,%d)=%d want%d", col, row, got, want)
			}
			if container&^mask != 0 {
				t.Fatal("unused output bits retained")
			}
		}
	}
}

func TestTransformImageGeometryAndStoredSamples(t *testing.T) {
	for _, kind := range []imagebuild.IOD{imagebuild.CT, imagebuild.MR} {
		for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
			source, stored := geometricFixture(t, kind, syntax, kind == imagebuild.CT, 12, 4, 6)
			before := geometricBytes(t, source)
			for _, interpolation := range []Interpolation{Nearest, Bilinear} {
				for _, rotate := range []int{0, 90, 180, 270} {
					for flips := 0; flips < 4; flips++ {
						name := fmt.Sprintf("iod%d/%s/interp%d/rot%d/flips%d", kind, syntax.UID, interpolation, rotate, flips)
						t.Run(name, func(t *testing.T) {
							opts := GeometricOptions{Crop: &CropRegion{Column: 1, Row: 1, Width: 4, Height: 3}, Rows: 5, Columns: 7,
								Interpolation: interpolation, RotateClockwise: rotate, FlipHorizontal: flips&1 != 0, FlipVertical: flips&2 != 0,
								SeriesInstanceUID: "2.25.132601", SOPInstanceUID: "2.25.132602"}
							out, report, err := TransformImage(context.Background(), source, opts)
							if err != nil {
								t.Fatal(err)
							}
							defer out.Close()
							if report == nil || report.Rows == 0 || len(report.InputIOD.Errors()) != 0 || len(report.OutputIOD.Errors()) != 0 {
								t.Fatalf("report=%+v", report)
							}
							assertGeometricValid(t, out)
							checkGeometricOracle(t, source, out, stored, opts, 5, 7)
							if out.TransferSyntax.UID != syntax.UID {
								t.Fatal("transfer syntax changed")
							}
							if !bytes.Equal(before, geometricBytes(t, source)) {
								t.Fatal("source changed")
							}
							// Reopen the publicly encoded Part 10 independently of the
							// operation's internal round trip and run both validators.
							back, err := object.ReadFile(bytes.NewReader(geometricBytes(t, out)))
							if err != nil {
								t.Fatal(err)
							}
							defer back.Close()
							assertGeometricValid(t, back)
							checkGeometricOracle(t, source, back, stored, opts, 5, 7)
						})
					}
				}
			}
		}
	}
}

func TestTransformImageSingletonAspectAndFormats(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		kind                         imagebuild.IOD
		signed                       bool
		bits                         int
		rows, cols, outRows, outCols int
		opts                         GeometricOptions
	}{
		{"singleton-to-singleton", imagebuild.CT, true, 16, 1, 1, 1, 1, GeometricOptions{}},
		{"one-row-grow", imagebuild.MR, false, 16, 1, 4, 3, 7, GeometricOptions{Rows: 3, Columns: 7, Interpolation: Bilinear}},
		{"one-column-grow", imagebuild.MR, true, 16, 3, 1, 5, 2, GeometricOptions{Rows: 5, Columns: 2, Interpolation: Bilinear}},
		{"physical-aspect-column", imagebuild.CT, false, 16, 4, 6, 3, 7, GeometricOptions{Columns: 7}},
		{"physical-aspect-row", imagebuild.MR, false, 8, 4, 6, 9, 25, GeometricOptions{Rows: 9}},
		{"one-bit-stored", imagebuild.MR, false, 1, 4, 6, 2, 3, GeometricOptions{Rows: 2, Columns: 3}},
		{"collapse-to-one", imagebuild.CT, true, 12, 4, 6, 1, 1, GeometricOptions{Rows: 1, Columns: 1, Interpolation: Bilinear}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, stored := geometricFixture(t, tc.kind, transfer.ExplicitVRLittleEndian, tc.signed, tc.bits, tc.rows, tc.cols)
			if tc.name == "one-bit-stored" {
				source.Dataset.Put(derivedio.CS(derivedio.TagPhotometricInterpretation, "MONOCHROME1"))
			}
			out, _, err := TransformImage(context.Background(), source, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			checkGeometricOracle(t, source, out, stored, tc.opts, tc.outRows, tc.outCols)
		})
	}
}

func TestTransformImageIdentityHistoryAndOwnedBuffers(t *testing.T) {
	source, _ := geometricFixture(t, imagebuild.CT, transfer.ExplicitVRLittleEndian, true, 12, 4, 6)
	ancestor := "2.25.132600"
	if err := derivation.Apply(source.Dataset, derivation.Derivation{Description: "Prior synthetic derivation", Codes: []derivation.Code{derivation.MultiplanarReformatting},
		Sources: []derivation.Source{{SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: ancestor}}}); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []core.Tag{core.NewTag(0x0028, 0x0106), core.NewTag(0x0028, 0x0107), core.NewTag(0x0028, 0x0108), core.NewTag(0x0028, 0x0109)} {
		source.Dataset.Put(object.SSElement(tag, binary.LittleEndian, -700))
	}
	source.Dataset.Put(derivedio.Strings(core.NewTag(0x0028, 0x0034), core.VRIS, []string{"7", "13"}))
	source.Dataset.Put(derivedio.Strings(core.NewTag(0x0020, 0x0020), core.VRCS, []string{"L", "P"}))
	source.Dataset.Put(derivedio.DS(core.NewTag(0x0018, 0x1050), .5))
	source.Dataset.Put(derivedio.DS(core.NewTag(0x0018, 0x1100), 300))
	source.Dataset.Put(derivedio.CS(core.NewTag(0x0028, 0x2110), "01"))
	source.Dataset.Put(derivedio.DS(core.NewTag(0x0028, 0x2112), 3))
	source.Dataset.Put(derivedio.CS(core.NewTag(0x0028, 0x2114), "ISO_10918_1"))
	private := core.NewTag(0x0011, 0x1001)
	source.Dataset.Put(core.NewRawElement(private, core.VROB, []byte{1, 2, 3, 4}))
	source.Dataset.Put(derivedio.Seq(core.NewTag(0x0088, 0x0200), core.DataSet{Elements: []core.Element{
		object.USElement(derivedio.TagRows, binary.LittleEndian, 1), object.USElement(derivedio.TagColumns, binary.LittleEndian, 1),
		object.USElement(derivedio.TagSamplesPerPixel, binary.LittleEndian, 1), derivedio.CS(derivedio.TagPhotometricInterpretation, "MONOCHROME2"),
		object.USElement(derivedio.TagBitsAllocated, binary.LittleEndian, 16), object.USElement(derivedio.TagBitsStored, binary.LittleEndian, 16),
		object.USElement(derivedio.TagHighBit, binary.LittleEndian, 15), object.USElement(derivedio.TagPixelRepresentation, binary.LittleEndian, 0),
		core.NewRawElement(core.TagPixelData, core.VROW, []byte{0, 0}),
	}}))
	assertGeometricValid(t, source)
	before := geometricBytes(t, source)
	out, report, err := TransformImage(context.Background(), source, GeometricOptions{RotateClockwise: 90})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	for _, tag := range []core.Tag{derivedio.TagSOPClassUID, derivedio.TagStudyInstanceUID, derivedio.TagFrameOfReferenceUID} {
		s, _ := source.Dataset.GetUID(tag)
		o, _ := out.Dataset.GetUID(tag)
		if s != o {
			t.Fatalf("changed %s", tag)
		}
	}
	if report.SOPInstanceUID == report.SeriesInstanceUID {
		t.Fatal("shared output identity")
	}
	assertString(t, out.Meta, derivedio.TagMediaStorageSOPInstanceUID, report.SOPInstanceUID)
	for _, tag := range []core.Tag{core.NewTag(0x0028, 0x0106), core.NewTag(0x0028, 0x0107), core.NewTag(0x0028, 0x0108), core.NewTag(0x0028, 0x0109), core.NewTag(0x0028, 0x0034), core.NewTag(0x0018, 0x1050), core.NewTag(0x0088, 0x0200)} {
		if out.Dataset.Has(tag) {
			t.Fatalf("stale %s", tag)
		}
	}
	assertString(t, out.Dataset, core.NewTag(0x0028, 0x2110), "01")
	assertString(t, out.Dataset, core.NewTag(0x0018, 0x1100), "300")
	refs, _ := out.Dataset.GetSequence(tagSourceImageSequence)
	if len(refs) != 2 {
		t.Fatalf("source chain len%d", len(refs))
	}
	assertString(t, refs[0], derivedio.TagRefSOPInstanceUID, ancestor)
	current, _ := source.Dataset.GetUID(derivedio.TagSOPInstanceUID)
	assertString(t, refs[1], derivedio.TagRefSOPInstanceUID, current)
	if d, _ := out.Dataset.GetString(tagDerivationDescription); !strings.Contains(d, "Prior synthetic derivation") || !strings.Contains(d, "pixel-edge FOV") {
		t.Fatal("missing derivation chain")
	}
	// Mutate raw/private/pixel and nested sequence buffers in the output.
	for _, tag := range []core.Tag{private, core.TagPixelData} {
		e, _ := out.Dataset.Get(tag)
		raw, _ := e.RawBytes()
		raw[0] ^= 0xff
	}
	e, _ := out.Dataset.Get(tagSourceImageSequence)
	seq := e.Value.(core.SequenceValue)
	for i := range seq.Items[0].Elements {
		if seq.Items[0].Elements[i].Tag() == derivedio.TagRefSOPInstanceUID {
			raw, _ := seq.Items[0].Elements[i].RawBytes()
			raw[0] = '9'
		}
	}
	if !bytes.Equal(before, geometricBytes(t, source)) {
		t.Fatal("source aliased result")
	}
}

func TestTransformImageRefusesBeforeUIDAndPublishesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*object.File, *GeometricOptions)
		want error
	}{
		{"input-limit", func(_ *object.File, o *GeometricOptions) { o.Limits.MaxInputPixels = 23 }, ErrGeometricLimit},
		{"output-limit", func(_ *object.File, o *GeometricOptions) { o.Rows = 5; o.Columns = 5; o.Limits.MaxOutputPixels = 24 }, ErrGeometricLimit},
		{"clone-bytes", func(_ *object.File, o *GeometricOptions) { o.Limits.Clone.MaxTotalBytes = 40 }, ErrGeometricLimit},
		{"element-count", func(_ *object.File, o *GeometricOptions) { o.Limits.Clone.MaxElements = 2 }, ErrGeometricLimit},
		{"overflow-size", func(_ *object.File, o *GeometricOptions) { o.Rows = int(^uint(0) >> 1); o.Columns = o.Rows }, ErrGeometricLimit},
		{"outside-crop", func(_ *object.File, o *GeometricOptions) { o.Crop = &CropRegion{Column: 5, Width: 2, Height: 1} }, ErrGeometricOptions},
		{"negative-limit", func(_ *object.File, o *GeometricOptions) { o.Limits.MaxOutputPixels = -1 }, ErrGeometricOptions},
		{"invalid-rotate", func(_ *object.File, o *GeometricOptions) { o.RotateClockwise = 45 }, ErrGeometricOptions},
		{"iod-required", func(f *object.File, _ *GeometricOptions) { f.Dataset.Remove(core.NewTag(0x0018, 0x0060)) }, ErrGeometricValidation},
		{"invalid-vm", func(f *object.File, _ *GeometricOptions) {
			f.Dataset.Put(derivedio.DS(core.NewTag(0x0018, 0x0060), 120, 130))
		}, ErrGeometricValidation},
		{"meta-identity", func(f *object.File, _ *GeometricOptions) {
			f.Meta.Put(derivedio.UI(derivedio.TagMediaStorageSOPInstanceUID, "2.25.909"))
		}, ErrGeometricValidation},
		{"geometry-vm", func(f *object.File, _ *GeometricOptions) { f.Dataset.Put(derivedio.DS(tagGeometricSpacing, .5)) }, ErrGeometricProfile},
		{"nonorthogonal", func(f *object.File, _ *GeometricOptions) {
			f.Dataset.Put(derivedio.DS(tagGeometricOrientation, 1, 0, 0, 1, 0, 0))
		}, ErrGeometricProfile},
		{"zero-spacing", func(f *object.File, _ *GeometricOptions) { f.Dataset.Put(derivedio.DS(tagGeometricSpacing, 0, 1)) }, ErrGeometricProfile},
		{"huge-geometry-vm", func(f *object.File, _ *GeometricOptions) {
			f.Dataset.Put(derivedio.Strings(tagGeometricSpacing, core.VRDS, make([]string, 1000)))
		}, ErrGeometricProfile},
		{"multiframe", func(f *object.File, _ *GeometricOptions) { f.Dataset.Put(derivedio.IS(derivedio.TagNumberOfFrames, 2)) }, ErrGeometricProfile},
		{"functional-groups", func(f *object.File, _ *GeometricOptions) { f.Dataset.Put(derivedio.Seq(core.NewTag(0x5200, 0x9229))) }, ErrGeometricProfile},
		{"overlay", func(f *object.File, _ *GeometricOptions) {
			f.Dataset.Put(object.USElement(core.NewTag(0x6000, 0x0010), binary.LittleEndian, 1))
		}, ErrGeometricProfile},
		{"float-pixels", func(f *object.File, _ *GeometricOptions) {
			f.Dataset.Put(core.NewRawElement(core.NewTag(0x7fe0, 0x0008), core.VROF, []byte{0, 0, 0, 0}))
		}, ErrGeometricProfile},
		{"wrong-pixel-length", func(f *object.File, _ *GeometricOptions) {
			f.Dataset.Put(core.NewRawElement(core.TagPixelData, core.VROW, []byte{0, 0}))
		}, ErrGeometricProfile},
		{"padding-bilinear", func(f *object.File, o *GeometricOptions) {
			f.Dataset.Put(object.SSElement(core.NewTag(0x0028, 0x0120), binary.LittleEndian, -700))
			o.Interpolation = Bilinear
		}, ErrGeometricProfile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, _ := geometricFixture(t, imagebuild.CT, transfer.ExplicitVRLittleEndian, true, 12, 4, 6)
			calls := 0
			opts := GeometricOptions{UIDGenerator: func() (string, error) { calls++; return "2.25.133001", nil }}
			tc.edit(source, &opts)
			out, r, err := TransformImage(context.Background(), source, opts)
			if !errors.Is(err, tc.want) || out != nil || r != nil || calls != 0 {
				t.Fatalf("out=%v report=%v err=%v calls%d", out, r, err, calls)
			}
		})
	}
}

func TestTransformImagePaddingCancellationUIDsAndConcurrency(t *testing.T) {
	source, stored := geometricFixture(t, imagebuild.CT, transfer.ExplicitVRLittleEndian, true, 12, 4, 6)
	source.Dataset.Put(object.SSElement(core.NewTag(0x0028, 0x0120), binary.LittleEndian, -700))
	source.Dataset.Put(object.SSElement(core.NewTag(0x0028, 0x0121), binary.LittleEndian, -663))
	out, _, err := TransformImage(context.Background(), source, GeometricOptions{Rows: 3, Columns: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	checkGeometricOracle(t, source, out, stored, GeometricOptions{Rows: 3, Columns: 4}, 3, 4)
	padding, err := pixeldata.PixelPaddingFromObject(out.Dataset, pixeldata.SampleFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 11, Signed: true})
	if err != nil || padding == nil || padding.Value != -700 || padding.RangeLimit != -663 {
		t.Fatalf("padding=%+v err%v", padding, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, r, err := TransformImage(ctx, source, GeometricOptions{}); !errors.Is(err, context.Canceled) || out != nil || r != nil {
		t.Fatal("cancellation")
	}
	ctx, cancel = context.WithCancel(context.Background())
	calls := 0
	if out, r, err := TransformImage(ctx, source, GeometricOptions{UIDGenerator: func() (string, error) { calls++; cancel(); return "2.25.133002", nil }}); !errors.Is(err, context.Canceled) || out != nil || r != nil || calls != 1 {
		t.Fatalf("UID cancellation out%v r%v err%v calls%d", out, r, err, calls)
	}
	srcSOP, _ := source.Dataset.GetUID(derivedio.TagSOPInstanceUID)
	for _, opts := range []GeometricOptions{
		{SeriesInstanceUID: "2.25.10", SOPInstanceUID: "2.25.10"},
		{SeriesInstanceUID: srcSOP, SOPInstanceUID: "2.25.11"},
		{SeriesInstanceUID: "2.25.10", SOPInstanceUID: commontest.FrameOfReferenceUID},
		{UIDGenerator: func() (string, error) { return "", errors.New("synthetic generator failure") }},
		{UIDGenerator: func() (string, error) { panic("synthetic generator panic") }},
	} {
		if out, r, err := TransformImage(context.Background(), source, opts); err == nil || out != nil || r != nil {
			t.Fatal("bad UID published")
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, _, err := TransformImage(context.Background(), source, GeometricOptions{RotateClockwise: 270})
			if err != nil {
				t.Error(err)
			} else {
				_ = out.Close()
			}
		}()
	}
	wg.Wait()
}

func TestTransformImageDeferredInputAndIndependentOutputLimit(t *testing.T) {
	source, stored := geometricFixture(t, imagebuild.CT, transfer.ExplicitVRBigEndian, true, 12, 4, 6)
	deferred, err := object.ReadFileWithOptions(bytes.NewReader(geometricBytes(t, source)), object.ReadFileOptions{DeferPixelData: true})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := deferred.Dataset.Get(core.TagPixelData)
	if e.Value != nil {
		t.Fatal("fixture pixels not deferred")
	}
	opts := GeometricOptions{Rows: 8, Columns: 8, Interpolation: Bilinear, Limits: GeometricLimits{MaxInputPixels: 24, MaxOutputPixels: 64}}
	out, _, err := TransformImage(context.Background(), deferred, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	e, _ = deferred.Dataset.Get(core.TagPixelData)
	if e.Value != nil {
		t.Fatal("source pixels materialized in place")
	}
	_ = deferred.Close()
	checkGeometricOracle(t, source, out, stored, opts, 8, 8)
	assertGeometricValid(t, out)
}

// Optional local synthetic evidence export, separate from the ordinary suite.
// It does not invoke external tools or make interoperability claims itself.
func TestTransformImageExportSyntheticEvidence(t *testing.T) {
	dir := os.Getenv("DICOM_GO_GEOMETRIC_FIXTURES")
	if dir == "" {
		t.Skip("optional synthetic fixture export")
	}
	for _, kind := range []imagebuild.IOD{imagebuild.CT, imagebuild.MR} {
		source, stored := geometricFixture(t, kind, transfer.ExplicitVRBigEndian, kind == imagebuild.CT, 12, 4, 6)
		opts := GeometricOptions{Crop: &CropRegion{Column: 1, Row: 0, Width: 4, Height: 3}, Rows: 5, Columns: 7, RotateClockwise: 90, FlipHorizontal: true, Interpolation: Bilinear}
		out, _, err := TransformImage(context.Background(), source, opts)
		if err != nil {
			t.Fatal(err)
		}
		checkGeometricOracle(t, source, out, stored, opts, 5, 7)
		caseDir := filepath.Join(dir, fmt.Sprintf("iod-%d", kind))
		if err := os.MkdirAll(caseDir, 0700); err != nil {
			t.Fatal(err)
		}
		for name, file := range map[string]*object.File{"source.dcm": source, "derived.dcm": out} {
			if err := os.WriteFile(filepath.Join(caseDir, name), geometricBytes(t, file), 0600); err != nil {
				t.Fatal(err)
			}
		}
		_ = out.Close()
	}
}

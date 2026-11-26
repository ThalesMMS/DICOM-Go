package registration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

type mincCancelReader struct {
	*bytes.Reader
	ctx    context.Context
	cancel context.CancelFunc
}

func (r mincCancelReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.Reader.Read(data)
	r.cancel()
	return n, err
}

func TestReadMINCXFMMatchesLibminc(t *testing.T) {
	points := []render.Vec3{{}, {X: 1.5, Y: -2.25, Z: 3.75}, {X: -7, Y: 9.5, Z: -11.25}}
	for _, fixture := range []struct {
		name string
		kind Kind
		want []render.Vec3
	}{
		{"linear", KindAffine, []render.Vec3{
			{X: -6.5, Y: 4.25, Z: .1},
			{X: -3.96875, Y: 4.625, Z: 8.1625},
			{X: -16.875, Y: .375, Z: -24.775},
		}},
		{"inverse", KindAffine, []render.Vec3{
			{X: 4.7070588235294117, Y: 4.0235294117647058, Z: .45294117647058824},
			{X: 5.3776470588235297, Y: 5.2588235294117647, Z: 2.4823529411764707},
			{X: 1.0188235294117645, Y: -2.2705882352941185, Z: -5.9588235294117649},
		}},
		{"composite", KindComposite, []render.Vec3{
			{X: -4.925, Y: .6, Z: 20.2},
			{X: -3.78828125, Y: 11.19375, Z: 36.325},
			{X: -7.39375, Y: -34.65, Z: -29.55},
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			data, err := os.ReadFile("testdata/mincxfm/" + fixture.name + ".xfm")
			if err != nil {
				t.Fatal(err)
			}
			transform, err := ReadTransformFile(data)
			if err != nil {
				t.Fatal(err)
			}
			if transform.Kind() != fixture.kind {
				t.Fatalf("kind %v, want %v", transform.Kind(), fixture.kind)
			}
			for i, point := range points {
				got := transform.TransformPoint(point)
				if !finiteVec(got) || got.Sub(fixture.want[i]).Length() > 1e-12 {
					t.Errorf("point %v: got %v, libminc %v", point, got, fixture.want[i])
				}
			}
		})
	}
}

const mincTestLinear = "Transform_Type=Linear;Linear_Transform=1 0 0 5 0 1 0 6 0 0 1 7;"

func TestReadMINCXFMSyntaxAndLimits(t *testing.T) {
	const header = mincTransformHeader + "\n"
	const matrixTag = "Transform_Type=Linear;Linear_Transform="
	for _, failure := range []struct {
		name  string
		input string
		want  error
	}{
		{"empty", "", nil},
		{"truncated", matrixTag + "1 0 0", io.ErrUnexpectedEOF},
		{"missing-terminator", strings.TrimSuffix(mincTestLinear, ";"), io.ErrUnexpectedEOF},
		{"extra-value", strings.TrimSuffix(mincTestLinear, ";") + " 1;", nil},
		{"trailing-content", mincTestLinear + "garbage", nil},
		{"invalid-flag", "Transform_Type=Linear;Invert_Flag=Maybe;", nil},
		{"non-finite", strings.Replace(mincTestLinear, " 5 ", " NaN ", 1), nil},
		{"singular", matrixTag + "1 0 0 5 0 1 0 6 0 0 0 7;", ErrSingularMatrix},
		{"non-finite-composition", strings.Repeat(matrixTag+"1e100 0 0 0 0 1e100 0 0 0 0 1e100 0;", 4), ErrInvalidMatrix},
		{"external-file", "Transform_Type=Grid_Transform;Displacement_Volume=never-open.mnc;", ErrUnsupportedTransform},
		{"thin-plate", "Transform_Type=Thin_Plate_Spline_Transform;", ErrUnsupportedTransform},
		{"long-word", matrixTag + strings.Repeat("1", maxMINCTokenBytes+1), nil},
		{"too-many-components", strings.Repeat(mincTestLinear, maxHDF5Transforms+1), nil},
	} {
		t.Run(failure.name, func(t *testing.T) {
			got, err := ReadTransformFile([]byte(header + failure.input))
			if got != nil || !errors.Is(err, ErrTransformFile) || (failure.want != nil && !errors.Is(err, failure.want)) {
				t.Fatalf("got %v, %v; want transform file error with cause %v", got, err, failure.want)
			}
		})
	}

	// Test CRLF/BOM, comments inside a record and incomplete final comment.
	data := []byte("\r\n" + utf8BOM + "\r\n" + mincTransformHeader + "\r\n" +
		" # " + strings.Repeat("comment", 1000) + "\r\n" +
		strings.Replace(mincTestLinear, " 5 ", " 5% offset \r\n", 1) + "% trailing")
	got, err := ReadTransformFile(data)
	if err != nil || got.TransformPoint(render.Vec3{}) != (render.Vec3{X: -5, Y: -6, Z: 7}) {
		t.Fatalf("read with comments: %v, %v", got, err)
	}

	data = []byte(header + mincTestLinear + mincTestLinear)
	if got, err := ReadTransformFileContext(nil, data, TransformFileReadOptions{MaxParameters: 23}); got != nil || !errors.Is(err, ErrTransformFile) {
		t.Fatalf("budget: %v, %v", got, err)
	}
	if got, err := ReadTransformFileContext(nil, data, TransformFileReadOptions{MaxParameters: 24}); err != nil || got.NumberOfParameters() != 24 {
		t.Fatalf("exact budget: %v, %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := ReadTransformFileContext(ctx, data, TransformFileReadOptions{}); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v, %v", got, err)
	}
	tooLarge := make([]byte, maxMINCFileBytes+1)
	copy(tooLarge, data)
	if got, err := ReadTransformFile(tooLarge); got != nil || !errors.Is(err, ErrTransformFile) || !strings.Contains(err.Error(), "encoded bytes") {
		t.Fatalf("encoded size: %v, %v", got, err)
	}
}

func TestReadMINCXFMCanceledBetweenInputBuffers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := []byte(mincTestLinear + "\n%" + strings.Repeat(" ", 8192) + "\n" + mincTestLinear)
	reader := mincCancelReader{Reader: bytes.NewReader(input), ctx: ctx, cancel: cancel}
	got, err := readMINCTransform(reader, 24)
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled input: %v, %v", got, err)
	}
}

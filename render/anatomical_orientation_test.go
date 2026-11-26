package render

import (
	"image"
	"image/color"
	"math"
	"testing"
)

const anatomicalTestEpsilon = 1e-12

func anatomicalVecClose(a, b Vec3) bool {
	return a.Sub(b).Length() <= anatomicalTestEpsilon
}

func TestNearestAnatomicalAxes(t *testing.T) {
	h := math.Sqrt2 / 2
	cos30, sin30 := math.Cos(math.Pi/6), math.Sin(math.Pi/6)
	tests := []struct {
		name string
		axes [3]Vec3
		want [3]AnatomicalAxis
		ok   bool
	}{
		{
			name: "axial supine head first",
			axes: [3]Vec3{{X: 1}, {Y: 1}, {Z: 1}},
			want: [3]AnatomicalAxis{{Axis: PatientAxisX}, {Axis: PatientAxisY}, {Axis: PatientAxisZ}},
			ok:   true,
		},
		{
			name: "sagittal",
			axes: [3]Vec3{{Y: 1}, {Z: -1}, {X: -1}},
			want: [3]AnatomicalAxis{{Axis: PatientAxisY}, {Axis: PatientAxisZ, Negative: true}, {Axis: PatientAxisX, Negative: true}},
			ok:   true,
		},
		{
			name: "coronal",
			axes: [3]Vec3{{X: 1}, {Z: -1}, {Y: 1}},
			want: [3]AnatomicalAxis{{Axis: PatientAxisX}, {Axis: PatientAxisZ, Negative: true}, {Axis: PatientAxisY}},
			ok:   true,
		},
		{
			// A per-vector argmax would send the row and slice axes to
			// the same patient axis here; the greedy rule keeps them
			// distinct and resolves the exact tie in image-axis order.
			name: "45 degree oblique in the axial plane",
			axes: [3]Vec3{{X: h, Y: h}, {Z: -1}, {X: -h, Y: h}},
			want: [3]AnatomicalAxis{{Axis: PatientAxisX}, {Axis: PatientAxisZ, Negative: true}, {Axis: PatientAxisY}},
			ok:   true,
		},
		{
			// The global maximum (0.9) is picked before the first
			// vector's own largest component would be.
			name: "global maximum wins over per-vector order",
			axes: [3]Vec3{{X: 0.6, Y: 0.8}, {X: -0.8, Y: 0.6}, {Z: 1}},
			want: [3]AnatomicalAxis{{Axis: PatientAxisY}, {Axis: PatientAxisX, Negative: true}, {Axis: PatientAxisZ}},
			ok:   true,
		},
		{
			name: "30 degree tilt keeps nearest axes",
			axes: [3]Vec3{{X: cos30, Y: sin30}, {X: -sin30, Y: cos30}, {Z: 1}},
			want: [3]AnatomicalAxis{{Axis: PatientAxisX}, {Axis: PatientAxisY}, {Axis: PatientAxisZ}},
			ok:   true,
		},
		{
			name: "unnormalized prone feet first",
			axes: [3]Vec3{{X: -2}, {Y: -3}, {Z: 4}},
			want: [3]AnatomicalAxis{{Axis: PatientAxisX, Negative: true}, {Axis: PatientAxisY, Negative: true}, {Axis: PatientAxisZ}},
			ok:   true,
		},
		{name: "parallel axes", axes: [3]Vec3{{X: 1}, {X: 1}, {Z: 1}}},
		{name: "zero axis", axes: [3]Vec3{{X: 1}, {}, {Z: 1}}},
		{name: "non-finite axis", axes: [3]Vec3{{X: math.NaN()}, {Y: 1}, {Z: 1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NearestAnatomicalAxes(tt.axes)
			if ok != tt.ok || (ok && got != tt.want) {
				t.Fatalf("NearestAnatomicalAxes = %+v, %v; want %+v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestReorientedAxes(t *testing.T) {
	axes := [3]Vec3{{Y: 1}, {Z: -1}, {X: -1}} // sagittal stack
	reorientation, ok := ReorientedAxes(axes)
	if !ok {
		t.Fatal("ReorientedAxes rejected a sagittal stack")
	}
	want := AxisReorientation{Order: [3]int{2, 0, 1}, Flip: [3]bool{true, false, true}}
	if reorientation != want {
		t.Fatalf("ReorientedAxes = %+v, want %+v", reorientation, want)
	}
	for axis, direction := range reorientation.Axes(axes) {
		if !anatomicalVecClose(direction, PatientAxis(axis).Unit()) {
			t.Fatalf("reoriented axis %d = %+v, want %+v", axis, direction, PatientAxis(axis).Unit())
		}
	}
	if _, ok := ReorientedAxes([3]Vec3{{X: 1}, {X: 1}, {Z: 1}}); ok {
		t.Fatal("ReorientedAxes accepted parallel axes")
	}
}

func TestImageOrientationPointsFractionsAndInverse(t *testing.T) {
	const w, h = 5, 3
	for _, orientation := range allImageOrientations() {
		outW, outH := orientation.Size(w, h)
		img := image.NewGray(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				img.SetGray(x, y, color.Gray{Y: uint8(y*w + x)})
			}
		}
		oriented := orientation.Image(img).(*image.Gray)
		if oriented.Bounds().Dx() != outW || oriented.Bounds().Dy() != outH {
			t.Fatalf("%+v: oriented size = %v, want %dx%d", orientation, oriented.Bounds(), outW, outH)
		}
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				ox, oy := orientation.Point(float64(x), float64(y), w, h)
				if got := oriented.GrayAt(int(ox), int(oy)).Y; got != uint8(y*w+x) {
					t.Fatalf("%+v: pixel (%d,%d) moved to (%v,%v) holding %d", orientation, x, y, ox, oy, got)
				}
				sx, sy := orientation.SourcePoint(ox, oy, w, h)
				if sx != float64(x) || sy != float64(y) {
					t.Fatalf("%+v: SourcePoint(Point(%d,%d)) = (%v,%v)", orientation, x, y, sx, sy)
				}
				ix, iy := orientation.Inverse().Point(ox, oy, outW, outH)
				if ix != float64(x) || iy != float64(y) {
					t.Fatalf("%+v: Inverse().Point = (%v,%v), want (%d,%d)", orientation, ix, iy, x, y)
				}
			}
		}
		// Fractions are the edge-based counterpart of pixel-centre points.
		fx, fy := orientation.Fraction(0.25, 0.75)
		px, py := orientation.Point(0.25*float64(w-1), 0.75*float64(h-1), w, h)
		if math.Abs(fx*float64(outW-1)-px) > 1e-12 || math.Abs(fy*float64(outH-1)-py) > 1e-12 {
			t.Fatalf("%+v: Fraction = (%v,%v), Point = (%v,%v)", orientation, fx, fy, px, py)
		}
		if sx, sy := orientation.SourceFraction(fx, fy); math.Abs(sx-0.25) > 1e-12 || math.Abs(sy-0.75) > 1e-12 {
			t.Fatalf("%+v: SourceFraction = (%v,%v)", orientation, sx, sy)
		}
	}
}

func TestImageOrientationPlaneMatchesPixelPermutation(t *testing.T) {
	const w, h = 4, 3
	plane := Plane{Origin: Vec3{X: 10, Y: 20, Z: 30}, U: Vec3{X: 3}, V: Vec3{Z: -2}}
	for _, orientation := range allImageOrientations() {
		outW, outH := orientation.Size(w, h)
		oriented := orientation.Plane(plane)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				ox, oy := orientation.Point(float64(x), float64(y), w, h)
				want := plane.At(float64(x)/float64(w-1), float64(y)/float64(h-1))
				got := oriented.At(ox/float64(outW-1), oy/float64(outH-1))
				if !anatomicalVecClose(got, want) {
					t.Fatalf("%+v: oriented plane at (%v,%v) = %+v, want %+v", orientation, ox, oy, got, want)
				}
			}
		}
		right, down := orientation.Axes(plane.U.Normalize(), plane.V.Normalize())
		if !anatomicalVecClose(right, oriented.U.Normalize()) || !anatomicalVecClose(down, oriented.V.Normalize()) {
			t.Fatalf("%+v: Axes = %+v, %+v; plane axes %+v, %+v", orientation, right, down, oriented.U, oriented.V)
		}
	}
}

func TestImageOrientationKeepsImageTypes(t *testing.T) {
	orientation := ImageOrientation{Transpose: true, FlipHorizontal: true}
	sources := []image.Image{
		image.NewGray(image.Rect(2, 3, 5, 5)),
		image.NewGray16(image.Rect(0, 0, 3, 2)),
		image.NewRGBA(image.Rect(0, 0, 3, 2)),
		image.NewNRGBA(image.Rect(0, 0, 3, 2)),
	}
	for _, source := range sources {
		source.(interface{ Set(int, int, color.Color) }).Set(source.Bounds().Min.X, source.Bounds().Min.Y, color.White)
		oriented := orientation.Image(source)
		if got, want := oriented.Bounds().Size(), image.Pt(source.Bounds().Dy(), source.Bounds().Dx()); got != want {
			t.Fatalf("%T: oriented size = %v, want %v", source, got, want)
		}
		if gotType, wantType := typeName(oriented), typeName(source); gotType != wantType {
			t.Fatalf("oriented type = %s, want %s", gotType, wantType)
		}
		// Source (0,0) transposes to (0,0) and then mirrors to the right edge.
		r, _, _, _ := oriented.At(oriented.Bounds().Dx()-1, 0).RGBA()
		if r != 0xffff {
			t.Fatalf("%T: source corner did not land at the top-right corner", source)
		}
	}
	paletted := image.NewPaletted(image.Rect(0, 0, 2, 1), color.Palette{color.Black, color.White})
	paletted.SetColorIndex(0, 0, 1)
	if _, ok := orientation.Image(paletted).(*image.NRGBA); !ok {
		t.Fatal("paletted image was not converted to NRGBA")
	}
	if got := (ImageOrientation{}).Image(paletted); got != image.Image(paletted) {
		t.Fatal("identity orientation copied the image")
	}
}

func typeName(img image.Image) string {
	switch img.(type) {
	case *image.Gray:
		return "gray"
	case *image.Gray16:
		return "gray16"
	case *image.RGBA:
		return "rgba"
	case *image.NRGBA:
		return "nrgba"
	default:
		return "other"
	}
}

func allImageOrientations() []ImageOrientation {
	var out []ImageOrientation
	for _, transpose := range []bool{false, true} {
		for _, flipH := range []bool{false, true} {
			for _, flipV := range []bool{false, true} {
				out = append(out, ImageOrientation{Transpose: transpose, FlipHorizontal: flipH, FlipVertical: flipV})
			}
		}
	}
	return out
}

// TestAnatomicalMPRViews checks that each anatomical view renders the index
// plane nearest to that anatomy and, after its permutation, shows the
// radiological screen axes for axial, sagittal, coronal and 45 degree oblique
// acquisitions.
func TestAnatomicalMPRViews(t *testing.T) {
	h := math.Sqrt2 / 2
	tests := []struct {
		name         string
		axisX, axisY Vec3
		planes       [3]MPRPlane
		orientations [3]ImageOrientation
		// wantAxes are the oriented screen axes of the axial, coronal and
		// sagittal views; zero vectors mean "the radiological axes".
		wantAxes [3][2]Vec3
	}{
		{
			name:  "axial supine head first",
			axisX: Vec3{X: 1}, axisY: Vec3{Y: 1},
			planes: [3]MPRPlane{MPRPlaneAxial, MPRPlaneCoronal, MPRPlaneSagittal},
		},
		{
			name:  "sagittal",
			axisX: Vec3{Y: 1}, axisY: Vec3{Z: -1},
			planes:       [3]MPRPlane{MPRPlaneCoronal, MPRPlaneSagittal, MPRPlaneAxial},
			orientations: [3]ImageOrientation{{Transpose: true}, {Transpose: true}, {}},
		},
		{
			name:  "coronal",
			axisX: Vec3{X: 1}, axisY: Vec3{Z: -1},
			planes:       [3]MPRPlane{MPRPlaneCoronal, MPRPlaneAxial, MPRPlaneSagittal},
			orientations: [3]ImageOrientation{{FlipVertical: true}, {}, {Transpose: true, FlipHorizontal: true}},
		},
		{
			name:  "axial with rows toward anterior",
			axisX: Vec3{X: 1}, axisY: Vec3{Y: -1},
			planes:       [3]MPRPlane{MPRPlaneAxial, MPRPlaneCoronal, MPRPlaneSagittal},
			orientations: [3]ImageOrientation{{FlipVertical: true}, {FlipVertical: true}, {FlipHorizontal: true, FlipVertical: true}},
		},
		{
			// Columns at 45 degrees between left and posterior, rows toward
			// the feet: the slice normal (-h, h, 0) ties with the columns;
			// the greedy rule gives the columns the x axis and the slices
			// the y axis, so the axial view is the row-index (coronal)
			// plane and no two views share a plane.
			name:  "45 degree oblique",
			axisX: Vec3{X: h, Y: h}, axisY: Vec3{Z: -1},
			planes:       [3]MPRPlane{MPRPlaneCoronal, MPRPlaneAxial, MPRPlaneSagittal},
			orientations: [3]ImageOrientation{{FlipVertical: true}, {}, {Transpose: true, FlipHorizontal: true}},
			wantAxes: [3][2]Vec3{
				{{X: h, Y: h}, {X: -h, Y: h}},
				{{X: h, Y: h}, {Z: -1}},
				{{X: -h, Y: h}, {Z: -1}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			views, ok := AnatomicalMPRViews(tt.axisX, tt.axisY)
			if !ok {
				t.Fatal("AnatomicalMPRViews rejected valid axes")
			}
			for index, view := range views {
				if view.Anatomical != AnatomicalPlaneOrder()[index] || view.Plane != tt.planes[index] || view.Orientation != tt.orientations[index] {
					t.Fatalf("view %d = %+v, want %s rendered by %s with %+v", index, view, AnatomicalPlaneOrder()[index], tt.planes[index], tt.orientations[index])
				}
				right, down, _ := MPRPlaneImageAxes(view.Plane, tt.axisX, tt.axisY)
				right, down = view.Orientation.Axes(right, down)
				wantRight, wantDown := tt.wantAxes[index][0], tt.wantAxes[index][1]
				if wantRight == (Vec3{}) {
					wantRight, wantDown, _ = RadiologicalImageAxes(view.Anatomical)
				}
				if !anatomicalVecClose(right, wantRight) || !anatomicalVecClose(down, wantDown) {
					t.Fatalf("%s view axes = %+v, %+v; want %+v, %+v", view.Anatomical, right, down, wantRight, wantDown)
				}
			}
			for _, plane := range []MPRPlane{MPRPlaneAxial, MPRPlaneCoronal, MPRPlaneSagittal} {
				if _, ok := MPRViewForPlane(views, plane); !ok {
					t.Fatalf("no view renders index plane %s", plane)
				}
			}
		})
	}
	if views, ok := AnatomicalMPRViews(Vec3{X: 1}, Vec3{X: 1}); ok || views != IndexMPRViews() {
		t.Fatalf("degenerate axes = %+v, %v; want the index fallback", views, ok)
	}
	if view, ok := MPRViewForAnatomical(IndexMPRViews(), AnatomicalSagittal); !ok || view.Plane != MPRPlaneSagittal {
		t.Fatalf("MPRViewForAnatomical = %+v, %v", view, ok)
	}
	if _, ok := MPRViewForAnatomical(IndexMPRViews(), AnatomicalPlane("oblique")); ok {
		t.Fatal("MPRViewForAnatomical accepted an unknown plane")
	}
}

// TestAnatomicalMPRViewsRenderRadiologicalImages renders a synthetic sagittal
// stack whose voxel values encode their patient position and checks that the
// oriented axial view has the patient's left on the right and posterior at
// the bottom.
func TestAnatomicalMPRViewsRenderRadiologicalImages(t *testing.T) {
	const rows, cols, depth = 6, 5, 4 // rows along F, columns along P, slices along R
	stack := &Stack{PixelSpacing: []float64{1, 1}, SliceThickness: 1, DefaultWindow: WindowLevel{Center: 127.5, Width: 256}}
	for k := 0; k < depth; k++ {
		pixels := make([]byte, rows*cols)
		for y := 0; y < rows; y++ {
			for x := 0; x < cols; x++ {
				// Patient x (left) decreases with k; y (posterior) grows with x.
				pixels[y*cols+x] = byte(40*(depth-1-k) + 5*x)
			}
		}
		frame := volumeTestFrame(rows, cols, pixels, 0)
		frame.ImageOrientation = []float64{0, 1, 0, 0, 0, -1}
		frame.ImagePosition = []float64{-float64(k), 0, 0}
		stack.Frames = append(stack.Frames, frame)
	}
	views, ok := AnatomicalMPRViews(Vec3{Y: 1}, Vec3{Z: -1})
	if !ok {
		t.Fatal("AnatomicalMPRViews rejected a sagittal stack")
	}
	axial := views[0]
	img, err := RenderMPRPlane(stack, axial.Plane, rows/2, WindowLevel{Center: 127.5, Width: 256})
	if err != nil {
		t.Fatalf("RenderMPRPlane: %v", err)
	}
	oriented := axial.Orientation.Image(img)
	bounds := oriented.Bounds()
	gray := func(x, y int) uint8 { return color.GrayModel.Convert(oriented.At(x, y)).(color.Gray).Y }
	// Screen x must sweep the slices (40 grey levels per millimetre toward
	// the patient's left) and screen y the columns (5 per millimetre toward
	// posterior); the unoriented row-index plane has them swapped.
	left, right := gray(bounds.Min.X, bounds.Min.Y), gray(bounds.Max.X-1, bounds.Min.Y)
	if int(right)-int(left) < 100 {
		t.Fatalf("axial view: screen left %d to right %d does not run toward the patient's left", left, right)
	}
	top, bottom := gray(bounds.Min.X, bounds.Min.Y), gray(bounds.Min.X, bounds.Max.Y-1)
	if diff := int(bottom) - int(top); diff < 15 || diff > 25 {
		t.Fatalf("axial view: screen top %d to bottom %d does not run toward posterior", top, bottom)
	}
}

package mesh

import (
	"context"
	"encoding/binary"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/render"
)

// ctSphereStack builds a synthetic 16-bit CT series (HU = stored - 1024) of
// a sphere of radius mm around center, with a 200 HU/mm ramp across the
// surface, on axial slices whose origins are given (shifted origins model a
// gantry tilt, uneven gaps irregular spacing).
func ctSphereStack(cols, rows int, colSpacing, rowSpacing float64, origins []render.Vec3, center render.Vec3, radius float64) *render.Stack {
	stack := &render.Stack{Modality: "CT", PixelSpacing: []float64{rowSpacing, colSpacing}}
	for _, origin := range origins {
		data := make([]byte, 2*cols*rows)
		for y := 0; y < rows; y++ {
			for x := 0; x < cols; x++ {
				p := origin.Add(render.Vec3{X: float64(x) * colSpacing, Y: float64(y) * rowSpacing})
				hu := math.Max(-1000, math.Min(1000, 200*(radius-p.Sub(center).Length())))
				binary.LittleEndian.PutUint16(data[2*(y*cols+x):], uint16(math.Round(hu+1024)))
			}
		}
		stack.Frames = append(stack.Frames, &render.Frame{
			Metadata: pixeldata.Metadata{
				Rows: uint16(rows), Columns: uint16(cols), SamplesPerPixel: 1,
				BitsAllocated: 16, BitsStored: 16, HighBit: 15,
				PhotometricInterpretation: "MONOCHROME2",
			},
			ByteOrder:        binary.LittleEndian,
			PixelBytes:       data,
			Rescale:          render.Rescale{Slope: 1, Intercept: -1024},
			ImageOrientation: []float64{1, 0, 0, 0, 1, 0},
			ImagePosition:    []float64{origin.X, origin.Y, origin.Z},
			PixelSpacing:     []float64{rowSpacing, colSpacing},
		})
	}
	return stack
}

// TestExtractVolumePatientSpace extracts a CT sphere through the canonical
// snapshot of regular, gantry-tilted and irregularly spaced series: the
// Marching Cubes surface is closed, lies on the sphere in patient LPS
// coordinates and encloses its volume; the voxel-exact surface has the area
// and volume of render.Volume.ISOSurfaceTriangles on the regular series.
func TestExtractVolumePatientSpace(t *testing.T) {
	const radius, iso = 9.0, 100.0
	center := render.Vec3{X: 15.2, Y: 12.4, Z: 14.1}
	regular, tilted, irregular := []render.Vec3{}, []render.Vec3{}, []render.Vec3{}
	z := 0.0
	for k := 0; k < 24; k++ {
		regular = append(regular, render.Vec3{Z: 1.25 * float64(k)})
		tilted = append(tilted, render.Vec3{Y: -0.3 * 1.25 * float64(k), Z: 1.25 * float64(k)})
		irregular = append(irregular, render.Vec3{Z: z})
		z += []float64{1, 1.5}[k%2]
	}
	for _, c := range []struct {
		name    string
		origins []render.Vec3
		// tolerance on the distance of a vertex from the sphere: the tilted
		// and irregular series are resampled onto a regular grid first.
		distance float64
	}{
		{"regular", regular, 0.03},
		{"tilted", tilted, 0.05},
		{"irregular", irregular, 0.05},
	} {
		t.Run(c.name, func(t *testing.T) {
			vol, err := render.BuildVolume(ctSphereStack(40, 46, 0.8, 0.7, c.origins, center, radius))
			if err != nil {
				t.Fatal(err)
			}
			if c.name == "tilted" && !vol.GantryTiltCorrection().Applied {
				t.Fatal("tilt not detected")
			}
			m, err := ExtractVolume(context.Background(), vol, Options{Iso: iso})
			if err != nil {
				t.Fatal(err)
			}
			if top := requireClosedManifold(t, m); top.euler() != 2 {
				t.Fatalf("Euler characteristic %d", top.euler())
			}
			// The iso surface of the ramp is the sphere of radius - iso/200.
			r := radius - iso/200
			var signed float64
			for i := range m.Vertices {
				p := m.Vertex(i)
				d := p.Sub(center).Length() - r
				signed += d
				if math.Abs(d) > c.distance {
					t.Fatalf("vertex %v is %g mm off the sphere", p, d)
				}
				if vertexNormal(m, i).Dot(p.Sub(center).Normalize()) < 0.95 {
					t.Fatalf("vertex %v normal %v", p, vertexNormal(m, i))
				}
			}
			// No systematic offset: a misplaced grid would shift the mean.
			if mean := signed / float64(len(m.Vertices)); math.Abs(mean) > 0.02 {
				t.Fatalf("mean radial error %g mm", mean)
			}
			area, volume := massProperties(m)
			if !relClose(area, 4*math.Pi*r*r, 0.02) || !relClose(volume, 4*math.Pi*r*r*r/3, 0.02) {
				t.Fatalf("area %g, volume %g; analytic %g, %g", area, volume, 4*math.Pi*r*r, 4*math.Pi*r*r*r/3)
			}
			if c.name != "regular" {
				return
			}
			voxels, err := ExtractVolume(context.Background(), vol, Options{Iso: iso, Method: VoxelFaces})
			if err != nil {
				t.Fatal(err)
			}
			soup, err := vol.ISOSurfaceTriangles(iso)
			if err != nil {
				t.Fatal(err)
			}
			area, volume = massProperties(voxels)
			soupArea, soupVolume := triangleSoupMass(soup)
			if len(voxels.Triangles) != len(soup) || !relClose(area, soupArea, 1e-6) || !relClose(volume, soupVolume, 1e-6) {
				t.Fatalf("voxel faces: %d triangles, area %g, volume %g; ISOSurfaceTriangles %d, %g, %g",
					len(voxels.Triangles), area, volume, len(soup), soupArea, soupVolume)
			}
			if len(voxels.Vertices) >= 3*len(soup) {
				t.Fatalf("%d vertices for %d triangles: corners not shared", len(voxels.Vertices), len(soup))
			}
		})
	}
}

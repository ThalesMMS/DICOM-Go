package render

import (
	"context"
	"fmt"
	"math"
)

// Depth-composited geometry in volume rendering (#1068), after VTK's ray
// termination at the depth buffer of opaque geometry, vtkTubeFilter and
// vtkVolumeOutlineSource (written from their behaviour, no VTK code is
// copied): opaque triangle meshes — surfaces, centreline tubes, landmark
// spheres — are rasterized with a z-buffer holding, per pixel, the ray
// parameter of the nearest surface. The ray march stops there and composites
// the surface's shaded colour behind the volume samples in front of it, so
// geometry inside the volume is occluded by the tissue before it.
//
// Translucent meshes (#1236) keep, per pixel, the VRMaxTranslucentFragments
// nearest fragments in front of the opaque depth, sorted by depth: a small
// k-buffer (L. Bavoil et al., "Multi-Fragment Effects on the GPU using the
// k-Buffer", I3D 2007), where a fragment beyond the k-th is blended under
// it. The DVR march composites each fragment, front to back, when it passes
// its depth, so a translucent surface inside the volume shows the tissue in
// front of it and is seen through by what lies behind it. VTK's
// vtkDualDepthPeelingPass informed the behaviour; no VTK code is copied.

// VRGeometryMesh is a triangle mesh in the renderer's box-centred
// millimetre frame (the frame of VRCamera), opaque unless Opacity says
// otherwise.
type VRGeometryMesh struct {
	Vertices  []Vec3
	Triangles [][3]uint32
	// Normals are optional unit per-vertex normals; faces use their plane
	// normal without them.
	Normals []Vec3
	// Occlusion is the optional per-vertex ambient accessibility in [0, 1]
	// (#1237, such as dicom-go's mesh.AmbientOcclusion): interpolated over
	// each triangle, it scales the reflected light, so creases and cavities
	// darken. Without it (or with a length other than Vertices') the mesh
	// is fully lit.
	Occlusion []float32
	// Color is linear-light RGB.
	Color RGBA
	// Colors optionally overrides Color with one linear-light RGB triplet per
	// vertex. A present list must match Vertices and contain finite [0,1]
	// components. Geometry is immutable after publication to a clip/renderer.
	Colors [][3]float32
	// Opacity in (0, 1) makes the mesh translucent (#1236); 0 (the zero
	// value) and 1 and above are opaque.
	Opacity float64
}

// ValidateVertexColors rejects partial or invalid vertex-color attributes;
// callers must not silently replace a malformed material with uniform Color.
func (m VRGeometryMesh) ValidateVertexColors() error {
	return m.ValidateVertexColorsContext(context.Background())
}

func (m VRGeometryMesh) ValidateVertexColorsContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(m.Colors) == 0 {
		return nil
	}
	if len(m.Colors) != len(m.Vertices) {
		return fmt.Errorf("render: vertex color count differs from geometry")
	}
	for i, rgb := range m.Colors {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		for _, v := range rgb {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) || v < 0 || v > 1 {
				return fmt.Errorf("render: vertex color must be finite linear RGB in [0,1]")
			}
		}
	}
	return nil
}

// Translucent reports whether the mesh is drawn translucent.
func (m VRGeometryMesh) Translucent() bool { return m.Opacity > 0 && m.Opacity < 1 }

// VRGeometry is the geometry composited into a VR frame.
type VRGeometry struct {
	Meshes []VRGeometryMesh
}

// Empty reports whether the geometry has no triangle.
func (g *VRGeometry) Empty() bool {
	if g == nil {
		return true
	}
	for _, m := range g.Meshes {
		if len(m.Triangles) > 0 && len(m.Vertices) > 0 {
			return false
		}
	}
	return true
}

// Translucent reports whether any mesh with triangles is translucent.
func (g *VRGeometry) Translucent() bool {
	if g == nil {
		return false
	}
	for _, m := range g.Meshes {
		if m.Translucent() && len(m.Triangles) > 0 && len(m.Vertices) > 0 {
			return true
		}
	}
	return false
}

// TriangleCount returns the number of triangles.
func (g *VRGeometry) TriangleCount() int {
	if g == nil {
		return 0
	}
	count := 0
	for _, m := range g.Meshes {
		count += len(m.Triangles)
	}
	return count
}

// VRTubeMesh is a tube of the given radius around a polyline, vtkTubeFilter
// style: a ring of sides vertices at every point, oriented by a
// rotation-minimising frame, joined by quads. Coincident points are skipped;
// fewer than two distinct points give an empty mesh.
func VRTubeMesh(points []Vec3, radius float64, sides int, color RGBA) VRGeometryMesh {
	mesh := VRGeometryMesh{Color: color}
	if sides < 3 {
		sides = 3
	}
	var path []Vec3
	for _, p := range points {
		if len(path) == 0 || p.Sub(path[len(path)-1]).Length() > 1e-9 {
			path = append(path, p)
		}
	}
	if len(path) < 2 || !(radius > 0) {
		return mesh
	}
	tangent := func(i int) Vec3 {
		switch i {
		case 0:
			return path[1].Sub(path[0]).Normalize()
		case len(path) - 1:
			return path[i].Sub(path[i-1]).Normalize()
		}
		return path[i+1].Sub(path[i-1]).Normalize()
	}
	t := tangent(0)
	normal := t.Cross(Vec3{X: 1})
	if normal.Length() < 0.1 {
		normal = t.Cross(Vec3{Y: 1})
	}
	normal = normal.Normalize()
	for i := range path {
		next := tangent(i)
		// Rotation-minimising: project the previous normal onto the plane
		// normal to the new tangent.
		normal = normal.Sub(next.Scale(normal.Dot(next)))
		if normal.Length() < 1e-9 {
			normal = next.Cross(Vec3{Z: 1})
			if normal.Length() < 0.1 {
				normal = next.Cross(Vec3{Y: 1})
			}
		}
		normal = normal.Normalize()
		binormal := next.Cross(normal)
		for s := 0; s < sides; s++ {
			angle := 2 * math.Pi * float64(s) / float64(sides)
			radial := normal.Scale(math.Cos(angle)).Add(binormal.Scale(math.Sin(angle)))
			mesh.Vertices = append(mesh.Vertices, path[i].Add(radial.Scale(radius)))
			mesh.Normals = append(mesh.Normals, radial)
		}
	}
	for i := 0; i+1 < len(path); i++ {
		for s := 0; s < sides; s++ {
			a := uint32(i*sides + s)
			b := uint32(i*sides + (s+1)%sides)
			c := a + uint32(sides)
			d := b + uint32(sides)
			mesh.Triangles = append(mesh.Triangles, [3]uint32{a, b, d}, [3]uint32{a, d, c})
		}
	}
	return mesh
}

// VRSphereMesh is a latitude-longitude sphere, for landmarks.
func VRSphereMesh(center Vec3, radius float64, color RGBA) VRGeometryMesh {
	const rings, segments = 8, 16
	mesh := VRGeometryMesh{Color: color}
	if !(radius > 0) {
		return mesh
	}
	for r := 0; r <= rings; r++ {
		theta := math.Pi * float64(r) / rings
		for s := 0; s < segments; s++ {
			phi := 2 * math.Pi * float64(s) / segments
			n := Vec3{X: math.Sin(theta) * math.Cos(phi), Y: math.Sin(theta) * math.Sin(phi), Z: math.Cos(theta)}
			mesh.Vertices = append(mesh.Vertices, center.Add(n.Scale(radius)))
			mesh.Normals = append(mesh.Normals, n)
		}
	}
	for r := 0; r < rings; r++ {
		for s := 0; s < segments; s++ {
			a := uint32(r*segments + s)
			b := uint32(r*segments + (s+1)%segments)
			c := a + segments
			d := b + segments
			mesh.Triangles = append(mesh.Triangles, [3]uint32{a, c, d}, [3]uint32{a, d, b})
		}
	}
	return mesh
}

// VRMaxTranslucentFragments is the number of translucent fragments kept per
// pixel; farther ones are blended under the last.
const VRMaxTranslucentFragments = 4

// VRDepthImage is rasterized geometry: per pixel, the ray parameter of the
// nearest opaque surface (+Inf where none) and its shaded linear-light
// colour, and the translucent fragments in front of it.
type VRDepthImage struct {
	Width, Height int
	T             []float32
	R, G, B       []float32
	// FragmentCount holds, per pixel, the number of its translucent
	// fragments, stored nearest first at FragmentT and FragmentRGBA from
	// index pixel·VRMaxTranslucentFragments: the ray parameter and the
	// premultiplied linear colour with its alpha. All three are nil without
	// translucent geometry.
	FragmentCount []uint8
	FragmentT     []float32
	FragmentRGBA  [][4]float32
}

// VRFragment is a translucent fragment: its ray parameter and its
// premultiplied linear-light colour (Color.A is the alpha).
type VRFragment struct {
	T     float64
	Color RGBA
}

// Fragments appends the translucent fragments of pixel (x, y), nearest
// first, to dst.
func (d *VRDepthImage) Fragments(x, y int, dst []VRFragment) []VRFragment {
	if d == nil || d.FragmentCount == nil || x < 0 || y < 0 || x >= d.Width || y >= d.Height {
		return dst
	}
	i := y*d.Width + x
	base := i * VRMaxTranslucentFragments
	for k := 0; k < int(d.FragmentCount[i]); k++ {
		c := d.FragmentRGBA[base+k]
		dst = append(dst, VRFragment{T: float64(d.FragmentT[base+k]), Color: RGBA{R: float64(c[0]), G: float64(c[1]), B: float64(c[2]), A: float64(c[3])}})
	}
	return dst
}

// HasFragments reports whether any pixel holds a translucent fragment.
func (d *VRDepthImage) HasFragments() bool {
	if d == nil {
		return false
	}
	for _, n := range d.FragmentCount {
		if n > 0 {
			return true
		}
	}
	return false
}

// insertFragment adds a fragment to pixel i's sorted k-buffer. A full
// buffer blends the farthest fragment under its neighbour.
func (d *VRDepthImage) insertFragment(i int, t float32, c [4]float32) {
	const k = VRMaxTranslucentFragments
	base := i * k
	n := int(d.FragmentCount[i])
	ts, cs := d.FragmentT[base:base+k], d.FragmentRGBA[base:base+k]
	at := n
	for at > 0 && ts[at-1] > t {
		at--
	}
	if n < k {
		copy(ts[at+1:n+1], ts[at:n])
		copy(cs[at+1:n+1], cs[at:n])
		ts[at], cs[at] = t, c
		d.FragmentCount[i]++
		return
	}
	if at == k {
		cs[k-1] = blendUnder(cs[k-1], c)
		return
	}
	evicted := cs[k-1]
	copy(ts[at+1:], ts[at:k-1])
	copy(cs[at+1:], cs[at:k-1])
	ts[at], cs[at] = t, c
	cs[k-1] = blendUnder(cs[k-1], evicted)
}

// blendUnder composites the premultiplied colour back behind front.
func blendUnder(front, back [4]float32) [4]float32 {
	inv := 1 - front[3]
	return [4]float32{front[0] + inv*back[0], front[1] + inv*back[1], front[2] + inv*back[2], front[3] + inv*back[3]}
}

// compositeVRFragments composites fragments, nearest first, in front of a
// linear-light colour (premultiplied with alpha base.A) and returns the
// result.
func compositeVRFragments(fragments []VRFragment, base RGBA) RGBA {
	var out RGBA
	for _, f := range fragments {
		inv := 1 - out.A
		out.R += inv * f.Color.R
		out.G += inv * f.Color.G
		out.B += inv * f.Color.B
		out.A += inv * f.Color.A
	}
	inv := 1 - out.A
	out.R += inv * base.R
	out.G += inv * base.G
	out.B += inv * base.B
	out.A += inv * base.A
	return out
}

// At returns the depth and colour of pixel (x, y); hit is false where no
// geometry covers it.
func (d *VRDepthImage) At(x, y int) (t float64, color RGBA, hit bool) {
	if d == nil || x < 0 || y < 0 || x >= d.Width || y >= d.Height {
		return math.Inf(1), RGBA{}, false
	}
	i := y*d.Width + x
	if math.IsInf(float64(d.T[i]), 1) {
		return math.Inf(1), RGBA{}, false
	}
	return float64(d.T[i]), RGBA{R: float64(d.R[i]), G: float64(d.G[i]), B: float64(d.B[i]), A: 1}, true
}

// vrGeometryMaterial lights geometry with the headlight.
var vrGeometryMaterial = VRLightingMaterial{Ambient: 0.25, Diffuse: 0.75, Specular: 0.25, SpecularPower: 24}

// vrRasterBand is the number of rows a rasterization task owns.
const vrRasterBand = 16

type vrScreenTriangle struct {
	mesh        int
	translucent bool
	index       [3]uint32
	x, y        [3]float64
	minX, maxX  int
	minY, maxY  int
	normal      Vec3 // face normal, box frame
}

// RasterizeVRGeometryContext rasterizes geometry for a w×h frame of cam:
// every pixel's depth is the exact ray parameter where its ray (the
// renderer's ray through the pixel centre) meets the nearest opaque
// triangle, and the translucent triangles in front of it add fragments
// (each pixel of a shared edge belongs to one triangle, so a translucent
// surface is blended once). Faces crossing the camera plane are dropped.
func RasterizeVRGeometryContext(ctx context.Context, cam VRCamera, w, h int, geometry *VRGeometry) (*VRDepthImage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if w <= 0 || h <= 0 {
		return nil, nil
	}
	if geometry != nil {
		for _, mesh := range geometry.Meshes {
			if err := mesh.ValidateVertexColorsContext(ctx); err != nil {
				return nil, err
			}
		}
	}
	depth := &VRDepthImage{Width: w, Height: h, T: make([]float32, w*h), R: make([]float32, w*h), G: make([]float32, w*h), B: make([]float32, w*h)}
	inf := float32(math.Inf(1))
	for i := range depth.T {
		depth.T[i] = inf
	}
	if geometry.Empty() {
		return depth, nil
	}
	projector := newVRProjector(cam, w, h)
	var triangles []vrScreenTriangle
	translucent := false
	for m, mesh := range geometry.Meshes {
		for _, tri := range mesh.Triangles {
			if int(tri[0]) >= len(mesh.Vertices) || int(tri[1]) >= len(mesh.Vertices) || int(tri[2]) >= len(mesh.Vertices) {
				continue
			}
			screen := vrScreenTriangle{mesh: m, index: tri, translucent: mesh.Translucent()}
			visible := true
			for k := 0; k < 3; k++ {
				x, y, ok := projector.project(mesh.Vertices[tri[k]])
				if !ok || math.IsNaN(x) || math.IsNaN(y) {
					visible = false
					break
				}
				screen.x[k], screen.y[k] = x, y
			}
			if !visible {
				continue
			}
			v0, v1, v2 := mesh.Vertices[tri[0]], mesh.Vertices[tri[1]], mesh.Vertices[tri[2]]
			screen.normal = v1.Sub(v0).Cross(v2.Sub(v0))
			if screen.normal.Length() < 1e-18 {
				continue
			}
			// Pixel centres are at i+0.5.
			screen.minX = max(0, int(math.Ceil(min(screen.x[0], screen.x[1], screen.x[2])-0.5)))
			screen.maxX = min(w-1, int(math.Floor(max(screen.x[0], screen.x[1], screen.x[2])-0.5)))
			screen.minY = max(0, int(math.Ceil(min(screen.y[0], screen.y[1], screen.y[2])-0.5)))
			screen.maxY = min(h-1, int(math.Floor(max(screen.y[0], screen.y[1], screen.y[2])-0.5)))
			if screen.minX > screen.maxX || screen.minY > screen.maxY {
				continue
			}
			translucent = translucent || screen.translucent
			triangles = append(triangles, screen)
		}
	}
	if translucent {
		n := w * h
		depth.FragmentCount = make([]uint8, n)
		depth.FragmentT = make([]float32, n*VRMaxTranslucentFragments)
		depth.FragmentRGBA = make([][4]float32, n*VRMaxTranslucentFragments)
	}
	rayGen := newVRRayGenerator(cam, w, h)
	bands := (h + vrRasterBand - 1) / vrRasterBand
	// Opaque triangles first, so the translucent pass sees the final
	// opaque depth.
	for _, pass := range []bool{false, true} {
		if pass && !translucent {
			break
		}
		err := parallelRowsContext(ctx, bands, func(band int) {
			top, bottom := band*vrRasterBand, min(h-1, band*vrRasterBand+vrRasterBand-1)
			for i := range triangles {
				if tri := &triangles[i]; tri.translucent == pass && tri.maxY >= top && tri.minY <= bottom {
					rasterizeVRTriangle(tri, &geometry.Meshes[tri.mesh], depth, rayGen, max(top, tri.minY), min(bottom, tri.maxY))
				}
			}
		})
		if err != nil {
			return depth, err
		}
	}
	return depth, nil
}

// rasterizeVRTriangle draws rows top..bottom of a screen triangle into the
// depth image: an opaque triangle where it is nearer than the opaque depth,
// a translucent one as fragments in front of it.
func rasterizeVRTriangle(tri *vrScreenTriangle, mesh *VRGeometryMesh, depth *VRDepthImage, rayGen vrRayGenerator, top, bottom int) {
	w := depth.Width
	area := (tri.x[1]-tri.x[0])*(tri.y[2]-tri.y[0]) - (tri.x[2]-tri.x[0])*(tri.y[1]-tri.y[0])
	if math.Abs(area) < 1e-12 {
		return
	}
	sign := 1.0
	if area < 0 {
		sign = -1
	}
	// owns reports whether a pixel centre exactly on edge k (opposite
	// corner k) belongs to this triangle: of the two triangles sharing the
	// edge, which run it in opposite directions, exactly one does.
	owns := func(k int) bool {
		dx := sign * (tri.x[(k+2)%3] - tri.x[(k+1)%3])
		dy := sign * (tri.y[(k+2)%3] - tri.y[(k+1)%3])
		return dy > 0 || dy == 0 && dx > 0
	}
	v0 := mesh.Vertices[tri.index[0]]
	for py := top; py <= bottom; py++ {
		row := rayGen.rowBase(py)
		cy := float64(py) + 0.5
		for px := tri.minX; px <= tri.maxX; px++ {
			cx := float64(px) + 0.5
			// Edge functions of the pixel centre; e[k] is opposite corner k
			// and divided by the area gives its barycentric weight. The
			// conversions round each product, so no fused multiply-add
			// breaks the exact antisymmetry two triangles sharing an edge
			// rely on.
			e := [3]float64{
				float64((tri.x[1]-cx)*(tri.y[2]-cy)) - float64((tri.x[2]-cx)*(tri.y[1]-cy)),
				float64((tri.x[2]-cx)*(tri.y[0]-cy)) - float64((tri.x[0]-cx)*(tri.y[2]-cy)),
				float64((tri.x[0]-cx)*(tri.y[1]-cy)) - float64((tri.x[1]-cx)*(tri.y[0]-cy)),
			}
			b0, b1 := e[0]/area, e[1]/area
			b2 := 1 - b0 - b1
			if tri.translucent {
				// Exact coverage: a translucent pixel must not be blended
				// twice on a shared edge.
				inside := true
				for k := 0; k < 3 && inside; k++ {
					v := sign * e[k]
					inside = v > 0 || v == 0 && owns(k)
				}
				if !inside {
					continue
				}
			} else {
				// Pixels on a shared edge belong to both triangles (the
				// depth test settles overlap), so rounding opens no crack.
				const edge = -1e-9
				if b0 < edge || b1 < edge || b2 < edge {
					continue
				}
			}
			o, d := rayGen.ray(px, row)
			denominator := d.Dot(tri.normal)
			if math.Abs(denominator) < 1e-18 {
				continue
			}
			t := v0.Sub(o).Dot(tri.normal) / denominator
			index := py*w + px
			if !(t > 0) || float32(t) >= depth.T[index] {
				continue
			}
			normal := tri.normal
			if len(mesh.Normals) == len(mesh.Vertices) {
				normal = mesh.Normals[tri.index[0]].Scale(b0).
					Add(mesh.Normals[tri.index[1]].Scale(b1)).
					Add(mesh.Normals[tri.index[2]].Scale(b2))
			}
			lit, specular := 1.0, 0.0
			if normal.Length() > 1e-12 {
				lit, specular = headlightShading(normal.Normalize(), d.Scale(-1), vrGeometryMaterial)
			}
			if len(mesh.Occlusion) == len(mesh.Vertices) {
				o := b0*float64(mesh.Occlusion[tri.index[0]]) + b1*float64(mesh.Occlusion[tri.index[1]]) + b2*float64(mesh.Occlusion[tri.index[2]])
				o = math.Max(0, math.Min(1, o))
				lit, specular = lit*o, specular*o
			}
			base := mesh.Color
			if len(mesh.Colors) == len(mesh.Vertices) {
				weights := [3]float64{b0, b1, b2}
				base = RGBA{A: 1}
				for k, index := range tri.index {
					base.R += weights[k] * float64(mesh.Colors[index][0])
					base.G += weights[k] * float64(mesh.Colors[index][1])
					base.B += weights[k] * float64(mesh.Colors[index][2])
				}
			}
			r, g, b := base.R*lit+specular, base.G*lit+specular, base.B*lit+specular
			if tri.translucent {
				a := mesh.Opacity
				depth.insertFragment(index, float32(t), [4]float32{float32(r * a), float32(g * a), float32(b * a), float32(a)})
				continue
			}
			depth.T[index] = float32(t)
			depth.R[index], depth.G[index], depth.B[index] = float32(r), float32(g), float32(b)
		}
	}
}

// SetGeometry composites opaque geometry into the VR frames rendered with
// this clip (#1068); nil removes it.
func (c *VRClip) SetGeometry(geometry *VRGeometry) {
	if c == nil {
		return
	}
	c.geometry = geometry
}

// Geometry returns the composited geometry.
func (c *VRClip) Geometry() *VRGeometry {
	if c == nil {
		return nil
	}
	return c.geometry
}

// geometryDepth rasterizes the clip's geometry for a frame, or returns nil.
func (c *VRClip) geometryDepth(ctx context.Context, cam VRCamera, w, h int) (*VRDepthImage, error) {
	if c == nil || c.geometry.Empty() {
		return nil, nil
	}
	return RasterizeVRGeometryContext(ctx, cam, w, h, c.geometry)
}

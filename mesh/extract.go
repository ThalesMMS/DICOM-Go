package mesh

import (
	"context"
	"fmt"
	"math"
	"sync"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
	"github.com/ThalesMMS/dicom-go/render"
)

// Method selects the surface construction.
type Method int

const (
	// MarchingCubes interpolates the iso-surface linearly along the grid
	// edges between samples: a smooth surface through the sample points.
	MarchingCubes Method = iota
	// VoxelFaces keeps the voxel-exact surface: the faces between voxels at
	// or above the iso value and their outside neighbours, half a voxel
	// from the sample points (render.Volume.ISOSurfaceTriangles as an
	// indexed mesh). It encloses exactly the inside voxels.
	VoxelFaces
)

func (m Method) String() string {
	switch m {
	case MarchingCubes:
		return "marching-cubes"
	case VoxelFaces:
		return "voxel-faces"
	}
	return fmt.Sprintf("Method(%d)", int(m))
}

const (
	// DefaultMaxTriangles is the triangle budget when Options.MaxTriangles
	// is zero.
	DefaultMaxTriangles = 25_000_000
	// DefaultMaxBytes is the output byte budget when Options.MaxBytes is
	// zero: 1 GiB.
	DefaultMaxBytes = 1 << 30
	// DefaultMaxWorkers caps the default worker count: each worker holds a
	// few slices, so more workers cost memory for little gain.
	DefaultMaxWorkers = 8
)

// Options configures Extract.
type Options struct {
	Method Method
	// Iso is the iso value in the field's unit (HU for a CT snapshot).
	// Samples at or above Iso are inside; non-finite samples are outside.
	Iso float64
	// OpenBoundary leaves a Marching Cubes surface open where it meets the
	// grid boundary, as VTK's vtkFlyingEdges3D does; an axis with a single
	// sample then has no cells and yields no surface. By default the grid is
	// padded with one layer of outside samples, which closes the surface
	// half a sample outside the boundary samples (where VoxelFaces puts its
	// boundary faces), so every surface is closed and a one-voxel-thick
	// volume yields a slab one voxel thick. VoxelFaces is always closed.
	OpenBoundary bool
	// SkipNormals leaves Mesh.Normals nil. Marching Cubes normals are the
	// interpolated central-difference gradients of the field (one-sided at
	// the grid boundary), mapped to patient space; VoxelFaces normals, and
	// Marching Cubes normals whose gradient vanishes, are the area-weighted
	// mean of the adjacent triangle normals.
	SkipNormals bool
	// Workers bounds the goroutines; 0 uses min(GOMAXPROCS, DefaultMaxWorkers).
	// Workers beyond the calling goroutine come from the module's shared
	// concurrency budget (GOMAXPROCS in total by default), so concurrent
	// extractions, filters and renders together stay within it.
	Workers int
	// MaxTriangles and MaxBytes bound the output (0 uses the defaults,
	// negative disables the bound). The exact counts are known before any
	// output is allocated, so an oversized surface fails fast with a
	// *BudgetError.
	MaxTriangles int64
	MaxBytes     int64
}

// ExtractVolume extracts the iso-surface of a render.Volume from its
// canonical snapshot: the regular grid that render.Volume.AcquireSnapshot
// provides, on which gantry-tilted and irregularly spaced series are
// resampled, with the patient LPS affine carrying direction and anisotropic
// spacing. The snapshot lease is held for the duration of the call only.
func ExtractVolume(ctx context.Context, volume *render.Volume, opts Options) (*Mesh, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if volume == nil {
		return nil, fmt.Errorf("%w: nil volume", ErrInvalidField)
	}
	lease, err := volume.AcquireSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lease.Release() }()
	snapshot, err := lease.Snapshot()
	if err != nil {
		return nil, err
	}
	field, err := SnapshotField(snapshot)
	if err != nil {
		return nil, err
	}
	return Extract(ctx, field, opts)
}

// Extract builds the iso-surface of field as an indexed mesh in patient LPS
// millimetres.
//
// The extraction streams the field slice by slice in three passes, after
// the Flying Edges organisation (Schroeder, Maynard and Geveci, 2015): the
// slices are split into contiguous slabs processed in parallel; the first
// pass counts, per slice, the vertices (one per grid edge the surface
// crosses, so neighbouring cells share them) and the triangles; a prefix sum
// then gives every slice its output offsets, so the second pass writes the
// vertices, normals and triangles in place, without locks and in an order
// independent of the worker count. Working memory is at most
// 48·(Dims[0]+2)·(Dims[1]+2) bytes per worker; the output takes
// Mesh.Bytes (24 bytes per vertex with normals, 12 per triangle). See the
// package documentation for the bounds of a 512×512×600 CT.
func Extract(ctx context.Context, field Field, opts Options) (*Mesh, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if field == nil {
		return nil, fmt.Errorf("%w: nil field", ErrInvalidField)
	}
	if math.IsNaN(opts.Iso) || math.IsInf(opts.Iso, 0) {
		return nil, fmt.Errorf("%w: iso value %v", ErrInvalidField, opts.Iso)
	}
	dims := field.Dims()
	if err := validateFieldDimensions(dims); err != nil {
		return nil, err
	}
	frame, err := newAffineFrame(field.IndexToPatient())
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = min(parallel.DefaultWorkers(), DefaultMaxWorkers)
	}
	switch opts.Method {
	case MarchingCubes:
		e := newCubesExtractor(field, frame, opts)
		return e.run(ctx, workers)
	case VoxelFaces:
		e := newVoxelExtractor(field, frame, opts)
		return e.run(ctx, workers)
	}
	return nil, fmt.Errorf("%w: method %v", ErrInvalidField, opts.Method)
}

func validateFieldDimensions(dims [3]int) error {
	for _, n := range dims {
		if n <= 0 || n > math.MaxInt32-2 {
			return fmt.Errorf("%w: dimensions %v", ErrInvalidField, dims)
		}
	}
	if plane := int64(dims[0]+2) * int64(dims[1]+2); plane > math.MaxInt32 {
		return fmt.Errorf("%w: slice of %dx%d samples", ErrInvalidField, dims[0], dims[1])
	}
	return nil
}

// budget checks the exact output size against the options.
func budget(opts Options, vertices, triangles int64, normals bool) error {
	return checkOutputBudget(opts, vertices, triangles, meshBytes(vertices, triangles, normals))
}

func checkOutputBudget(opts Options, vertices, triangles, bytes int64) error {
	maxTriangles, maxBytes := opts.MaxTriangles, opts.MaxBytes
	if maxTriangles == 0 {
		maxTriangles = DefaultMaxTriangles
	}
	if maxBytes == 0 {
		maxBytes = DefaultMaxBytes
	}
	if vertices > math.MaxUint32 || (maxTriangles > 0 && triangles > maxTriangles) || (maxBytes > 0 && bytes > maxBytes) {
		return &BudgetError{Vertices: vertices, Triangles: triangles, Bytes: bytes, MaxTriangles: maxTriangles, MaxBytes: maxBytes}
	}
	return nil
}

// slabFunc processes the layers [z0, z1).
type slabFunc func(ctx context.Context, z0, z1 int) error

// forEachSlab runs contiguous slabs [z0, z1) of n layers on up to workers
// workers (the calling goroutine plus helpers from the module's shared
// concurrency budget) claiming slabs from a shared cursor; newWorker is
// called once per worker, so its scratch is allocated once and owned by it.
// The slabs depend on workers, not on how many helpers the budget grants.
// The first error stops the others; the context is checked between slabs
// and by the work itself.
func forEachSlab(ctx context.Context, n, workers int, newWorker func() slabFunc) error {
	if n <= 0 {
		return ctx.Err()
	}
	slabs := min(n, workers*4)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		once     sync.Once
		firstErr error
	)
	fail := func(err error) {
		once.Do(func() { firstErr = err; cancel() })
	}
	_ = parallel.Run(ctx, slabs, workers, 1, func() func(int) {
		work := newWorker()
		return func(s int) {
			z0, z1 := s*n/slabs, (s+1)*n/slabs
			if err := work(ctx, z0, z1); err != nil {
				fail(err)
			}
		}
	})
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// prefix turns per-slice counts into offsets and returns the total.
func prefix(counts []int64) int64 {
	var total int64
	for i, c := range counts {
		counts[i] = total
		total += c
	}
	return total
}

// faceNormals sets every vertex normal selected by need (all when need is
// nil) to the normalised area-weighted sum of its triangles' normals.
func faceNormals(m *Mesh, need []bool) {
	acc := make([][3]float64, 0)
	index := make([]int32, len(m.Vertices))
	for i := range index {
		index[i] = -1
		if need == nil || need[i] {
			index[i] = int32(len(acc))
			acc = append(acc, [3]float64{})
		}
	}
	if len(acc) == 0 {
		return
	}
	for _, t := range m.Triangles {
		a, b, c := m.Vertices[t[0]], m.Vertices[t[1]], m.Vertices[t[2]]
		u := [3]float64{float64(b[0] - a[0]), float64(b[1] - a[1]), float64(b[2] - a[2])}
		v := [3]float64{float64(c[0] - a[0]), float64(c[1] - a[1]), float64(c[2] - a[2])}
		n := cross3(u, v)
		for _, k := range t {
			if j := index[k]; j >= 0 {
				acc[j] = add3(acc[j], n)
			}
		}
	}
	for i, j := range index {
		if j >= 0 {
			m.Normals[i] = unit32(acc[j])
		}
	}
}

func unit32(v [3]float64) [3]float32 {
	l := math.Sqrt(dot3(v, v))
	if l == 0 || math.IsNaN(l) || math.IsInf(l, 0) {
		return [3]float32{}
	}
	return [3]float32{float32(v[0] / l), float32(v[1] / l), float32(v[2] / l)}
}

// finite32 reports whether a float32 sample is finite.
func finite32(v float32) bool {
	f := float64(v)
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

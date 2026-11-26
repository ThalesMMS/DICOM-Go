// Package mesh extracts iso-surfaces of scalar volumes as indexed triangle
// meshes in DICOM patient LPS millimetres.
//
// Extract takes any Field (a render.VolumeSnapshot through SnapshotField,
// an in-memory DenseField, or an analytic FuncField) and ExtractVolume a
// render.Volume, whose canonical snapshot resolves gantry tilt and
// irregular slice spacing onto a regular grid; the index-to-patient affine
// carries the direction cosines and anisotropic spacing to every vertex.
//
// Two constructions are available:
//
//   - MarchingCubes: the smooth surface through the samples, with vertices
//     linearly interpolated on the grid edges the surface crosses. The case
//     table is generated in this package from a face-consistent rule (see
//     cases.go), so neighbouring cells always agree on their shared face
//     and the surface is watertight; by default the grid is padded with
//     outside samples so surfaces touching the boundary close there too.
//   - VoxelFaces: the voxel-exact surface render.Volume.ISOSurfaceTriangles
//     builds, as an indexed mesh with shared corners.
//
// Meshes can then be post-processed: SmoothWindowedSinc (non-shrinking
// smoothing; SmoothWindowedSincContext is cancellable, runs its passes in
// parallel and can keep feature edges sharp), DecimateQuadric (quadric-error edge collapse), Clean (vertex
// merging and removal of degenerate, duplicate and unused elements),
// ConnectedComponents and LargestComponent, and FillHoles. Area, Volume and
// CheckClosure measure them, AmbientOcclusion shades their vertices by how
// much of the surface hides their hemisphere, and WriteSTL, WritePLY,
// WriteOBJ and WriteGLB (binary glTF 2.0, several coloured parts in one
// asset) export them.
//
// Boolean computes bounded union, intersection and A minus B of closed,
// oriented solids in patient space, preserving face provenance and corner
// attributes. Ambiguous contacts and unsupported coplanar overlaps are
// refused. Merge retains its separate concatenation semantics.
//
// Distances on and between surfaces: a Locator finds the closest point of a
// mesh to any point, its signed distance and the first triangle a ray
// meets; SurfaceDistances compares two meshes vertex by vertex against the
// other's triangles (per-vertex and signed distances, area-weighted mean,
// RMS, 95th percentile and Hausdorff distance), for the quality control of
// smoothing and decimation; a GeodesicGraph gives the shortest path along a
// surface between two surface points and geodesic distance fields
// (Dijkstra and A* on the edges plus Steiner points, within about 1% of the
// exact geodesic).
//
// Label volumes (segmentations) have their own construction: ExtractLabels
// builds the multi-label SurfaceNets surface of a LabelField, one boundary
// per pair of touching labels shared by both, and LabelSurface.LabelMesh
// returns one label's closed, outward surface. Voxels of one label that
// touch only along an edge or at a corner get separate vertices, so every
// label's surface is a manifold except at the junctions of three or more
// labels that LabelSurface.UnresolvedCells counts.
//
// Every vertex is shared by the triangles that use it (one vertex per
// crossed grid edge, or per voxel corner), triangles are consistently wound
// with outward normals, and a closed surface uses each edge exactly twice in
// opposite directions. Vertex normals come from the field gradient.
//
// # Bounds
//
// The field is streamed a slice at a time by at most Options.Workers
// goroutines (default min(GOMAXPROCS, 8)); each holds four slices of
// samples and inside flags plus the vertex ids of two slices, at most
// 48 bytes per sample of the padded slice (Dims[0]+2)·(Dims[1]+2): 12 MiB
// for a 512×512 slice, 97 MiB for eight workers, whatever the slice count.
// A first pass counts the output exactly, so the triangle and byte budgets
// (DefaultMaxTriangles, DefaultMaxBytes) are enforced before the output is
// allocated. The output takes 24 bytes per vertex (position and normal,
// float32) and 12 bytes per triangle (uint32 indices); a Marching Cubes
// surface has about half as many vertices as triangles, so about 24 bytes
// per triangle. A 512×512×600 CT therefore needs, besides its snapshot,
// under 100 MiB of working memory and the mesh: the tests extract a
// 3.5-million-triangle surface of such a grid (80 MiB) with 151 MiB
// allocated in total, in about a second on four workers; the default
// budget of 25 million triangles caps the mesh near 600 MiB. When some
// Marching Cubes gradients vanish, and always for VoxelFaces, the normals
// are averaged over the triangles with a further 28 bytes per vertex of
// scratch. The context is checked between slices, so cancellation takes
// effect within one slice of work per worker.
//
// # Provenance
//
// The algorithms follow the published papers: W. E. Lorensen and H. E.
// Cline, "Marching Cubes: A High Resolution 3D Surface Construction
// Algorithm", SIGGRAPH 1987, and W. Schroeder, R. Maynard and B. Geveci,
// "Flying Edges: A High-Performance Scalable Isocontouring Algorithm", IEEE
// LDAV 2015 (the count, prefix-sum and fill passes, edge-owned vertices and
// slab parallelism). No VTK code or table is copied; VTK's
// vtkFlyingEdges3D results (testdata/upstream/vtk/mesh.json) are used only as
// numerical baselines in the tests.
package mesh

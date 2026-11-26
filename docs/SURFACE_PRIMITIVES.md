# Surface Segmentation primitives

`surfaceseg` preserves non-triangle primitives in the Surface Mesh Module of
Surface Segmentation Storage. Coordinates remain patient LPS millimetres;
indices are one-based in DICOM and zero-based in the Go model. The representation
follows [PS3.3 C.27.4](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.27.4.html).

| DICOM geometry | Go representation | Read / Write behavior |
| --- | --- | --- |
| Vertex Point Index List | `Surface.Primitives.Vertices []uint32` | Preserves explicit point indices and order |
| Edge Point Index List | `Surface.Primitives.Edges []Edge` | Preserves ordered pairs; incomplete pairs are invalid |
| Line Sequence | `Surface.Primitives.Lines []Polyline` | Preserves directed paths and item order; each path needs at least two indices |
| Triangle Point Index List | `Surface.Mesh.Triangles` | Preserves triangles and winding |
| Triangle strips / fans / facets | `Surface.Mesh.Triangles` | Existing triangulation remains; Write emits triangle lists |
| Vector Accuracy | `Surface.VectorAccuracy []float32` | Preserves one shared or three XYZ standard deviations for the normals |

All primitives share `Surface.Mesh.Vertices`. Repeated references and closed
paths are retained. Every referenced point must exist. `SegmentPrimitives`
merges referenced surfaces in segment reference order and remaps indices to
`SegmentMesh`'s combined coordinates. Segment descriptions, surface associations,
colours, opacity and source references are retained.

A populated Surface Points Normals Sequence contains one XYZ normal per
vertex in its single item. Optional Vector Accuracy (0066,0020) uses FL and
requires finite nonnegative values. Read treats an empty attribute as absent.
Write requires normals before it emits Vector Accuracy.
[PS3.3 C.27.1](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.27.html#sect_C.27.1) · [PS3.3 C.27.3](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.27.3.html)

Long OL lists are the default output. Set `Surface.RetiredIndexLists` to write
retired OW lists, with at most 65535 points. Read preserves all-retired output;
mixed representations normalize to Long. An empty Long attribute does not hide
a populated retired list. Two populated alternatives for the same list are
refused with `ErrInvalidObject` rather than choosing and losing one.

Point/line primitives imply no closed solid, area or volume. `TriangleParts`
returns `ErrNonTrianglePrimitives` if a triangle-mesh export would lose any
explicit vertex, edge or line geometry. Existing `Parts` remains the documented
mesh-only projection. STL accepts triangles; these APIs never triangulate a
line into a mesh. Voxelization of point/line primitives is outside this profile.

## Geometry bounds

`ReadWithLimits` accepts lower limits; zero uses the defaults and a negative
limit is invalid. Higher limits are clamped to the defaults. Preflight happens
before copying coordinate/index arrays and includes the triangle expansion of
strips, fans and facets. `Write` checks equivalent default bounds before
constructing encoded arrays.

| Limit | Default |
| --- | --- |
| Points | `MaxReadPoints = 1 << 26` |
| Triangles | `MaxReadTriangles = 1 << 27` |
| Primitive count, including triangles | `MaxReadPrimitives = 1 << 27` |
| Point indices | `MaxReadIndices = 1 << 29` |
| Geometry bytes | `MaxReadGeometryBytes = 256 << 20` |

The byte budget charges encoded geometry payloads and reconstructed numeric
arrays, including Vector Accuracy, as well as line slice headers. It excludes
the caller-owned Object, unrelated patient/segment metadata and decoder
internals. Budget failures wrap `ErrResourceLimitExceeded`. Malformed
geometry wraps `ErrInvalidObject` and returns no partial document.


The shared study adapter retains point-only, line-only and mixed segments and
maps every coordinate into the same VR frame as triangles. Both MacStyle and
WindowsStyle's **Imported surfaces in 3D** action report each affected segment's
point, edge, polyline and triangle counts. The message identifies unavailable
GPU presentation of non-triangle primitives and the triangle GPU prepass, plus
the limitation of triangle-mesh exports. Original DICOM export preserves the
encoded primitives. The application performs no CPU 3D rendering or implicit
triangulation for this feature.

## Offline evidence

The library's synthetic tests cover point-only, line/edge-only and mixed
surfaces with Long and retired index lists, exact round trips, metadata
association, malformed geometry, lower budgets, hostile declared point counts,
and the 65535-point boundary. Existing triangle, strip, fan, facet and mesh
export tests remain in the same package:

```sh
go test ./surfaceseg -count=1
```


```sh
go test ./internal/viewer/study -run '^(TestImportedNonTriangleSurfaces|TestSurfaceSegmentationExportImportAndVR)' -count=1
go test ./internal/ui/macstyle/viewer ./internal/ui/windowsstyle -run '^TestVRImported(NonTriangleSurfacesDiagnostic|SurfacesToggle)$' -count=1
```

These tests run on Windows/amd64 with Fyne's software test driver. They prove
adaptation and diagnostic behavior; they do not qualify native macOS/Windows
GUI execution or GPU primitive presentation.

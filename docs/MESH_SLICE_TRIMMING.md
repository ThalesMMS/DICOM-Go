# Marching Cubes slice trimming

Marching Cubes caches the occupied sample rectangle of each slice alongside
the existing inside flags. An edge crossing or a non-uniform cell has at
least one inside corner. The extraction uses the slice rectangle for XY
edges and the union of two adjacent rectangles for Z edges and cells. It
includes the preceding row and column wherever a cell or edge can start
outside the rectangle, then clips its scan to the available grid.

Counting and generation visit all candidates in the original row-major
order. Field reads, padding, interpolation, normals, shared vertex IDs,
output budgets and cancellation checks use the existing paths. Triangle
generation binds the vertex ID slice of each case-table edge once per
layer, moving the axis selection outside the per-triangle loop. The four
cached rectangles per worker require no separate allocation or larger
slice buffers. The extraction still supports streaming `Field` inputs.
The previous row trimming experiment measured cell classification and
triangle emission. [Marching Cubes row trimming experiment](MESH_TRIMMING_EXPERIMENT.md)

## Performance

Measurements used Windows 11/amd64, an Intel i7-14700K, Go 1.26.4 and
VTK 9.5.2. The first bounds-only candidate produced mixed dense-field
results, including a 20.7% slower block with four workers and normals.
The final candidate includes the generation change above.

The Go qualification alternated baseline and candidate on every pair in
a single process, with one warm-up pair and 30 measured pairs per case.
Both used the same field and work budget. The table gives elapsed
medians, with the percent change computed from the ratios in each
execution order independently. Negative values indicate reduced time.

| Field | Workers | Normals | Baseline ms | Trimmed ms | Median change by order |
| --- | ---: | --- | ---: | ---: | ---: |
| Sparse | 1 | off | 70.484 | 20.296 | −71.0% / −71.3% |
| Sparse | 1 | on | 70.198 | 21.523 | −69.0% / −69.9% |
| Sparse | 4 | off | 19.693 | 6.330 | −67.8% / −68.4% |
| Sparse | 4 | on | 20.517 | 6.706 | −67.0% / −68.0% |
| Larger sphere | 1 | off | 74.975 | 37.378 | −49.9% / −50.0% |
| Larger sphere | 1 | on | 83.008 | 44.213 | −47.7% / −46.8% |
| Larger sphere | 4 | off | 22.002 | 10.922 | −49.5% / −49.7% |
| Larger sphere | 4 | on | 24.611 | 13.342 | −46.7% / −44.7% |
| Dense noise | 1 | off | 16.250 | 10.815 | −29.7% / −33.3% |
| Dense noise | 1 | on | 31.305 | 25.660 | −18.7% / −16.2% |
| Dense noise | 4 | off | 6.394 | 4.872 | −29.0% / −26.2% |
| Dense noise | 4 | on | 10.696 | 9.594 | −9.3% / −18.0% |

An external comparison used `Extract` and `vtkFlyingEdges3D.Update`
on the same resident inputs and timed 13 extractions after a warm-up per
block. The two blocks reversed implementation order. VTK used
Sequential for one worker and STDThread for four. The one-worker test
used logical processor 2, and the four-worker test used processors
2, 6, 10 and 14 on distinct performance cores. Windows ran other
builds during the campaign, and the elapsed times varied between blocks.

| Field | Workers | Normals | Go baseline ms | Go trimmed ms | VTK ms |
| --- | ---: | --- | ---: | ---: | ---: |
| Sparse | 1 | off | 38.975 / 76.237 | 19.868 / 35.149 | 14.875 / 6.761 |
| Sparse | 1 | on | 169.491 / 149.711 | 56.970 / 33.121 | 11.097 / 6.186 |
| Sparse | 4 | off | 62.243 / 39.112 | 20.811 / 21.337 | 4.236 / 5.028 |
| Sparse | 4 | on | 152.917 / 92.410 | 17.067 / 12.900 | 4.350 / 7.141 |
| Larger sphere | 1 | off | 115.976 / 104.035 | 72.716 / 58.182 | 29.813 / 11.650 |
| Larger sphere | 1 | on | 164.359 / 99.634 | 113.626 / 81.472 | 19.093 / 22.873 |
| Larger sphere | 4 | off | 32.304 / 63.518 | 29.941 / 27.419 | 7.407 / 7.384 |
| Larger sphere | 4 | on | 42.217 / 52.877 | 20.542 / 29.899 | 8.083 / 10.430 |
| Dense noise | 1 | off | 15.559 / 17.776 | 17.589 / 10.549 | 13.228 / 10.143 |
| Dense noise | 1 | on | 40.536 / 39.973 | 35.690 / 30.101 | 14.303 / 16.459 |
| Dense noise | 4 | off | 13.168 / 7.561 | 6.268 / 5.902 | 3.978 / 4.698 |
| Dense noise | 4 | on | 25.239 / 17.974 | 13.210 / 12.704 | 4.668 / 4.946 |

### Phase and memory measurements

Instrumentation ran separately from the timing matrix. With one worker
and normals off, the sparse field's count phase fell from 34.82 ms to
10.19 ms and generation from 36.29 ms to 9.96 ms. Aggregate time in
`Field.ReadSlice` varied from 4.49 ms to 5.16 ms across those blocks.
The dense noise count stayed around 2.1 ms while generation fell from
15.63 ms to 8.04 ms. A 160-plane prefix microbenchmark measured
1.45–2.23 µs and 3,840 bytes per operation across both builds.

CPU profiles covered 35 extractions per mode. Sparse `cellCase` samples
fell from 690 ms to 10 ms, with 750 ms attributed to reading and
preparing the cached planes in both versions. On dense noise, the
cumulative samples in triangle emission fell from 310 ms to 40 ms.
The isolated `BenchmarkCubesTrimBaseline` cases also measured the
unchanged 96³ field copy at 0.093–0.104 ms per volume and the
unbounded classifier at 2.48–3.33 ms.

In the 96³ dense benchmark, the candidate used 640 more bytes per
operation in both normal modes, with the same 90 or 92 allocations.
The highest Go working-set peak over three fresh processes per matrix
case was 52.29 MiB before and 52.24 MiB after. The absolute difference
in any Go case was at most 0.12 MiB. VTK's Python processes peaked
between 69.92 and 93.77 MiB, including the Python and native library
setup. The 512×512×600 Go memory test allocated 116 MiB against a
161 MiB limit while producing 3,522,832 oriented triangles.

## Validation

The comparison started at `7a2ea876` and used identical resident float32
fields for Go and `vtkFlyingEdges3D`. Both 160³ sphere inputs use
`r² − ((x−79.5)² + (y−79.5)² + (z−79.5)²)`, radii 17 and 58, iso 0,
open boundaries and 0.75 × 0.85 × 1.4 mm spacing. A third field used a
48³ uniform noise grid over [−1, 1] with NumPy's default generator and
seed 1681. A 48³ exact plane on a reflected patient-LPS affine checked
orientation and open grid boundaries.

All four fields had byte-identical Go vertex, triangle and normal arrays
before and after trimming with 1 and 4 workers. The closed spheres
had 5,472/63,480 vertices and 10,940/126,956 triangles in Go and VTK.
Area and volume matched within `2e-8` relative error. Every edge was
manifold and consistently oriented, and neither library emitted a
degenerate triangle. Bidirectional distances from the sampled vertices
and triangle centroids to the other surface stayed within 0.000031 mm.

The dense noise produced 162,556 vertices, 332,407 triangles, 13,293
boundary edges and 1,198 components in both libraries. Their Euler
characteristics and manifold-edge checks agreed. Of the combined face
sets, 343,278 unoriented triangles appeared in only one set. Nearest
triangle centroid samples differed by up to 0.4591 mm, even though
the edge vertices coincided within float32 precision. The two
extractors' triangulations have distinct geometry on ambiguous input.

The reflected open plane had 214 boundary edges and 36 zero-area
triangles at exact iso values in both extractors. Go's 5,640 nondegenerate
faces and 2,946 vertex normals pointed outward in patient LPS. VTK's
nondegenerate faces and vertex normals pointed inward for this reflected
affine. Distances from sampled points stayed within 0.000004 mm.
The closed-surface and nonfinite-sample Go checks also passed.

The scoped Go checks can be reproduced from the module directory.

```sh
go test ./mesh -short -run '^(TestMarchingCubes|TestCubesTrim|TestVoxelFacesMatches)' -count=1
go test ./mesh -run '^TestLargeCTWithinDocumentedMemory$' -count=1
go test ./mesh -run '^$' -bench '^BenchmarkCubesTrimBaseline$' -benchmem -benchtime=120ms -count=5 -cpu=2
```

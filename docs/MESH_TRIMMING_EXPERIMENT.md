# Marching Cubes row trimming experiment

The baseline is revision `d62a5140c67f281322bcaf0450daef0dd75b65cc`.
The experiment preserves the public mesh API, case table, interpolation,
normal computation, edge IDs, prefix offsets, slab ownership and output budgets.
The independent reference is the existing eight-flag `cellCase` classifier and
the versioned VTK 9.6.2 numerical fixtures in `testdata/upstream/vtk/mesh.json`.

## Criteria declared before measurement

Both inputs are 96³ `DenseField` volumes, iso 0, closed boundaries, workers 2
and `GOMAXPROCS=2`. Sparse is a radius-10 sphere centred at (47.5,47.5,47.5);
dense is uniform noise in [-1,1], random seed 1495. Construction is excluded
from benchmark time, allocations and the CPU profiles; process RSS includes it.
Extraction is measured with and without normals. Five samples are collected.

Promotion requires bitwise identical output on the geometry/cancellation/budget
corpus, at least 10% less median sparse extraction time, no more than 5% median
dense regression, allocation bytes no more than baseline +5%, and peak RSS no
more than baseline +max(5%,4 MiB). The live scratch payload must stay within the
existing 48 bytes per padded-plane sample per worker. Failing a gate means the
experiment is retained as a reproducible negative result without promotion.

First measure `Field.ReadSlice`, classification of already loaded flags, the
entire counting pass, extraction without normals, and extraction with normals.
Read instrumentation records calls/samples/time separately: its timings are
diagnostic and are not used to claim a performance gain. CPU profiles identify
the proportion attributable to classification, emission, read and normals.
Baseline and candidate use the same standalone test binary commands, inputs and
limits. Builds and measurements run in an exclusive root-coordinated slot;
other agents may still run lightweight tests on this shared host.

## Bounded candidate

The public [VTK Flying Edges contract](https://vtk.org/doc/nightly/html/classvtkFlyingEdges3D.html)
describes packed cases and computational trimming as ways to avoid repeated
work. This experiment derives its classifier from this package's corner bits;
it does not import upstream implementation code or tables.

The test-only row classifier reuses the four bits of the preceding x column,
reads four new bits, and forms the same eight-bit cube case. A worker keeps a
single byte per cell in one layer and one active interval per row. The interval
includes every nonempty/nonfull case, including crossings solely on y/z edges.
It never skips output edge numbering, reads a volume-sized case cache, or skips
padding of an entirely inside field. Later triangle emission reuses those cases
and skips rows and x prefixes/suffixes without a surface. Holes within an active
interval are still checked.

## Results and decision

**Do not promote this candidate.** The sparse extraction without normals
improves by 6.55%, below the declared 10% minimum. Sparse extraction with normals
barely reaches that minimum (10.005%); that does not approve the other mode or
establish a robust general improvement. The production `Extract`, `cubes.go`
and mesh API remain unchanged. The private candidate and benchmarks are kept
to make this negative result reproducible; they are not a selectable backend.


| Corpus / phase | Baseline ms/op | Candidate ms/op | Time change |
| --- | ---: | ---: | ---: |
| Sparse / Field.ReadSlice | 0.045141 | 0.043382 | -3.90% |
| Sparse / flags classification | 0.780743 | 0.509440 | -34.75% |
| Sparse / entire count pass | 1.503644 | 1.303013 | -13.34% |
| Sparse / extraction, no normals | 3.151408 | 2.945054 | -6.55% |
| Sparse / extraction, normals | 3.340762 | 3.006512 | -10.005% |
| Dense / Field.ReadSlice | 0.044060 | 0.043533 | -1.20% |
| Dense / flags classification | 0.824151 | 0.554963 | -32.66% |
| Dense / entire count pass | 5.584770 | 5.279706 | -5.46% |
| Dense / extraction, no normals | 35.326910 | 35.399708 | +0.206% |
| Dense / extraction, normals | 66.362271 | 66.455844 | +0.141% |

The read benchmark is the same code on both paths, providing a noise control;
its small change is not a read optimization. Classification excludes field
reads and emission, so its improvement must not be presented as an extraction
improvement. CPU profiles corroborate the other costs: on the dense baseline,
vertex generation accounts for 53.89% cumulative CPU samples, gradients 23.61%
and triangle emission 25.00%. These overlapping cumulative percentages must
not be added. The sparse profile is dominated by runtime pthread/scheduling
samples; compare unprofiled wall-time measurements rather than treating sampled
CPU percentages as wall-time savings.

| Corpus / extraction | Baseline B/op | Candidate B/op | Baseline peak RSS MiB | Candidate peak RSS MiB |
| --- | ---: | ---: | ---: | ---: |
| Sparse / no normals | 1,467,155 | 1,488,594 | 20.016 | 20.000 |
| Sparse / normals | 1,493,781 | 1,515,218 | 20.078 | 20.344 |
| Dense / no normals | 51,692,317 | 51,713,757 | 112.938 | 112.875 |
| Dense / normals | 69,133,088 | 69,154,528 | 128.547 | 128.562 |

RSS is the maximum of five separate processes per path/corpus/normal mode;
each process performs three extractions. It includes the input, Go runtime,
output, garbage and scratch, unlike the logical worker payload. Every memory
gate passes: B/op increases by at most 1.462%, the largest RSS increase is
0.266 MiB, and per-worker slice payload is 421,024 bytes baseline versus
431,412 bytes candidate. For arbitrary supported cell grids, with padded
plane P=nX*nY, candidate payload is 41P+4nx*ny+8nY when closed and 41P+8nY
when open. Closed nX>=3 bounds this below (45+8/3)P<48P; open nX>=2 bounds
it by 45P. Empty open grids return before constructing workers. Counts/prefix
vectors keep the existing O(nZ) overhead; fallback normals keep the existing
additional 28 bytes per vertex. No volume-sized case cache is retained.

The sparse surface has 1,896 vertices / 3,788 triangles, and the dense surface
has 1,341,256 vertices / 2,849,920 triangles. The candidate adds ten allocations
per extraction (baseline 90 without normals, 92 with normals). The isolated
classifier gain does not justify those allocations, another layer scan,
scratch and maintenance when one extraction mode fails the promotion gate.

## Geometry and lifecycle qualification

`TestCubesTrimAllCases`, `TestCubesTrimGeometry`, `TestCubesTrimVTKFixtures` and
`TestCubesTrimScratchAndBudget` perform 647 bitwise mesh comparisons against
the current implementation: every case with open/closed boundaries; 15 fields
including anisotropic/oblique spheres, mirrored ambiguity noise, torus,
constant outside/on-iso/inside, x/y/z planes and thin volumes, clipped plane
and nonfinite boundary samples; workers 1/3 and normals enabled/disabled.
Byte-identical vertices, normals and triangle indices imply zero distance
between the surfaces and identical orientation, degeneracies and topology.
Every nonempty closed corpus surface also passes the independent edge-pair
manifold check. Sphere area=450.3318068 mm² / volume=896.9965005 mm³ / Euler=2;
torus area=472.6780719 mm² / volume=469.8710181 mm³ / Euler=0. Both analytic
area/volume checks pass within 1%. Sphere/ellipsoid VTK 9.6.2 fixtures retain
their exact triangle counts and area/volume agreement within 0.1%.

Exact output budgets reject before output allocation. Counting-phase blocked
read cancellation and generation-phase cancellation/I/O faults return a nil
mesh only after every reader has joined. Five blocked-read repetitions measure
join tails of 5.750–108.750 µs baseline / 4.583–11.041 µs candidate; these
synthetic read waits prove ownership and teardown, not dense CPU cancellation
latency. An independent read-only review found no classification, trimming,
scratch, prefix or worker-ownership defect. It did not replace functional tests
or the declared performance gates.

Focused race tests for all cases, geometry and count/generation failure paths
pass. A separate five-sample cancellation test uses the real 96³ inputs with
normals enabled and a monotonic clock. It cancels immediately after actual
slice read 8 (counting) or 192 (generation: counting reads at most 103 slices),
returns that successful read normally, and measures until extraction returns.
Thus the current layer's CPU work must finish and join, rather than merely
propagating an injected read error. Every return has a nil mesh and no active
reader. These tails include scheduling noise and are not a hard deadline.

| Corpus / cancellation phase | Baseline median µs | Candidate median µs | Baseline max µs | Candidate max µs |
| --- | ---: | ---: | ---: | ---: |
| Sparse / count | 31.583 | 27.458 | 65.334 | 32.166 |
| Sparse / generation | 32.333 | 32.291 | 36.959 | 34.541 |
| Dense / count | 110.417 | 107.542 | 112.625 | 112.708 |
| Dense / generation | 638.875 | 684.875 | 699.083 | 1,501.042 |

The dense generation tail increases by 46 µs in the median and has a larger
maximum sample. No cancellation speedup is claimed. The same per-layer context
checks and shared worker admission are preserved; the negative promotion
decision already follows from the extraction gate.

## Reproduction

From `dicom-go/`, use a private output directory. Benchmarks and RSS runs
should be serialized against other heavy work. The baseline always calls
production `Extract`; the candidate calls the private `trimPrototypeRun`.

```sh
MESH_TRIM_OUTPUT=$(mktemp -d /tmp/dicomgo-mesh-trim.XXXXXX)
GOMAXPROCS=2 go test -p 2 ./mesh -run '^TestCubesTrim(BaselineInstrumentation|AllCases|Geometry|VTKFixtures|ScratchAndBudget|CancellationJoin|GenerationFailureAtomic)$' -v
GOMAXPROCS=2 go test -p 2 -race ./mesh -run '^TestCubesTrim(AllCases|Geometry|CancellationJoin|GenerationFailureAtomic)$' -count=1 -v
GOMAXPROCS=2 go test -p 2 ./mesh -run '^TestCubesTrimCancellationLatency$' -count=5 -v
GOMAXPROCS=2 go test -p 2 -c ./mesh -o "$MESH_TRIM_OUTPUT/mesh.test"
GOMAXPROCS=2 "$MESH_TRIM_OUTPUT/mesh.test" -test.run '^$' -test.bench '^BenchmarkCubesTrim(Baseline|Candidate)$' -test.benchmem -test.benchtime=200ms -test.count=5 > "$MESH_TRIM_OUTPUT/paired-bench.log"
for variant in Baseline Candidate; do
  for corpus in sparse dense; do
    for normals in ExtractNoNormals ExtractNormals; do
      for sample in 1 2 3 4 5; do
        GOMAXPROCS=2 /usr/bin/time -l "$MESH_TRIM_OUTPUT/mesh.test" -test.run '^$' -test.bench "^BenchmarkCubesTrim$variant/$corpus/$normals$" -test.benchtime=3x -test.count=1 > "$MESH_TRIM_OUTPUT/$variant-$corpus-$normals-$sample.log" 2> "$MESH_TRIM_OUTPUT/$variant-$corpus-$normals-$sample.time"
      done
    done
  done
done
for variant in Baseline Candidate; do
  GOMAXPROCS=2 "$MESH_TRIM_OUTPUT/mesh.test" -test.run '^$' -test.bench "^BenchmarkCubesTrim$variant/sparse/ExtractNoNormals$" -test.benchtime=2s -test.cpuprofile="$MESH_TRIM_OUTPUT/$variant-sparse.cpu"
  GOMAXPROCS=2 "$MESH_TRIM_OUTPUT/mesh.test" -test.run '^$' -test.bench "^BenchmarkCubesTrim$variant/dense/ExtractNormals$" -test.benchtime=2s -test.cpuprofile="$MESH_TRIM_OUTPUT/$variant-dense.cpu"
done
go tool pprof -top "$MESH_TRIM_OUTPUT/mesh.test" "$MESH_TRIM_OUTPUT/Baseline-dense.cpu"
```

`/usr/bin/time -l` is the macOS RSS command. Use an equivalent process peak-RSS
measurement when reproducing on another OS and record that environment; do not
compare raw units without checking the tool's definition.

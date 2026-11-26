# Synthetic PCA shape qualification corpus

This folder contains project-authored analytic numerical fields, under the
library's [Apache-2.0 license](../../../LICENSE). There are no patient images,
reference application assets or copied upstream solver implementations. The
ITK probe only calls public SDK APIs. It qualifies a numerical representation,
not anatomy or clinical segmentation.

The [research API and limits](../../../docs/PCA_SHAPE_PRIOR.md) describe the
normalization, physical geometry, weight, residual and evolution contracts.

## Fixed corpus

The Go analytic family uses circles of radii 2, 3, 4, 5 and 6 mm on a 41×41 grid
with 0.5×0.8-mm spacing and explicit patient origin. Radius 4.75 mm is held out.
True analytic circle distance fields permit exact mean/eigenvalue/basis and
projection checks. The prior-only 150-step evolution starts from radius 2 mm;
its resulting held-out mask has Dice=1. Weight zero returns the complete base
solver result unchanged.

[generate.py](generate.py) fixes eight 3D sphere specifications and three unseen
test fields before running Go. The 11×9×7 grid has 0.8×1.2×1.4-mm spacing,
nonzero world origin and a 0.3-radian physical rotation. A NumPy SVD retains six
components. Its [compressed numerical receipt](numpy.json.gz) records every
training field/specification, mean, basis, sigmas, two held-out spheres, an unseen
elongation, reconstruction/coefficients/residuals, NumPy version and generator
hash. Held-out samples are never used to train the model or select rank/thresholds.

The fixed 0.1-mm RMS and 3-SD review thresholds flag the elongation: residual
0.2787158 mm, maximum coefficient 95.3546913 SD. The two unseen sphere residuals
are 0.05759258 and 0.02881842 mm. These thresholds are explicit numerical review
criteria for this corpus, with no calibration as anatomical confidence.

`itk.json.gz` records 15 real ITK **5.4.6** evaluations: mean and two held-out
coefficient vectors, five off-grid world points, an explicit rigid world-to-model
pose, and the rotated anisotropic grid. It includes the independent NumPy model,
all actual SDK output values and source/executable hashes. The largest Go/native
delta is 1.3322676295501878e-15 mm (fixed acceptance 1e-10 mm). The oracle does
not establish equivalence with ITK's whole sparse-field/MAP shape-prior solver.

## Reproduce

Run `python generate.py` with NumPy to regenerate its receipt (recorded NumPy
2.3.5). The project tests compare reconstruction/projected subspaces and sigmas,
so a harmless eigenvector sign change is not mistaken for a different model.

For the native opt-in oracle, use an explicitly installed **ITK 5.4.6** SDK and
build this project-authored probe into a separate output directory:

```sh
cmake -S roi/testdata/pca_shape -B /path/to/pca-oracle-build -G Ninja \
  -DCMAKE_BUILD_TYPE=Release -DITK_DIR=/path/to/lib/cmake/ITK
cmake --build /path/to/pca-oracle-build -j 4
```

Run from the library module. On this Windows machine UCRT64 GCC/Ninja and
`C:/msys64/ucrt64/lib/cmake/ITK` were used. Keep the SDK DLL directory on PATH.
Set `DICOM_GO_PCA_SHAPE_ITK_ORACLE` to `pca-shape-oracle.exe`, then:

```sh
go test ./roi -run '^TestPCAShapeITKRepresentationAndPose$' -count=1 -v
```

Optionally set `DICOM_GO_PCA_SHAPE_ITK_RECEIPT` to a separate `.json.gz` output
path to record the actual SDK run. Review that receipt before replacing the
frozen fixture. Default tests never discover, build, execute or download ITK.

## Validation and measured cost

Commands executed on Windows 11 amd64, Intel Core i7-14700K, Go 1.26.4:

- `go test -race ./roi -run '^TestPCAShape' -count=1 -v`: passed analytic,
  independent NumPy, ownership/concurrency, malformed model/geometry/basis,
  limits, precancel and in-flight train/fit/evaluate/evolve checks. The native
  test skips unless its executable is configured; frozen results run offline.
- `go test -race ./roi -run '^(TestPCAShape|TestEdgeLevelSet|TestShape|TestLevelSet)' -count=1`:
  passed affected base-solver and new-contract regressions (73.739 s).
- Explicit native ITK representation/pose gate: passed all 15 comparisons.
- `make -k check`: formatting, vet, build, documentation, codec policy,
  optional-adapter, conformance and manifest gates passed. The module test
  baseline still fails with the same 33 pre-existing test failure identities
  recorded before this issue; no new failure identity appeared. Documentation
  validation covered 226 Markdown files, 681 local links and 301 anchors.
- `go test ./roi -run '^$' -bench '^BenchmarkPCAShape' -benchmem -benchtime=200ms -count=3 -cpu=1`:
  measured without concurrent heavy builds; individual rows are stored in
  [benchmark.txt](benchmark.txt).

| CPU workload | Range ns/op | Go B/op | Allocs/op |
|---|---:|---:|---:|
| Train 8 spheres / 693 voxels / 6 components | 146056–147156 | 88184 | 31 |
| Fit held-out sphere on that model | 5415–5534 | 112 | 2 |
| Prior-only 41×41 circle evolution / 150 steps | 1874528–1919037 | 278768 | 949 |

These are local CPU research workloads. Go allocated bytes are not measured RSS
or native memory; the ITK helper was used for correctness, not a timing contest.
No GPU, native macOS, application UI or patient/PACS qualification is claimed.

# Bounded file batches and complete volume preparation

`object.OpenFilesContext` loads an explicit ordered list of Part 10 files with a
caller-selected worker ceiling. Helpers come from the same process-wide budget
as the library's CPU kernels; an exhausted budget uses the calling goroutine.
It preserves `ReadFileOptions`, including exact deferred value locations and
source ownership. It does not discover directories, select a Series UID or
infer a volume. Callers must bound the list and aggregate retained data, set
per-file limits for untrusted files and close every successful result.

Any parse error or cancellation joins active reads, closes all opened sources
and returns no partial batch. The first failing input index determines the
parse error regardless of completion order. Cancellation is checked between
reads; a blocking filesystem read must finish before the batch returns.
Mutable per-file streaming sinks are refused with `ErrSharedFileSink`. Options,
descriptors and custom dictionaries must support concurrent reads; select one
worker for a stateful dictionary.

Complete `render.Volume` preparation fills disjoint packed signed/unsigned
16-bit or float32 planes using the shared helper budget. Volumes below 65,536
voxels stay serial. Each destination is published only after every helper has
joined and all slices succeeded. Cancellation/failure drops the whole attempt;
the existing canceled-build retry remains available. Source frames must remain
immutable during preparation, as before. Source-order decode errors and stored
samples, rescale, geometry, descriptor and canonical generation are preserved.

Progressive preparation keeps its ordered slice/band/preview publication
contract. Twin already has a bounded directory loader; this change does not
replace its catalog/warning/recovery policy with the all-or-nothing file helper.
The application keeps GPU-only 3D rendering and its existing bands-only loading
policy. Parallel CPU data preparation does not authorize CPU 3D rendering.

## Measured Windows profile


| Median phase, ms | Before, round 1 / 2 | Parallel, round 1 / 2 | VTK, round 1 / 2 |
|---|---:|---:|---:|
| File load and frame adaptation | 11.6033 / 12.0639 | 4.8480 / 4.9135 | 26.5827 / 26.3022 |
| Complete geometry/preparation | 27.7243 / 27.5594 | 8.4480 / 8.2123 | Included in reader |
| Scalar reslice | 2.3091 / 2.2722 | 2.0532 / 2.0368 | 1.6903 / 1.8920 |
| End-to-end | 41.6093 / 42.1834 | 15.3491 / 15.1283 | 28.6621 / 28.3283 |

The local end-to-end median fell 63.1% / 64.1%; the candidate is faster than
this VTK reference on both rounds. Phase medians need not sum to total medians.
An additional candidate run retaining serial file loading measured 22.9501 ms,
separating the contribution of parallel volume filling from file concurrency.
Go median total allocation was 53,577,808 → 53,581,584 bytes in round 1 and
53,578,112 → 53,581,120 in round 2: a few KiB of scheduling/error state, not a
second volume allocation. These allocation counters are not peak RSS.

All before/candidate/native-VTK canonical volumes have SHA-256
`194034ce795e8429aa2096763a20cd520c9c31e093557bc4dd27b07d1d87093b`.
All 262,144 Go scalar outputs match the analytical plane to `1e-8`; VTK's
quantized linear interpolation has maximum absolute error `4.524e-5`, admitted
under the explicit `1e-4` stored-value tolerance. VTK's reader reverses Y/Z for
this fixture; the timed reslice maps the patient plane to those coordinates.
Axis-normalization and all hashing/verification occur outside the timed path.
Both routes include their own file read/preparation and reslice, excluding
process startup, serialization and teardown. They have different parser/layout
contracts, so the comparison qualifies this fixture and workload only.


## Reproduction and checks

Run from the library with a private fixture directory and report path:

```powershell
$env:GOMAXPROCS = '4'
$env:DICOM_VOLUME_LOAD_CAMPAIGN = '1'
$env:DICOM_VOLUME_LOAD_FIXTURES = Join-Path $env:TEMP 'own-volume-load-fixtures'
$env:DICOM_VOLUME_LOAD_REPORT = Join-Path $env:TEMP 'own-volume-load.json'
$env:DICOM_VOLUME_LOAD_MODE = 'parallel' # 'serial' selects serial file reads
go test ./render -run '^TestVolumeLoadCampaign$' -count=1 -v
py -3.13 scripts/measure_volume_load_vtk.py --fixtures $env:DICOM_VOLUME_LOAD_FIXTURES --report (Join-Path $env:TEMP 'own-volume-load-vtk.json')
```

The Go opt-in test validates existing fixture bytes before reuse and refuses
to overwrite another corpus. Windows measurements use QueryPerformanceCounter
(10 MHz here), avoiding zero sub-tick samples from the observable runtime clock.
The VTK script uses Python's QPC-backed `perf_counter_ns`; VTK/NumPy are opt-in
dependencies and no native package is installed by the library baseline. Its
API mapping follows [VTK's DICOM reader](https://vtk.org/doc/nightly/html/classvtkDICOMImageReader.html)
and [linear reslice contract](https://vtk.org/doc/nightly/html/classvtkImageReslice.html).

Focused default and race checks cover ordered materialized/deferred reads,
shared-budget ceilings, deterministic errors, partial-source cleanup,
cancellation/join, packed/float snapshot identity, corrupt-plane refusal,
progressive bands and concurrent canonical preparation. Affected Twin
MPR/loading/GPU-policy callers are checked separately. These are functional and
numerical library results, without clinical or macOS runtime qualification.

Current local validation passed the focused selectors
`^TestOpenFilesContext|^TestParallelVolume|^TestPacked16Snapshot|^TestVolumePreparation|^TestPrepareSnapshot|^TestRetainedVolumeBand|^TestParallelRows|^TestN4AndVRShareTheConcurrencyBudget`
with `go test -race ./object ./render -count=1`, plus the Twin study selector
from `TestMPRVolume`/`TestPrepareMPRVolume` through bands-only, directory loading
and GPU-only policy tests with `go test -race ./internal/viewer/study -count=1`.
The current library `make check` passed formatting/vet but failed on the same
33 existing baseline tests; the previously intermittent audit test passed this
run. No new failing test identity appeared. Build, documentation (218 Markdown,
654 links, 301 anchors, 25 help commands), codec policy, optional dependency-free
adapters, conformance and manifest checks passed separately after the test gate
stopped the baseline. This is not a passing full module baseline.

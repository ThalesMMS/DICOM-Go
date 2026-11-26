# Bounded local GLCM maps

`roi.VolumeMask.LocalTexture` produces one selected research feature per
finite masked centre, independent of the existing region-wide GLCM/GLRLM
statistics. It implements published symmetric co-occurrence formulas without
copying an upstream filter. It does not provide a radiomic classifier.

```go
options := roi.LocalTextureOptions{
    Feature: roi.LocalContrast,
    RadiusMM: [3]float64{1, 1, 2},
    BinWidth: 25,
    MaxGreyLevels: 16,
    Quantization: roi.LocalTextureGlobal,
    Aggregation: roi.TextureAveraged,
    Workers: 4,
}
plan, err := mask.PlanLocalTexture(nil, options) // before image allocation
// Check err, plan.ReservedBytes and plan.WorstCaseVisits.
result, err := mask.LocalTexture(ctx, modalityValueAt, nil, options)
```

## Numeric domain

Axes must be finite, regular and increasing, in mm. Anisotropic spacing is
retained: each integer radius is `floor(radiusMM/spacing)` (with a 1e-10 index
round-off allowance). The rectangular neighbourhood includes centres up to
that many indices in each axis; it is not a physical sphere. Singleton axes
have radius zero. Radius zero on every axis yields no pairs, hence NaN.

Windows are clipped to the acquired image. Both endpoints of a pair must be
inside the window, inside the mask, finite and not below `LowerBound`, when
provided. A centre outside that domain is NaN even if nearby pairs exist.
Neither background, padding nor non-finite values become quantitative zero.

Bins have fixed width in input value units: `floor((value-origin)/BinWidth)`.
The global origin is the whole finite eligible mask's minimum, or the explicit
lower bound. A regional global request scans the whole mask for the same
origin/range, while caching only its requested box plus halo. Window origin is
the minimum eligible value in each window; the optional lower bound remains
an exclusion threshold. It is not a fixed-bin-count min/max rescaling.
`GlobalMinimum` is NaN for window quantization. Every result owns its options,
directions, output and region descriptors; input/accessor must remain stable
until the operation returns.

The default directions are the existing 13 unique 3D neighbour offsets, four
of which contain pairs in a single slice. Explicit offsets have components in
[-32,32]; zero and duplicate opposite/symmetric offsets are refused. Both
directions of each accepted pair enter a symmetric histogram. Average mode
computes each populated direction's feature, then takes its unweighted mean.
Merged mode combines counts first, weighting directions by pair counts.
Empty directions contribute neither zero nor an average denominator.

Supported features, with `p(i,j)` the normalized symmetric histogram:

| Feature | Formula | Constant window with pairs |
| --- | --- | --- |
| Contrast | sum p(i,j) * (i-j)^2 | 0 |
| Joint entropy | -sum p(i,j) * log2(p(i,j)) | 0 |
| Angular second moment | sum p(i,j)^2 | 1 |
| Inverse difference moment | sum p(i,j) / (1+(i-j)^2) | 1 |

All features are undefined (NaN) without pairs. Contrast uses squared bin-index
differences, not squared HU. Entropy uses bits; the other outputs are
dimensionless. No local GLRLM or correlation map is included in this profile.

## Regional execution and admission

A `LocalTextureRegion{Min, Max}` is a half-open source-grid box. Returned values
are x-fastest in that box, with `Plan.Region.Min` giving their source offset.
Windows and quantization use the original source domain, so splitting requests
does not create seams. The finite halo is clipped to the source image.
Sampling calls the accessor serially; only owned cached values are processed
in parallel. Fixed bin/direction reduction order gives identical float32 output
for different worker counts. Any failure/cancellation returns nil and never
changes masks, values or aggregate texture statistics.

Caps are 1,048,576 source-grid voxels, 256 MiB managed reservation, 200 million
worst-case visits, 256 grey levels, 16 workers, 13 directions, 32 mm radius and
32 indices per axis. Overrides may only lower resource caps. Large windows or
bin counts can reach the visit cap before the voxel or byte cap. Admission
includes retained input mask/axes, halo float64 cache, float32 output, two
bins-squared float64 histograms per worker and fixed overhead. The work estimate
includes source scans, window-origin scans, histogram clearing and reductions.
It is a conservative operation bound, not a throughput or process-RSS promise.
Context checks occur at scan rows and processing rows/centres; one bounded
normalization/origin/array phase is not promised to stop mid-instruction.

## Independent evidence and measured cost

Focused tests use an independent sparse, all-ordered-pair enumeration for
constants, periodic/heterogeneous patterns, partial masks and non-finite
values. They cover four features, both quantizations and both aggregations,
anisotropic radii, all border centres, requested regions and worker invariance.
Admission and cancellation tests reject work before accessor calls or return
no partial result. Existing aggregated texture tests remain unchanged.

The authored [ITK fixture campaign](../roi/testdata/local_texture/README.md)
uses installed ITK **5.4.6** co-occurrence and histogram-feature public APIs
on the exact clipped windows. Eight 7-cubed cases (four patterns, two direction
aggregations), four features and all 343 centres agree within
`2e-6 * max(1, abs(reference))`. The observed largest absolute difference was
`9.17458834e-7`, accounting for double accumulation followed by float32 output.
The producer took 0.181505 s with peak Windows working set 11,378,688 bytes.
The Go kernel for the same eight cases/four features took 0.0074661 s in one
non-race run (excluding JSON parsing and comparisons). A standalone Go test
process repeating those cases 100 times had peak working set 59,412,480 bytes,
including Go runtime, JSON oracle loading and comparison allocations. These
are different process/campaign measurements, not a comparable speedup or RSS
reduction claim; the ITK producer constructs a fresh filter for each window.

The Go process measurement used an independently compiled `go test -c ./roi`
binary, `-test.run=^TestLocalTextureIndependentITKWindows$ -test.count=100 -test.v`,
GOMAXPROCS=4, and Windows `Process.PeakWorkingSet64` sampled every 5 ms.

On Windows 11/amd64, Go 1.26.4, i7-14700K, GOMAXPROCS=4:

```sh
go test -race ./roi ./parametricmap -run '^TestLocalTexture' -count=1 -v
go test ./roi -run '^$' -bench '^BenchmarkLocalTextureMap$' -benchtime=5x -count=1
```

The 24-cubed full-mask contrast benchmark, radius 1 mm, eight grey levels,
four workers, measured 8,761,880 ns/op, 179,209 B/op and 33 allocations/op.
It is a small authored workload, not clinical-volume performance or a speedup
claim against ITK. No GPU rendering or clinical qualification is implied.

Parametric Map references now retain optional one-based source frame numbers,
including multiple frames of one SOP. Writing delegates the DICOM provenance
macro to `derivation`; reading refuses malformed frame numbers. A missing
frame list retains its whole-instance meaning.

Float and Double Float PMAP readers admit dimensions and encoded length before
replaying deferred payloads through `Object.CopyValueTo` into a fixed-size owned
buffer. Lazy frame decoding survives closing or subsequently editing the source
file. Metadata-only reads still retain no payload. Little/Big Endian disk tests
cover calibrated values and NaN validity after source closure/replacement.

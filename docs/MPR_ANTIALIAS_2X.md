# Opt-in 2x scalar MPR anti-aliasing

`render.ResliceObliqueScalarWithOptionsContext` accepts
`MPRResliceOptions{Antialias2x: true}` for a single-plane scalar reslice. The
existing source-owned `InteractiveVolumeContext` supplies one cached BinShrink
level. Every output voxel averages a 2x2x2 source block, with doubled spacing and
the corresponding half-source-voxel origin shift. Axes shorter than four voxels
keep their original samples. The requested output plane and its patient-space
field of view do not change.

Both output pixel spacings must be at least twice the largest source spacing.
This conservative threshold does not select the level just because one finely
sampled axis is undersampled. Native-density output, nonregular geometry and
odd-sized shrinkable axes use the original grid. Odd axes are excluded because
the existing floor-sized BinShrink would discard their final source bin.
`ScalarPlane.Antialiased()` tells the caller which source was used; the filtered
values remain in modality units but are not unchanged quantitative samples.

The default scalar API is unchanged. Nearest-neighbor label sampling refuses
this option rather than averaging label identifiers. Thick-slab reductions also
refuse it; a slab request that actually selects a single plane preserves the
option. This profile is neither an ideal low-pass filter nor a complete mipmap
pyramid: a single box-filtered 2x level cannot remove every alias at arbitrary
angles or larger zoom-out ratios.

Admission includes the 4-byte-per-voxel level payload and the 9-byte-per-pixel
scalar output in `MPRLimits.MaxWorkingBytes`, before building a new level. A
warm cache does not bypass a tighter request budget. The already-owned source,
metadata and runtime overhead are outside that payload accounting. No universal
process-RSS limit is claimed. Cancellation, closed stores and failed acquisition
return an error; they do not publish a successful empty filtered plane. The
level is closed with its source, using the existing store lease lifetimes.


## Reproduction

Run from `dicom-go`:

```sh
go test ./render -run '^TestMPRAntialias' -count=1
go test -race ./render \
  -run '^Test(MPRAntialias.*|BinShrinkVolumeOriginSpacingAndAverages|InteractiveVolumeIsCachedAndClosedWithItsSource)$' \
  -count=1
go test ./render -run '^Test(MPRAntialias|ResliceOblique|Scalar|MPR|BinShrink|InteractiveVolume)' -count=1
GOMAXPROCS=4 go test ./render -run '^$' \
  -bench '^BenchmarkMPRAntialiasWarm$' -benchmem -benchtime=300ms -count=5
make check
```

The original synthetic alternating-strut fixture uses 64 cubed voxels and a
22x22 output with a noninteger sampling step. Its analytical block mean is
1000 modality units: point sampling produces a low-frequency beat, while the
averaged output matches the mean at every pixel, including the edges. A rotated,
anisotropic oblique ramp checks patient-space coordinates independently from
the averaging code. Other tests cover unchanged native/odd grids, label/slab
refusal, payload limits before allocation, cancellation and cached lifetime.
These synthetic tests do not establish clinical stent visibility, native GUI
quality or a universal negligible-cost claim.


Windows/amd64, Go 1.26.4, Intel Core i7-14700K, `GOMAXPROCS=4`, five warm
300 ms benchmark observations per mode, 128 cubed voxels and a 48x48 coronal
output. Fixture creation and the first level build are outside the timed loop.

| Sampling | Median per reslice | Allocations per reslice |
|---|---:|---:|
| Original point sampling | 30.216 microseconds | 18 |
| Cached 2x level | 34.638 microseconds | 18 |


The three new tests, the affected scalar/oblique/MPR regressions and the selected
race cases passed. `make check` was attempted and failed at its test stage in
14 packages. Replacing this change with the original HEAD source through a Go
overlay reproduced failures in all 14: `cmd/dcmodify`, `cmd/dcmsign`, `cmd/qrscp`,
`cmd/seg2nii`, `cmd/storescp`, `cmd/syslogrcv`, `internal/dcmdump`,
`internal/hl7cli`, `internal/wrappercli`, `mesh`, `net/audit`, `net/ul`,
`registration` and `render`. They include POSIX permission expectations,
executable naming, unsupported socket inspection, fixed numerical hashes,
one-gray-level CPR rounding and timing-sensitive LOD expectations.

Formatting and vet passed before that test stage. The remaining baseline
targets were explicitly run separately and passed: build, documentation,
codec policy, dependency-free optional adapters, conformance and manifest
checks. This does not turn the failed complete baseline into a pass. Desktop
visual qualification and equivalent GPU anti-aliasing remain outstanding.

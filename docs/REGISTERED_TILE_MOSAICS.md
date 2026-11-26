# Registered scalar acquisition tile mosaics

`microscopy.RegisterMosaic` estimates relative translations for an explicitly
identified rectangular acquisition neighbourhood. It reuses
`registration.EstimateMaskedFFTTranslation`, then qualifies the complete graph.
`ComposeMosaicRegion` separately samples an owned derived region or overview.
Existing declared WSI tiles and `ComposeViewport` are unchanged.

## Input and coordinate contract

`MosaicTile` borrows an ID, a nonnegative rectangular `Grid`, a scalar
`registration.Image` and an optional independent mask. Inputs must remain
stable during each call. Images have z=1, equal dimensions, compatible **exact**
spacing/direction, one common orthonormal physical plane, finite samples and
unique IDs/grid cells. The caller establishes a single optical path/channel
and common physical coordinate frame. There is no inference of patient, slide,
optical-path identity or arbitrary image correspondence from scalar pixels.

Grid cells define immediate horizontal/vertical neighbours only. Missing cells
are permitted, but disconnected accepted links make the result inconclusive.
An absent mask includes every sample; supplied masks must match their own
image's geometry. Positive mask samples include data. All image/mask samples,
including excluded samples, must be finite. No inpainting, deformation,
rotation/scale estimation or automatic source repair occurs.

The first grid cell in row/column order anchors the result unless `AnchorID`
selects another source. Its declared sample-zero origin is unchanged. Pair
matching uses `movingIndex = fixedIndex + IndexShift`. Therefore the proposed
B-origin minus A-origin is **minus** direction*spacing*IndexShift. The NCC
`TranslationLPS` instead maps declared fixed points to moving points; its
negative is the relative correction to their declared origins. Translations,
residuals and correction limits use physical millimetres, not screen pixels.

`MaxCorrectionMM` is explicitly required and positive. It defines a symmetric
physical component search box for each correlation; after graph fitting, the
Euclidean displacement of each origin from its declared origin must also fit
that bound. Integer correlation peaks are not refined to subpixel peaks.
The continuous least-squares graph can yield fractional origins for constraints
that disagree within the caller's admitted residual tolerance.

## Local proposals and global qualification

Each neighbour uses the existing masked, linear FFT NCC with actual overlapping
mask support. Defaults are minimum 16 samples, overlap fraction 0.1 of the
smaller mask population, score 0.9 and ambiguity tolerance 1e-8. Repeated near-
best peaks, extreme linear borders, a peak touching the search bound, low score,
insufficient overlap and unresolved variance remain explicit rejected links.
Input, numerical admission, context and resource failures return nil/error.

Over actual overlap, a stable centred regression estimates
`moving = gain*fixed + offset`. The default admitted gain interval is
[1/1.25, 1.25]; absolute offset divided by the fixed overlap's intensity range
must be at most 0.1. Divergent illumination is rejected; no intensity
normalization is silently applied to the mosaic. Correlation is not confidence.

A spanning forest identifies components and fundamental cycles. A connected
accepted graph is fitted by an anchored equal-weight least-squares Laplacian
solve. Every accepted constraint contributes; inconsistent edges are not
silently removed to force a good fit. Default maximum global link residual and
fundamental-cycle closure are 1e-6 mm. A failed global check gives an owned
diagnostic layout with `Conclusive=false`; composition refuses that layout.
Rejected links, components and fitted origins remain available for review.

`CycleRank=acceptedEdges-tiles+components` states how many independent cycles
exist. A connected tree has no measured independent cycle. Per-link
`CycleClosureMeasured` distinguishes a non-tree fundamental cycle from a tree
edge; `ResidualMeasured` distinguishes a connected fitted constraint from a
rejected/unfitted link. Zero is not evidence that an unavailable measurement
passed. `Conclusive` qualifies this bounded numerical profile, not anatomical
correspondence or general montage robustness.

## Regional composition, coverage and identity

`MosaicBounds` computes an enclosing anchor-grid rectangle without allocating
that matrix. A floating-point enclosing boundary can include a transparent
outer pixel. `MosaicRegionOptions.Region` chooses any bounded rectangle in that
grid, including background outside the mosaic. `OutputSize` zero uses native
region dimensions; a nonzero size makes a bounded overview. For output column
x, the source-grid sample coordinate is
`Region.Min.X + (x+0.5)*Region.Dx()/OutputSize.X - 0.5`, and likewise for rows.
Returned origin/spacing describe those output pixel centres in physical mm.

Interpolation is bilinear; borders clamp within each tile's half-pixel outer
support. A contribution requires every interpolation sample with nonzero
weight to be included by its mask. Overlaps use the arithmetic mean of valid
contributions, without exposure correction. `Coverage` is a uint16 contributor
count at the sampled point, not area coverage or confidence. Coverage zero
uses the finite requested background. Overview point sampling is not area
averaging and can alias high-frequency content.

The layout owns metadata/provenance and source SHA256 digests, not source
pixels. Its `mosaic-v1:` identity includes the public derivation metadata.
Composition verifies original IDs, geometry, pixels/masks and the private
layout binding. Mutating a layout or replacing source content is refused.
Serialized metadata are an inspection record, not a supported reload API.
No DICOM object, patient coordinate registration or archive entry is created.
All returned arrays are owned. Error/cancellation publishes nil, including
mid-kernel, empty-output and terminal publication checks.

## Finite resources

Zero optional quotas select these finite defaults; invalid options are refused:

| Registration option | Default |
| --- | ---: |
| `MaxTiles` | 128, hard maximum 1024 |
| `MaxSamples` | 8 Mi combined image samples |
| `MaxBytes` | 256 MiB logical live scratch envelope |
| `MaxFFTWork` | 2^32 summed FFT-work proxy |
| `MaxWork` | 64 Mi logical inspections |

The scratch envelope includes generated masks, metadata, dense graph storage
and the peak serial FFT workspace. Borrowed input image/mask storage and
runtime/allocator overhead are excluded. `registration.MaskedFFTWorkspace`
admits the existing kernel's documented bytes/work before execution, so even
inconclusive attempted kernels count against the aggregate budget. This is
logical admission, not measured RSS or exact allocation traffic. Kernel
padding and index/addressability ceilings also apply. All work is serial.

Regional defaults are 4 Mi output pixels, 8 Mi source image samples, 64 MiB
combined logical metadata/payload/scratch and 64 Mi logical inspections.
The payload envelope is 14 bytes per output sample plus bounded metadata:
float32 output, uint16 coverage and float64 accumulation. It does not scale
with the total mosaic matrix area. Source validation still scans supplied
inputs and sampling can inspect every tile per output pixel. No performance
or large-data scalability claim follows from bounded allocation.

## Independent local qualification

Windows amd64 / Go 1.26.4 synthetic fixtures fix acceptance before execution:
physical placement/cycle error at most 1e-8 mm and intensity/seam error at most
1e-4 source units. Four 32x28 tiles use a known texture, known 24/20-pixel
steps, deliberately incorrect declared stage positions and 0.04/0.06-mm
spacing. An independent exhaustive two-pass spatial Pearson scorer checks
pair shifts; a separately generated whole-texture oracle checks global
composition and seams, not only local scores. The observed maximum residual
was 1.7764e-15 mm, cycle closure 8.8818e-16 mm and seam error 3.7002e-12 units;
768 pixels had multiple contributors. These are numerical errors, not timing.

Additional tests cover a bounded 4x3 whole-mosaic overview with independent
half-pixel samples, inconsistent cycles, missing/periodic/illumination/
disconnected data, mask holes, geometry/schema/precision admission, every
quota category, ownership/source binding, deterministic concurrent reads and
pre/mid/terminal cancellation. Reproduce the focused checks and baseline:

```sh
go test -race ./microscopy ./registration -run '^Test(Mosaic|MaskedFFT)' -count=1
make check
```

The external `TestMaskedFFTITKOptIn` oracle is not supplied by this campaign;
that skip does not substitute for a new ITK runtime comparison. The existing
FFT estimator is unchanged. The implementation and synthetic fixtures are
independent; no ITK source was copied. ITK's public
[montage](https://github.com/InsightSoftwareConsortium/ITK/blob/be770907fc62a2ae0b1395a1f092465d7ab9b535/Modules/Registration/Montage/include/itkTileMontage.h)
and [merge](https://github.com/InsightSoftwareConsortium/ITK/blob/be770907fc62a2ae0b1395a1f092465d7ab9b535/Modules/Registration/Montage/include/itkTileMergeImageFilter.h)
contracts informed the separation of registration and composition, not an
equivalence claim. No clinical, desktop/GPU or performance qualification is
implied by these library tests. The Twin workflow has a
separate application profile.

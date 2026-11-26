# Presented image comparison

`pixeldata/compare` retains the existing `Files` stored/modality behavior and
`Accumulator` numerical definitions. The opt-in presented APIs compare aligned
2D presentation planes or already-produced images. They neither register nor
resize images, and never generate a CPU 3D frame. A GPU-produced RGBA8 frame can
be compared through `ComparePresentedRGBA8`, which also preserves the existing
`render/qualification` numerical/silhouette metrics **before** background
composition. There is no Twin dependency or implicit clinical equivalence.

## Explicit presentation

`ComparePresented` accepts tightly packed normalized `[0,1]` planes, declaring
source quantization of 8 or 16 bits and either grayscale P-values, sRGB code
values or linear RGB. Geometry, depth, components and encoding must agree.
RGB similarity is computed per channel and aggregated across channels, with no
hidden luminance conversion. Values outside the declared range and NaN/Inf are
refused. The range is fixed at 1, never independently derived from image extrema.

`ComparePresentedImages` accepts the standard Gray/Gray16, NRGBA/NRGBA64 and
RGBA/RGBA64 types. The declared depth must match the actual image type. Alpha
must be explicitly `opaque` (reject transparency), `ignore` (unassociated color)
or `composite` over the declared normalized background. Compositing occurs in
the chosen comparison encoding. `linear-rgb` uses IEC sRGB decoding before
composition; `srgb-code` performs code-value composition. Grayscale background
components must agree. Arbitrary custom models are refused rather than guessed.
Input buffers must remain immutable during a call.

`ComparePresentedReaders` borrows two `pixeldata.FrameReader` instances through
`PresentedReader`, decoding a single pair at a time. The caller owns their files,
sources, codec/index limits and Close. The options must declare `Mode: "common"`
(reference profile applies to both), or `Mode: "per-image"` (both profiles).
Grayscale VOI is explicit LINEAR (width >= 1), LINEAR_EXACT or SIGMOID (width > 0),
or an explicit validated VOI LUT. There is no auto-window or object-window
fallback. Modality transforms use the existing per-frame DICOM implementation.
The profile also supplies Presentation LUT and effective inversion, including
MONOCHROME1 when desired. Quantization occurs after VOI at the selected depth,
then the existing depth-specific Presentation LUT/inversion implementation runs.
8-bit LUT indexing uses its descriptor; 16-bit indexing spans the table.
These effective profiles, modality transforms, palette tables when used, decoded metadata and frame index
are delivered in each `PresentedFrameRecord` and can be included in export.
Color uses the existing DICOM RGB/YBR/palette conversion. Grayscale transforms
supplied for color are refused. ICC-bearing inputs require an explicitly produced,
color-managed image instead of silently assuming sRGB. Display calibration,
GSPS shutter/overlay composition and any other already-applied rendering belong
in a caller-produced image; this adapter records only its actual stated stages.

## Local similarity and differences

Choose `DefaultPresentedOptions()` explicitly, or provide every option. Defaults
are a 7x7 Gaussian kernel (sigma 1.5), K1=0.01, K2=0.03, fixed range 1 and reflect
borders. Odd windows 1..31, uniform/Gaussian weights, positive Gaussian sigma,
and finite K1/K2 in `[0.000001,1]` are accepted. The finite lower bound avoids
underflow in constant-image stabilization. Weighted **population** moments use:

```
C1=(K1*range)^2; C2=(K2*range)^2
SSIM=((2*meanR*meanC+C1)*(2*covariance+C2)) /
     ((meanR^2+meanC^2+C1)*(varianceR+varianceC+C2))
```

Constants and black images therefore have a defined value, including SSIM 1 for
identical images. Computation uses centered moments and clips floating-point
roundoff to `[-1,1]`. Border policy is `crop` (renormalize in-bounds weights),
`reflect` (reflect without repeating the boundary pixel), or `valid` (centres
with incomplete windows omitted). One shared per-pixel mask excludes both
inputs and all excluded neighbours; retained kernel weights are renormalized.
An entirely empty mask or valid interior is refused. Map entries outside the
comparison mask are zero and invalid, not observations.

Difference is signed `candidate-reference` or absolute, in normalized component
units. Numerical MAE/RMSE/max/PSNR/SNR reuse `Accumulator`; PSNR uses the actual
reference peak as in the existing numerical API, while SSIM uses fixed range 1.
Similarity never substitutes for numerical sample error. The whole image and
up to 256 named bounded regions report arithmetic mean SSIM, minimum local SSIM,
and independent numerical errors. A region with no valid SSIM centre has count
0 and mean/minimum 0; the count distinguishes this from measured similarity.
Regions can overlap. Local maps/minima reveal changes a global mean may hide.

## Reproducible artifacts

`ExportPresented` requires six caller-owned writers. It writes difference and
SSIM as exact little-endian IEEE754 float64, row-major interleaved components;
one validity byte per pixel (bit 0: numerical sample included, bit 1: SSIM centre
valid); two 16-bit PNGs; and JSON metadata last. For a reader result pass its
matching record to include the actual presentation/modality parameters. Outputs
have identical image dimensions. PNG difference uses fixed scale: absolute
`[0,Scale]` or signed `[-Scale,Scale]` mapped to `[0,65535]`. Out-of-scale values
clip only in PNG, not the float map. SSIM PNG maps `[-1,1]` to `[0,65535]`.
Grayscale is repeated in RGB channels; excluded pixels are transparent. Metadata
records dimensions, source depth/encoding, policies, scale, SSIM options,
region results and SHA256/length for each map, validity and PNG. The validity
artifact records the exact effective mask. Infinite numerical dB values are
JSON strings `+Inf`/`-Inf`, not discarded or converted to fake finite numbers.

Export errors/cancellation can leave a written prefix. Writers are never closed;
the caller must discard incomplete artifacts and publish only a successful set.
No filename, storage path or clinical identifiers are inferred.

## Resource limits and cancellation

Every call validates checked dimensions, maximum pixels and maximum logical
working bytes before allocating comparison maps. Core admission includes two
float maps, validity/mask storage, kernel and bounded region records. Image
adapters also admit both float input planes. Incremental readers reserve a
conservative additional 128 bytes/pixel for their decoded/rendered/float buffers
and 4 MiB for detached profiles/LUTs. MaxFrames is explicit. No all-frame result
array is retained: the synchronous consumer receives one complete result at a
time. Its retained results, reader indexing/source buffers, codec scratch and
process RSS are separate responsibilities, not claimed to fit this quota.
Export additionally admits conservative PNG/encoder buffers against the result's
working limit and bounds aggregate output bytes across all six writers. Logical
buffer admission is not a physical/RSS measurement.

Cancellation is checked during validation, per row and within local-map loops,
between frame reads and before delivery, and while writing exports. Per-frame
rendering work is bounded; an external codec must honor the same context through
FrameReader. Invalid/cancelled single-frame calls return no result. Incremental
calls return the successfully consumed prefix plus an error; a failed consumer
or frame stops future decoding and does not close borrowed resources. Callback
metadata arrays are detached, so changing a delivered profile cannot change a
later frame's presentation.

## Qualification scope

Authored tests cover analytic identical/constants/offset/contrast, blur and local
regions, kernels/borders/mask/color, alpha/depth/explicit sRGB, exact exports and
errors, common/per-image VOI, Presentation LUT/inversion, incremental budget,
borrowed lifetime and cancellation/failure. `presented_oracle.json` is a synthetic
weighted local-map oracle generated independently with raw population moments;
closed-form rational fixtures provide a separate check. The fixture generator
and source/provenance are part of the private qualification handoff.

`BenchmarkPresentedComparison` compares numerical-only cost with difference plus
local SSIM on a fixed 64x64 fixture. `BenchmarkPresentedMultiframe` measures
incremental 1/16-frame cases. Report ns/op, B/op and allocs/op as additional costs,
not speedup, clinical equivalence or a universal memory-flat/RSS claim.
Qualification on darwin/arm64, Apple M4 Pro, CGO=1, GOMAXPROCS=2 and
GOMEMLIMIT=1GiB: the root executed 32 selected top-level tests in four packages
(47 PASS test events, zero skips/failures). The exact 4710 selected source
paths/hashes/modes and nine owned paths matched before and after all gates;
processes exited zero, their owned groups were reaped/drained, and capture EOF
was recorded. Existing stored/modality tests passed alongside the new cases.
Independent static G3 alias review PASS and the independent population/weighted
oracle review are preserved. The earlier G2 compile failure occurred before any
tests because the new stdlib image import collided with existing `type image`;
it is historical RED, not a passing test run. G1 policy alias was also corrected
without discarding its original static finding.

The unchanged benchmarks ran with `-benchtime=1x -count=1 -benchmem`; all four
observations follow. The numerical-only and difference/local-SSIM cases use
64x64 normalized planes with a 3x3 uniform reflect kernel. Incremental cases use
one or sixteen synthetic 8x8 native grayscale frames and the explicitly declared
16-bit presentation/VOI. Backing source data belongs to the caller and is outside
timed preparation; the timed operation includes per-frame read, presentation,
comparison and synchronous callback.

| Caso | Iterações | ns/op | B/op | allocs/op |
|---|---:|---:|---:|---:|
| `BenchmarkPresentedMultiframe/1` | 1 | 161542 | 3696 | 39 |
| `BenchmarkPresentedMultiframe/16` | 1 | 751459 | 51936 | 519 |
| `BenchmarkPresentedComparison/numerical-only` | 1 | 50084 | 0 | 0 |
| `BenchmarkPresentedComparison/difference-local-ssim` | 1 | 2052375 | 70192 | 5 |

One observation per case establishes bounded actual execution and records
additional cost; it is not a distribution, speedup, comparative recommendation
or clinical equivalence. B/op is total allocated bytes per operation and allocs/op
is allocation count, not peak retained memory or RSS. No peak-RSS measurement was
performed. Multiframe allocations reflect per-frame work; no whole-process
memory-flat assertion is made. Comparison logical quota tests and caller/codec/
consumer responsibilities remain as documented above.

Focal and cost receipts are bound to source G3. Root's execution disk policy
records a 4 GiB operational minimum after prior idle Go build/test-cache cleanup,
and a separate 40,000,000,000-byte user cleanup trigger. Actual free bytes were
35,186,126,848→34,232,606,720 for focal and 32,061,083,648→32,041,488,384 for cost;
cleanup-needed-after is true in both receipts. This is not a claim that a 40 GiB
admission gate passed. The recorded cleanup reclaimed 6,405,849,088 bytes and
preserved module cache.


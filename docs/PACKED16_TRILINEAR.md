# Packed16 trilinear affine sampling


## Path and numerical contract

A reader of a tightly packed canonical `int16`/`uint16` stored generation may
interpolate eight normalized stored integers in float64, then apply its common
`RescaleSlope` and `RescaleIntercept` once. Path selection happens once while
constructing the sampler. Existing stored-value decoding normalizes
BitsStored/HighBit and sign extension before this path; this change introduces
no alternative bit decoder, per-slice rescale inference or new public option.

Let `M = maxStoredAbs * abs(slope) + abs(intercept)`, with `maxStoredAbs=32768`
for signed data and 65535 for unsigned data. The new path is admitted only when
`M <= 1e-8 / (64 * 2^-52)`. This also excludes overflow and non-finite scales.
The absolute difference from the old per-corner interpolation is bounded by
`1e-8` in modality units. The gate is deliberately conservative and depends on
the complete scalar-format range rather than a scan of the current voxels.
Large affine ranges, padded/byte-backed payloads and float32 modality data keep
the former interpolation. No additional volume copy, decoded allocation,
resident payload, worker or generation is introduced.

The bound follows the tensor-product weights on fractions in `[0,1]`:
mathematically their sum is one and the affine operation commutes with
interpolation. With binary64 arithmetic, a convex lerp takes two products and
one sum; three nested stages plus affine evaluation have a conservative
forward bound of fewer than 32 machine epsilons times `M` per path, including
rounding of `(1-f)`. The two paths therefore differ by less than
`64 * 2^-52 * M`. Tiny underflow rounding is also far below the admitted
absolute budget. This is an absolute error bound, so cancellation from a
negative slope/intercept is covered without dividing by the result.

Integer-coordinate sampling is unchanged and still bypasses interpolation.
The half-voxel border/clamp policy is unchanged. `voxelSample` and its corner
values remain in modality units for derivative/DVR consumers. The reader
retains the same immutable generation lease through replacement/volume close;
closing it releases that lease. Existing frame context and work limits still
apply.

## Presentation and validation scope

A scalar tolerance is not a universal bound on displayed pixels. Continuous
VOI functions may magnify scalar error according to window width, while a
width-one LINEAR threshold or discontinuous VOI LUT can jump at a boundary.
No bit-exact presentation claim is made for arbitrary windows/LUTs. The
fixtures explicitly compare output through LINEAR, LINEAR_EXACT and SIGMOID,
including narrow windows centered on an actual sampled value; their accepted
maximum difference is one 8-bit grayscale level. Slab sums admit the sum of the
per-sample error bounds. Exact scalar/frame equivalence remains required for
paths that did not select the new interpolation.

[The focused tests](../render/volume_sampler_packed16_test.go) cover signed and
unsigned 16-bit data, 12-bit samples at HighBit 11 and 15, positive/negative and
fractional slopes, large intercept/fallback, axes of size 1/2/3, integer and
fractional positions, border clamping/non-finite coordinates, padded and
float32 fallback, modality corner semantics, volume/reader lifetime and
canceled CPR. The scalar oracle is an independent eight-weight analytic
formula. A separate test reuses the committed
[VTK reslice fixture](../testdata/upstream/vtk/reslice.json), with its original
2^-16 coordinate quantization and declared
[provenance](../testdata/upstream/vtk/README.md), under several explicit affine
rescales. No VTK implementation was copied or external runtime required.

MPR/slab/CPR fixtures compare against byte-backed canonical data using the
unchanged per-corner calculation. These are synthetic numerical/presentation
checks, not clinical validation.


```sh
go test ./render -run '^TestPacked16Affine' -count=1
go test -race ./render -run '^TestPacked16Affine' -count=1
go test ./render -run '^$' -bench '^BenchmarkPacked16(Trilinear|Frames)1307$' -benchmem
```

## Local paired result


| Workload | Baseline median | Candidate median | Baseline/candidate | B/op | Allocs/op |
| --- | ---: | ---: | ---: | ---: | ---: |
| Signed packed16 sample | 26.20 ns | 22.05 ns | 1.19× | 0 → 0 | 0 → 0 |
| Unsigned packed16 sample | 25.81 ns | 20.02 ns | 1.29× | 0 → 0 | 0 → 0 |
| Float32 sample | 23.27 ns | 23.73 ns | 0.98× | 0 → 0 | 0 → 0 |
| Oblique 256² frame | 3.020 ms | 2.695 ms | 1.12× | 66,536 → 66,536 | 10 → 10 |
| Average slab 128² × 5 | 3.500 ms | 3.088 ms | 1.13× | 17,480 → 17,480 | 10 → 10 |
| Straightened CPR, width 128 | 1.284 ms | 1.253 ms | 1.03× | 22,976 → 22,976 | 14 → 14 |

The 96³ canonical payload stayed at 1,769,472 bytes for packed16 and 3,538,944
bytes for float32. The float32 median was about 2% slower; ranges overlapped
(baseline 22.99–23.73 ns, candidate 23.55–23.94 ns). Its arithmetic is unchanged,
and this result does not establish a float32 gain or statistical equivalence.
These are local synthetic microbenchmarks, not universal performance claims.
The focused default/race gates and Go 1.22.12 compatibility gate passed;
`go vet ./render` and scope-limited whitespace checks passed. Complete library
integration checks remain the integrator's responsibility.

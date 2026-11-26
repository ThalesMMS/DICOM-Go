# Context-aware headless VR empty-space skipping


## Conservative metadata

The existing 8-voxel scalar grid includes trilinear corner support. A lazy
one-voxel gradient halo bounds each central-difference component. Covariant axis
norms give an upper magnitude bound for anisotropic patient geometry. The
piecewise gradient function is checked over its entire bounded interval, including
interior opacity peaks. A 2D table uses prefix sums to classify a rectangle of
intensity and gradient corners. Clip-plane replacement intensities retain the
scalar-only classification because the source halo cannot bound those gradients.

Exactly equal finite scalar bounds identify a constant interpolation stencil.
It does not need numerical widening. LUT endpoint ranges use floor/ceil support,
so a constant PET background at a zero alpha endpoint can be skipped even if
its adjacent LUT entry is positive. Nonconstant ranges retain outward bounds.
Nonfinite or unrepresentable scalar bounds retain source sampling. All skipped
intervals preserve the original repeated-addition sample lattice.

An immutable label map stores visible/opacity occupancy for each block. A voxel
at a block boundary belongs to both neighbouring blocks, including all eight
neighbours at a three-axis boundary. This preserves nearest-neighbour label
selection. Only-inside blocks without a visible label skip their scalar work;
labels that add opacity keep their blocks even when scalar/gradient alpha is zero.
The constructor also rejects overflowing dimension products before allocating.

Fused rendering intersects primary and secondary transparency bounds. Eight
corners in patient coordinates bound an overlay block for supported grids without tilt;
patient origins, orientation and spacing may differ. Rounding margins scale with
patient-coordinate magnitude and secondary spacing. Partially in/out-of-bounds
blocks retain sampling. Tilted geometry or an arbitrary mapping callback retains
full marching because corner samples cannot bound an unknown interior transform.
A real probe at the first retained logical sample of a skipped interval preserves
overlap diagnostics even when every contributing opacity is zero. Cut/label
metadata is checked before that probe. A pending cut cap observes the first
retained sample before any block can be skipped.

## Lifetime and costs

Gradient bounds add eight bytes per block to an immutable prepared generation.
The new gradient build coalesces concurrent callers; waiting callers may cancel
and a canceled build is not published. A label map retains one occupancy byte
per block. Existing read-only label/style ownership rules remain in force.
The secondary scalar/empty grid is transient and rebuilt per fused frame: roughly
nine bytes per secondary block plus classification scratch. A 2D prefix array
uses `(width+1)*(height+1)*sizeof(int)` bytes per frame. All source reads remain
under the existing sampler leases. These metadata costs may dominate small frames,
dense volumes or secondary grids much larger than the presented primary region.

## Windows measurements

Windows amd64, Go 1.26.4, Intel Core i7-14700K, default 28-way parallelism.
Three repetitions with 300 ms benchmark windows; the table reports independent
medians. Camera rotation is 17/-9 degrees, output 112x96. Gradient and label
fixtures use an 80-cube synthetic sphere; the label fixture has one sparse
16-bit segment. The 8x8 2D table has transparent low-gradient rows. Fusion uses
a 32-cube CT and a separate anisotropic PET grid. Prepared sources are warmed
before timing; the secondary grid rebuild is included in each fused frame.

| Profile | Full march ms | Skip ms | Ratio | Full / skip B per frame |
| --- | ---: | ---: | ---: | ---: |
| gradient | 14.075 | 2.475 | 5.69x | 61952 / 101756 |
| tf2d | 13.698 | 2.373 | 5.77x | 59668 / 61600 |
| labels | 6.240 | 0.632 | 9.88x | 59666 / 70254 |
| fusion | 4.877 | 3.063 | 1.59x | 71155 / 122871 |

Fused exact-image cases spanning perspective/parallel, 0/27/90-degree secondary
orientation and crop avoided approximately 26-56% of secondary samples. Pixel
buffers remained byte-identical to full marching. Additional cases cover
16-bit boundary labels, only-inside masks, jitter, gradient cut faces, nonmonotone
opacity, concurrent metadata callers, large valid patient origins, transparent
overlap and unknown mapping callbacks. These synthetic improvements do not imply
an equal gain for every dataset or transfer function.

```sh
go test -race ./render -run '^Test(SpaceLeap|SpaceLeaping|VRFusion|FusedVR|VRLabels|VRLabel|NewVRLabelMap|VRGradientOpacity|VRTransfer2D)' -count=1
go test ./render -run '^$' -bench '^BenchmarkVRContextSpaceLeap$' -benchtime=300ms -count=3 -benchmem
```

The module `make -k check` baseline ran; formatting, vet, builds, documentation,
codec policy and adapter/fixture/profile checks passed. The broad suite remains
red in Windows permission/file-publication fixtures, unrelated timing/compatibility
expectations and two independent CPR rounding comparisons. Both CPR failures
were reproduced on isolated pre-change HEAD. Focused empty-space/image tests
and affected Twin study callers are reported separately rather than treating the
broad baseline as green.

See [capabilities](CAPABILITIES.md) and the
Twin GPU-only policy.

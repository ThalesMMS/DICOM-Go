# Surface materials sampled from a scalar volume

`render.SurfaceProbeState` binds patient-LPS geometry to a separately owned
scalar `VolumeDescriptor`. V27 view state freezes its color table, identifies
the secondary generation through `OverlaySource`, and refuses an implicit
combination with volume fusion or RGB component presentation.

The contract records source identity, source/surface Frame of Reference,
verified same-frame identity or explicit affine registration, and the composed
surface-patient-to-source-index matrix. `ValidateSurfaceProbeSource` verifies
that matrix against the actual source lattice and generation. No missing Frame
of Reference is treated as verified alignment. Application orchestration resolves
the current source and any Spatial Registration; the contract does not discover
or infer registration.

Point probes evaluate one surface position. Minimum, maximum and mean evaluate
an inclusive signed interval along a provided unit surface-patient normal.
The sample count is `ceil((end-start)/maximumStep)+1`, capped at 512. Endpoints
are included; the actual equal step does not exceed the requested maximum.
Normal reversal changes the signed interval direction. The millimetre offset
is applied before registration, so an affine scale does not redefine physical
surface thickness. Complete per-vertex unit normals with consistent orientation
within each nondegenerate triangle are required for bands; consistently inverted
normals are valid. Missing, mixed or degenerate normals cannot enable a band.

Nearest and trilinear interpolation operate on the closed source voxel-centre
domain. Outside samples are omitted rather than clamped into the domain or
introduced as zeros. No valid samples produces an explicit invalid color and
zero validity count; a measured scalar zero remains valid. The finite nonzero
scale and offset apply to decoded modality values BEFORE the operator, including
negative scales. The color transfer maps the resulting quantity in its declared
unit/domain; it must have opaque entries. Mesh opacity remains independent.

The reusable library supplies validation, provenance and ownership contracts.
Twin's GPU implementation samples
at rasterized surface fragments and composes the result on the device. It does
not call a CPU volume sampler. Numeric GPU qualification records use the same
sampling function at analytic points.

Units and scale are explicit authoring data, not an assertion of calibrated
quantitative or clinical validity. A verified affine and a valid color map alone
do not qualify a source's calibration.

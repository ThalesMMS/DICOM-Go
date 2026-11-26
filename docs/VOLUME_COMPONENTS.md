# RGB and independent volume components

VolumeSnapshot V1 remains scalar-only. V2 carries two to four interleaved F32
modality components, with explicit row/slice strides and the same patient-space
affines. `ComponentModalityAt` addresses a component; `ModalityAt` refuses a
multicomponent snapshot. Streaming preserves all components and removes padding.
Store replacement owns the input; leases retain the complete generation.

`BuildVolume` decodes three-channel color frames into RGB display values in
[0,255], using the existing color decoder, including planar/YBR handling.
These are decoded RGB8 presentation values, not raw high-bit-depth channel
measurements. Source DICOM bytes remain retained. Mixed scalar/color stacks are
refused. Color preparation publishes a complete generation without a scalar
progressive preview. Scalar preparation and packed stored-16 paths retain V1.

ViewState V26 carries `ComponentPresentation`:

- RGB directly normalizes/clamps channels by `RGBMaximum`. Three-channel DVR
  opacity is the maximum normalized RGB channel; four-channel opacity uses A.
  Plain RGB MPR has opaque in-volume pixels; RGBA MPR retains alpha.
- Independent mode gives every component its own finite scalar-domain RGBA LUT
  and weight in [0,1]. Weighted premultiplied colors are summed, divided by the
  alpha mass, and total alpha is clamped to one after the opacity multiplier.
  The global physical opacity unit distance controls DVR step correction.
- Two to four LUTs must match the source count; their combined sample budget is
  4096. Freeze/clone own all tables and weights. Invalid, mismatched or implicit
  scalar requests against a component source are refused. MPR returns RGBA.

`SourceRGBVRPreset` supplies neutral DVR defaults without the clinical HU
gradient/scattering profile. The public component material supplies source
color/opacity rather than a gray preset's transfer. Scalar quantitative helpers
must not treat an RGB channel as an automatically selected HU signal.

The contract is implemented by Twin's shared GPU component backend.
Its current presentation boundary is DVR and planar/slab MPR, on a regular affine
grid. Scalar headless rendering entrypoints do not gain implicit RGB sampling.
Separate component LUTs and dependent/direct color follow the distinction in
the [VTK volume-property documentation](https://github.com/Kitware/VTK/blob/master/Rendering/Core/vtkVolumeProperty.h);
the implementation and fixtures are project-authored.

Tests qualify padded interleaved payloads, ownership, cancellation, scalar V1
rejection rules, real RGB source decoding, anisotropic source geometry,
version/channel matching and immutable material publication. Native worker
qualification remains separate from these library tests.

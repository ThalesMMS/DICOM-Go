# Volume filter request metadata

ViewState V17 (header 472) carries an optional value-only VolumeFilterState.
The zero value preserves old serialization and raw source selection. Gaussian
mode accepts finite sigma in (0,100] mm. PlanVolumeGaussian derives per-axis
kernel weights from affine column lengths, without reading voxel data.

The discrete separable kernel truncates at floor(3 sigma / spacing), clipped
to dimension-1, with maximum radius 64. Axes below a one-voxel radius are
identity. Out-of-volume taps are omitted and surviving weights renormalized.
This matches the existing offline library Gaussian border convention. Kernel
planning is CPU metadata work; application volume sampling/filtering belongs
to its GPU backend. Unsupported radii/geometry must be reported, not clamped
into a different requested filter.

The request retains original volume generation, geometry and presentation
identity. A FrameWarningVolumeFiltered marks completed filtered frames. The
filter does not combine with MPR averaged levels, fused MPR, or preprojected
geometry in this profile. Backends must implement or explicitly refuse it;
o original raster may be silently substituted. CPU offline filter APIs remain
available to independent library consumers.

BasicSmooth5x5 mode (value 2) requires zero SigmaMM. It is an in-plane x/y
kernel, independently applied to each z slice. VolumeBasicSmoothKernel returns
an owned metadata copy with rows 1/1/1/1/1, 1/4/4/4/1, 1/4/12/4/1,
1/4/4/4/1, 1/1/1/1/1; divide each weight by the total 60. Its boundary
convention is clamp-to-edge, unlike Gaussian tap omission. V17's header remains
472 bytes; the application worker protocol increments when adding this mode.

# Volumetric Presentation State support

The `vps` package reads, validates, applies, and writes a deliberately bounded
subset of the standard DICOM Volumetric Presentation State modules. Standard
objects are never interpreted as the historical `THALESMMS_VPS` private
payload. Unsupported transforms return `vps.ErrUnsupportedPayload`; malformed
or internally inconsistent modules return `vps.ErrInvalidObject`.

## SOP Class matrix

| SOP Class | Standard subset | Current boundary |
| --- | --- | --- |
| Grayscale Planar MPR VPS (`…11.6`) | One `VOLUME` input; PLANAR THIN or SLAB geometry; WC/WW VOI; IDENTITY/INVERSE presentation LUT; bounding-box crop; AVERAGE/MAXIMUM/MINIMUM IP for slabs | VOI LUT Sequence, non-planar MPR, registration, oblique/segmentation crop, animation |
| Compositing Planar MPR VPS (`…11.7`) | One-input EQUAL_RGB or bounded RGB16/alpha8 TABLE classification with embedded sRGB ICC profile, plus the MPR geometry/VOI/crop subset above | Two-input classification, compositor components, multiple classifications; Twin currently applies monochrome MPR only and refuses selected MPR crops |
| Volume Rendering VPS (`…11.9`) | One volume stream; PERSPECTIVE or ORTHOGRAPHIC viewpoint; WC/WW VOI; `VOLUME_RENDERED`, `MAXIMUM_IP`, or `MINIMUM_IP`; EQUAL_RGB or 16-bit RGB TABLE with NONE/IDENTITY or 8-bit TABLE alpha and embedded sRGB ICC profile; source-axis bounding-box crop; Phong shading fields retained | 8-bit RGB or segmented palettes, palettes above 4096 entries, multiple streams/components, spatial registration, animation, non-bounding crop |
| Segmented Volume Rendering VPS (`…11.10`) | The same single-stream scalar subset when the referenced input itself is directly renderable | segmentation-driven crop/classification and segmentation composition |
| Multiple Volume Rendering VPS (`…11.11`) | The same single-stream scalar subset | multiple volume streams and compositor chains |

The private creator payload written by older versions remains readable and
round-trippable for all five SOP Classes. A state without standard geometry is
written through that legacy path for source compatibility. A state with
standard geometry is written only with standard modules and does not acquire
private camera or preset tags.

## Application constraints


The application's square GPU VR surface can apply centered square Render Field
of View values. Asymmetric or non-square fields fail closed. Near and far
depth bounds become clipping planes.

BOUNDING_BOX follows the encoded source axes. The two opposite corners are
encoded as (x1,y1,z1,x2,y2,z2) in VPS-RCS millimetres. The Go
CroppingSpecification.BoundingBox field retains its existing interleaved
layout (x1,x2,y1,y2,z1,z2) and the reader/writer convert at the boundary.
Reversed or equal individual patient coordinates are permitted because an
oblique source cuboid need not increase along every patient axis. Coincident
corners are invalid.

Twin converts the two corners through the source volume's patient-to-voxel
transform and orders their coordinates in texture space. This preserves
the crop on regular oblique grids. Crops on resampled, irregular or gantry-tilted
source grids remain unsupported. An empty intersection, zero source-axis
width, nonfinite coordinate or invalid affine is rejected before applying the
view. Older package versions wrote six interleaved values directly into the
DICOM attribute. Such nonstandard files can be explicitly migrated through ReadWithOptions
with LegacyInterleavedBoundingBox, LegacyMissingAlphaDescriptor or
LegacySixteenBitAlpha selected
only for confirmed old-writer provenance. Write then emits canonical corners
and the required 8-bit alpha descriptor. The default reader follows the standard
and does not infer which layout an anonymous file intended.

References are retained as SOP Class UID/SOP Instance UID pairs and input
numbers must be contiguous from one. `Crop=YES` and `GlobalCrop=YES` must point
to existing cropping specifications. The supported crop path currently accepts
one effective bounding-box specification; composing several specifications is
rejected by the viewer application layer.

Authored standard states carry a non-empty Frame of Reference UID (0020,0052).
Applications must verify that every referenced input instance belongs to that
single frame before calling `vps.Write`; the writer rejects an authored
standard state that omits the frame UID. `vps.Validate` performs the same
non-mutating validation used by the writer, and `vps.SRGBICCProfile` returns an
independent profile header suitable for the supported one-input TRUE_COLOR
display contract.

## Current authoring and validation matrix

| Module or state | Library read/write | Twin import/application | Twin authoring |
| --- | --- | --- | --- |
| Single volume, 16-bit RGB and 8-bit alpha table | Zero-based RGB16/alpha8 descriptors with matching counts, 2-4096 entries, direct OW data, embedded sRGB | Normalized-domain DVR with the same bounded transfer table | Current normalized transfer sampled at 4096 entries, 16-bit RGB and 8-bit alpha quantization, explicit rejection of HU-domain, gradient-opacity or unsupported transfer state |
| Source-axis box on regular oblique grid | Two opposite XYZ corners in VPS-RCS | Transform both corners to the displayed source grid | Transform the selected texture endpoints to VPS-RCS |
| Input SOP Class/Instance pairs and one Frame of Reference | Preserved | Application import checks source references | Preflight verifies source references before producing the state |
| Two registered volumes, stream compositor, spatial registration reference | Unsupported | Explicit refusal | Explicit refusal |
| OBLIQUE_PLANES or segmentation crop | Unsupported | Explicit refusal | Explicit refusal |
| Private historical camera/preset state | Existing private payload read/write | Existing compatible application path | Standard geometry uses standard modules |

RGB and alpha descriptors must use the same entry count and starting index.
RGB entries are 16-bit and alpha entries are 8-bit. Missing alpha descriptors and
incompatible entries fail with ErrInvalidObject. Unsupported palette forms
fail before a renderer state is returned. Palette quantization gives a
bounded numerical difference. GPU round-trip qualification compares the
original and reloaded view at the same camera, crop, image size and quality.

Normative module definitions are in
[PS3.3 C.11.24](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.11.24.html),
[PS3.3 C.11.27](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.11.27.html) and
[PS3.3 C.11.23.3](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.11.23.3.html).


## Registered two-volume DVR

The existing Twin fused DVR classifies both sources at each ray sample,
corrects their opacities for physical sample spacing, adds the opacities with
saturation, and mixes RGB by their relative corrected opacities. The native
shader also shades the primary contribution separately. Spatial alignment
alone does not make this operation representable by a DICOM compositor.

A standard RGBA compositor uses the same two nonnegative weighting values
for red, green, blue and alpha. Their sum is at most one. Its alpha output is
therefore a convex combination of its two input alphas. For input alphas 0.25
and 0.50 the standard combination is at most 0.50, while Twin's fused sample
has alpha 0.75. Replacing only the compositor description would change both
opacity and subsequent ray accumulation. The weighting tables also contain
8-bit integer values. A different rendering algorithm can be implemented
under the standard, but it would be a separately selected presentation and
needs its own image qualification.

The current exact authoring profile consequently refuses active two-volume
fusion before writing any file. It retains both input volumes, registration,
presentation and the displayed frame. A Multiple Volume Rendering instance
with multiple streams, a compositor chain, or explicit spatial registration
continues to return an unsupported-payload error on import. Encoding a
single-stream substitute would lose the secondary source and is rejected.

The applicable definitions are
[PS3.3 C.11.32 Render Display](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.11.32.html),
[PS3.4 FF.2.1.2.1 volume rendering pipelines](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_FF.2.html) and
[PS3.4 FF.2.3.3.2 compositor equations](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_FF.2.3.3.2.html).

## Native GPU round-trip oracle

Both frontend packages provide TestLiveVPSTableObliqueCropRoundTrip. The
shared testutil/vpsqualification package also provides the direct worker check
TestLiveVPSSharedRoundTrip. The frontend checks
capture a non-default normalized RGB/alpha table and source-axis crop on an
oblique anisotropic synthetic DICOM volume, write and reload Part 10, verify
SOP references and Frame of Reference, and apply the reloaded state through
the frontend's VPS entry point. The original and restored states are rendered
on the native GPU worker at the same 65 by 65 output and physical sample
spacing. Every channel is compared with a maximum difference of two 8-bit
levels and mean difference of 0.10 levels. The test then verifies that the
frontend adopts the exact returned GPU image and that a malformed crop leaves
that image and presentation intact.

These tests require GPU_WORKER_BIN and WGPU_NATIVE_PATH. Without them they
report a skip. They use the native GPU worker and the Fyne software
presentation adapter.


## Window-function fidelity

VOI retains the standard LINEAR, LINEAR_EXACT or SIGMOID identifier through
library read and write. Authored normalized DVR uses an explicit LINEAR_EXACT
identifier because its address is continuous over center minus half-width to
center plus half-width. A standard LINEAR DVR import with width greater than
one is converted to equivalent continuous coordinates, center minus 0.5 and
width minus one. This preserves the standard's scalar lookup function.
Width-one LINEAR thresholds and SIGMOID DVR imports are refused because the
current GPU classification path cannot reproduce them. Planar MPR retains the
VOI identifier. Unknown identifiers are rejected.

The mapping follows
[PS3.3 C.11.2 VOI LUT](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.11.2.html).

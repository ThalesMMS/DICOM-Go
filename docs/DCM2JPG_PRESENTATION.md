# dcm2jpg presentation and export

`dcm2jpg` renders a DICOM image to JPEG, PNG or TIFF. Existing defaults remain:
all frames, JPEG quality 90, 8-bit grayscale or RGB; output extension selects
PNG/TIFF, otherwise JPEG. `-format jpeg|png|tiff` overrides the extension.
Multiple frames use `out-0001.ext`, `out-0002.ext`, etc.; `-frame n` selects
one 1-based frame. Output publication uses a temporary file and never replaces
the input image, explicitly supplied GSPS, or calibration curve. A later-frame
failure can leave earlier successfully published frames; this is not a batch
transaction.

```sh
dcm2jpg -overlays -window 40,400 in.dcm out.png
dcm2jpg -presentation -overlays -bits 16 in.dcm out.tiff
dcm2jpg -gsps state.dcm -rotate 90 -flip horizontal in.dcm out.png
dcm2jpg -auto-window ignore-extremes in.dcm out.png
dcm2jpg -auto-window histogram -histogram-ignore 0.05 in.dcm out.png
dcm2jpg -window-region 20,30,100,80 in.dcm out.png
dcm2jpg -monitor-curve monitor.txt -display-function gsdf in.dcm out.png
dcm2jpg -printer-curve printer.txt -display-function cielab -bits 16 in.dcm out.tif
dcm2jpg -rotate 270 -flip both -scale 2 in.dcm out.png
dcm2jpg -size 1024,768 in.dcm out.png
```

## Window selection

Exactly one explicit selector is accepted: `-window center,width`, `-voi n`,
`-min-max`, or `-auto-window strategy`. Without one, the first frame-specific
file window/VOI LUT is used; if absent, modality min/max is used. Malformed VOI
content or conflicting functional-group attributes are reported rather than
silently replaced. `-min-max` retains the existing full modality-range path,
including nonlinear Modality LUTs and constant images.

The new `-auto-window` strategies use `render.AutoWindowWith`: `min-max`,
`ignore-extremes` (drop all pixels equal to the minimum and maximum), and
`histogram` (drop the requested fraction at each tail). `-histogram-ignore`
defaults to 0.05 and requires the histogram strategy when explicitly supplied.
Its range is 0..0.5. Pixel Padding values/ranges are excluded. These strategies
support 8/16-bit grayscale with linear rescale; a nonlinear Modality LUT or
an empty/constant remaining range is refused. Use `-min-max` or an explicit
window for a nonlinear Modality LUT.

`-window-region x,y,width,height` restricts the automatic histogram to a
positive rectangle wholly inside the original frame (zero-based, before
GSPS/rotation/scaling). It selects automatic min/max when no strategy was
given and can accompany either other automatic strategy. It excludes manual
and file-VOI selectors. This differs from the unqualified full-range default.

## Presentation stages

`-overlays` burns separate Overlay Data and supported embedded overlay planes.
Image Frame Origin and Number of Frames in Overlay determine coverage; overlays
are not repeated on unrelated image frames. Invalid planes are reported.
`-presentation` applies the image's Presentation LUT/Shape and Display Shutter.
The non-GSPS order is modality → VOI → Presentation LUT and photometric inversion
→ shutter → overlays. MONOCHROME1 and an INVERSE presentation shape combine
by XOR. These image options are opt-in; the default continues to invert
MONOCHROME1 without applying its image shutter or overlays.

`-gsps state.dcm` reads a referenced Grayscale Softcopy Presentation State and
delegates to `gsps.Read`/`gsps.Render`. It checks the image SOP Instance UID and
1-based frame reference. The library applies the state's modality/softcopy VOI,
Presentation LUT, activated overlay layers, shutter, clockwise rotation and
horizontal flip, displayed area/crop/padding, then graphic and text annotations
in layer order. In this path the shutter covers the activated overlays.
Unactivated image planes remain invisible. The state's VOI replaces the image
VOI; there is no implicit min/max override when the state has none. Its
Presentation LUT controls polarity, including MONOCHROME1 conventions.

GSPS output is 8-bit because the existing GSPS compositor exposes 8-bit
P-values. `-gsps` with `-bits 16`, explicit VOI selectors, `-window-region`,
`-overlays` or `-presentation` is refused. It never expands an 8-bit GSPS image
and labels it as a precision-preserving 16-bit result. Displayed-area size modes
do not invent device-dependent physical dimensions; output has one pixel per
source pixel before an explicitly requested additional transform.

## Calibration and geometric transforms

`-monitor-curve` and `-printer-curve` are mutually exclusive measured device
curve inputs, using the library's `ParseCharacteristicCurve` text format
(`max`, optional `amb`/`lum`/`ord`, then DDL/value pairs). `-display-function`
chooses `gsdf` (default) or `cielab`; an explicitly selected function requires
a curve. `-display-ambient` uses cd/m² (0 takes curve defaults, -1 means none).
`-display-illumination` is printer-only, nonnegative, with 0 taking defaults.
Calibration maps the completed P-value image, including overlays/annotations,
to the measured device's driving levels scaled to the output's 8/16-bit range.
It is an offline export transform, not certification of a physical display.
The CIELAB library algorithm requires nondecreasing luminance with increasing
DDL; incompatible measured curves are explicitly refused. GSDF supports curves
whose luminance ordering differs through the library's sorted lookup.

Additional `-rotate 0|90|180|270` runs clockwise, then `-flip
none|horizontal|vertical|both` in the rotated coordinates, then nearest-neighbour
scaling. These transforms run after presentation/calibration and include all
burned overlays and annotations. `-scale` accepts finite factors 1/64..64;
`-size width,height` selects both positive output dimensions and excludes scale.
Nearest neighbour selects `floor(destinationCoordinate * sourceDimension /
outputDimension)`; it preserves each selected 16-bit value. No physical spacing
or aspect-ratio correction is inferred.

## Precision, formats and finite limits

`-bits 16` requires PNG or TIFF and a grayscale image. It uses
`display.RenderGray16`, `Presentation.ApplyGray16` and a 16-bit calibration LUT;
the LIN OD presentation shape also builds a 16-bit input LUT for wide output.
adjacent acquired/modality values can remain distinct after VOI mapping. Output
samples are presentation values 0..65535 (or calibrated DDLs), not an export of
the original signed stored-pixel bytes. JPEG and color outputs remain 8-bit.
The grayscale-only presentation/calibration/automatic-window options are
refused on color images instead of being silently ignored. Existing RGB, YBR
and PALETTE COLOR conversion is unchanged.

The reusable `display.TransformImage` preserves Gray/Gray16 precision and
produces RGBA for other inputs. `display.EncodeTIFF` writes
little-endian, uncompressed, single-strip TIFF for Gray8, Gray16 and opaque
RGBA as RGB8. Gray16 samples are encoded directly, including subimage strides.
BigTIFF, alpha, compression, multipage TIFF and patient/resolution metadata are
not emitted. PNG uses the standard Go encoder.

Rendered input/output allocations are limited to 67,108,864 pixels per image,
including scaled and GSPS displayed-area output. GSPS input is a regular file
limited to 32 MiB, curve input to 1 MiB; both reads enforce those byte limits.
Calibration permits at most 4096 measured points and fitting order 8, and
refuses nonfinite interpolated luminance before constructing the display LUT.
These are local output/auxiliary-input bounds, not quotas on DICOM source file
size, decoding, all frames combined, process RSS or execution time. The CLI
does not add GPU/3D rendering, desktop workflow, CI, or a new codec dependency.

Tests cover each selector/transform, reference and coverage refusals, real
adjacent 16-bit PNG/TIFF samples, image versus GSPS stage ordering, annotations,
monitor/printer calibration, and atomic source protection. Existing opt-in
DCMTK min/max tests retain their one-gray-level tolerance. No new claim of full
DCMTK option or pixel parity is made by these synthetic checks.

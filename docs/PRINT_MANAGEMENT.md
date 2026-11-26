# Print Management

Package `printmgmt` implements the Basic Grayscale (`1.2.840.10008.5.1.1.9`)
and Basic Color (`1.2.840.10008.5.1.1.18`) Print Management Meta SOP Classes of
PS3.4 Annex H (2026d) on the normalized DIMSE layer. Commands for the Film
Session, Film Box, Image Box and Printer SOP Classes travel on the Meta SOP
Class presentation context; the SCP checks every command SOP Class against
that context (and a Printer-only context) instead of disabling context
validation. Nothing is printed implicitly: an application accepts the Meta SOP
Class, calls `Service.Bind` per association and serves the binding's
`NormalizedOptions`.

## Operations

| SOP Class | Supported | Notes |
|---|---|---|
| Basic Film Session | N-CREATE, N-SET, N-ACTION (print, collated), N-DELETE | One per association (H.4.1.2.1.3); Memory Allocation answers B600H |
| Basic Film Box | N-CREATE, N-SET, N-ACTION (print), N-DELETE | Image Display Format `STANDARD\C,R` only; the response carries the Referenced Image Box Sequence; N-SET, N-ACTION and N-DELETE address only the last created Film Box |
| Basic Grayscale Image Box | N-SET | MONOCHROME1/2, 8 bits, or 12 bits in 16-bit containers, unsigned |
| Basic Color Image Box | N-SET | RGB, 8 bits, Planar Configuration 1 |
| Printer | N-GET | Printer Status, Printer Status Info, Printer Name, Manufacturer, Model |
| Presentation LUT (opt-in) | N-CREATE, N-DELETE | Separate context/backend affirmation; Film Box and Image Box references; hardcopy IDENTITY/LIN OD or bounded 8/12-bit tables |
| Basic Annotation Box (opt-in) | N-SET | Film Box owns object lifecycle; configured vendor format and assignable positions; ASCII/UTF-8 text |

Film Box Min Density, Max Density, Illumination and Reflected Ambient Light are
kept; `FilmBoxPresentation.PrintDensity` returns them in optical density and
cd/m², falling back to 0.20–3.00 OD, 2000 cd/m² and 10 cd/m². A backend can
build a GSDF printer LUT from them (`display.DisplayCalibration`) or a LIN OD
table (`display.PrintDensity.LinODPresentationLUT`).

Native LUT/Annotation support requires explicit options and separately accepted
contexts. Zero options retain the legacy subset; Film Box/Image Box LUT
references are refused with 0106H. Not supported: Print Job (no Referenced Print Job Sequence is returned),
Printer Configuration Retrieval, retired SOP Classes, ROW/COL/SLIDE/CUSTOM
display formats, printer raster composition by the default backend, and SCP emission of Printer
N-EVENT-REPORT. Receiving interleaved Printer events is an explicit client
option described below. Optional attributes the subset
does not use are ignored with warning 0107H and listed in the Attribute
Identifier List.

## Receiving interleaved Printer events

`NewClient` preserves its existing unexpected-command error/cleanup policy.
`NewClientWithOptions` can opt into `ClientOptions.PrinterEvents`: it accepts
Printer SOP Class event types 1, 2 and 3 on the negotiated Print Meta or Printer
presentation context, and requires the well-known Printer SOP Instance before
calling the handler. A configured option with a nil handler explicitly consumes
and refuses a bounded event with `0211H`, then continues waiting for the
original operation's response. An unconfigured option retains legacy behavior.

The generic `NormalizedOperationOptions.EventReports` API likewise requires
explicit SOP Class/event-type subscriptions for a handler and an explicit
context policy for a Meta SOP Class. During a confirmed operation, the same
reader consumes and validates the event, sends its correlated response, and
continues under the original operation's response deadline. Default limits are
32 events, 4 MiB per wire/expanded dataset or reply, 16,384 graph nodes (including
sequence items and fragments), and 32 graph levels. Invalid context/class/type
or a handler failure can be acknowledged without replacing the original
DIMSE status. Dataset overflow, malformed command or transport failure instead
follows the operation's existing uncertain-outcome/abort policy.

Handlers run synchronously and must observe their context; they must not read
the association or start another operation. No second reader or abandoned
handler goroutine is created. The exchange records acknowledged event status
and local errors without retaining datasets. It does not listen between
operations, implement Print Job monitoring, infer completed physical printing,
or resubmit an N-ACTION with unknown outcome.

Synthetic loopback tests cover three interleaved events before N-CREATE,
N-SET and N-ACTION responses, fragmented messages, original correlation and
subsequent association reuse, explicit refusal, deadline/cancel and bounded
Deflated datasets/replies. Printer Meta context and well-known instance are
covered separately. These tests do not qualify a real printer or DCMTK session.

## Statuses

Film Session: B600H memory allocation not supported; C600H print without a Film
Box; C601H backend queue full; B602H session printed with no images. Film Box:
C602H backend queue full; B603H empty page. Image Box: C605H image above the
image or session byte limit. PS3.7: 0106H invalid attribute value (display
format other than `STANDARD\C,R`, C or R not an unsigned decimal in 1..limit,
or C x R above the image box limit; film session reference, enumerations,
pixel constraints, image box position), 0110H processing failure (second film
session, addressing an earlier Film Box, backend failure or timeout), 0112H
unknown instance, 0213H film box limit.

## Backend, jobs and limits

N-ACTION copies the working hierarchy (session attributes, film boxes, image
pixels) into a `Job` and calls `Backend.Print` once, under `PrintTimeout`; later
N-SETs cannot change a submitted job (H.4.1.2.4.3). A refused, failed or timed
out print is reported and never resubmitted, so a film is not printed twice;
the client likewise never repeats an N-ACTION whose outcome it did not see.
`MemoryBackend` captures jobs and reports a NORMAL printer; a real device
integration is an application Backend. Releasing or aborting the association
deletes the Film Session hierarchy (H.5); submitted jobs are unaffected.

Limits per association: film boxes per session (16), image boxes per film box
(64), rows/columns (8192), bytes per image (64 MiB) and per session (512 MiB),
Number of Copies (10), print timeout (30 s). The image box limit cannot exceed
`MaxImageBoxesCeiling` (4096); `NewService` refuses a larger value. Columns and
rows are each bounded by the limit before their product is checked, so no
`STANDARD\C,R` wraps the count (for example `STANDARD\4611686018427387904,4`)
and an N-CREATE never allocates more image boxes than the limit.
`Client.CreateFilmBox` refuses non-positive layouts and layouts above the
ceiling before sending them.

Images are preformatted: existing annotations and overlays may be burned in;
the service chooses no implicit modality, VOI or LUT transform. Explicit native
objects are separate backend composition inputs. `ImageFromGray` and
`ImageFromRGBA` convert rasters the caller rendered, for example with
`pixeldata/display` under its own window, without further processing.

## Printing one film

`Client.PrintFilm` runs a whole session for one `Film` (session attributes,
`STANDARD\C,R` layout, Film Box presentation, polarity, row-major images):
Film Session N-CREATE, Film Box N-CREATE, one Image Box N-SET per non-nil
image (other positions print with the Empty Image Density), Film Box
N-ACTION and Film Session N-DELETE. It refuses before sending anything a
layout above `MaxImageBoxesCeiling`, more images than boxes, a polarity other
than NORMAL/REVERSE, an image whose grayscale/color kind does not match the
client, or pixels outside the preformatted constraints (`ErrInvalidFilm`).
Warning statuses (PS3.7 0001H, 0107H, 0116H and Bxxx, such as B603H for an
empty page) are collected in `FilmResult.Warnings` when the response is valid.
Both Film Box client methods validate every returned Image Box reference before
returning usable positions. The reference count must match the layout, the
referenced classes must match the negotiated grayscale or color Meta SOP Class,
and the instance UIDs must be valid and distinct from each other and from the
known Film Session, Film Box and Presentation LUT. A malformed reference stops
`PrintFilm` before it sends image pixels or attempts N-ACTION. A failure
stops the session with a `*PrintError` naming the step,
the image box position and the status, and a best-effort Film Session
N-DELETE removes the hierarchy. A Film Box N-ACTION whose response was not
received is reported with `OutcomeUnknown` and is never repeated.
`PrintError.Error` and `StatusText` give the standard meaning of the status
and leave out the SCP's free-text Error Comment, which stays available
through `Unwrap` for applications that decide to show it.

## Example and evidence

`go run ./examples/print` renders a synthetic 12-bit frame with an explicit
window, prints it in a 1x1 film box and shows the captured job.

- `TestGrayscalePrintSession` (8/12-bit images, print film box and session,
  job isolation from later N-SETs, printer N-GET, deletion),
  `TestColorPrintSession`, `TestPrintRefusals`,
  `TestPrintWarningsForUnsupportedOptions`, `TestPrintTimeoutAndReleaseCleanup`
  and `TestConcurrentAssociationsHaveSeparateSessions` (with `-race`), all over
  loopback associations.
- Opt-in independent peer (`DICOMGO_PYNETDICOM_INTEGRATION=1`,
  `DICOMGO_PYTHON`): `TestPynetdicomSCUAgainstPrintSCP` has pynetdicom 3.0.4
  drive the SCP with `meta_uid` commands and compares the captured job pixels
  with the SHA-256 of what it sent; `TestClientAgainstPynetdicomPrintSCP` has a
  pynetdicom SCP record the command sequence, SOP Classes, Meta SOP Class
  context, positions, pixel attributes and pixel hashes the client sent;
  `TestPrintFilmTwoByTwoAgainstPynetdicomPrintSCP` prints a 2x2 sheet with
  `PrintFilm` on the same peer.
  Only grayscale is exercised against the peer; color is covered locally.
- `PrintFilm`: `TestPrintFilmTwoByTwoGrayscale`, `TestPrintFilmColor`,
  `TestPrintFilmCollectsWarnings`, `TestPrintFilmFailures` and
  `TestPrintFilmRefusesInvalidFilmLocally`.

## Requested physical width and oversize policy

`ImageBoxPresentation` adds Requested Image Size `(2020,0030)` as **horizontal
image width in mm**, and Requested Decimate/Crop Behavior `(2020,0040)` as
`DECIMATE`, `CROP` or `FAIL` (PS3.3 2026d C.13.5). Width is optional; a supplied
value must be positive, finite, at most 10,000 mm, and encoded as DS VM 1 within
16 ASCII bytes, including padding. Invalid lexical forms, VR/VM, non-finite or non-positive values,
and unknown enum values are refused. `BuildImageBoxWithPresentation` and
`Client.SetImageBoxWithPresentation` extend the existing APIs. An optional
row-major `Film.ImagePresentations` slice gives each non-empty image its own
request. All presentations are validated before the film session starts.

Physical DS/CS values and Pixel Aspect Ratio IS are validated from bounded
resident raw/string components before trimming ASCII SPACE. Nil or opaque
payloads, TAB/NUL, and incorrect VR/VM are refused without calling custom Value
methods. Pixel Aspect Ratio is IS VM 2, at most 12 bytes per component; the
single final whole-field SPACE padding allowed by PS3.5 6.4 is preserved even
when it makes the final component 13 bytes. A 13-digit IS or overlength DS with
trailing spaces is refused.

`Image.PixelAspectRatio` preserves the vertical:horizontal integer ratio
(default square) through parsing, encoding and captured jobs. The pure
`PlanImageBox` API computes image extent and oversize framing from explicit
printable box dimensions; it never treats a film-size name as printable area.
When width is omitted, normal presentation fills the box optimally. A behavior
without width, or magnification `NONE`, requires a configured native horizontal
pixel pitch; missing native geometry is explicitly unresolved. The image's
vertical physical pitch follows its Pixel Aspect Ratio. Geometry is bounded to
65,535 rows/columns, aspect components 1..2,147,483,647, and positive finite
printable dimensions at most 10,000 mm. Degenerate/underflowed extent is refused.

`PhysicalServiceOptions` is opt-in. Capabilities for requested size and each of
DECIMATE/CROP/FAIL independently distinguish unknown, supported and unsupported.
The supplied synchronous area resolver must return real configured printable
geometry for the film layout/position. It must obey its context and remain
bounded; it runs under the binding lock. A physical configuration is refused
unless `Backend` also implements `PhysicalBackend` and explicitly affirms
`HonorsPhysicalImageBoxPlans()`. This is a contract to **apply every plan during
print composition**, not an attribute-advertisement flag. Backend authors must
not affirm it while ignoring crop/resampling. The default `MemoryBackend` does
not affirm it; the protocol fixtures use an explicitly synthetic plan-capture
double, with no claim of printing or physical measurement.

With physical options, known unsupported requests and unknown capability are
refused with tagged 0106H. Unavailable geometry is refused; no target image,
polarity or presentation is changed. A subsequent valid update remains usable.
Absent attributes in a later N-SET retain their previously accepted values.
Zero service options retain the legacy 0107H response for valid optional physical
attributes. The new request is retained as descriptive data, with unresolved
plan and no physical capability promise.

A fitting image keeps its extent. Oversized `FAIL`, and oversized `DECIMATE`
combined with effective magnification `NONE`, return C603H as required by
PS3.4 H.4.3.1.2.1/H.4.3.2.2. `DECIMATE` reduces extent uniformly to fit.
`CROP` deletes whole source columns and/or rows around the center: the retained
count is floored to fit, and an odd discarded pixel goes to the right/bottom.
If even one source row/column cannot fit, the request fails C603H. No arbitrary
policy is substituted. An omitted policy defaults to FAIL unless the service
explicitly configures a different default.

Accepted crop/decimation plans return B609H/B60AH, respectively. These warnings
mean the specified deferred presentation has been accepted for processing
(PS3.4 H.2.5). They are **not proof of completed printing or millimetre accuracy**.
The original preformatted pixel data remains immutable in the working hierarchy
and copied `Job`. Each `ImageBox` carries the requested presentation and a value
`PhysicalPlan` with requested/accepted mm bounds, effective magnification and
normalized retained source interval. The affirmed backend performs raster
composition/resampling using the effective Magnification Type at print time.
This package does not implement a hardware printer, DPI negotiation, physical
calibration, or a fallback renderer.

For a strict caller, `Film.RequireImagePresentation` stops before N-ACTION when
a peer answers an explicit request with optional-attribute 0001H/0107H/0116H
warnings. Crop/decimation warnings are retained and can proceed because the
selected policy was accepted. No N-ACTION is retried and unknown outcomes retain
the existing error contract. Acceptance by a software peer proves protocol
exchange and plan/state coherence only. Physical scale still requires a measured
film from the configured printer.

Twin uses a persisted value-only per-node profile, common controls in both
frontends, and the same geometry API for a bounded 2D preview. Unknown profile,
film mismatch, orientation or native pitch is identified explicitly. Known
refusals preserve entered choices; explicit policies cannot silently downsample
an image beyond the print raster admission cap. The preview samples already
rendered source images and never performs application 3D rendering.

## Native Presentation LUT and Basic Annotation Box

`NativePresentationContexts` proposes Presentation LUT (1.2.840.10008.5.1.1.23)
and Basic Annotation Box (1.2.840.10008.5.1.1.15) on individual contexts, apart
from the Basic Grayscale/Color Meta SOP Class. `SupportedAbstractSyntaxes`
reports the service's finite list; `Bind` checks actual accepted contexts.
Negotiation alone says nothing about fonts, composition or optical calibration.
`NativeServiceOptions` therefore requires `NativeBackend` to affirm, separately,
that it applies the requested LUT with GSDF/film viewing conditions and composes
annotation objects at its configured positions. `MemoryBackend` captures jobs
and never makes these affirmations. Physical options and their independent
`PhysicalBackend` commitment remain required when physical plans are requested.

Presentation LUT supports N-CREATE and N-DELETE only. `PresentationLUT` is exactly
one hardcopy shape (`IDENTITY` or `LIN OD`) or one table. The strict resident
parser checks VR, VM and bounds before copying: US descriptor [256 or4096,0,
10..16], OW data exactly two bytes per unsigned entry, entries within output
precision. Shape/table combinations, wrong depth, unsupported VR or opaque/nil
resident values are refused. Tables align with grayscale Bits Stored 8/12;
color cannot reference these grayscale LUTs. Source image bytes and photometric
interpretation remain unchanged. `DisplayLUT` consumes the existing display
LUT/PrintDensity/GSDF APIs; it does not duplicate GSDF or prove physical optics.
LIN OD denotes linear optical-density input, whereas IDENTITY/table output is
P-values. The backend performs the requested conversion at print composition.

This explicit reference subset covers Film Box and Image Box, with the Image
Box reference overriding the Film Box reference. Film Session LUT references
are **unsupported**, reported as 0107H instead of silently claiming acceptance.
Film/image/density changes validate every affected image and physical plan
before replacement. Refused updates leave prior image, polarity, annotation,
LUT reference and plans usable. Native density preserves supplied zero minimum
density/reflected ambient light through presence flags; invalid US bounds,
illumination or degenerate ranges are refused without a calibrated-default
substitution. Legacy printer defaults remain available through `PrintDensity`.

Association-owned LUTs can be created before Film Session. Duplicate UIDs are
0111H; an absent instance is 0112H. N-DELETE while referenced returns 0110H.
Deleting Film Session removes its references and annotation hierarchy but leaves
LUTs available until explicit deletion or association close. Jobs copy each
unique referenced LUT once, alongside per-image/film UID references. No mutable
table is copied per Image Box, and captured jobs do not alias live objects.

Annotation Boxes are created/deleted by their Film Box; only N-SET is supported.
Configured `AnnotationFormat` IDs identify vendor formats and finite slot counts;
they never invent page coordinates. Newly created boxes have Position0
(unassigned). Mandatory N-SET Annotation Position can assign/reassign a slot
1..configured Slots on any UID of the last Film Box. UID order does not fix a
position. Duplicate assigned slots or out-of-range values fail atomically with
0106H. Client reference ordinals are a deterministic UID-selection convention.
Omitted Text String retains text; a zero-length value erases it while retaining
the assigned slot. An older Film Box is not addressable until it becomes last
again. A negotiated Annotation context produces the Referenced Basic Annotation
Box Sequence in Film Box N-CREATE, including an empty sequence when no format
is requested.

Native text supports default/ISO_IR6 ASCII and ISO_IR192 UTF-8, inherited from
Film Session; a supplied Film Box charset must match. Other native annotation
charsets are refused. LO allows at most64 characters including resident padding
and256 UTF-8 bytes, no controls/backslash, and known resident value types only.
Odd-byte64-character strings that would exceed the limit after writer padding
are refused. Position US/VM1 is checked independently from text. Changing the
Film Session charset while Annotation Boxes exist is refused before mutation.

`NativeLimits` bounds LUT objects (default16, hard64), resident LUT words
(default128KiB, hard512KiB; maximum4096 entries/8192bytes per table), and
Annotation Boxes per film (default16, hard64). Configured formats are at most32,
with CS IDs at most16 bytes. Existing Film Box, image/session bytes and DIMSE
wire/deadline budgets still apply. Admission precedes table cloning. Statuses
reuse existing 0120/0121 missing-attribute/value, 0106 invalid, 0213 resource and
0211 unsupported-operation responses, plus explicit 0107 ignored attributes.

`Film.Native` / `PrintFilm` validates native values and actual contexts before
creating anything, creates LUT then session/film, sets separate annotations and
images, sends N-ACTION once, and cleans session then LUT with one shared finite
cleanup deadline. Required native requests receiving 0001H/0107H/0116H stop
before N-ACTION. CROP/DECIMATE warnings B609H/B60AH remain accepted finite-plan
warnings. A lost N-ACTION response is never retried. Cleanup failure is reported
separately and cannot imply that a print did not happen.

Twin persists separate tri-state native and physical profiles and uses common
controls in MacStyle and WindowsStyle. Known refusal stops before dial; unknown
remains visible and can request native contexts under strict warning handling.
For already-rendered grayscale P-values Twin offers IDENTITY only, and separate
UTF-8 annotation text/position. It does not relabel these pixels as linear OD.
Color can request annotations but refuses a grayscale LUT. The 2D preview leaves
IDENTITY source pixels unchanged and shows a textual format/position summary;
it does not claim printer fonts, coordinates, measured scale or optical fidelity.
Refusal preserves entered choices, and unrelated node edits preserve both profiles.

Independent fixtures cover both directions with pinned pydicom3.0.2 /
pynetdicom3.0.4 and exact native statuses, context separation, charset/table
payloads, refusal preservation and strict zero-N-ACTION cases. The pinned Python
peer explicitly encodes/decodes inherited LO charset rather than relying on a
standalone dataset's default encoding. Optional DCMTK tests use real dcmpsprt /
dcmprscu for native LUT+annotation spooling, dcmprscp for LUT shape/table films,
and the existing dcm2img GSDF/LIN OD calculation comparisons. DCMTK's Print SCP
does not support Basic Annotation Box; that coverage comes from its SCU and the
independent Python peer. All are synthetic software evidence, never a hardcopy,
clinical acceptance or calibration measurement. Windows crossbuild is separate
from the explicitly deferred human native Windows run.

Authority: [PS3.4 H.4.9.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_H.4.9.2.html),
[H.4.4.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_H.4.4.2.html),
[H.4.2.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_H.4.2.2.html)
and [PS3.3 C.11.4](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.11.4.html),
[C.13.7](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.13.7.html),
current 2026d. The first C.13.7 cached read identified2026c; the final primary
refresh identified2026d. No claim of the full optional Film Session LUT scope,
Print Job, new film layouts or Printer Configuration Retrieval is made.

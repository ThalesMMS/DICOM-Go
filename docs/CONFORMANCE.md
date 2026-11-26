# dicom-go v0.1.0 capability scope

This document describes the implemented capabilities of `dicom-go` `v0.1.0`.
It is not a formal DICOM Conformance Statement and must not be read as a claim
of complete DICOM conformance.

[JPEG 2000 Part 2 collection encoding](JPEG2000_PART2_ENCODER.md) is an
explicit bounded profile. The direction-specific capability remains
provisional pending independent complete Annex J decoding.

## Non-patient Query/Retrieve

The opt-in [Hanging Protocol and Color Palette profiles](NON_PATIENT_QR.md)
reuse streaming C-FIND, AsyncSession and existing C-MOVE/C-GET suboperations.
`dimse.ColorPaletteModel` and `HangingProtocolModel` provide `SOPClasses`,
`PresentationContexts`, `ValidateFindIdentifier`, `Match`, `FindRoute`,
`RetrieveUIDs` and `StartCFind`/`StartCMove`/`StartCGet`. Applications explicitly
select contexts and install catalog/transport handlers; no catalog or service
is installed globally. The Association SCP dispatches their MOVE/GET SOP
classes through the shared retrieve machinery.

Color Palette FIND supports SOP Class/Instance UID matching and Content Label
single/wildcard/universal matching. Content Description and Content Creator's
Name are return-only; alternate descriptions and other optional keys are not
in this subset. Text accepts ASCII/ISO_IR 6 or UTF-8 ISO_IR 192. Patient, study,
series and Query/Retrieve Level keys are rejected. Retrieval accepts only SOP
Instance UID, with at most 128 distinct canonical UIDs, and validates selected
storage class/identity. C-GET additionally requires the matching accepted
Storage context, negotiated role and reverse C-STORE handler; C-MOVE uses
caller-owned destination policy and storage transport. Applications retain
IOD validation, persistence, indexing and authorization responsibilities.

The finite key/catalog/transport limits and cancellation contracts are detailed
in the profile document. Offline regression tests include
`TestNonPatientProfilesMatchAndRejectInvalidKeys`,
`TestNonPatientFindRetrieveWireAndIdentity` and
`TestNonPatientAsyncCancellationLimitsAndRoles`.
`TestNonPatientProfilesAgainstPynetdicom` is a separate opt-in gate enabled by
`DICOMGO_PYNETDICOM_INTEGRATION=1` and `DICOMGO_PYTHON`; the profile document and
[interop matrix](INTEROP_MATRIX.md) record its historical synthetic-peer scope.
Those records do not establish current acceptance by a production PACS or
complete IOD/optional service-class conformance. Retired Patient/Study Only
models remain outside the implemented profile.

## Private dictionaries

Optional caller-owned [private creator catalogs](PRIVATE_CREATORS.md) resolve
implicit VRs and inspection metadata by creator, group and block-relative offset.
Each dataset/item reserves its own blocks; explicit VRs remain authoritative and
unverified values remain opaque. Reservation helpers avoid collisions and report
invalid or ambiguous creators. No vendor catalog or de-identification retention
policy is implied by this capability.

The separate opt-in [curated catalog](CURATED_PRIVATE_CATALOG.md) supplies 246
reviewed DCMTK 3.6.9 definitions: GE CT, Siemens CSA headers, two explicit
Philips MR creator spellings, and the Philips, Siemens, Fuji (FDMS) and SPI
attributes found unresolved in the gdcmData corpus. Each carries its pinned source, hashes and license.
This qualifies dictionary resolution, not manufacturer validation or
permission to retain private data; CSA blobs are interpreted by the separate
[`siemens`](#siemens-csa-headers-and-mosaic-split) package. Unknown values keep their
existing fallback; the default parser does not import this package.

The independent opt-in [complete private DATA catalog](PRIVATE_CATALOG.md)
embeds pinned unmodified DCMTK/GDCM sources: 13,356 rows, including quarantined
records. Immutable row provenance preserves source, line, hash, VR alternatives
and disposition. All matching candidates participate in resolution; conflicting,
contextual or known contradictory VRs stay UN. Relative block offsets and fixed
full elements remain distinct. Finite source/normalization/partition limits are
checked before expansion. Source inclusion is not a promise that every row
resolves or validates a manufacturer's encoding. Defaults and the curated
catalog remain unchanged; license notices accompany the imported DATA.

## Dataset Deflate and trailing padding

[Writer options](DEFLATE_AND_PADDING.md) select raw dataset Deflate levels 0–9
with nil preserving the previous default, uncompressed File Meta and PS3.5 A.5
odd-stream NULL padding. Recursive padding Preserve/Remove/Create uses a shared
finite budget, reserves generated headers/values and writes zero chunks without
alignment-sized allocation. Item alignment counts Item Value only; Part 10
alignment includes its prefix. Under Deflate these are logical pre-compression
lengths, without physical compressed-size alignment. Group Length policies are
explicit. Generic writers can leave partial output; `TranscodePath` retains its
bounded transactional publication. An independent walker and 37 DCMTK zlib
reader/reconversion cases qualified this serializer subset, not full IOD or PACS
interoperability.

## Defined-length sequence writing

`parser.Writer` plans structural headers and encoded lengths, then emits defined
SQ and Item Values directly to the destination. It borrows unchanged values and
shares primitive encoding rules, syntax/charset inheritance, recursive padding
and Group Length policy with emission. CP-246 UN under Big Endian keeps its
required Little Endian item normalization. A Group Length made stale by a
serialization hook is refused before committing its defined ancestor.

[Defined sequence contracts](DEFINED_SEQUENCE_STREAMING.md) specify finite
combined SQ/Item depth 128, one million plan nodes, 64 MiB of logical plan backing
and 32 MiB of retained normalization/known pre-serialization hook-copy payload
by default. Positive overrides are explicit. The budget reserves the three
known callback-isolation copies cumulatively before Handle, together with
normalization and live policy scratch. These are separate logical admissions,
not a caller-input, validation-rule, arbitrary-hook or process-heap/RSS ceiling.
Ordinary unchanged resident values do not require sequence-sized wire buffers.

Pure bulk/deferred length metadata permits measurement without opening, reading,
seeking, replaying or spooling the source. Unknown necessary lengths are refused;
owned sources open only for one emission and close exactly once. Pre-serialization
hooks prepare once in depth-first order; post-write notifications describe actual
committed bytes and flush after the enclosing defined ancestor commits. Short
writes, cancellation and late hook/I/O errors can leave partial output; existing
completion/error information distinguishes committed bytes from success.

A frozen local synthetic campaign compared eleven size/depth/provider/validation
scenarios, five paired time/allocation samples and three fresh-process memory
pairs each. All 33 wire-byte/SHA pairs matched. For unchanged resident 64 MiB,
median B/op fell from 207.97 MiB to 0.252 MiB and sampled additional heap from
190.07 MiB to 0.282 MiB, with 21.0% lower median time. The deferred fixture had
28.2% more allocations despite lower bytes/time; caller/provider `io.Copy` also
contributes its copy buffers. Validation remains substantial: the no-hook 8 MiB,
depth-four case used 441.04 MiB/op and 63.55 MiB sampled additional heap after
this change because default validation copies and repeated subtree validation
remain. Heap/RSS samples can miss short peaks, and absolute RSS includes the
fixture/runtime. These measurements do not qualify all datasets or deployments.

## Canny and edge-guided segmentation

`imagefilter.Canny` supplies physical recursive-Gaussian derivatives,
interpolated non-maximum suppression and full 8/26-connected hysteresis in 2D/3D.
It returns independent binary/strength arrays. The finite dense profile requires
known positive spacing and finite pixels without holes/padding; zero thresholds
retain every positive peak, including noise, without automatic threshold inference.

`roi.ShapeDetectionLevelSet` uses explicit propagation/curvature and a supplied
potential or intensity-derived sigmoid. `roi.CannyLevelSet` uses unsigned distance
D in mm and D-squared/2 advection toward retained edges. Both share the existing
narrow-band engine and expose stopping reason, mean/sum curvature and source/crop
boundary policy. RMS tolerance retains the spacing-scaled convention. Stable
mask, iteration limit, empty band and zero time step do not imply RMS convergence;
RejectTouches returns nil/error when source or crop contact occurs.

Logical defaults bound Canny to 8 Mi voxels/512 MiB, new level sets to 16 Mi voxels/
1 GiB and livewire source/state/queue sizes. Checked products, metadata/mask/seed
visits, physical float32 ranges, dense arrays, worker scratch, EDT coordinates
and output headers precede callbacks/allocation. These budgets exclude caller
input and decoder caches and do not claim a process heap/RSS ceiling. Context
polling reaches Gaussian, NMS/hysteresis, EDT, initialization, band updates and
result publication; errors return no partial result or new cached livewire map.

Twin offers the two new Smart Contour methods and Canny livewire through its
shared backend and parameter form in both Fyne shells. Sigma is mm and thresholds
are rescaled intensity/mm. Mode changes invalidate incompatible cached trees;
frame capture follows library admission. Existing methods remain defaults.
These are segmentation/data operations and add no application 3D rendering path.

Ten frozen public ITK Canny cases and both original Canny-refinement cases meet
their fixed criteria. Four additional Shape probes qualify exact zero-potential
barriers and three-step curvature horizons, with unchanged Dice/Hausdorff bounds.
The two original weak-positive-edge Shape cases remain non-equivalent: Dice
0.43357/0.89282 and Hausdorff 15.556/6 mm. Their arrays, settings and original
criteria remain intact and their numerical/boundary differences remain work to
resolve; matching barriers are not full ITK parity or numerical convergence.


## Native transform families and ITK interchange

`registration` adds Translation3D, ScaleVersor3D and ScaleSkewVersor3D, plus a
separate native 2D translation/Euler/similarity/affine/composite API. ScaleVersor
uses the public additive matrix convention, finite atomic setters, copied
parameters, analytic Jacobians and independent clones. Finite constructed
zero/negative scales remain valid forward maps; inversion and bounded-FFD
positive-Jacobian certification impose their own stricter conditions.

Composite2D admits up to 1024 built-in linear components in application order,
with no nesting or repeated references. Reusing a component requires its Clone;
refusal precedes mutation, and optimized parameter blocks remain independent.
Serialization reverses ITK queue order once and does not preserve optimization
flags. This API adds transform mathematics and interchange, without a 2D
registration engine or application image rendering.

Native 2D readers/writers are separate from existing 3D APIs. Supported ITK
text/MATLAB/classic-HDF profiles reject dimensional/type mismatches explicitly.
HDF writing has a default 256 MiB complete encoded-file cap, bounded metadata
and writes at most 64 KiB per call. Dense arrays are borrowed from stable
read-only input, with geometry/count admission before scanning; this cap is
neither a heap/RSS ceiling nor a generic HDF reader-allocation cap.

Small linear HDF records must reconstruct the stored concrete family and
matrix/offset within `32*float64 epsilon*max(1,abs(actual),abs(reconstructed))`.
Noncanonical zero-value states and a native versor clamp that changes geometry
are refused before output; explicit affine representation can retain the latter
mapping. Cancellation and I/O errors can leave partial output. The writer
emits ASCII VLEN strings for metadata and classic symbol-table groups.

Pinned public SimpleITK 2.5.3/ITK 5.4 qualified 25 cases: 68 Go outputs read by
ITK, 68 public rewrites read back by Go, plus maximum-record and h5py layout
checks. Full affected-package native/Go1.22 tests, focused race and vet passed;
one unchanged legacy optional-tool test remained skipped. These results do not
qualify ANTs/Slicer/elastix deployments, clinical registration or performance.
See [transform fixture contracts](../registration/testdata/itk1345/README.md)
and [bounded-FFD initial-transform admission](LBFGSB_AND_BOUNDED_FFD.md).

## Static dictionaries and CLI startup

The standard tag/UID dictionaries use generated sorted entries and keyword
indices. Lookup normalization, canonical duplicate choice, contextual VRs and
owned type-query snapshots retain their documented behavior; contextual VR
metadata still has its small existing map. File diagnostics and tag parsing
let dcmdump avoid importing DICOM network and pixel codec packages.

A fixed Go1.27.1 darwin/arm64 study with warm executable/fixture file caches
measured ten alternating fresh-process pairs for help and synthetic Part 10.
Median paired launch-and-tool reductions were 4.087/4.026 ms and maximum-RSS
reductions 11.305/11.234 MiB. All forty outputs/exits matched; all pairs, including
the first-help outlier, remain recorded. Timing includes the time-wrapper launch
and process wait; RSS is BSD maximum utility RSS, not heap. No universal,
cold-cache or Windows claim follows. See [source, measurements and limits](CLI_STARTUP_MEMORY.md).

## Native photometric recovery and retired ARGB

RepairPhotometricInterpretation is explicit, native-only and absent from default
detectable recovery. Declared sample counts can select documented mono/RGB/ARGB
assumptions; known interpretations can supply sample counts. Every correction
has a diagnostic and requires consistent geometry and payload length; byte size
does not establish the producer's color model. Strict and legacy policies remain
unchanged. Compressed/ambiguous inputs fail explicitly. Retired ARGB alpha zero
uses source RGB; positive alpha overrides it by palette index, with opaque output.
Valid declared planar ARGB remains supported. HSV/CMYK metadata recognition does
not imply display conversion. See [profiles and limits](PHOTOMETRIC_RECOVERY.md).
Separate explicit [legacy pixel repairs](LEGACY_PIXEL_REPAIRS.md) normalize known
native planar/OW conventions, reversed RLE byte-plane significance, wrong palette
tags and retired ARGB/CMYK/HSV models, with diagnostics and strict defaults.
Same-syntax transcodes verify the normalized source; independent RGB16
qualification and mixed-depth/display differences are documented.

## CLI inspection and explicit repair

The [CLI profile](CLI_TOOLS.md) composes independent read policies in dcmdump,
dcmconv, dcmodify and storescp. Recovery defaults remain disabled, odd lengths
are rejected and reserved-byte strictness is an explicit opt-in. File adapters
apply finite byte/count/depth/fragment limits; storescp retains its reception
limits. Reports contain structural reasons, paths, offsets, VRs and lengths.
The combined Meta/dataset report defaults to 128 events, and incomplete or
truncated diagnostics prevent publication, editing or successful C-STORE ACK.

Meta diagnostics use a bounded replay of the same retained regular descriptor,
with unchanged cursor and explicit lifetime; bit-preserving SCP verification
replays its own private uncommitted spool serially. Checks establish recovered
Meta consistency and ordinary source-change detection, without promising
protection against concurrent dataset mutation. Absolute parser positions and
total-byte ceilings share one domain, including inputs near the configured cap.

Text selectors form a union of tags and concrete/wildcard item paths. Raw-value
exports use original source spans and 64 KiB copies, including native byte order,
text padding and encapsulated framing. Deflate uses a private bounded inflated
spool and identifies its logical coordinates; physical Deflate offsets, JSON
selection, SQ export and unavailable original spans are refused. Create-only
publication verifies lengths/hashes and rejects unsafe/existing destinations;
a batch can publish individual files before a later failure and reports them.

Dataset-only dcmconv output copies the exact body of a structurally verified
private Part 10 precursor. Nonzero Part 10 alignment and destination replacement
are refused in that mode. Group/sequence policies delegate to existing serializers.
A 96-case local matrix covers native/Deflate, policies and both output containers;
Deflate NULL padding is conditional on compressed-stream parity, not universal.

Twelve bounded GDCM fixtures qualify seven strict controls, two raw inputs using
existing syntax inference and three actual new parser-recovery policies. These
also have real dcmconv/dcmodify write/readback controls. Native/race tests, Go
1.22 external-link compatibility, CLI subprocesses and Windows cross-builds
passed. This is a local structural profile, without external IOD-oracle, target
PACS, clinical, native Windows runtime or performance acceptance.

## Durable audit transport and receiver

[DurableLogger](AUDIT_DURABLE_SPOOL.md) is opt-in and preserves the RAM default.
Private bounded disk records and checkpoints recover independently per
destination, with finite retry/age/count/byte limits, cancellation and explicit
suppression. Accepted unexpired records receive at-least-once send attempts;
TCP/TLS write success is not remote persistence, and crash before checkpoint can
repeat bytes. Corruption fails closed; uncertain disk failure requires reopening
and verification. Filesystem sync is not context-interruptible.

Twin's shared backend optionally joins its best-effort archive trail to the
spool using a persistent source cursor and pause epoch. Clinical disk payloads
require separate consent, and payload lost before enqueue cannot be reconstructed
from the minimal trail. The bounded RFC5424 receiver accepts UTF-8 controls and
unknown escapes in PARAM-VALUE while enforcing header/structured-data grammar;
`syslogrcv` provides a bounded private collector. Local socket/restart/failure
tests and Windows crossbuilds qualify the documented subset. Native Windows
durability, external repositories and complete ATNA conformance are unqualified.

## Dataset timezone conversion

`datasettime.Convert` and `ConvertWithReport` transform recognized DA/TM event
pairs and implicit DT recursively under an explicit fixed UTC offset. They use
stored/inherited context or a caller-supplied fallback; no machine timezone is
assumed. Explicit DT offsets remain independent. Partial precision and leap
seconds are preserved when representable, with errors for ambiguous shifts.

The [conversion contract](DATASET_TIMEZONE.md) specifies default refusal of DA
without a usable TM and the opt-in calendar-date policy/report. Structure and
changed text are copied; unchanged payloads, including pixels, remain shared
and read-only. Finite depth, element-visit and item-copy budgets bound authored
DAG expansion before structural allocation. Errors return no partial dataset
or report. This reusable conversion API supplies no geographic/DST inference,
complete IOD validation or persistence/workflow policy.

## Tractography Results stored objects

`tractography` reads and writes the materialized Tractography Results profile:
track sets, patient-space OF points, CIELab colors, algorithms/code macros,
all-point/subset measurements, per-track and FD set statistics, common metadata
and source-reference hierarchies. It stores results and performs no tract
reconstruction. Explicit source origin avoids inferred provenance. Outputs own
mutable data; finite cumulative structural/payload limits precede clones and
helpers. Root image bulk is excluded before generic Object construction;
deferred retained values are refused without provider reads. Signed content may
be inspected, while retained MAC/DigitalSignatures prevent rewriting.

Independent synthetic fixtures qualify modeled fields, IEEE bit fidelity,
Explicit/Implicit LE and BE reads, scoped IOD rules, ownership and cancellation.
Pinned DCMTK comparisons match modeled whole-instance fields after an explicit
SeriesNumber setter; its lost segment selectors and empty Laterality remain
non-equivalent. Authored inputs pass dciodvfy with an unknown-Laterality warning;
reference rewrites fail its missing-Laterality condition. This does not establish
an integral external IOD round trip or clinical/PACS acceptance. See
[the profile and qualification limits](./TRACTOGRAPHY_RESULTS.md).

## MR Spectroscopy stored signal access

`spectroscopy.ReadContext` reads `(5600,0020)` OF through the existing Object
accessors, including deferred streams. It preserves float32 IEEE bits, byte
order, frame/voxel/spectral ordering and COMPLEX, REAL, IMAGINARY or MAGNITUDE
representation. Finite output limits and actual/header value counts are checked
before allocation; the returned signal and axis metadata are owned copies.

The [spectroscopy contract](MR_SPECTROSCOPY.md) defines grid indexing and
optional Columns/sampling and Rows/evolution calibration. Coordinate helpers
require an explicit reference index and sampling convention; missing calibration
or an incompatible signal domain returns an error. The package does not infer
an absolute frequency origin, perform an FFT or validate the complete MR
Spectroscopy IOD. Synthetic endian/layout/ownership tests and the opt-in GDCM
fixture qualify stored signal access, not clinical interpretation or PACS use.

## Explicit C-STORE transcode sources

The receiving CLIs separately enforce [instance persistence](INSTANCE_PERSISTENCE.md):
canonical matching SOP identity, descriptor-relative no-follow access, complete
file publication without replacement, cancellation and redacted storage logs.
Windows and Linux tests qualify the documented trusted-directory model; this
does not add peer authentication or protection against privileged local mutation.

An optional [StoreSession source adapter](CSTORE_TRANSCODE_SOURCE.md) prepares
requested representations through the existing transcoder before offering them.
The default source behavior remains unchanged. Bounded preparation, source
revalidation, explicit lossy identity and value-free reports are separate from
negotiation and delivery. Independent peers qualify the documented RLE/JPEG-LS
lossless and synthetic JPEG Baseline profiles; unavailable runtimes do not create
codec offers. Pipeline refusal and unknown-outcome handling match the confirmed
C-STORE service semantics.

The separate [original-first retrieval source](NATIVE_RETRIEVE_SOURCE.md)
selects the accepted representation before preparation. Original raw bytes use
zero pixel decodes; selected native LE fallback is bounded and prepared once
before its command. Representation-specific bounds, caller-owned aggregate
spool/codec resources and real-response/selected-metadata observers are explicit
opt-ins. The local wire fixture matrix does not establish target-PACS behavior.

[Incremental dataset preparation](TRANSCODE_DATASET_PATH.md) shares the
frame planner, verification and atomic publication used by `TranscodePath`.
Its qualified profile is lossless little-endian, nondeflated data with ordinary
top-level integer Pixel Data. Only explicit streaming ineligibility permits the
established bounded resident route; codec, verification, cancellation and quota
failures remain preparation failures. Unsupported layouts and identity-changing
or lossy policies retain their documented resident handling.

Input snapshots count against the adapter's spool budget. Its remaining quota
bounds ordered frame spools plus staged output, independently of frame-in-flight
and per-output limits. A complete retained output is validated before publishing
a send handle; network copying skips Part 10 framing and carries only the
selected dataset bytes with the original SOP identity. Local synthetic C-STORE,
C-GET and C-MOVE cases checked complete pixels and selected syntax, including a
later-frame preparation failure before command submission. These are local
loopback observations, not qualification of a target PACS or Windows runtime.

A fixed 36-run resident/incremental comparison on macOS arm64 measured lower
whole-test-process lifetime RSS and higher preparation time in all six workload
pairs. Sampled temporary-file peaks are lower bounds; exact retained snapshot
and output totals are inventoried separately. The guide gives the corpus,
median/range tables and measurement limits. No universal speedup, process-memory
limit or representation cache is claimed by that historical incremental
comparison. Ordinary `NewSource` still prepares per Inspect/Open. The separate
opt-in `PreparedSession` retains verified built-in representations under an
aggregate owner quota and revalidates before each attempt. See
[C-STORE source ownership and limits](CSTORE_TRANSCODE_SOURCE.md#explicit-session-owned-prepared-representations).
A separate 12-worker tiny synthetic reuse campaign verified encoder66 versus2,
full pixels/identity/size and cleanup on darwin/arm64; its complete per-run
costs, logical-storage accounting and whole-worker RSS limits are in that guide.
The historical 36-run comparison does not measure this prepared-session path.

## Storage attribute coercion

`StorageSCPOptions.AttributeCoercion` applies metadata changes to an independent,
bounded dataset before `StoreHandler`. Received SOP identity must still match the
command; protected pixel/layout/LUT/overlay attributes cannot change. Reported
changes followed by handler success return B000. Hook failures/panics are refused
with a fixed diagnostic; streaming sinks cannot be combined with coercion.

`NewCoercedStoreSource` freezes every advertised representation before exposing
its descriptor. Subsequent opens replay those bytes without rerunning the hook;
SOP identity must agree across representations. Per-source clone/byte limits and
an optional shared `StoreCoercionBudget` bound retained encoded payloads. This
budget excludes transient copies, buffer capacity and callback allocations.
Close the source and its opened handles to release it. The repeatable `storescu
-s keyword-or-tag=value` applies after transcode and preserves its verification
report; per-source and batch coercion limits default to 2 GiB. These primitives
do not prove full IOD validity or interrupt a running context-free callback.
See [coercion, status, ownership and memory contracts](STORAGE_ATTRIBUTE_COERCION.md).

## Opt-in lossy transcode verification

`TranscodeOptions.MaxPixelError` bounds the error of newly encoded lossy
frames after a target decode. Its unit is an integral stored-sample step,
before modality or display transformations. The default block side is one;
larger blocks compare means independently for each component and include
partial edges. Signed errors can cancel within a block; the report also gives
the maximum unaveraged sample error. This is an encoding metric, not a measure
of source decode, display quality or clinical equivalence.

The comparison requires matching geometry, effective precision, signedness
and photometric domain. The supported integral MONOCHROME1/2, RGB and YBR_FULL
layouts normalize container high bits and planar order. Unsupported domains
fail explicitly. Zero requires identical samples only with a block side of
one. Nil preserves the previous verification policy; pass-through bytes do
not acquire a new measurement. Lossless verification remains pixel exact.

The serial, streaming and path adapters share bounded frame verification;
`dcmconv` and `storescu` expose the limit and block size. C-STORE source reports
bind measurements to the selected writer handle, and threshold refusal yields
no representation to publish or send. Fixtures prove local refusal and codec
behavior; they do not qualify a target PACS. See
[lossy transcode verification](LOSSY_TRANSCODE_VERIFICATION.md).

## Transcode output controls

`TranscodeOptions` and `dcmconv` expose maximum fragment bytes, Basic/Extended/
empty offset-table policy, native color planar layout, classic single-frame
Secondary Capture conversion and UID policy. Defaults keep the previous output.
Same-syntax repacking preserves codestreams without a codec; native planar
conversion reorders stored samples and lossless verification compares their
canonical layout. RLE, Deflated Image Frame, Encapsulated Uncompressed, HTJ2K
and JPEG XL retain one fragment per frame. Unsupported option/layout
combinations fail explicitly.

Secondary Capture conversion accepts only the documented classic single-frame
image profile, supplies derived identity and source references, and rebuilds
File Meta. It does not validate arbitrary IODs. Preserving identity is incompatible
with newly lossy encoding or Secondary Capture conversion. The IOD planar policy
includes a compatibility choice for retired color/1996 ultrasound profiles;
it does not claim that every current ultrasound IOD requires planar output.

Synthetic outputs were decoded per frame by DCMTK and GDCM for the qualified
planar, identity, Secondary Capture, JPEG fragmentation and JPEG-LS
single-fragment/Extended-table profiles, including the main pixels of files with
compressed icons. DCMTK also decoded split multi-frame
JPEG-LS; the tested GDCM rejects that split layout. Very small JPEG fragments
fail in both readers despite a valid encapsulation layout. External acceptance
is therefore profile-specific, without PACS or clinical-image qualification.
See [output contracts and reproducible reader evidence](TRANSCODE_OUTPUT_OPTIONS.md).

## RGB8 palette quantization

`pixeldata.QuantizePaletteColor` produces an owned Image Pixel Module from
native unsigned RGB8 (8/8/7, explicit planar 0 or 1), with one palette shared by
all frames. A bounded weighted median-cut histogram and optional serpentine
Floyd–Steinberg dithering produce unsigned 8-bit indices and direct 16-bit
RGB LUTs. Only the complete Pixel Data value is padded. The output is little
endian and shares no source values or identity/clinical metadata.

The default allows 256 colors and 16 million total pixels, with dithering off.
Reported RMSE and maximum absolute error are measured in stored RGB8 channel
units. An optional positive RMSE limit rejects the result. These measurements
are neither perceptual nor color-managed, and dithering may increase them.

The caller must build a compatible derived instance. Classic single-frame
Secondary Capture fixtures rendered identically by DCMTK and dicom-go within
the declared synthetic error bounds; this does not establish arbitrary-IOD or
clinical-image qualification. Multi-frame True Color SC requires RGB and is
incompatible with this output. Encapsulated, YBR, signed and higher-depth inputs
must first become native RGB8. See
[palette quantization and oracle reproduction](PALETTE_COLOR_QUANTIZATION.md).

## Attribute path templates

`pathtemplate.Compile` resolves top-level tags or dictionary keywords into an
immutable formatter. Explicit VM selection, absence policy, SHA-256/hash and
MD5 hexadecimal modifiers, and full-DA calendar-day offsets produce a portable
relative slash path. Literals are validated; attribute values become individual
sanitized components; the final path is validated again after concatenation.
Finite template, attribute, expansion, component and output budgets precede
unbounded decoding or growth. Deferred values are not read.

This is lexical formatting. Consumers own filesystem containment, publication
and uniqueness. Hash encodings and grammar are explicit API choices and are
not byte-compatible with dcm4che MessageFormat. Charset/numeric/date helpers
reuse the library contracts; sharing mutable Objects requires synchronization.
See [path templates, policies and limits](PATH_TEMPLATES.md).

## HL7 ADT events and sparse DICOM mappings

`hl7.ParseADT` supports A01 admission, A04 registration, A08 patient update,
A28 person creation, A31 person update, A40 merge and A47 identifier correction.
A01/A04 require encounter PV1/class; A08 keeps its optional visit behavior.
A28/A31 do not create a visit from a pseudo PV1. A47 exposes corrected and prior
identifiers separately, without implying an A40 merge or demographic update.

Explicit mapping APIs produce independent sparse UTF-8 patient or visit
attributes. The caller chooses the primary identifier and issuer; remaining
identifiers become Other Patient IDs Sequence items. Absent/empty fields mean
no update, while HL7 null can produce an explicit empty value. Unrepresentable
separators, invalid VRs and partial dates that cannot become a full DA fail
with value-free diagnostics. An admission timestamp offset affects dataset
DA/TM semantics and must be reconciled before merging these sparse attributes.

Messages retain shared ER7 escape/charset and ACK handling. These APIs describe
operations and attributes; applications apply archive updates and reconcile
identity conflicts. Synthetic message/mapper/ACK fixtures do not establish
hospital-interface qualification. See [HL7 ADT mappings and limits](HL7_ADT.md).

## Embedded DICOMweb server scope

`net/dicomweb` includes both the existing neutral client and a new embeddable,
deny-by-default server handler. The server profile covers all six native QIDO
search resources with bounded DICOM JSON or negotiated Native DICOM Model XML
queries, WADO study/series/instance
objects and metadata plus frames and same-origin bulk data, and STOW at
collection or study scope using either multipart Part 10 or DICOM JSON/Native
XML metadata with request-local uncompressed bulk data. An injected
`dicomweb.Renderer` provides
study, series, instance, selected-frame rendered representations and thumbnails
with bounded JPEG/PNG output. WADO object payloads and STOW requests are
streamed/staged without whole-study payload buffering; storage conflict and
idempotency decisions remain atomic backend responsibilities.

The current server profile uses DICOM JSON by default and supports negotiated
multipart Native DICOM Model XML for QIDO and WADO metadata. STOW accepts Part
10 instances or metadata-based payloads whose `BulkDataURI` values match unique
`Content-Location` parts in the same request. The metadata path currently
supports uncompressed `application/octet-stream` in Explicit VR Little Endian
and the bounded JPEG (.50/.51/.57/.70) / JPEG-LS (.80/.81) still-frame subset
documented in [DICOMWEB_SERVER.md](DICOMWEB_SERVER.md). Original compressed
codestreams are encapsulated without transcoding, with verified syntax/File
Meta, explicit frame URI bindings and refusal of contradictory or non-derivable
metadata. Other compressed STOW media and video/document transformations remain
unsupported. DELETE and implicit
transcoding remain outside this profile. Optional `RetrieveTranscodePolicy`
prepares WADO-RS objects/frames through bounded, verified, identity-preserving
lossless conversion with explicit registries and target allowlists. Accepted
stored lossy representations can pass through; new lossy encoding, UID changes,
repairs and verification opt-out are refused. Full top-level Dataset/Meta UID
values and unique occurrences bind staged Part 10 to the requested resource;
the additional selective scan skips pixels, while Deflate requires another
bounded inflate pass. MIME defaults, quality exclusions and syntax negotiation
select actual available representations. Raw frames preserve encoded/native
bytes; prepared video frames are explicitly unsupported.

`EnableWADOURI` adds the separately authorized `/wado` URI-service profile,
with strict UID/query/Accept negotiation and translation to the existing
injected Renderer. Region, Presentation State, anonymization and charset
operations return explicit 501. `EnableBulkDataRanges` enables single byte
ranges only on direct, known-size bulk with explicit immutable, value-relative
ReaderAt. Closed/open/suffix ranges return framed 206 or unsatisfiable 416;
malformed/multiple ranges or any If-Range return the full representation.
Reader owns the backing lifetime; ReaderAt is borrowed. Unknown-size,
sequential and multipart values retain full retrieval. Finite jobs/bytes,
private spools, cleanup and context checks apply; native blocking I/O is not
forcibly interruptible. Single-instance preparation waits for backend
completion; collections may truncate after later failures. Local loopback
clients qualify the implemented codec/URI/Range profiles, without arbitrary
codec, live-PACS, clinical or complete PS3.18 conformance claims. See
[retrieval transformation and limits](./DICOMWEB_RETRIEVE_TRANSFORM.md).

The Worklist Service (UPS-RS, PS3.18 section 11) is served when a
`WorkitemBackend` is configured: create, retrieve, update, change state,
request cancellation, search, subscribe (specific, global and filtered
global, with deletion lock), unsubscribe, suspend global subscription and
the WebSocket event channel, in DICOM JSON only. `ups.NewDICOMwebBackend`
implements it over the same state machine, subscriptions and outbox as the
DIMSE UPS SCPs; the client offers the matching calls and the event channel.
The state and cancellation transactions accept the optional `requester`
query parameter; the client sends it when `Options.WorkitemRequester` is
set, and sends the values of one search key with literal commas. See
[`DICOMWEB_SERVER.md`](DICOMWEB_SERVER.md) for endpoints, status behavior,
limits, security defaults, ownership, and interop gates.

The client covers QIDO-RS searches, WADO-RS metadata, objects, frames,
rendered and thumbnail representations (study, series, instance and
selected frames, with `viewport`, `window`, `quality`, `annotation` and
`iccprofile`), same-origin bulk data, and STOW-RS with Part 10 instances or
DICOM JSON/XML metadata plus bulk data parts. `Client` implements
`object.BulkDataURIResolver`, so `object.ResolveBulkData` can fetch the
`BulkDataURI` values of retrieved metadata; relative references resolve
against the endpoint and a reference to another origin is refused before any
request. See [`DICOMWEB_CLIENT.md`](DICOMWEB_CLIENT.md).

OpenID Connect is optional and separate: `net/dicomweb/oidc` provides
discovery, the native-app authorization code (PKCE S256, loopback) and device
logins, refresh and revocation as a `TokenRefreshFunc`, JWT/JWKS verification
of ID tokens, and a bearer-JWT `Authorizer` for the server. It is not part of
the PS3.18 profile and uses only the standard library; see
[`DICOMWEB_CLIENT.md`](DICOMWEB_CLIENT.md#openid-connect).

## Supported Transfer Syntaxes

Top-level JPEG/JPEG-LS Pixel Data also has an opt-in
[incremental encoded-frame API](ENCAPSULATED_FRAME_STREAMING.md). It delivers
complete frames through the existing parser, with bounded Item assembly,
cancelable backpressure, explicit payload discard and separate seekable deferred
roundtrip. Native frame streaming keeps nested Pixel Data separate from the main
image. Encoded streaming does not qualify other compressed syntaxes or progressive
pixel decoding.

Dataset parsing/writing is supported for:

| Transfer Syntax | UID | Scope |
|---|---|---|
| Implicit VR Little Endian | `1.2.840.10008.1.2` | Native dataset read/write. |
| Explicit VR Little Endian | `1.2.840.10008.1.2.1` | Native dataset read/write. |
| Deflated Explicit VR Little Endian | `1.2.840.10008.1.2.1.99` | Part 10 and raw dataset read/write with raw Deflate applied to the dataset stream after uncompressed File Meta. An odd-length deflate bitstream is written with one trailing NULL pad byte (PS3.5 A.5); readers stop at the final deflate block and ignore what follows. Over DIMSE every PDV of the data set is even (PS3.8 E.1): a bitstream copied verbatim from a source file without the pad is padded when sent, and a received data set is consumed through its last fragment whether or not the peer padded it. Pixel Data stays native (never fragments) and needs no pixel encoder or decoder; `MaxTotalBytes` bounds inflated bytes. Deferred value replay is not available while inflating. |
| Explicit VR Big Endian | `1.2.840.10008.1.2.2` | Native dataset read/write. An object keeps raw binary values in its `ValueByteOrder`; when that differs from the destination syntax, `WriteFile`, `WriteFileFrames`, `WriteDataSet` and `WriteDataSetWithValidation` reverse every fixed-width value (AT/OW/SS/US by 2, FL/OF/OL/SL/UL by 4, FD/OD/OV/SV/UV by 8; OB, UN and text unchanged) at any sequence depth, including values deferred on the source, and refuse a value that is not a whole number of units. Native frames a `FrameSource` gives `WriteFileFrames` are already in the destination order and are written unchanged. Byte-order conversion reverses 32/64-bit native OW Pixel Data at the sample width; see [pixel transcoding](PIXEL_TRANSCODING.md#byte-order). |
| Encapsulated Uncompressed Explicit VR Little Endian | `1.2.840.10008.1.2.1.98` | Encapsulated fragment parsing/preservation and frame assembly into native bytes without a pixel codec. |
| Deflated Image Frame Compression | `1.2.840.10008.1.2.8.1` | Explicit VR Little Endian data set (never inflated as a whole) with encapsulated Pixel Data: one raw DEFLATE (RFC 1951) stream per frame, one fragment per frame, padded with one NULL byte when odd (PS3.5 8.2.16, A.4.13). Builtin pure-Go decode through `pixeldata/deflateframe` with the decoded size bounded by the expected frame size (`ExpansionLimitError`), and explicitly selected pure-Go encode for per-frame transcode in either direction. Distinct from Deflated Explicit VR Little Endian. |
| JPEG-LS Lossless | `1.2.840.10008.1.2.4.80` | Builtin pure-Go still-image decode and explicitly selected pure-Go encode for the qualified NEAR=0 subset: ILV=0 decode/encode and unsigned RGB ILV=1/2 decode/encode. |
| JPEG-LS Near-Lossless | `1.2.840.10008.1.2.4.81` | Builtin pure-Go decode for [qualified unsigned profiles](JPEGLS_NEAR_LOSSLESS.md); `.80` remains strictly lossless. |
| MPEG-2, MPEG-4 AVC/H.264 and HEVC/H.265 video media payloads | `1.2.840.10008.1.2.4.100` through `1.2.840.10008.1.2.4.108` | Metadata and encapsulated Pixel Data preservation; no local decode or render. `video.ExportFragmentedMP4` and `video.MuxFragmentedMP4` remux H.264/HEVC (MPEG-TS or Annex B elementary stream) into fragmented MP4 without re-encoding. |
| JPEG XL still-image payloads | `1.2.840.10008.1.2.4.110` through `1.2.840.10008.1.2.4.112` | Base profile: metadata and encapsulated Pixel Data preservation; the default registry has no JPEG XL decoder. The explicitly registered pure-Go `pixeldata/jpegxl` codec decodes modular and JPEG-compatible VarDCT frames and `.111` JPEG reconstruction, encodes `.110` (lossless, signed monochrome by offset-binary level shift) and `.112` (8-bit, lossy authorization), and converts `.50`/`.51` to and from `.111` without decoding pixels. XYB VarDCT needs the optional `codecfull` libjxl adapter. See the [codec matrix](CODEC_CAPABILITY_MATRIX.md); neither becomes a default. |

Part 10 File Meta Information is read and written as Explicit VR Little Endian,
as required by the file format.

### Text value padding

PS3.5 6.2 pads UI values with one NUL and the other text VRs with spaces.
Readers take any trailing run of spaces and NUL bytes as padding in every text
VR (AE, AS, CS, DA, DS, DT, IS, LO, LT, PN, SH, ST, TM, UC, UI, UR, UT), at the
end of the value and at the end of each value of a multi-valued element, so
`CT\0`, `CT\0\0` and `CT \0` read and compare as `CT`, and a DICOMDIR whose
Directory Record Type is `PATIENT\0` loads. The trim runs before Specific
Character Set decoding, where NUL is otherwise a forbidden control character.
Only trailing bytes are removed: leading spaces, which are significant in LT,
ST and UT, and interior NULs stay. This matches DCMTK's automatic input
correction (commit `31924835b`). `core.TrimTextValue` and
`core.TrimTextValueBytes` implement the rule and the object, DICOMDIR, query
matching and validation readers share them. Raw access (`Element.RawBytes`,
`Object.GetRaw`) returns the bytes as read, and writing keeps them; only
character set conversion, which re-encodes each text value, drops the padding.
The validator reports NUL padding of a space-padded VR as `CodeValuePadding`
([validation](VALIDATION.md#uid-syntax-and-normalization)), so strict
validation still flags the deviation.

### Contextual VRs and authored values

Dictionary VRs such as "US or SS" and "OB or OW" are resolved from the data
set, following PS3.5 Annex A.1/A.2 and the IOD rules they defer to:

| Dictionary VR | Resolution |
|---|---|
| US or SS | US when Pixel Representation (0028,0103) is 0, SS when 1; taken from the element's own data set or item, else the nearest enclosing data set. An item's value never applies to its siblings. |
| OB or OW Pixel Data | Implicit VR: OW. Authored: OB for encapsulated values or Bits Allocated 8 or less, otherwise OW. |
| Other OB or OW, Overlay Data | OW. |
| US or SS or OW LUT Data | Implicit VR: OW (PS3.5 A.1 permits US or OW). Authored: US when LUT Descriptor's first value is 1, otherwise OW. |

Implicit VR readers read Private Creators (gggg,0010-00FF) as LO, the VR PS3.5
7.8.1 fixes for them, with or without a private dictionary (as DCMTK and GDCM
do); an undefined-length value in a creator slot, which only the sequence
grammar can read, stays UN. Private data elements without a private
dictionary definition stay UN.

Implicit VR readers replace the dictionary's default US with SS when the scoped
Pixel Representation is 1, without changing any bytes. Without a Pixel
Representation the default US is kept. A VR read from an Explicit VR stream is
never overwritten. `Object.ResolveContextualVRs` applies the table to authored
elements whose VR is empty and returns `ErrAmbiguousVR` when the deciding
attribute is missing or invalid; writers never pick an alternative themselves
and encode an empty VR as opaque UN. Descriptor values that PS3.3 defines as
unsigned (for example LUT Descriptor entries) remain unsigned regardless of VR.

Authored values must be representable in their VR on every output (Part 10, raw
data set, DIMSE, DICOM JSON and Native XML). Typed numbers must use the Go type
of their VR, text is accepted only under textual VRs, and IS/DS text must match
PS3.5 6.2 syntax and length, with IS inside the signed 32-bit range. Every UI
component must be a UID as defined in PS3.5 9.1: at most 64 bytes of digits
and dots, no empty component, no leading zero in a multi-digit component, and
an ISO/IEC 8824 root arc (0, 1 or 2, with a second arc of at most 39 under 0
and 1). Trailing NUL or space padding is accepted and an empty value stays
empty; a backslash inside a component starts another value, which is checked
too. Violations return `core.ErrValueNotRepresentable`, with diagnostics that
give the value index and length, not the value; values are never truncated,
reinterpreted or dropped. Raw bytes read from existing objects are preserved as
read and are not re-validated: a file with a non-conformant UID can still be
read, rewritten and cloned, and File Meta derived from such a read UID keeps
its bytes. DIMSE command UIDs repeat the identity of the dataset or service
they address and are encoded as given, so forwarding a legacy object works;
receivers keep their own UID checks. Library code that derives UIDs (for
example SR tracking UIDs) produces conformant values. 64-bit integers are
carried exactly, never through float64.

UN elements are preserved as received; undefined-length UN sequences are read
and written with Implicit VR Little Endian items in every syntax, and
`Object.RecoverUN` is the opt-in recovery of known VRs (PS3.5 6.2.2). See
[`UN_RECOVERY.md`](UN_RECOVERY.md).

Sequences and items are checked against their enclosing defined-length
container; overruns fail with `parser.ErrContainerOverrun`, and the opt-in
`RecoverContainerOverruns` repairs complete items with a wrong length and
reports each repair. `VRRecovery.CorrectGETextLength` separately enables the
bounded GE Implicit VR text-length repair (declared 13, proven 10); it reports
`ge-text-length-13-read-as-10` and leaves ambiguous or truncated inputs refused.
See [`CONTAINER_BOUNDARY_RECOVERY.md`](CONTAINER_BOUNDARY_RECOVERY.md).

`AllowPartialRead` / `ReadDataSetPartial` additionally retain completed items
and complete child elements inside interrupted sequences. Incomplete SQ/item
wrappers carry numeric paths and explicit container metadata; incomplete
primitives and fragments are omitted without fabricated delimiters or source
spans. `Elements` counts complete roots, while `RetainedElements` includes a
retained SQ prefix. Validation and ordinary write/decode refuse the incomplete
File; `AllowPartial` permits a new structurally closed prefix rewrite, without
restoring omitted data. Strict, deliberate prefix and validation-lifecycle
readers retain their contracts. See [partial sequence reads](PARTIAL_SEQUENCE_READ.md).

Malformed legacy files and raw datasets may be inspected through a dedicated,
explicitly opt-in Transfer Syntax recovery API. The default reader remains
strict. Recovery evaluates only the three native uncompressed syntaxes by
default, requires confidence and runner-up margins, preserves the original File
Meta, and returns value-free diagnostics. It never infers compressed or
deflated payloads. See
[`TRANSFER_SYNTAX_RECOVERY.md`](TRANSFER_SYNTAX_RECOVERY.md).

Header indexing can explicitly assume ascending root tags and stop after the
largest required root selector. Opaque sequence pruning can skip an unrequested
defined-length body without validating its descendants; undefined-length bodies
retain bounded framing checks. Both modes are opt-in and preserve the default
strict traversal. Their limits and synthetic benchmark results, including
default overhead and the absence of a consistent CSA primitive speedup, are
documented in [INDEX_HEADER_PRUNING.md](INDEX_HEADER_PRUNING.md).

`object.ReadFileOptions.StopBeforeTag` provides a bounded seekable Part 10
prefix read with complete File Meta. A stopped read consumes the qualifying
root header but leaves its value untouched and returns an incomplete File with
`ReadStopError`; ordinary write/decode paths refuse that incomplete File.
Prefix token accounting applies to the dataset pass, separately from File Meta.
Discarded sequence contents and an unread suffix are not conformance checks.
Deflated offsets and bytes scanned are logical, not physical source offsets.

Structural/value-representation validation and parser/writer lifecycle hooks
are available only through dedicated opt-in APIs. Existing readers and writers
do not enable them automatically. The engine covers VR/VM, dictionary and
selected cross-field rules. `validation/iod` checks IOD/Module Types 1, 2 and
the 1C/2C conditions it can formalize (about 63% of the 1C/2C rows, reported
by `iod.ConditionCoverage`), using rules generated from the PS3.3 2026d
DocBook. Conditions it cannot formalize are left to manual review, so
it does not claim complete IOD/module conformance. See
[`VALIDATION.md`](VALIDATION.md).

Validated writes borrow known immutable values for built-in reporting and retain
detached ownership for public hooks, rules and resolver fallbacks. Effective
replacements, retags, filters and sequence changes invalidate request-local
validation state; a replacement UID is checked before its header is emitted.
Finite copied/work/node budgets admit the introduced work before allocation.
They do not bound arbitrary callback allocations, unknown value backing or
process RSS. Unsupported changes of an opaque validation scope fail explicitly;
callbacks are not replayed to recover that scope. Existing callback ordering,
report occurrences, committed-byte accounting and partial-output semantics
remain applicable. See [VALIDATION_WRITER_MEMORY.md](VALIDATION_WRITER_MEMORY.md).

## Unsupported Transfer Syntaxes

The following transfer syntax families are not decoded or transcoded in
`v0.1.0`:


The registry may recognize these UIDs, but recognition does not imply decode
support.

See [CAPABILITIES.md](CAPABILITIES.md) for the current repository inventory.

## Headless Render and ROI Scope

`MPRViewState`, `VRViewState` and `MPRPresentationState` use explicit JSON
marshaling to preserve zero-value omission on Go1.22 and newer. The nine optional
fields retain Go zero-value semantics, including ignored sidecars and nil versus
non-nil empty slices. Forty-six frozen public-API cases preserve exact native
Marshal, MarshalIndent and Encoder bytes on both toolchains. Serialization stays
separate from validation. Anonymous embedding can promote MarshalJSON and omit
additional wrapper fields; named fields retain them. Encoding errors can gain a
MarshalerError wrapper. These are library JSON contracts, with no render-math,
GPU, frontend or performance claim. See [compatibility and limits](VIEW_STATE_JSON_COMPATIBILITY.md).

`dicom-go` now exposes application-independent clinical viewer primitives:

- `render`: 2D frame rendering, bounded PNG and 8-bit JPEG Baseline encoding,
  crop-before-scale aspect-preserving viewport rendering, pixel sampling, auto-window,
  stack geometry, volume construction, MPR/MIP/slab, oblique reslice, VR and
  CPR. Fused MPR accepts a secondary volume and explicit target-to-source
  mapper, reduces both scalar layers over the same physical slab extent, and
  applies VOI, palette, and alpha only after sampling. Mapping failure is
  atomic; domain coverage is reported and a wholly out-of-domain overlay
  returns `ErrFusionNoOverlap`. A mapper error matching
  `ErrMapperOutsideDomain` leaves only that sample unset.
  `ResliceMappedMPRScalarContext` runs the same overlay kernel on a target grid
  the volume does not own (for example a rendered 2D slice), and
  `ScalarPlane.CompositeOver` draws the windowed, optionally palette-coloured
  layer over that already presented base image.
- `roi`: raster masks, vector ROI rasterization, measurements, segmentation
  operations, 2D statistics and volumetric statistics.
- `object.FrameGeometryAt`: frame-specific Image Position/Orientation and Pixel
  Measures resolved from Shared and Per-Frame Functional Groups, with legacy
  top-level Image Plane attributes retained as fallbacks. NM tomographic
  reconstructions are placed from the Detector Information Sequence, Slice
  Vector and signed Spacing Between Slices (PS3.3 C.8.4); legacy multi-frame
  positions and Slice Thickness steps are opt-in recoveries
  (`FrameGeometryAtWithOptions`). Every value reports its source, and
  `object.ResolveSliceSpacing` is the Spacing Between Slices, Slice
  Thickness, 1 mm precedence for single-slice depth (see
  `GEOMETRY_GUARDRAILS.md`). The NIfTI exporter applies it to opt-in
  single-slice exports, including DX/CR/US frames without patient geometry
  written as a method-1 image grid (see `NIFTI_EXPORT.md`).

These packages are library computation surfaces only. They do not implement UI,
mouse interactions, overlays, menus, local viewer preferences, catalog
integration or persistence. SEG, SR, GSPS and RTSTRUCT serialization cover the
focused application data models exposed by the library; `gsps.Render` burns a
grayscale presentation state into a frame, while volumetric presentation states
remain outside this phase.

`VolumeMask.SignedDistanceTransform` and `RasterMask.SignedDistanceTransform`
return owned 3D and 2D maps in physical millimetres, or signed square millimetres
with `SquaredDistance`. Distances use foreground boundary voxel centres with
full-neighbourhood adjacency; only neighbours inside the grid define a boundary.
Inside is negative by default and the sign is configurable, including boundary
zero. Orthogonal anisotropic spacing and strictly ascending irregular volume
axes are supported. Empty/full masks have no boundary sites and return signed
infinity, an explicit difference from ITK's finite implementation sentinel.
This does not define a continuous half-voxel surface or a clinical margin.
Overflow-safe voxel and managed-capacity limits precede allocations; source
storage must remain stable, and cancellation returns no partial result. The
existing unsigned transform's active line or final square-root loop can finish
before cancellation returns. A pinned SimpleITK 2.5.3 / ITK 5.4 corpus covers
20 synthetic cases, all four sign/unit options and 15,120 finite values with
fixed absolute `1e-5` plus relative `2e-6` tolerance. Independent exhaustive
boundary-site searches cover irregular axes. These checks are numerical
qualification, without clinical or RSS claims. See
[signed distance maps](./SIGNED_DISTANCE_MAP.md).

RTSTRUCT rasterization accepts `CLOSED_PLANAR` contours with union semantics and
homogeneous `CLOSEDPLANAR_XOR` contours with symmetric-difference semantics.
Contour vertices keep their sub-pixel image position; they are not rounded to
the pixel grid (only values within 1e-6 px of a pixel centre are snapped, to
absorb coordinate round-trip error). `roi.RasterizePolygon` includes a voxel
when its centre is inside the polygon by the even-odd rule, with left/top
edges inclusive and right/bottom edges exclusive, so XOR-combined contours
equal the even-odd fill of the whole slice and polygons sharing an edge do
not overlap.
`POINT`, open contours, and private geometric types return
`ErrUnsupportedContourType` instead of disappearing silently.
`FromVectorROIs` also writes pixel masks (`VectorROI.Masks`) as
`CLOSEDPLANAR_XOR` contours traced by marching squares
(`roi.RasterMask.SubpixelContours`) half-way between inside and outside pixel
centres, so `Rasterize` returns the same mask; an ROI cannot mix masks with
vector contours. `roi.IsoContours` traces sub-pixel iso-lines of a scalar
field (samples outside the grid or NaN count as below the level, so every
line is closed) and `roi.LabelIsoContours` the boundaries of each label of a
label field.

### Grid conventions

Point↔index conversion follows one convention across `render`, `roi`, `seg`,
`parametricmap`, `rtdose` and `nifti`, the one of ITK: integer
indices are pixel or voxel centres; a continuous index maps to the discrete
index `floor(x+0.5)` (`render.NearestIndex`, ITK's `RoundHalfIntegerUp`, so
-0.5 is index 0); and a continuous index is inside an axis of `n` voxels when
`-0.5 <= x < n-0.5`, so an axis of zero voxels holds nothing. Samplers repeat
the edge voxels over the outer half voxel (`render.ClampContinuousIndex`); the
volume sampler widens that border by 1e-3 voxel for rounding. RT Dose and
parametric-map sampling follow the same domain, which is the one the DVH
already used. `render.Grid` holds a size, origin, spacing and independent
directions (non-positive spacing and singular directions are rejected), with
`ContinuousIndex`, `Index`, `IsInside`, `Congruent` and `SameGeometry`.
`Congruent` bounds the position of every index of the grid within a fraction
of its smallest spacing (ITK compares the origin and the spacing each against
that tolerance, which lets a spacing difference accumulate across the grid)
and the direction cosines within an absolute tolerance; `SameGeometry` also
compares the size. The ITK defaults are 1e-6; grids read from DICOM (SEG
frame placement, SEG slices in the viewer) use `DICOMGridCoordinateTolerance`
(a twentieth of a voxel) and `DICOMGridDirectionTolerance` (1e-3).
`Volume.VoxelToNormalized` returns the renderers' texture coordinate
(division by `n-1`, the inverse of `TextureToVoxel`); it divided by `n`
before.

### Physical-space VOIs, 3D statistics and peak

`roi.Sphere`, `roi.Ellipsoid` and `roi.Box` are volumes of interest in patient
millimetres (ellipsoids and boxes may be oriented by orthonormal axes).
`roi.RasterizeVOI` places them on a `roi.VoxelGrid` (origin plus one
displacement per index, so oblique and anisotropic grids are exact);
`roi.VolumeVOI` uses the canonical grid of a `render.Volume` snapshot, the
regular `IndexToPatientLPS` grid irregular or tilted series are resampled to.
Membership follows ITK's spatial objects: a voxel belongs when its centre is
inside, the surface of spheres and ellipsoids is outside, box faces are
inside, and a point within a 1e-9 relative distance of the surface counts as
on it so ties do not depend on rounding. `VOIPartialVolume` instead weighs
each voxel by the covered fraction on a 5³ subvoxel lattice (1 to 16 per
axis). Voxels outside the grid never enter the mask; `VOIMask.Clipped`
reports a primitive whose bounding box reaches past the image.

`VOIMask.Statistics` gives the weighted voxel count, volume, sum, mean,
population variance and standard deviation and weighted median, plus the
minimum and maximum over voxels whose centre is inside; voxels without a
value are counted in `MissingVoxels` and excluded. `VOIMask.Peak` is the
PERCIST peak (SUVpeak when the values are SUV): the 1 cm³ sphere (radius
6.2035 mm, configurable) centred on a voxel centre inside the VOI with the
highest mean. The sphere has partial-volume weights by default
(`CentreInsideKernel` counts whole voxels only). `PeakSearch` chooses the
candidate centres: the whole VOI (default), within a radius of the VOI
maximum, or on the maximum. A sphere that leaves the image or meets a voxel
without a value is skipped and `ErrNoPeak` is returned when none remains;
averaging the available voxels is opt-in (`AllowIncompleteSphere`) and
reported by `PeakResult.Incomplete`. Values are modality values; SUV
normalization is the caller's (it is a positive scale for SUVbw, so the peak
position does not change).

### Segmentation (SEG) types, overlap and conversion

`seg` reads and writes native (uncompressed) Segmentation objects of every
Segmentation Type (PS3.3 C.8.20.2):

`Segment.AlgorithmIdentification` optionally retains one Segmentation Algorithm
Identification Sequence item (0062,0007): family and optional name codes, name,
version, source and parameters. It follows the
[PS3.3 Algorithm Identification Macro](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_10.16.html).
The zero value omits this optional macro. Present macros require a complete
family, name and version; invalid VR/VM, empty/multiple identification items and
empty/multiple code items are refused rather than losing provenance. This
profile supports the short Code Value representation, LO/SH character limits,
and a conservative 10240-byte LT parameter limit. Parameters can retain a model
hash and preprocessing configuration; retaining a claim does not authenticate
its publisher or establish clinical accuracy. The existing Segment Algorithm
Name remains required for AUTOMATIC and SEMIAUTOMATIC segments.

| Type | SOP Class | Pixel Data | Frame payload |
| --- | --- | --- | --- |
| BINARY | Segmentation Storage | 1 bit, frames packed back to back | `Frame.Mask` (run-length) |
| FRACTIONAL (PROBABILITY or OCCUPANCY) | Segmentation Storage | 8 bits, 0 to Maximum Fractional Value (1-255) | `Frame.Fractional` |
| LABELMAP | Label Map Segmentation Storage | 8 bits, or 16 when a label exceeds 255 (`LabelMapBitsAllocated`) | `Frame.LabelMap`, every segment of a position |

Bits Allocated that contradicts the type is refused (`ErrUnsupportedPixelData`).
Pixel Data shorter than Number of Frames × Rows × Columns samples is refused
with `ErrTruncatedPixelData` (as DCMTK checks the frame count against the
buffer). `ReadWithOptions` with `AllowTruncatedPixelData` reads it anyway:
the missing samples are background (0, or a label map's background) and the `ReadReport` gives the
expected and actual lengths and the incomplete frames. Samples in an OW value
of a Big Endian data set are read as words (8-bit label maps byte-swapped
within each word, 16-bit ones Big Endian); OB values are byte streams.
A label map's background is its Pixel Padding Value (0028,0120) (PS3.3
A.51.4), or 0 when it has none (`LabelMapBackground`): frame masks, `ToROI`,
`ToBinary`, resampling and `seg2nii` leave it out, and a value 0 beside
another background is an ordinary segment. Writing records it as Pixel
Padding Value (US) in every label map, and never in BINARY or FRACTIONAL
objects; short frames are padded with it.
Writing follows the IOD: label maps carry no per-frame Segmentation Functional
Group and every encoded value, the background included, must be a described
segment; Dimension Index Values sit in the Frame
Content Sequence, over Referenced Segment Number (not for label maps) and
Image Position (Patient), or In-Stack Position Number when frames have no
position; the Shared Functional Groups Sequence is always present; Segments
Overlap (0062,0013) is computed (always NO for a label map). Earlier dicom-go
objects, with Dimension Index Values at the top of the per-frame item, still
read. `ToROIWithOptions` thresholds FRACTIONAL frames (default 0.5 of the
maximum).

`Overlaps` finds the segments that share pixels at the same frame position by
sweeping the run-length masks row by row, so its cost follows the runs, not
the pixels (200 segments of 512 x 512 in about 10 ms); `DisjointGroups` splits
segments into non-overlapping groups the way DCMTK's `OverlapUtil` does.
`ToLabelMap` converts BINARY segments that do not overlap into a label map
(adding a background segment, numbered `BackgroundLabel`, 0 by default) and otherwise returns the overlap report with
`ErrOverlappingSegments`; `ToBinary` converts a label map back, renumbering
segments from 1 as PS3.3 C.8.20.2.4 requires.

`Read` and `ReadWithOptions` remain native-only. `seg.ReadContext` explicitly
opts into a bounded lossless profile through `PixelDataReadOptions`, including
the source `TransferSyntax`, a caller-owned decoder `Registry`, and
`pixeldata.DecompressLimits`. No syntax is inferred from fragments; a nil
registry never uses the process global registry for compressed SEG. Registered
compressed decoders must implement `pixeldata.ContextCodec`.

| Encapsulated syntax | SEG types accepted by `ReadContext` | Sample layout |
| --- | --- | --- |
| Deflated Image Frame Compression (`1.2.840.10008.1.2.8.1`) | BINARY | 1 bit |
| RLE Lossless (`1.2.840.10008.1.2.5`) | FRACTIONAL; LABELMAP | 8 bits; 8 or 16 bits |


Each compressed frame occupies one nonempty even-length Item. Populated BOT/EOT
must match the frame Items exactly; EOT and Lengths must both be present with
an empty BOT. Number of Frames must equal Per-frame Functional Groups count,
and Bits Allocated/Stored, High Bit, unsigned single-sample layout and photometric
interpretation must agree with the SEG type. Decoded count, dimensions and frame
size are checked before a Document is returned. Frame-local unused bits in
Deflated BINARY are stripped when repacking continuous native bits; native
`ReadContext` uses the continuous stride explicitly, including an even-length
Pixel Data value pad. The older native readers retain their legacy padding
heuristic and opt-in truncation recovery.

Zero limit fields select finite decompression defaults: 100,000 frames,
256 Mi pixels, 512 MiB input and decoded pixel bytes, expansion ratio 1024.
The existing hard SEG dimension/frame limits still apply. Limits are checked
before invoking a codec; context is observed during decode, repacking, geometry
and mask/sample materialization. Pixel-byte limits do not claim a total RSS
bound: frame buffers, typed labels, mask runs and metadata also consume memory,
with work bounded by frame/pixel counts. A caller-supplied codec remains
responsible for honoring its context and owned-output contract.

Failures return no partial Document: `ErrInvalidEncapsulation` identifies bad
Items/offset tables/counts, `ErrResourceLimitExceeded` wraps typed decompression
limit errors, and codec/context causes remain discoverable with `errors.Is`.
Synthetic fixtures use standard-library raw DEFLATE and independent literal
PackBits planes; they cover native/encapsulated exact values, geometry/references,
non-byte-aligned frames, invalid tables, truncation, budgets and cancellation.
Twin's `LoadOptions.SEGReadOptions` pointer is also explicit opt-in: nil retains
native-only loading. Within that opt-in, nil Registry uses the build's codecs;
a supplied registry stays authoritative and the file's syntax is checked.

`seg.NewResampler` / `seg.Resample` place a segmentation on its own
patient-space grid (`roi.Grid`: the document's volume geometry, or its frame
planes with omitted empty planes restored) and resample it onto any grid of
the same Frame of Reference (the caller checks the UID): other size, spacing,
anisotropy, oblique orientation, gantry-tilt shear or irregular slices.
`Resampler.ResampleMapped` (with `roi.LabelResampleMode.Mapper` and
`roi.FractionResampleOptions.Mapper` underneath) resamples onto a grid of
another Frame of Reference: a `roi.PointMapper`, such as a Spatial
Registration's registered-to-source mapping or its per-point inverse, takes
each target voxel centre into the segmentation's space, and voxels it cannot
map stay uncovered; the mapped report lists no source, preserved or vanished
labels, which would need the inverse mapping. `rtstruct.StructureSet.Transform`
maps every contour point through such a mapper into another Frame of
Reference and fails closed on a point it refuses. BINARY
segments are resampled one by one, so overlapping segments keep their shared
voxels; a label map is resampled as one label image; FRACTIONAL samples are
interpolated linearly and thresholded as `ToROIWithOptions` does. The label
modes of `roi.ResampleLabels` are nearest neighbour (the default) and
`roi.LabelGaussian(sigma mm)`, which takes the argmax of Gaussian-smoothed
label indicators as ITK's `LabelImageGaussianInterpolateImageFunction` does
(its test case is ported) and then writes back any label the argmax dropped
at its strongest target voxel, so small labels do not vanish. Target voxels
outside the source's half-voxel extent are reported by a per-slice coverage
mask, never read as background; resampling honours context cancellation and
refuses targets over `roi.DefaultMaxResampleVoxels` unless the mode raises
the budget.

With DCMTK 3.7.0 (opt-in `DICOMGO_DCMTK_INTEGRATION=1` and
`DICOMGO_DCMTK_SEGORACLE`, a program built from
`seg/testdata/dcmtk_segoracle.cc`), BINARY, FRACTIONAL PROBABILITY and
OCCUPANCY, and 8- and 16-bit LABELMAP objects written by DCMTK dcmseg read here
bit for bit, the same objects written here read in DCMTK bit for bit, a label
map converted here reads in DCMTK, and the overlap matrix and disjoint groups
equal DCMTK's.

Every SEG `Write` produces carries the Type 1 and 2 attributes an archive
needs to file it under its patient (PS3.3 A.51). `Document.Common` (an
`object.DerivedCommon`, see [Common modules of derived
objects](#common-modules-of-derived-objects)) copies Patient Name, ID, Birth
Date and Sex, Study Date and Time, Referring Physician's Name, Study ID,
Accession Number and Position Reference Indicator from a source image, or
writes them empty (Type 2) without one, and names the creating software as
Manufacturer, Manufacturer's Model Name, Device Serial Number and Software
Versions (Enhanced General Equipment, Type 1; dicom-go unless
`Common.Equipment` says otherwise). `Document.Identification`
(`seg.Identification`) sets Series Number and Instance Number (1 by default)
and the Content Identification: Content Label (a CS value, `SEGMENTATION` by
default), Content Description and Content Creator's Name (empty by
default). Content Date and Time default to the time of `Write`, and Lossy
Image Compression is always `00`. With the DCMTK oracle enabled,
`DcmSegmentation::loadFile` logs no warning for objects of every type
written with and without a source; a control with Patient Name removed
shows the check sees such warnings.

### Surface Segmentation

`surfaceseg` reads and writes Surface Segmentation Storage
(1.2.840.10008.5.1.4.1.1.66.5, PS3.3 A.57). `Document.Segments` hold the
Segment Description Macro (`seg.Segment`) and the Referenced Surface
Sequence: per surface, its number, the Segment Surface Generation Algorithm
Identification (family code, name and version, Type 1) and the Segment
Surface Source Instance Sequence. `Document.Surfaces` are the Surface Mesh
Module: points as Point Coordinates Data (OF, patient millimetres, with the
Points Bounding Box Coordinates), unit normals as the Vectors Macro of the
Surface Points Normals Sequence (empty, Type 2, without normals), triangles
as a 1-based Long Triangle Point Index List (OL), Recommended Display
Grayscale and CIE Lab values, Recommended Presentation Opacity and Type,
Finite Volume and Manifold (derived from the mesh's closure when not set)
and Surface Processing with its algorithm. `ReferencedImages` fill the
Common Instance Reference Module. `Write` refuses a document that does not
validate: malformed UIDs, no segment or surface, surface numbers other than
1 to N in order, duplicate or out-of-range segment numbers, missing labels
or incomplete codes, algorithms without family, name or version, references
to missing surfaces, non-finite points, indices outside the points, normals
that do not match the points, opacity outside [0, 1] or values outside the
enumerations. `Read` also accepts triangle strips (their alternating winding
restored), fans, facets (fan-triangulated) and the retired 16-bit index
lists. `Surface.Primitives` preserves vertex lists, ordered edge pairs and
directed polylines alongside triangles without implicit triangulation. Invalid
edge pairing, short lines, out-of-range indices, conflicting populated Long and
retired lists, inconsistent counts and dangling references are refused with
`ErrInvalidObject`. `ReadWithLimits` checks aggregate point, triangle, primitive,
index and geometry-byte budgets before copying numeric arrays; `Write` enforces
the same default bounds. `RetiredIndexLists` permits explicit OW output up to
65535 points, while mixed representations normalize to Long. `FromMeshes`
builds a document from coloured segment meshes. `Document.TriangleParts`
refuses non-triangle geometry before glTF, OBJ, PLY or STL mesh export;
`Document.Parts` remains a mesh-only projection for existing callers. See
[Surface primitives](SURFACE_PRIMITIVES.md) for the representation and bounds.
The writer applies `object.DerivedCommon`; package `validation/iod`
finds no error in the written instances, and the round trip keeps every
point, normal, triangle, colour, opacity and reference.

`rtdose` parses native 16/32-bit RT Dose grids, applies Dose Grid Scaling,
retains dose units/type/summation and RT references, validates relative or
absolute Grid Frame Offset Vector geometry, samples registered patient-space
points, and exposes embedded or computed cumulative DVHs. Computed DVHs
rasterize RTSTRUCT contours on the dose grid, preserve XOR holes, account for
anisotropic/non-uniform voxel volume, report partial grid overlap, and are
cancellable and caller-bounded by voxel/bin limits. Encapsulated dose pixels,
ambiguous geometry, mismatched Frame of Reference UIDs, and unsupported dose
semantics fail closed. Only `GY` plus `PHYSICAL` is labelled physical dose;
relative and effective/biological values remain explicit.

`rtplan` reads RT Plan Storage (`1.2.840.10008.5.1.4.1.1.481.5`) into a typed
model: RT General Plan (label, intent, geometry, structure set, dose and plan
references), RT Prescription dose references, tolerance tables, patient
setups, the fraction scheme (beams, metersets and beam doses per fraction,
brachy setups, dose references), external beams with their beam limiting
devices (X/Y and asymmetric jaws, MLCX/MLCY with leaf position boundaries),
wedges, blocks and applicators. Conventional `Block.Contour` retains ordered
closed polygons in IEC BEAM LIMITING DEVICE millimetres at the isocentric plane,
with DS/count/finite and block-relation validation. `Plan.InspectAperture`
exposes one control point's jaws, clipped MLC gaps and declared block contours,
keeping unavailable outlines and derived jaw/MLC area explicit. See
[Conventional aperture inspection](RT_PLAN_APERTURE.md). Typed brachytherapy sources, application
setups, channels and control points are covered by the bounded family profile
described below. Conventional control points are resolved: each carries the
machine state inherited from earlier control points (angles, rotation
directions, energy, dose rate, table top, isocentre, jaw and leaf positions),
and records which attributes its own item encodes; Source to Surface
Distance and Cumulative Meterset Weight are not inherited. `Read` is
fail-closed and reports bounded structural problems in a `*ValidationError`:
counts that do not match their sequences, fewer than two control points,
control point indexes out of order, a first control point without the
attributes PS3.3 C.8.8.14 requires or without the position of every device,
leaf/jaw position counts other than 2N, bank 1 beyond bank 2, leaf
boundaries that are not N+1 increasing values, decreasing meterset weights,
a Final Cumulative Meterset Weight that differs from the last control point,
angles outside [0, 360), unknown rotation directions, duplicate numbers, and
beam, dose reference, wedge, patient setup, tolerance table or application
setup references that do not resolve. A STATIC beam that moves, unknown
defined terms and references to absent optional sequences are warnings.
Helpers compute the gantry arc (per-segment rotation direction), the jaw
opening and the MLC aperture area clipped by the jaws, in the plan's IEC
BEAM LIMITING DEVICE coordinates. These aperture helpers apply to conventional
beams; Ion and brachy data retain their distinct coordinate meanings. The
synthetic conventional test plans meet the
RT Plan IOD of `validation/iod`, and DCMTK's `drtdump` (DRTPlanIOD) reads
them without diagnostics when the DCMTK oracles are enabled; a control with
a Beam Number removed shows that oracle reports missing Type 1 attributes.
The model is written from PS3.3.

### Bounded RT family models

`rtplan` additionally reads and writes documented materialized profiles for
RT Ion Plan, temporary HDR/LDR/MDR/PDR/MANUAL brachy RT Plan, Beams/Ion/Brachy
Treatment Records, Treatment Summary Record and unsigned MONOCHROME native
single-frame RT Image. Typed models preserve declared references, optional
Type 2 empties, inherited machine state, FL spot data, specified versus delivered
quantities, brachy time/position fields and IEC X-RAY IMAGE RECEPTOR geometry.
Unsupported scanning/accessories, USER-origin salvage records, deferred metadata
and unsupported image layouts return explicit errors instead of partial models.

Writers require an explicit matching template and a new SOP Instance UID for
semantic edits. Retained MAC/Digital Signatures metadata is refused before
cloning; inspection does not verify signatures. File Meta and dataset share
finite metadata/value/depth/count/model admissions before generic facades,
byte-order conversion or cloning. Ignored plan/record bulk is excluded recursively
before Object bookkeeping and never replayed or republished. Output models own
their mutable data; source Objects remain stable during calls.

The seven-SOP synthetic corpus covers Explicit/Implicit LE and BE, round trips,
local scoped IOD/VR/VM checks, malformed data, limits, ownership and cancellation.
DCMTK reads 53 files and renders 16 RT Images with exact stored samples; drtdump
refuses 12 Beams/Brachy Record files despite exit zero. The pinned dciodvfy emits
169 errors and 57 warnings on positive fixtures; every occurrence is retained
and classified against actual fixture fields and consulted PS3.3 2026d rows.
This is qualified structural/profile evidence, with manual IOD conditions still
explicit and no blanket external IOD pass, dose calculation, delivery validation
or clinical/PACS acceptance. See [RT family models](RT_FAMILY_MODELS.md) for the
exact admitted fields, coordinate systems, quotas and diagnostic disagreements.


### Image registration

`spatialreg` parses Spatial Registration Storage
(`1.2.840.10008.5.1.4.1.1.66.1`) and Deformable Spatial Registration Storage
(`1.2.840.10008.5.1.4.1.1.66.3`). Matrix registrations compose Matrix Sequence
items in DICOM order, validate RIGID, RIGID_SCALE, or AFFINE constraints, reject
singular or ill-conditioned transforms, and name Source-to-Registered and
Registered-to-Source directions explicitly. Point, plane, and fixed-size
volume-extent mapping preserve affine scale and shear. Registered-coordinate
sampling delegates through the validated inverse to `render.VolumeReader`, so
the source `VolumeStore` remains the memory-budget authority and no transformed
voxel copy is materialized. Deformable registration uses only the normative
registered-to-source direction. It applies optional pre-matrix,
trilinearly interpolated Vector Grid Data displacement, and optional post-matrix
in that order. Grid dimensions,
byte length, orientation, origin, anisotropic resolution, endian, and
homogeneous matrices are validated before decoded-vector allocation. Points
outside the grid, triple-NaN neighborhoods, partial NaNs, non-finite values,
and malformed lengths fail explicitly. `MaxDeformableVectorGridBytes` rejects
oversized OF values in nested sequences before parser allocation, while
`spatialreg.Options` adds bounded defaults for bytes, voxels, and registration
items. Both Spatial Registration SOP Classes are in the default DIMSE Storage
profile. The reverse, source-to-registered direction is solved per point
(`Registration.MapSourceToRegistered` and `InvertPoint`) by fixed-point
iteration after ITK's InvertDisplacementFieldImageFilter: through the
inverse post-matrix, x ← x + ε·A⁻¹(y − A·x − b − D(x)) with the step halved
while the residual does not fall, at most 50 iterations. It fails closed
with `ErrFolding` when det(A + ∇D)/det(A) ≤ 0 at the solution and with
`ErrInverseNotConverged` when the residual stays above 0.1 mm (both
configurable); `Registration.VolumeChange` maps that determinant over the
grid for quality assurance and counts folded nodes, and
`Registration.Jacobian` computes it for a block of nodes only
(`GridRegion`, with the same neighbours as the whole grid) or as a summary
that does not allocate the grid (bounds, the most negative node, folded
nodes and their bounding block), on several goroutines and cancellable;
`VolumeChangeAt` gives it at one point. `NewGrid` builds a validated grid
from displacements. No inverse field is stored.
`spatialreg.Hops` and `FindChains` compose registrations that share a Frame
of Reference: affine items are used in both directions, deformable items
only Registered-to-Source, and no Frame of Reference is visited twice.
Every path up to the hop limit is returned, and a `Chain` reports its
SOP Instance UIDs, its product matrix and the most constrained type the
product satisfies; nothing is chosen among several paths.
`spatialreg.ResolveFrames` selects one mapping from parsed candidates. It
treats equal nonempty Frame of Reference UIDs as identity, refuses cycles
connected to either requested frame, and prefers a unique direct link over a
two-hop chain. A pointwise inverse deformation is eligible only for a direct
link. Ambiguity and unavailable mappings return typed reasons, and a selected
mapping returns its registration hops in application order. Point mapping
preserves domain errors.
`spatialreg.ValidateMatrix` exposes the reader's
per-type matrix checks so writers can apply them before emitting a matrix.
`spatialreg.Write` creates Spatial Registration Storage (PS3.3 A.39) from
validated matrices: the patient, study, equipment and registered Frame of
Reference come from a source image of the fixed series, the first
Registration Sequence item states the registered Frame of Reference with an
identity RIGID matrix, one item per source Frame of Reference follows with
its Frame of Reference Transformation Comment (for example the fit and its
FRE), and the images used are listed per item and in the Common Instance
Reference module. The result reads back with `ReadAffine` and passes the
PS3.3 module rules of `validation/iod`.
`spatialreg.WriteDeformable` creates Deformable Spatial Registration Storage
(PS3.3 A.39.3) the same way: one Deformable Registration Sequence item per
source Frame of Reference with optional pre- and post-deformation matrices,
an empty Registration Type Code Sequence, and the Deformable Registration
Grid with Vector Grid Data as little-endian OF. The result reads back with
`Read`, passes the PS3.3 module rules of `validation/iod`, and parses with
DCMTK dcmdump and GDCM gdcmdump.

`registration` holds the parametric transforms that automatic registration
optimizes: Euler angles in ZXY or ZYX order, versor rigid, similarity, affine
and composite transforms with ITK's parameter and fixed-parameter layouts
(centre of rotation kept as a fixed parameter), analytic Jacobians with
respect to the parameters and to the position, and inverses. Like ITK, a
registration transform maps fixed-image points to moving-image points, while
the DICOM Frame of Reference Transformation Matrix maps the Source Frame of
Reference (the moving image) into the registered one (the fixed image).
`registration.DICOMMatrix` therefore emits the inverse, typed RIGID for Euler
and versor transforms, RIGID_SCALE for similarity and AFFINE otherwise, and
validates it with the reader's checks; `FromDICOMMatrix` and
`FromAffineRegistration` invert a stored matrix back into a versor rigid,
similarity or affine transform. Stored RIGID matrices are accepted within an
orthogonality tolerance of 1e-6 (Decimal String rounding) and
re-orthonormalized through the versor. Coordinates stay in LPS patient space
throughout.
`ImagePrincipalMoments` computes physical LPS center and covariance with
explicit nonnegative intensity weights and a finite voxel cap.
`PrincipalAxisCandidates` returns four owned proper Euler rotations mapping
fixed center to moving center; repeated or weakly separated principal moments
refuse orientation. The actual image metric must resolve axis-sign ambiguity.
`optimize.Exhaustive` and `MultiStart` bound serial objective evaluations and
owned logical workspace. `CoarseSearchMinimizer` uses the existing Method hook
only at level zero, then the caller's independently bounded local refiner.
Coexisting immutable options, multi-start and nested grid scratch are reserved;
caller callback/local-optimizer allocations and RSS are excluded. Source images
stay borrowed and stable. Independent point/grid oracles and asymmetric
90°/180° voxel-permutation fixtures qualify the documented local behavior;
no ITK bitwise parity or clinical convergence is claimed. See
[global registration starts](GLOBAL_REGISTRATION_STARTS.md).

`optimize.LBFGSB` provides compact limited-memory Hessian updates, a
projected generalized Cauchy point and free-variable subspace minimization
with feasible line search. Finite parameter/workspace, iteration, evaluation
and line-search budgets are admitted before objective calls. Bounds and
returned parameters are detached. `FreeFormDeformation.BoundedOptimizer`
opts into absolute world-axis coefficient boxes, admitting all future meshes
and built-in Initial clones before control allocation; nil preserves the
legacy L-BFGS path. An optional conservative displacement-gradient norm below
one certifies a positive Jacobian only within the spline's full-support
region and an admitted orientation-preserving matrix composition. Bounds
alone do not prevent folds. Logical optimizer/control budgets exclude
caller callbacks, image smoothing, metrics, allocator overhead and RSS.
Independent dense/KKT/Jacobian oracles and an unregularized synthetic fit
qualify this library behavior, without clinical diffeomorphism or global
convergence claims. See [bounded FFD](LBFGSB_AND_BOUNDED_FFD.md).


`CorrectMotion` registers every phase of a dynamic series rigidly to a
reference phase (Mattes mutual information by default, mean squares or
correlation optionally; each phase starts from its neighbour's motion) and
resamples it onto the reference grid with `Resample`; phases are streamed
through a source and a sink and the run is cancellable. Only rigid motion is
corrected (no deformation, no correction of motion within a phase), and
nothing is written: a corrected series is a derived product of the caller.

`parametricmap` parses Parametric Map Storage with native integer Pixel Data,
Float Pixel Data, or Double Float Pixel Data. It resolves shared and per-frame
geometry, Real World Value Mapping, quantity definitions, UCUM units, source
references, and supported spatial dimension indices. Calibrated frames are
decoded lazily into a bounded LRU cache and can be sampled or resampled in
patient space and measured through raster ROI statistics. Float Pixel Data and
Double Float Pixel Data are IEEE 754 streams (PS3.5 VR OF/OD), so a stored NaN
or ±Inf is a legal voxel that producers use for unmeasured locations such as
the outside of a fitting mask; it is decoded as NaN or as the linearly mapped
±Inf instead of rejecting the map. Float Pixel Padding Value (0028,0122) with
Float Pixel Padding Range Limit (0028,0124) for Float Pixel Data, and Double
Float Pixel Padding Value (0028,0123) with Double Float Pixel Padding Range
Limit (0028,0125) for Double Float Pixel Data (PS3.3 C.7.6.24/C.7.6.25, with
the closed-range semantics of C.7.5.1.1.2), mark stored padding; padding
voxels decode to NaN and `DecodeFrameInto` returns an explicit padding mask
and per-kind `VoxelCounts`. Padding, NaN and ±Inf voxels never enter
sampling, resampling (they are invalid, hence transparent) or ROI statistics
(`Statistics.Unmeasured` counts them). A padding value without its Type 1C
range limit, a wrong VR or value count, or a NaN endpoint paired with a number
fails with `ErrInvalidPadding`; the other float module's pair is ignored.
Native integer Pixel Data uses Pixel Padding Value (0028,0120), optionally
with the inclusive Pixel Padding Range Limit (0028,0121). Both are interpreted
in the stored sample domain using Pixel Representation and Bits Stored. A
range limit without a value, inconsistent VR or multiplicity, or an endpoint
outside the stored value range returns `ErrInvalidPadding`. Padding is
identified before calibration and excluded from window estimation and
quantitative results. When `Read` accepts deferred native Pixel Data, it
retains the admitted byte payload so the input can close before frame decoding.

Multiple Real World Value Mapping items in the applicable dataset, shared, or
per-frame sequence can contribute to a scalar frame when their inclusive
stored-value intervals are disjoint and their coded units and quantity
definitions match. Quantity definitions also retain their coded modifiers.
The sequence order does not select a transform. Each finite sample uses its
own interval. An uncovered finite value becomes NaN and increments
`VoxelCounts.OutsideMapping`. Window estimates, spatial sampling and ROI
statistics then omit the sample as unmeasured. Explicit single-item bounds
use the same rule. Legacy single linear items without either bound still work.

Overlapping intervals, absent domains on multi-item sequences and incompatible
units or quantity require an explicit alternative. Use `ReadWithOptions`,
`ReadDatasetWithOptions` or `ReadMetadataWithOptions` with
`parametricmap.ReadOptions{MappingLabel: label}` to select exactly one
case-sensitive LUT Label per frame. The selected units and quantity must
agree across frames. Missing and ambiguous choices return
`ErrMissingMapping` and `ErrAmbiguousMapping` respectively. Admission is
bounded to 256 items per applicable sequence and 4,194,304 decoded LUT
entries per read. Integer data supports signed and unsigned domains and
lookup tables. Floating-point data uses linear mappings. See
[DICOM PS3.3 C.7.6.16.2.11.1](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.7.6.16.2.html#sect_C.7.6.16.2.11.1).
An overflowing linear calibration decodes to NaN for that voxel and
increments `VoxelCounts.MappingOverflow`. Window estimation, sampling,
resampling, and ROI statistics omit that voxel. Applying the individual
mapping directly still reports `ErrNonFinite` for the overflow.

Missing mappings, invalid mapping metadata, ±Inf through a LUT mapping
(`ErrMissingMapping`), unsupported non-spatial dimensions, inconsistent
geometry, and registration mismatches fail explicitly. Quantitative values are
returned with their units and are not passed through ordinary image VOI transforms.

`dynamic` extracts a frame-level temporal model without decoding Pixel Data. It
keeps temporal position, stack identity and in-stack spatial position as
independent coordinates and reads them from enhanced functional groups,
Dimension Index Sequence / Dimension Index Values and legacy temporal
attributes. Timing can use Frame Acquisition DateTime, Frame Reference Time,
Trigger Time / Nominal Cardiac Trigger Delay, Actual Frame Duration, Frame Time
or Frame Time Vector. The resulting timeline preserves irregular offsets,
gated phase metadata and multiple stacks. When explicit temporal indices are
missing, repeated spatial positions can be grouped by acquisition time; an
ordinary spatial CT stack with unique positions is not classified as dynamic
merely because its slices have different acquisition times. This package does
not schedule playback, decode frames, perform motion correction, register
stacks or infer clinical timing absent from the dataset.

## Structured Report reference and template scope

`sr` preserves structural by-reference Content Items, resolves forward and
backward paths into an immutable navigation index, and writes reference slots
without by-value macros. The relationship content constraints of all 23 SR
Document IODs (PS3.3 2026d Tables A.35.x-2, generated from the standard by
`cmd/srrelgen`) apply to by-value relationships (`ValidateRelationships`,
`ReadOptions.ValidateRelationships`) and, with each IOD's by-reference policy,
to by-reference relationships (`ResolveReferences`). Findings name the item
path, value types, relationship and table. Document serialization and
`WriteMeasurementReport` refuse a content tree that violates them.

Reference resolution and template validation are explicit operations with
strict or warning modes and caller-controlled resource limits. Generic
`ReadDocument` remains validation-compatible with existing callers, while
generic document serialization rejects an invalid by-reference graph rather
than emitting it. Built-in diagnostics describe only structural paths, stable
codes, and value-free messages.

Template validation runs against an immutable registry of explicitly
versioned definitions: the caller's own, or the generated PS3.16 ones
(`StandardTemplates`, `StandardContextGroups`). The template catalog holds 407 templates of PS3.16 2026d, extracted from the
same pinned DocBook: 404 numeric TIDs and TID 10003A-C. Integer APIs remain
available; additive canonical string-ID APIs expose every identifier. Includes
stay immutable references and resolve only the current content level under
shared matching/visit budgets. The four normative CAD self-includes accept
finite nested findings through an explicit productive by-value profile;
custom opt-in edges and every ordinary ancestor require ReferenceModeByValue.
Default, mutual, root and by-reference/either recursive cycles are refused.
TID 3900/3902 validate finite fixtures without materializing include combinations.
Definition admission, output/scopes, depth/items/steps/findings stay bounded;
truncated findings retain a typed resource error. MC rows whose condition
does not only test sibling rows are validated as optional, and value sets
are enforced only for DCIDs of non-extensible groups. Successful validation
applies only to those definitions and is not a general DICOM conformance
or clinical claim. `MeasurementReport` writes and reads the TID 1500 tree
(language, observer context, procedures reported, image library, Imaging
Measurements with TID 1501/1411 groups), item for item as DCMTK's TID 1500
classes write it, without the optional rows listed in the scope below.
See [`SR_TEMPLATES_AND_REFERENCES.md`](SR_TEMPLATES_AND_REFERENCES.md) for the
profile matrix, limits, PHI-safe diagnostic boundary, and independent-fixture
interop gates.

## DICOMDIR and De-identification Scope

`dicom-go` exposes small reusable object utilities:

- `dicomdir`: extracts existing references and provides bounded authoring for
  34 standard directory record schemas, with explicit SOP mappings and
  required/conditional selection keys, portable File IDs, append/refresh,
  two-pass relocated offsets, source revalidation and atomic publication.
- `deid`: provides a data-driven PS3.15 2026b Basic Application
  Confidentiality Profile planner, explicit profile options, recursive action
  processing, stable UID remapping, injected temporal shifting, verified
  safe-private rules, redacted reports, and caller-attested pixel/visual
  cleaning. Reversible de-identification stores the original values in the
  Encrypted Attributes Sequence (0400,0500) as CMS EnvelopedData (RSAES-OAEP
  or RSAES-PKCS1-v1_5 key transport, AES-CBC content encryption) for one or
  more recipient certificates, and `Reidentify` restores them with a recipient
  private key; Pixel Data is not stored. The earlier simple anonymization API
  remains as a compatibility helper.

These are not full application workflows. `dicomdir` does not copy or rename
source files or manage catalogs. Unknown SOP Classes and PRIVATE authoring fail
explicitly. Strict imported graphs retain hierarchy/order/opaque keys and
support selected Refresh while retaining SOP identity; Append preserves
existing keys/transfer syntax. Explicit retired MRDR compatibility preserves
aliases and recomputes live counts/targets; new file-sets use direct references.
Imported baselines must be prepared again after publication. Logical payload,
record/depth/control and encoded-byte budgets precede admitted copies; they
are distinct from temporary buffers and RSS. Six complete synthetic sources
and their DICOMDIR passed dicom3tools dciodvfy; DCMTK dcmmkdir append/update
reimported all six with documented charset/PN and optional ImageType warnings.
The 34-type matrix has internal schema/byte-graph qualification, without full
source-IOD/profile certification. External UV-reader abort, Surface Scan enum
refusal and current-rule retired MRDR rejection are preserved as limitations.
See [file-set authoring](./DICOM_FILE_SETS.md) and
[record schemas, updates and MRDR](./DICOMDIR_RECORDS_AND_UPDATE.md).
The strict `deid` planner implements the
versioned Table E.1-1 action surface and, when selected, the concept- and
Value-Type-specific Table E.3.4-1 Clean Structured Content actions. A
single-file de-identification plan rejects DICOMDIR because its directory
records and referenced media require one application-level transaction.
Output still requires IOD-, media-, and deployment-specific validation before
any conformance claim. It does not
perform automated PHI detection or OCR. The optional bounded image helper
proposes text-like edge regions for human review and carries no attestation.
Pixel and recognizable-visual-feature options require explicit
caller-owned cleaners and are only attested after those callbacks succeed. See
[`DEIDENTIFICATION.md`](DEIDENTIFICATION.md).

### Icon Image Sequence generation

`iconimage.Generate` creates an owned native 8-bit MONOCHROME2 or fixed Plan9
PALETTE COLOR Icon Image Sequence, with square output pixels and physical
aspect preserved by letterboxing. Width and height independently default to
64 and are bounded to 1..1024. Grayscale supports source VOI, automatic range
or explicit modality-domain min/max, with optional stored padding exclusion.
Source MONOCHROME1 is inverted once; color can be reduced to luminance.

The representative frame is decoded once through `FrameReader`, preserving
source buffers and supporting deferred pixels with a caller-owned `ReaderAt`.
Public defaults bound the entire source to 1024 frames, 16M pixels and 64MiB
native/input data; unsupported layouts, missing codecs and limits return
errors. The DICOMDIR adapter retains its existing 64/128 monochrome profile
and legacy decompression admission limits, now also bounded for native input.
Its explicit blank-icon fallback policy remains distinct from successful
image generation. Palette byte order, embedding ownership and cancellation
limits are documented in [icon generation](ICON_IMAGE_GENERATION.md).

### Video, JPIP and Streaming Transfer Syntax Policy

`v0.1.0` treats video, JPIP and streaming-oriented syntaxes as separate from
the still-image pixel codec path. Dataset metadata can be parsed when the
underlying dataset transfer encoding is otherwise readable, and encapsulated
Pixel Data fragments can be preserved as bytes. The library offers bounded
application-owned extraction/retrieval adapters, but it does not include a
video player or handle SMPTE ST 2110 media. The explicit-backend AVC
extension supports the bounded classic MP4 profile in [VIDEO_CODEC.md](VIDEO_CODEC.md).
Deflated Image Frame Compression is not a streaming syntax: it is a builtin
still-image codec (see the transfer syntax table above).

| Family | Policy | Expected behavior |
|---|---|---|
| MPEG-2, MPEG-4 AVC/H.264 and HEVC/H.265 video | Recognized encapsulated media with bounded validation, streaming extraction, and H.264/HEVC Annex B or MPEG-TS remux into fragmented MP4. `video.DecodeAVCMP4` and `TranscodeAVCMP4` use an explicit backend for single-track progressive AVC High/4.1 classic MP4, with constant presentation timing and bounded YUV420 samples. The new transcode emits a separate derived DICOM instance in .102 with synchronized compression history. | The optional `videoffmpeg` command adapter has no default registration. MPEG-2/HEVC retain extraction/remux scope. `pixeldata.DecodeFrames` keeps its typed non-renderable media error. [Profile and verification](VIDEO_CODEC.md). |
| JPIP Referenced and JPIP HTJ2K Referenced, including deflated variants | `jpip.Client` performs cancellable HTTP(S) retrieval under an explicit host/scheme allowlist, exact-origin credential policy, redirect revalidation, bounded retries/response size, and bounded representation LRU. `jpip.Server` serves JPEG 2000/HTJ2K targets as complete codestreams, JPP-streams (precinct data-bins) and JPT-streams (tile data-bins), with sessions and bounded resources ([`JPIP_SERVER.md`](JPIP_SERVER.md)). | Complete `image/jp2`, `image/jph`, and `image/jphc` responses decode through the registered JPEG 2000/HTJ2K codec. JPP/JPT responses that end with "image done" or "window done" are rebuilt into a codestream and decoded; streams cut short by byte or response limits, denied origins, partial/corrupt/oversized responses, offline endpoints, and codec failures return typed errors. |
| SMPTE ST 2110 video/audio | Recognized as media transport syntax; no active-video or PCM audio decoding/playback. | Treat as a media non-goal for `v0.1.0`; parse metadata where possible and preserve values, but do not attempt frame rendering. |

JPP/JPT data-bin assembly covers single responses and caller-driven
sessions (`jpip.DataBinSet`); a progressive client that keeps requesting a
window, and broader streaming-media decoding, remain outside this profile. Current rendering and command-line boundaries are documented in
their focused capability and command documents.

### Physiologic Waveform Profile

The `waveform` package parses 12-lead, general, ambulatory and 32-bit ECG,
hemodynamic, cardiac-electrophysiology, arterial-pulse, respiratory, routine
EEG, EMG, EOG, sleep-EEG, and body-position Waveform Storage. It preserves
multiplex groups, encoded channel order, source codes, timing, units,
sensitivity/correction/baseline calibration, padding, and supplied textual,
coded, numeric, and temporal annotations. The integer decoder recognizes `SB`,
`UB`, `SS`, `US`, `SL`, `UL`, `SV`, and `UV` in either DICOM byte order; a
Waveform Storage group is rendered only when its SOP Class permits the encoded
sample interpretation (`SB`, `UB`, `SS`, or `SL` in the supported physiologic
IODs). Other combinations remain an explicit raw fallback. Incomplete
calibration remains explicitly raw; the package performs no lead inference or
diagnostic interpretation.

Waveform Data can be deferred even when nested inside Waveform Sequence items.
`object.ReadFileOptions.DeferWaveformData` records each value location in
source order together with its enclosing item identity for seekable,
non-deflated sources. `Object.CopyNestedValueTo` reads a nested deferred
value, and `WriteFile` streams it back from the retained source, binding it by
its item offset; `WriteDataSetWithValidation`, or an item rebuilt without its
source offset, reports `object.ErrDeferredSequenceValueWrite`.

`object.ReadFileOptions.DeferNestedValues` extends
`InlineValueBytesThreshold` into sequence items: nested OB, OW, OF,
OD and UN values above the threshold, such as private blobs, overlay or LUT
data, stay on the source like top-level ones, while other nested VRs stay
materialized. At the root too, only OB, OW, OF, OD and UN values are deferred:
a large OL, OV, SV, UV, UC or text value is read into memory within
`MaxElementBytes` instead of failing the parse, so STOW-RS and C-STORE
re-encoding keep it. `DeferValueTags` limits the threshold to listed tags.
Sequences themselves are always materialized; reading, deferring and
rewriting reproduces the written bytes.
Applications may supply bounded `ReaderAt` sources to `waveform.Open`; the
lazy multiresolution min/max index preserves spikes, applies one recording-wide
memory budget, and caps each envelope query by viewport width.
Retired trial, industrial ultrasound-waveform, malformed, and unsupported
sample encodings return explicit raw-fallback metadata or errors.
The default DIMSE Storage SOP Class profile includes the supported physiologic
and recorded-audio Waveform Storage UIDs.

### Recorded Audio Waveform Profile

Basic Voice Audio and General Audio Waveform Storage decode to bounded recorded
PCM ranges through `Recording.AudioPCM`. The [audio profile](AUDIO_WAVEFORM.md)
specifies one aligned multiplex group, one or two channels, AU modality,
class-specific frequency/encoding constraints, PCM16 scaling and DICOM G.711
character representation. Padding and nonzero skew/offset are explicitly
unsupported. Audio decoding does not infer physiological or clinical meaning,
and library capability does not establish an application's device availability.

### Storage SOP Class catalog

`dictionary/uid` carries the Storage SOP Classes of PS3.4 2026e Table B.5-1
(current), Table B.6-1 (retired but listed) and Table GG.3-1 (non-patient
objects), plus the Storage SOP Classes of PS3.6 Table A-1 that PS3.4 does
not list (retired classes, DICOS and DICONDE): 214 classes, each with a
category (RT Dose and Parametric Map are images, the Image Storage SOP
Classes of CP-2588 in PS3.6 2026e; RT images, segmentations, DICOS and
DICONDE keep their own categories), the Modality its IOD requires when there
is one, its source table
and whether it is retired (`uid.StorageSOPClasses`, `uid.IsStorage`,
`uid.LookupStorageSOPClass`). The data is extracted by
`cmd/storageclassgen` from the chtml pages of the edition into the vendored
`internal/standard/storage_sop_classes.tsv`; an unknown class name stops the
extraction instead of being classified by guess, and a test fails when the
generated Go file is stale. The Media Storage Directory (DICOMDIR) and
Storage Commitment are not Storage SOP Classes for C-STORE.


## Storage SCP relative layout and event hooks

Storage SCP offers opt-in study directories, attribute path templates, collision
policies and bounded argv hooks after durable publication or generation
inactivity. Called AE and Study UID identify generations across associations;
pending stores suppress expiry. A separate poller retires quiet generations
even when a hook blocks. Events keep FIFO reservations and are attempts within
the current process, without durable outbox/retry/restart reconstruction.
Inactivity does not certify study completeness or Storage Commitment.

Publication uses pinned parents without following links/reparse points. Trusted
operator executables run without shell/PATH lookup, with bounded arguments,
queue, output and cancellation. Windows uses job/handle lists during creation;
Darwin/Linux use process groups. Successful Close joins; a deadline error can
return before remaining cleanup. Windows crossbuilds and host encoding proofs
are distinct from native Windows execution, which was not run. See
[layout, hooks, durability and platform limits](STORESCP_LAYOUT_AND_HOOKS.md).

## DIMSE Operations

For design notes and roadmap details on Query/Retrieve and Storage Commitment support, see:

- `docs/DIMSE_QR_AND_STORAGE_COMMITMENT_DESIGN.md`

Implemented:

- Asynchronous Operations Window User Information sub-item `0x53` and an
  explicit multiplexed DIMSE runtime:
  - absent negotiation preserves the synchronous `1/1` default;
  - finite and zero/unlimited asymmetric windows are encoded, decoded,
    negotiated and exposed in local endpoint orientation;
  - `dimse.AsyncSession` is the sole receive owner, correlates complete
    command/dataset messages by direction and Message ID, serializes whole
    outbound messages, and enforces invoked/performed backpressure;
  - explicit concurrent entry points cover C-ECHO, C-STORE (including
    encoded payloads via `StartCStoreEncoded`), C-FIND, C-MOVE,
    C-GET (including reverse C-STORE), and generic N-DIMSE;
  - legacy clients and SCP helpers remain synchronous and cannot be mixed with
    a session on the same association.
  - See [`ASYNCHRONOUS_OPERATIONS.md`](ASYNCHRONOUS_OPERATIONS.md).


Not implemented (still out of scope / planned):

Query/Retrieve (QR):

- Retired Patient/Study Only Query/Retrieve models.

Retired Patient/Study Only Query/Retrieve models are
classified as out of scope for `v0.1.0` unless a separate use case is opened.
Color Palette is implemented as the explicit bounded
[non-patient Query/Retrieve profile](#non-patient-queryretrieve); optional-key
and IOD coverage retain the limits documented above.

Notes / limitations:

- C-MOVE sub-operations (C-STORE) occur on a **separate association** from the
  C-MOVE association. As an SCU, a C-STORE SCP must be running at the Move
  Destination AE. As an SCP, application callbacks are responsible for opening
  and reporting those storage sub-operations.
- The C-GET SCU helper handles C-STORE sub-operations interleaved with C-GET
  responses for that one operation. The matching Study Root and Patient Root
  SCP wrappers use caller-provided callbacks and do not provide a production
  archive.

### Status and error mapping (design)

This section defines how status codes returned by planned Query/Retrieve and
Storage Commitment services will be interpreted and surfaced.

#### Query/Retrieve (C-MOVE / C-GET)

Status is taken from **Status (0000,0900)** in each response.

The implementation will classify response status values into these buckets:

- **Pending**: operation is still in progress; more responses are expected.
  - Typical code: `0xFF00`.
- **Success**: final response indicating completion.
  - Typical code: `0x0000`.
- **Cancel**: final response indicating the operation was canceled.
  - Typical code: `0xFE00`.
- **Warning**: final response indicating completion with warnings.
- **Failure**: final response indicating the operation failed.

Retrieve responses may also include sub-operation counters (informational):

- **Number of Remaining Sub-operations** (0000,1020)
- **Number of Completed Sub-operations** (0000,1021)
- **Number of Failed Sub-operations** (0000,1022)
- **Number of Warning Sub-operations** (0000,1023)

Unknown/unrecognized status values will be treated as **Failure** for API-level
error reporting, while preserving the raw status code.

A requestor may keep the N-ACTION association open and serve the committer's
N-EVENT-REPORT on it with
`StorageCommitmentWorkflow.AwaitResultOnAssociation`, which polls idle reads
without consuming a partial PDU, returns once the transaction is terminal in
the store (including a result delivered on a separate callback association),
when its bounded wait elapses, or when the committer releases; the requestor
then releases the association itself.

#### Storage Commitment (Push Model)

Storage Commitment uses N-ACTION to request commitment and N-EVENT-REPORT to
deliver results. Status is taken from **Status (0000,0900)**.

- **Success**: `0x0000`
- **Warning**: warning statuses (implementation-dependent)
- **Failure**: any non-zero status not classified as warning

For N-EVENT-REPORT, the returned dataset indicates per-instance success/failure.
Unknown/unrecognized status values will be treated as **Failure** for API-level
error reporting, while preserving the raw status code.

### Association negotiation and runtime behavior (design)

This section documents the intended/assumed association behavior for the planned
Query/Retrieve and Storage Commitment work.

#### AE titles


#### Presentation contexts


Notes:

- Identifiers (C-FIND/C-MOVE/C-GET request/response datasets) are encoded using
  the accepted transfer syntax for the QR presentation context.
- If multiple presentation contexts are accepted for a given SOP Class, the
  implementation uses the first accepted context.

#### Maximum PDU length

- The SCU proposes a configurable maximum PDU length.
- Defaults should remain conservative to work with common peers.
- Outgoing P-DATA-TF PDUs use the peer's Maximum Length (unlimited when it
  declared 0) capped by `DialOptions`/`AcceptOptions.MaxSendPDU`, default
  128 KiB as in DCMTK (`storescu -max-send-pdu`), and never below one PDV
  header plus one value byte (`Association.SendPDULimit`). PDV values keep an
  even length whenever the limit allows. With DCMTK 3.7.0 (opt-in
  `DICOMGO_DCMTK_INTEGRATION=1`) an 8 MiB C-STORE reaches `storescp` intact
  with `--max-pdu` 131072, 16384 and 4096, and dicom-go reads the PDUs of
  `storescu` at a 1 MiB Maximum Length.
- A-ASSOCIATE-RQ and -AC bodies, read before any authentication, are bounded
  by `AcceptOptions`/`DialOptions.MaxAssociationPDU`, default 1 MiB
  (`DefaultMaxAssociationPDU`, as DCMTK's `dcmAssociatePDUSizeLimit`);
  P-DATA-TF remains bounded by the negotiated Maximum Length. A larger request
  is refused from its header with an A-P-ABORT, invalid-PDU-parameter-value,
  and bodies are read in steps of at most 64 KiB beyond the bytes that
  arrived, so a declared length alone allocates nothing
  (`ReadPDUWithAssociationLimit`).
- Each PDU is encoded into one pooled buffer and written at once; the reader
  grows its buffer only with bytes that arrived (64 KiB ahead at most) and
  reuses it after P-DATA-TF, whose values own copies of their bytes.

#### Socket buffers

- `DialOptions.SocketBuffers` and `ListenOptions.SocketBuffers` set SO_SNDBUF
  and SO_RCVBUF (`ul.SocketBuffers{Send, Receive}`, at most 64 MiB each) on the
  requestor's socket before connect and on the listening socket before
  listen, so the window scale offered in the handshake can cover them; the
  listener also applies them to every accepted socket. `EffectiveSocketBuffers`
  reports what the kernel granted (Linux doubles the request; the kernel may
  cap it). DCMTK sets both options to one size taken from the
  `TCP_BUFFER_LENGTH` environment variable; dicom-go reads no environment.
- The default stays zero: the operating system size and its automatic tuning,
  which an explicit size disables on Linux and macOS. On a path with simulated
  latency the automatic tuning matched an explicit 4 MiB receive buffer and
  explicit smaller buffers lost up to fourfold
  ([`LARGE_DATASET_BENCHMARKS.md`](LARGE_DATASET_BENCHMARKS.md#dimse-socket-buffers)),
  so explicit sizes are for paths whose bandwidth-delay product exceeds the
  operating system's ceiling or for capping memory.

#### Timeouts

- TCP dial context and A-ASSOCIATE negotiation timeout are independently
  configurable. The negotiation timeout does not become the association's
  lifetime context.
- UL idle, read-progress, write-progress and release timeouts are configurable.
  Progress deadlines renew when bytes move and therefore permit a slow active
  transfer to exceed the configured interval.
- `dimse.SCPControls` can override command/dataset progress policy and configure
  an explicit absolute operation timeout plus cancel grace.
- DIMSE response timeout: configurable.
- For services with multiple pending responses (e.g., C-MOVE/C-GET), the timeout
  applies per response read, with an overall operation timeout configurable at a
  higher level.

#### Operation concurrency, cancellation, retry and backpressure

Current behavior:

- Legacy DIMSE SCU helpers guard one active request/response operation per
  association. A second helper operation on the same association fails with
  `ErrOperationInProgress`.
- `ul` implements Asynchronous Operations Window Negotiation, with absent
  negotiation defaulting to `1/1`. `dimse.AsyncSession` is the explicit opt-in
  runtime for negotiated concurrent operations; ordinary helper channels do
  not imply multiplexing.
- `OperationOptions` configures context cancellation, an overall operation
  timeout, and a per-response timeout for helpers with `WithOptions` variants.
- Operation failures preserve `errors.Is` matching for local cancellation and
  timeout through `ErrOperationCanceled` and `ErrOperationTimeout`. Errors after
  a partial write, timeout or canceled receive loop also match
  `ErrAssociationStateUncertain`.
- C-FIND leaves Release or Abort to the caller by default. C-MOVE uses
  `OperationErrorPolicyAbort` by default because in-flight retrieve operations
  can leave the association state uncertain. Callers using `WithOptions` variants
  can choose leave-open, release or abort behavior.
- The library does not automatically retry DIMSE command or dataset writes.
  Retry policy belongs to callers or service code that can decide whether an
  operation is idempotent and whether the peer or association state is still
  valid.

Implemented operation boundaries:

- The shared DIMSE operation executor provides an active-operation guard,
  context and timeout options, typed cancellation/timeout errors and explicit
  release-vs-abort policy after uncertain transport state.
- The bounded SCP service queue provides backpressure options such as maximum
  concurrent associations, maximum active operations, queue depth and enqueue
  timeout. `dimse.ServiceQueue` provides the reusable worker/queue boundary, and
  `cmd/echoscp` wires it to queue flags. When capacity is exhausted after an
  echo association has been accepted but before a command is accepted, `echoscp`
  aborts the affected association with a clear diagnostic. Future service paths
  should return a service-specific refusal or out-of-resources status when the
  protocol path supports it.
- `ul.AssociationServer` now applies the association limit before negotiation,
  including silent peers. Saturation may backpressure or return the PS3.8 local-
  limit-exceeded transient rejection. Its deadline-aware shutdown cancels and
  closes remaining associations after the grace deadline. Application handlers
  must still cooperate with context cancellation.
- `net/telemetry` provides safe, correlated lifecycle/PDU/DIMSE summaries and
  atomic counters. Endpoints are omitted unless an explicit plaintext or HMAC
  policy is selected. Raw P-DATA is a separate opt-in sink with mandatory byte
  budgets; A-ASSOCIATE/User Identity bytes are excluded.
- An operation window greater than one is qualified only through
  `dimse.AsyncSession`, which owns association reads, assembles command plus
  dataset, and routes both directions by Message ID. The legacy C-GET helper's
  scoped single-operation loop remains supported but is not a concurrent
  session.

#### Malformed A-ASSOCIATE-RQ

- Each User Information sub-item that PS3.7 Annex D allows once (Maximum
  Length, Implementation Class UID and Version Name, Asynchronous Operations
  Window, User Identity request and response) is refused when repeated.
- The malformed requests of DCMTK 3.7.0's `tparseassoc` and `tassocleak` tests
  (truncated or undersized SOP Class Extended Negotiation items, a duplicate
  User Identity, a Transfer Syntax item running past its presentation context,
  a presentation context without Transfer Syntax) are regenerated from the
  PDU layout in `internal/dicomtest` and fail to parse with a typed error; the
  acceptor answers each with an A-P-ABORT from the service-provider and its
  PS3.8 reason, allocating under 64 KiB. A username identity with a secondary
  field parses whole and is refused by policy with an A-ASSOCIATE-RJ.

#### Role selection (SCU/SCP)

- By default, an association is negotiated with the local application acting as
  SCU for the requested operation (QR or Storage Commitment).
- `ul.DialOptions.RoleSelections` can propose SCP/SCU role selection items by
  SOP Class UID, and `ul.AcceptOptions.RoleSelections` defines which requested
  roles the accepting side is willing to return: the reply is the
  intersection with the proposal and is omitted when that is empty, so the
  requestor keeps its default roles (an SCP-only entry that enables C-GET does
  not deny a C-STORE sender its SCU role). It never rejects a context.
- `ul.AcceptOptions.PresentationContextRules` configure an abstract syntax
  like a DCMTK association configuration profile: the rule makes it supported,
  its `TransferSyntaxes` replace `SupportedTransferSyntaxes` for it, and its
  optional `Roles` policy decides the context with the requestor/acceptor
  table of DCMTK's `dul.h`. A proposal keeps the roles both sides allow and
  the reply carries them; when none remain the context is rejected
  (user-rejection). A requestor proposing no role takes the default SCU role,
  so an SCP-only policy rejects it unless `AcceptOptions.AlwaysAcceptDefaultRole`
  is set (DCMTK `alwaysAcceptDefaultRole`, for requestors that omit the
  sub-item). A rule without `Roles` keeps the default roles and sends no role
  reply. Rules are validated when the association is accepted
  (`ErrInvalidPresentationContextRule`).
  `ul.AcceptOptions.CalledAEPresentationContextRules` replaces those rules
  for the Called AE titles it names (compared after padding normalization,
  case-sensitively), so a single-catalog listener answering several aliases
  can support a SOP Class, or allow a role, on some aliases only; other
  Called AEs keep the global rules. It is exclusive with `ul.CalledAERegistry`. With DCMTK 3.7.0 (opt-in
  `DICOMGO_DCMTK_INTEGRATION=1`), `storescu` gets the rule's transfer syntax,
  is refused by an SCP-only Storage rule unless `AlwaysAcceptDefaultRole` is
  set, and `getscu` negotiates the SCP role for the Storage SOP Classes it
  proposes.
- `ul.Association` exposes requested and accepted role selections so DIMSE
  service code can inspect the negotiated result.
- For operations involving sub-operations (notably C-GET, which delivers C-STORE
  sub-operations on the same association), role selection must allow the local
  application to act as SCP for Storage SOP Classes.

#### SOP Class Extended Negotiation

- `ul.DialOptions.ExtendedNegotiation` can propose raw SOP Class Extended
  Negotiation items for SOP Classes already present in presentation contexts.
- `ul.AcceptOptions.ExtendedNegotiationHandler` can accept, ignore or reject
  each requested item. Accepted data is returned in the A-ASSOCIATE-AC user
  information and exposed through `ul.Association.AcceptedExtendedNegotiation`.
- When no handler is configured, requested SOP Class Extended Negotiation items
  are ignored rather than implicitly accepted. A handler rejection rejects the
  association with `ErrExtendedNegotiationRejected`.
- Other unsupported user-information sub-items are preserved by the PDU codec as
  `UnknownUserItem`; association negotiation ignores them unless a future API
  assigns explicit policy.
- Query/Retrieve C-FIND service-class application information (PS3.4
  C.5.1.1) is typed by `dimse.CFindExtendedNegotiation`, all seven bytes:
  relational queries, combined date-time matching, fuzzy semantic matching of
  person names, timezone query adjustment, Enhanced Multi-Frame Image
  Conversion, empty value matching and multiple value matching. Absent
  trailing bytes read as "not requested/supported". `Item` builds the
  requestor's sub-item; `dimse.CFindExtendedNegotiationHandler` answers with the
  requested options the acceptor supports, at the request's length (a
  one-byte request gets a one-byte answer); and
  `dimse.AcceptedCFindExtendedNegotiation` reads the agreed options on either
  side. The C-FIND SCP dispatcher copies them into
  `CFindRequestContext.ExtendedNegotiation`.
- C-MOVE/C-GET service-class application information (PS3.4 C.5.2.1/C.5.3.1:
  relational retrieval, Enhanced Multi-Frame Image Conversion) is typed by
  `dimse.CRetrieveExtendedNegotiation` and reaches C-MOVE/C-GET handlers as
  `CMoveRequestContext.ExtendedNegotiation` / `CGetRequestContext.ExtendedNegotiation`;
  the library's retrieve SCPs do not change behavior on it. Storage
  service-class application information (PS3.4 B.3.1: level of storage
  support, digital signature support, element coercion, reserved bytes sent
  as 00H) is typed by `dimse.StorageExtendedNegotiation`;
  `StorageSCUExtendedNegotiation` gives the SCU defaults and
  `AcceptedStorageExtendedNegotiation` reads the acceptor's answer, which
  states the acceptor's own capabilities rather than an intersection.
- `dimse.ExtendedNegotiationPolicy{Find, Retrieve, Storage}.Handler()` is one
  `AcceptOptions.ExtendedNegotiationHandler` for the three service classes; a
  nil field or a malformed item ignores the request instead of rejecting the
  association. Private Storage SOP Classes are added with
  `StorageSOPClassUIDs`.
- Library behavior for the negotiated C-FIND options:
  - Relational queries: with `AssociationSCPOptions.CheckCFindIdentifier`
    the Patient and Study Root C-FIND SCP checks each Identifier with
    `dimse.ValidateCFindIdentifier` (PS3.4 C.4.1.2): keys below the
    Query/Retrieve level are refused, and unless relational queries were
    negotiated the Unique Key of each higher level must hold one value
    without wild cards and no other higher-level key may appear. A refused
    Identifier gets 0xA900 (Identifier does not match SOP Class). Attributes
    outside the PS3.4 C.6 key tables are not classified. Without the option
    the SCP stays lenient, as before.
  - Combined date-time matching: `qrmatch.Options.CombinedDateTime` (applied
    by `MemoryFindIndex` when negotiated) matches a DA key and its TM key
    given in the same form as one date-time range (PS3.4 C.2.2.2.5).
  - Timezone query adjustment: `qrmatch.Options.TimezoneAdjustment` reads DT
    keys without an offset and combined DA/TM pairs in the query's Timezone
    Offset From UTC and candidates in their own offset (or
    `CandidateTimezone`), comparing instants. DA or TM keys matched alone are
    not adjusted.
  - Fuzzy semantic matching of person names: see `qrmatch` and
    `MemoryFindIndex.FuzzyPersonNames`.
  - Empty and multiple value matching are available in `qrmatch` and used by
    `MemoryFindIndex` when each option is accepted. The ASCII `""` marker
    matches an absent or empty value for the VRs in [PS3.4 C.2.2.2.7](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_C.2.2.2.7.html).
    An eligible key with a defined VM greater than one requires every value
    to match, without wildcards. The response keeps all the candidate's
    values. [PS3.4 C.2.2.2.8](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_C.2.2.2.8.html)
  - Enhanced Multi-Frame Image Conversion remains an application-owned
    behavior.
- `findscu -relational`, `-datetime`, `-fuzzy`, `-timezone`, `-empty-value`
  and `-multiple-value` request the C-FIND options and print the requested and
  accepted sets to stderr.
  pynetdicom 3.0.4 decodes the Go requestor's seven bytes and its one-byte
  answer is read back; a pynetdicom requestor's relational, date-time query is
  answered by the checking Go SCP (opt-in, `DICOMGO_PYNETDICOM_INTEGRATION=1`).

#### SOP Class Common Extended Negotiation

- The PS3.7 D.3.3.6 sub-item (57H) is typed as
  `ul.SopClassCommonExtendedNegotiationItem`: SOP Class UID, Service Class UID
  and Related General SOP Class UIDs, each with a two-byte length, the related
  classes behind their own two-byte identification length. Version 0 is
  written (the header's reserved byte); a later version's Reserved field is
  ignored on receipt. Truncated or overrunning lengths and empty UIDs fail to
  parse with `ErrInvalidUserItem`/`ErrMissingPDUField`.
- `ul.DialOptions.CommonExtendedNegotiation` proposes the items, at most one
  per proposed SOP Class (`validateDialOptions` refuses others).
  `ul.Association.RequestedCommonExtendedNegotiation` holds them on both
  sides; the sub-item is A-ASSOCIATE-RQ only, so the acceptor sends no answer.
- `ul.AcceptOptions.CommonExtendedNegotiationHandler` lets the acceptor take
  a proposed abstract syntax it does not support as a Related General SOP
  Class it does support (by `SupportedAbstractSyntaxes` or a
  `PresentationContextRule`), tried in the requestor's order; the related
  class's transfer syntaxes and role policy then apply, the context keeps the
  proposed abstract syntax, and `AcceptedContext.RelatedGeneralSOPClassUID`
  names the class on the acceptor (also visible to C-STORE handlers through
  `CStoreRequestContext.PresentationContext`). `ul.AcceptRelatedGeneralSOPClasses`
  accepts any; a handler may check the Service Class UID. Without a handler
  such contexts stay rejected (abstract syntax not supported).
- `dimse.RelatedGeneralStorageSOPClasses` builds a Storage item (Service Class
  1.2.840.10008.4.2). A `StoreSession` treats
  `DialOptions.CommonExtendedNegotiation` as a catalog and proposes, on each
  association, the items whose SOP Class that association carries, so a new
  or private Storage SOP Class reaches a peer that only knows its related
  class. The C-STORE keeps the instance's own SOP Class UID; the library does
  not resend it under the general class, because the request's Affected SOP
  Class UID must match both the context and the data set.
- pynetdicom 3.0.4 decodes the Go requestor's sub-item, and a pynetdicom
  requestor's private SOP Class is accepted by a Go SCP that supports only its
  related class (opt-in, `DICOMGO_PYNETDICOM_INTEGRATION=1`). DCMTK 3.7.0's
  command-line tools do not send the sub-item, so there is no DCMTK oracle.

Current scope assumptions:

- For initial C-GET support, accept Storage SOP Classes with SCP role enabled on
  the same association so the peer can issue C-STORE requests.
- For C-MOVE, sub-operations occur on a separate association initiated by the
  C-MOVE SCP to the Move Destination AE. This does not require Storage SCP role
  selection on the original C-MOVE association, but does require a C-STORE SCP at
  the destination and application code that opens and reports those
  sub-operations.

#### Handling multiple contexts for retrieve + storage

- Query/Retrieve uses Study Root or Patient Root QR presentation contexts.
- Storage sub-operations use Storage SOP Class presentation contexts.
- Transfer syntax selection for storage follows the accepted context for the
  Storage SOP Class; if no suitable context is accepted, storage sub-operations
  will fail.

## Non-goals and production-readiness boundaries (v0.1.0)

`dicom-go` `v0.1.0` is a library-focused release that implements only a small
subset of DICOM networking and does **not** claim full DICOM conformance.

The following are explicit **non-goals** and **boundaries** for this version:

- **Not a complete DICOM network stack**: only basic DIMSE primitives required by
  current implemented features and a generic normalized-service layer are
  provided; service-class workflows remain application responsibilities.
- **DICOMweb belongs at the library boundary**: reusable QIDO-RS, WADO-RS and
  STOW-RS request/response helpers are a `dicom-go` responsibility, not an
  application-backend responsibility. The `net/dicomweb` package provides the
  reusable client and embeddable bounded server core. Twin delegates those
  mechanics to the library; application endpoint profiles, credential lookup/storage, jobs,
  archive import/export, receiver policy, operation history, UI summaries and
  auto-query stay outside this library.
- **Query/Retrieve is limited**:
  - Implemented: Study Root C-FIND SCU/SCP, C-MOVE SCU/SCP workflow helpers and
    C-GET SCU/SCP workflow helpers, plus Patient Root C-FIND/C-MOVE/C-GET
    SCU/SCP wrappers and presentation-context/identifier helpers.
  - Study Root C-FIND matching is provided by `qrmatch` and covers `STUDY`,
    `SERIES` and `IMAGE`. Callers still own the archive/index; the library does
    not bundle a production PACS catalog.
  - Modality Worklist is implemented as a separate single-level C-FIND profile
    with a deliberately bounded Table K.6-1 key subset; it is not treated as a
    hierarchical Query/Retrieve model.
  - Implemented as opt-in subsets: Hanging Protocol and Color Palette
    FIND/MOVE/GET, with model-specific key validation, UID-only retrieval,
    application-owned catalogs and storage transport; see
    [non-patient Query/Retrieve](#non-patient-queryretrieve).
  - Not implemented: retired Patient/Study Only models.
  - C-MOVE still requires a C-STORE SCP at the Move Destination AE; the library
    does not provide a production archive or destination registry.
- **Storage Commitment policy remains application-owned**:
  - Implemented: N-ACTION and N-EVENT-REPORT primitives, strict action/event
    datasets, persistent-store/CAS seams, bounded listener and retry workflow,
    same/separate/manual delivery modes, and an in-memory store for tests.
  - Not bundled: a database, clinical commitment decision, callback directory,
    automatic deletion policy, audit-retention backend, or always-on worker.
    The application supplies these through the documented interfaces and owns
    their lifecycle. Automated external-peer coverage remains opt-in.
- **Normalized DIMSE is a protocol layer, not six complete applications**:
  N-EVENT-REPORT, N-GET, N-SET, N-ACTION, N-CREATE and N-DELETE have generic
  SCU/SCP support. UPS, MPPS ([`MPPS.md`](MPPS.md)), Instance Availability
  Notification
  ([`INSTANCE_AVAILABILITY_NOTIFICATION.md`](INSTANCE_AVAILABILITY_NOTIFICATION.md)),
  Print Management
  ([`PRINT_MANAGEMENT.md`](PRINT_MANAGEMENT.md)) and Storage Commitment add
  narrow, separately documented workflows; other service-specific state
  machines are not bundled.
- **UPS persistence and clinical policy remain application-owned**:
  package `ups` supplies the normative state machine, atomic repository/outbox
  seam, bounded in-memory reference, streaming query routes and callback
  delivery worker. It does not bundle a production database, scheduler,
  clinical priority policy, AE directory, authorization, audit backend or UI.
- **Limited advanced association semantics**: role selection and raw SOP Class
  Extended Negotiation items are available at the UL boundary. The
  Query/Retrieve and Storage service-class application information is typed;
  C-FIND relational queries (Identifier check), combined date-time matching,
  timezone adjustment, fuzzy semantic matching of person names, empty and
  multiple value matching have library behavior. SOP Class Common Extended
  Negotiation is typed, and acceptors can
  take a SOP Class as a supported Related General SOP Class.
- **Explicit async boundary**: legacy helpers still assume one outstanding
  operation per association. Multiple operations require negotiated capacity
  and exclusive ownership by `dimse.AsyncSession`; mixing session and legacy
  readers/writers is unsupported.
- **Limited interoperability guarantees**: peers may require capabilities not
  implemented here (e.g. specific transfer syntaxes, SCP/SCU role selection,
  or optional command elements).
- **Partial security profiles**: TLS is an opt-in UL transport. The PS3.15
  Secure Transport Connection Profiles are available as `tls.Config`
  constructors (see Network Security), limited to the cipher suites Go's TLS
  stack implements. User Identity negotiation
  is available as an opt-in association item with caller-supplied accept policy.
  Authentication, authorization, credential storage and access policy remain
  deployment responsibilities. `net/audit` provides legacy audit builders
  (PS3.15 A.5: Application Activity, Begin Transferring, Instances
  Transferred and Accessed, Query, Export, User Authentication, Security
  Alert) and sends them as RFC 5424 syslog records over TLS (RFC 5425) or UDP
  (RFC 5426) from the default bounded RAM spool that keeps unsent records across network
  failures and drops the oldest when full; `ATNASink` turns the in-process
  DIMSE events into messages. Which events to audit, the audit repository
  and its trust remain the deployment's choice. The separate
  [clinical audit profile](CLINICAL_AUDIT.md) provides eleven typed,
  explicitly authorized PS3.15 specializations and bounded association
  aggregation. Its strict spool refuses overflow without eviction. Legacy
  telemetry is not proof of clinical specialization or IHE certification. The
  separate opt-in [durable spool](AUDIT_DURABLE_SPOOL.md) refuses overflow and
  provides bounded restart/retry mechanics; it does not confirm remote delivery.
- **Not production hardened**: error handling, resource limits, and timeouts are
  suitable for development and targeted integrations, but are not a substitute
  for a full-featured DICOM toolkit.

## Pixel Data Codecs

Supported:


Compressed pixel codecs are not registered by package initialization.
Applications explicitly create a caller-owned registry with the complete
built-in decoder baseline and then layer optional codecs on it:

```go
decoders, err := builtin.NewRegistry()
if err != nil {
	panic(err)
}

encoders := pixeldata.NewMemoryEncoderRegistry()
if err := jpeg.RegisterEncoder(encoders, jpeg.DefaultQuality); err != nil {
	panic(err)
}
```

Current metadata support:

| Codec | Bits Allocated | Samples Per Pixel | Pixel Representation | Planar Configuration | Photometric Interpretation |
|---|---:|---:|---:|---:|---|
| JPEG Baseline Process 1 decode | 8 allocated, 1-8 stored | 1 | 0 or 1 | absent or 0 | `MONOCHROME1`, `MONOCHROME2`; SOF0 |
| JPEG Baseline Process 1 decode | 8 | 3 | 0 | absent or 0 | `RGB`, `YBR_FULL`, `YBR_FULL_422`; SOF0; interleaved output per the color policy (RGB by default) |
| JPEG Baseline encode (lossy) | 8 stored in 8 | 1 | 0 | absent | `MONOCHROME1`, `MONOCHROME2`; quality 1–100 is required |
| JPEG Baseline encode (lossy) | 8 stored in 8 | 3 | 0 | 0 | native `RGB`; encoded metadata is `YBR_FULL_422`, Planar Configuration 0; quality 1–100 is required |
| JPEG Extended Process 4 encode (lossy) | 16 allocated, 9-12 stored, HighBit=BitsStored-1 | 1 | 0 | absent | `MONOCHROME1`, `MONOCHROME2`; SOF1 precision 12; quality 1–100 and `AllowLossy` are required |
| JPEG Extended Process 2 decode | 8 allocated, 1-8 stored | 1 | 0 or 1 | absent or 0 | `MONOCHROME1`, `MONOCHROME2`; SOF1, or Baseline SOF0 (a subset of Process 2 & 4) reported as `jpeg-sof0-under-extended` in `Frames.Diagnostics` |
| JPEG Extended Process 2 decode | 8 | 3 | 0 | absent or 0 | `RGB`, `YBR_FULL`, `YBR_FULL_422`; SOF1 or Baseline SOF0 (diagnostic as above); interleaved output per the color policy (RGB by default) |
| JPEG Extended Process 4 decode | 16 allocated, 9-12 stored, HighBit=BitsStored-1 | 1 | 0 or 1 | absent or 0 | `MONOCHROME1`, `MONOCHROME2`; SOF1 precision 12; native output is little-endian |
| JPEG Extended Process 4 decode | 16 allocated, 9-12 stored, HighBit=BitsStored-1 | 3 | 0 | absent or 0 | `RGB`, `YBR_FULL`, `YBR_FULL_422`; SOF1 precision 12; any sampling factors; output per the color policy, interleaved little-endian |
| JPEG Spectral Selection / Full Progression decode (retired `.53`/`.55`) | 8 allocated, 1-8 stored | 1 | 0 or 1 | absent or 0 | `MONOCHROME1`, `MONOCHROME2`; SOF2 (8-bit Huffman progressive) |
| JPEG Spectral Selection / Full Progression decode (retired `.53`/`.55`) | 8 | 3 | 0 | absent or 0 | `RGB`, `YBR_FULL`, `YBR_FULL_422`; SOF2; interleaved output per the color policy (RGB by default) |
| JPEG Spectral Selection / Full Progression 12-bit decode (retired `.53`/`.55`) | 16 allocated, 12 stored, HighBit 11 | 1 | 0 | absent | `MONOCHROME1`, `MONOCHROME2`; SOF2 precision 12, 1x1 sampling, complete scans |
| JPEG Spectral Selection / Full Progression 12-bit decode (retired `.53`/`.55`) | 16 allocated, 12 stored, HighBit 11 | 3 | 0 | 0 | raw `RGB`, 4:4:4 SOF2 precision 12; interleaved little-endian, complete scans; `JPEGColorAlways` refused |
| JPEG Lossless Process 14 | 8 allocated / 2-8 stored; 16 allocated / 2-16 stored; HighBit=BitsStored-1 | 1 | any stored value | absent or 0 | `MONOCHROME1`, `MONOCHROME2`; SOF3 precision equals BitsStored; Huffman lossless, predictors 1-7 and point transforms 0 through precision-1 |
| JPEG Lossless Process 14 | 8 allocated / 2-8 stored; 16 allocated / 2-16 stored; HighBit=BitsStored-1 | 3 | 0 | 0 | `RGB`, `YBR_FULL` (`YBR_FULL_422` decodes as `YBR_FULL`, see below); SOF3 precision equals BitsStored; 1x1 sampling; one interleaved scan or three single-component scans; predictors 1-7 and point transforms 0 through precision-1; decoded bytes are interleaved |
| JPEG Lossless Process 14 SV1 | 8 allocated / 2-8 stored; 16 allocated / 2-16 stored; HighBit=BitsStored-1 | 1 | any stored value | absent or 0 | `MONOCHROME1`, `MONOCHROME2`; SOF3 precision equals BitsStored; Huffman lossless, predictor 1 only |
| JPEG Lossless Process 14 SV1 | 8 allocated / 2-8 stored; 16 allocated / 2-16 stored; HighBit=BitsStored-1 | 3 | 0 | 0 | `RGB`, `YBR_FULL` (`YBR_FULL_422` decodes as `YBR_FULL`, see below); SOF3 precision equals BitsStored; 1x1 sampling; one interleaved scan or three single-component scans; predictor 1 in every scan; decoded bytes are interleaved |
| RLE Lossless decode/encode | Multiple of 8 up to 120 (15 segments; stored bits may be fewer, e.g. 12 in 16 allocated) | 1 | 0 or 1 for monochrome; 0 for palette | ignored | `MONOCHROME1`, `MONOCHROME2`, `PALETTE COLOR` |
| RLE Lossless decode/encode | Multiple of 8 up to 40 (15 segments) | 3 | 0 | absent, 0 or 1 on decode (output always interleaved, decompressed data set says 0); 0 on encode | `RGB`, `YBR_FULL`; native frames are interleaved little-endian sample bytes |
| Deflated Image Frame Compression decode/encode | 1, or a multiple of 8 up to 64 (any stored bits) | 1 or 3 (4 decode) | 0 or 1 | absent, 0 or 1; preserved | decode: any; encode: `MONOCHROME1`, `MONOCHROME2`, `PALETTE COLOR`, `RGB`, `YBR_FULL`, `YBR_FULL_422`; frames are the native little-endian frame bytes; per-frame transcode needs Bits Allocated of at least 8 |
| JPEG-LS Lossless decode/encode | 8 or 16 (stored bits may be 12 in 16 allocated) | 1 | 0 or 1 for monochrome; 0 for palette | absent or 0 | `MONOCHROME1`, `MONOCHROME2`, `PALETTE COLOR`; NEAR=0, ILV=0; default or LSE preset ID=1 |
| JPEG-LS Lossless decode/encode | 8 or 16, 2-16 stored | 3 | 0 | 0 | `RGB`; native frames are interleaved sample bytes; NEAR=0; ILV=0/1/2 decode/encode (ILV=1/2 encoding through `EncoderOptions.Interleave`); default or LSE ID=1 within [qualified limits](JPEGLS_INTERLEAVE.md) |
| JPEG-LS optional adapter | 8 or 16 | 1 | 0 or 1 | absent | `MONOCHROME1`, `MONOCHROME2`; alternative backend for Near-Lossless and modes outside the builtin subset |
| JPEG-LS optional adapter | 8 or 16 | 3 | 0 or 1 | absent or 0 | `RGB`; alternative backend for Near-Lossless and modes outside the builtin subset; decoded frames are interleaved RGB bytes |
| JPEG 2000 / HTJ2K optional adapter | 8 or 16 allocated; stored bits up to allocated bits | 1 | 0 | absent | `MONOCHROME1`, `MONOCHROME2`; JPEG 2000, JPEG 2000 Part 2 and HTJ2K still-image UIDs; `HighBit` must equal `BitsStored-1` |
| JPEG 2000 / HTJ2K optional adapter | 8 or 16 allocated; stored bits up to allocated bits | 3 | 0 | absent or 0 | `RGB`, `YBR_RCT`, `YBR_ICT`; decoded frames are returned as interleaved RGB bytes; `HighBit` must equal `BitsStored-1` |
| JPEG 2000 / HTJ2K optional adapter, core backend | 8 (8 stored) | 3 | 0 | absent or 0 | `YBR_FULL_422` with a 4:2:2 codestream only: component 0 at XRsiz=YRsiz=1, components 1 and 2 at XRsiz=2, YRsiz=1, unsigned 8-bit, no component transform. Chroma is repeated over the columns it covers and converted to RGB; decompression writes `RGB` and Lossy Image Compression `01`. Other layouts and backends are refused |
| JPEG 2000 optional adapter, registered `.90` | 8 or 16 allocated; stored bits up to allocated bits | 1 | 0 | absent | `PALETTE COLOR`; `Register` and `RegisterCore` preserve palette indices and LUTs under the lossless-only syntax; this does not enable palette under `.91`, Part 2 or HTJ2K |


Unsupported metadata returns typed errors that include the rejected field name
and value. JPEG Baseline/Extended/Lossless, builtin JPEG-LS and its optional
adapter use [shared bounded Item assembly](ENCAPSULATED_FRAME_ASSEMBLY.md):
validated BOT, normative one-Item EOT/Lengths and structural marker inference
for ambiguous empty-table layouts. Only one compressed frame is joined at a
time; deferred sources use existing parser Item locations. The JPEG-LS adapter
reports typed errors for unavailable decoders, unsupported metadata, malformed
frames returned by the decoder backend, and unsupported fragment layouts. The
JPEG 2000 / HTJ2K adapter validates codestream dimensions,
component count, component precision against `BitsStored` and signedness before
decode, assembles multiple fragments for a single frame, and rejects ambiguous
multi-frame fragment layouts with a typed error. Its current controlled-profile
decision and benchmark evidence are
tracked in
[`examples/codec-adapters/jpeg2000/PRODUCTION_PROFILE.md`](../examples/codec-adapters/jpeg2000/PRODUCTION_PROFILE.md).
The OpenJPEG fallback profile is selected explicitly with `jpeg2000_openjpeg` or
`RegisterOpenJPEG`; it registers OpenJPEG for JPEG 2000 Part 1 still-image UIDs
and keeps JPEG 2000 Part 2 plus HTJ2K on the pure-Go fallback path. It requires
`opj_decompress`, returns a typed OpenJPEG-unavailable error when that runtime
tool is absent, validates PNM precision against `BitsStored`, and bounds
subprocess output before reading decoded frames. Its packaging, transitive
dependency audit, and license notes are tracked in
[`examples/codec-adapters/jpeg2000/OPENJPEG_PROFILE.md`](../examples/codec-adapters/jpeg2000/OPENJPEG_PROFILE.md).

### Legacy ACR-NEMA image metadata (opt-in)

Missing Image Pixel Module attributes are typed `ErrMissingMetadata` errors by
default. `DecompressOptions.Recovery` holds two opt-in repairs for ACR-NEMA
objects, applied to a copy of the data set and reported in
`DecompressOptions.Report`:

- `DefaultImagePixelModule` assumes Samples per Pixel 1
  (`default-samples-per-pixel`) and Photometric Interpretation MONOCHROME2
  (`default-photometric-interpretation`) when they are absent, as GDCM does,
  provided Rows, Columns and Bits Allocated are present and native Pixel Data
  holds one sample per pixel but not three; a multi-sample Photometric
  Interpretation is never overridden. A three-dimensional image (Image
  Dimensions 3) without Number of Frames takes it from Planes (0028,0012)
  (`number-of-frames-from-planes`).
  A native image without a declared SOP Class and with exact Recognition Code
  `ACRNEMA_LIBIDO_1.0` or `ACRNEMA_LIBIDO_1.1` also exchanges legacy reversed
  Rows/Columns (`libido-dimensions-swapped`). Its exact unsigned 24/24/23-bit,
  single-sample layout with missing photometric and matching byte length is
  described as interleaved RGB8 (`libido-rgb24-normalized`) without changing
  Pixel Data. The consumed marker is removed to prevent a second swap.
  `object.UpgradeLegacy` applies the same normalization and reports corrected
  tags and diagnostics. Declared SOP Classes and other recognition codes do
  not enter this repair.
- `Unpack12BitPacked` reads native Pixel Data with Bits Allocated 12 as 16
  bits allocated, as GDCM does. A value of three bytes per two samples (plus
  one pad byte when that is odd) is unpacked in the ACR-NEMA order: the first
  sample is the first byte with the low nibble of the second byte as its high
  bits, the next is the high nibble of the second byte with the third byte as
  its high bits (`unpacked-12-bit`). A value of two bytes per sample already
  holds 16-bit words and only Bits Allocated changes
  (`bits-allocated-12-as-16`). Bits Stored and High Bit are kept; any other
  length is refused with `ErrPixelDataSizeMismatch`. Without the option Bits
  Allocated 12 stays an invalid stored-sample format.

The decompressed data set carries the assumed values explicitly. On the
gdcmData corpus all 13 ACR-NEMA files that lacked these attributes decode with
the GDCM pixel MD5 (two after GDCM's masking of bits above Bits Stored).

### JPEG metadata reconciliation and encoder quirks (opt-in)

By default a data set that contradicts its JPEG codestream is rejected with a
typed error. `DecodeRecovery.ReconcileWithCodestream` (JPEG Baseline,
Extended and Lossless) reads the frame header of the first frame and
replaces Rows, Columns, Samples per Pixel, Photometric Interpretation (from
the component count), Planar Configuration, Bits Allocated (8 or 16, never
below the precision), Bits Stored and High Bit (the precision) when they are
absent or disagree, as GDCM does when it retries with the codestream
precision. Each replacement is a typed `MetadataReconciliation` in
`DecompressReport.Reconciliations` (attribute, tag, declared and used values)
plus the `metadata-reconciled-with-codestream` diagnostic, and the
decompressed data set carries the codestream values. The input is never
mutated.

For JPEG 2000 (Part 1, Part 2 and HTJ2K) the same option reads the SIZ marker
segment of the first codestream (ISO/IEC 15444-1 A.5.1): the image area gives
Rows and Columns, Csiz the Samples per Pixel, and the component precision and
sign (Ssiz, the same in every component, sampled at full resolution) give Bits
Stored, High Bit and Pixel Representation, as GDCM does; Planar Configuration
becomes 0, since the codestream decodes to interleaved samples, and an absent
or impossible Bits Allocated becomes 8 or 16. `DecodeRecovery.UnwrapJP2Container`
decodes the codestream of Pixel Data that holds JP2 files instead (PS3.5 A.4.4
requires the bare codestream): the Contiguous Codestream box (`jp2c`) of a
single frame whose fragments form one JP2 file, or of one JP2 file per frame,
reported as `jp2-container-unwrapped`. Both run before the codec, so any
registered JPEG 2000 decoder benefits. On gdcmData the three TOSHIBA files
whose SIZ sign contradicts Pixel Representation, `Osirix10vs8BitsStored`
(Bits Stored 8 for 10-bit data), `HardcopyColor_YBR_RCT_J2K_PC1` and the JP2
file `GDCMJ2K_TextGBR` decode with the GDCM pixel MD5; the lossy JP2 files
`ELSCINT1_JP2vsJ2K` and `MAROTECH_CT_JP2Lossy` decode without an independent
lossy oracle.

`DecodeRecovery.RepairBitCountMasks` repairs Bits Allocated and Bits Stored
written as a bit mask instead of a count (2^n-1 above 16, 0xFFFF for 16) and
High Bit written as 2^n-2 (0xFFFE for 15), as some FUJIFILM CR devices do and
GDCM accepts. It runs first, only when the counts agree with each other (Bits
Allocated 8 or 16, Bits Stored at most Bits Allocated, High Bit Bits Stored
minus one) and with the size of native Pixel Data, and reports
`bit-count-masks-repaired` with one reconciliation per attribute. gdcmData
`FUJI-ffff-MONO1-J2K` (0xFFFF, 0xFFFF, 0xFFFE) then decodes with the GDCM
pixel MD5. Under the same consistency conditions (Bits Allocated may also be
32 when no mask is replaced) the option replaces a High Bit of 0 with Bits
Stored above 1, which places no Bits Stored field in the container, by Bits
Stored minus one, as the MFSPLIT export of a Siemens MAGNETOM Skyra writes it
and GDCM repairs it, reporting `high-bit-zero-repaired` and a HighBit
reconciliation. Without the option such a format stays refused with
`ErrInvalidSampleFormat` (High Bit).

A JPEG Lossless object labelled `YBR_FULL_422` is not conformant (PS3.5
Table 8.2.1-2 lists only `RGB` and `YBR_FULL` for colour lossless JPEG), but
PS3.5 8.2.1 lets the sampling stated in the codestream control decompression.
When every SOF3 component is 1x1, the samples decode unconverted as full
resolution Y Cb Cr, the decoded frames and the decompressed data set are
`YBR_FULL`, and `jpeg-lossless-ybr-full-422-full-resolution` is reported
(`pixeldata.DiagnosticJPEGLosslessYBRFull422FullResolution`). A SOF3 with
subsampled components is refused with `ErrUnsupportedScan` whatever the
label. gdcmData `JPEGLosslessYBR_FULL_422.dcm` matches the GDCM pixel MD5 and
DCMTK `dcmdjpeg +cn` (opt-in `DICOMGO_GDCMDATA_ROOT`).

`DecodeRecovery.JPEGLosslessEncoderQuirks` retries a JPEG Lossless frame
that fails strict decoding, accepting first a nonzero spectral selection end
in SOS (`jpeg-lossless-se-ignored`) and entropy fill bits that are not all
ones (`jpeg-lossless-invalid-fill-bits`), then 16 extra bits after each
category-16 difference (`jpeg-lossless-category16-extra-bits`), which the
Cornell and MARCONI encoders write although T.81 H.1.2.2 defines none.
The extra bits are read as an ordinary 16-bit extension, as `dcmdjpeg +wc`
does; the encoders write 0x7fff or 0x8000, both ±32768 modulo 2^16.
Conforming streams decode on the strict attempt and report nothing.
Synthetic Cornell vectors for every predictor decode to the source samples,
and `dcmdjpeg +wc` agrees for predictors 1-4 and 7 (for 5 and 6 it does not
read the extra bits of these vectors). The gdcmData `MITRA_CORNELL.dcm`
matches both GDCM and `dcmdjpeg +wc`.

`DecodeRecovery.JPEGLosslessPredictor6Overflow` reproduces encoders that
computed predictor 6 with 16-bit integers: Ra and Rc are taken as signed
16-bit values in Rb + ((Ra - Rc) >> 1), as `dcmdjpeg +w6` does. The defect
cannot be detected from the stream and the option misdecodes conforming
16-bit predictor 6 images, so it is never tried automatically; it changes
nothing below 16 bits or for other predictors. A frame it changed reports
`jpeg-lossless-predictor6-overflow`. Synthetic defective vectors decode
bit-exactly like `dcmdjpeg +w6` (opt-in `DICOMGO_DCMTK_INTEGRATION=1`).

## Enhanced CT/MR/PET to Classic single-frame conversion

`multiframe.ConvertToClassic` implements the PS3.4 C.3.5 Enhanced-to-Classic
conversion profile for Enhanced CT Image Storage
(`1.2.840.10008.5.1.4.1.1.2.1`), Legacy Converted Enhanced CT Image Storage
(`1.2.840.10008.5.1.4.1.1.2.2`), Enhanced MR Image Storage
(`1.2.840.10008.5.1.4.1.1.4.1`), Legacy Converted Enhanced MR Image
Storage (`1.2.840.10008.5.1.4.1.1.4.4`), Enhanced PET Image Storage
(`1.2.840.10008.5.1.4.1.1.130`) and Legacy Converted Enhanced PET Image
Storage (`1.2.840.10008.5.1.4.1.1.128.1`). It emits one CT Image Storage
(`1.2.840.10008.5.1.4.1.1.2`), MR Image Storage (`1.2.840.10008.5.1.4.1.1.4`)
or PET Image Storage (`1.2.840.10008.5.1.4.1.1.128`) Part 10 object per
source frame. Every output has
a new SOP Instance UID, all outputs in one conversion use a new Series Instance
UID, and the source Study Instance UID and Frame of Reference UID are retained.
Patient, Study, and remaining spatial or temporal frame-of-reference attributes
are copied without mutation.

The converter applies Shared Functional Groups and then the corresponding
Per-Frame Functional Groups item to each output. It maps the standard classic
attributes required by the target IOD, including:

- Pixel Measures to Pixel Spacing, Slice Thickness, and Spacing Between Slices;
- Plane Position (Patient) and Plane Orientation (Patient) to Image Position
  (Patient) and Image Orientation (Patient);
- CT or general Pixel Value Transformation to Rescale Intercept, Rescale Slope,
  and Rescale Type;
- Frame VOI LUT attributes to their classic top-level equivalents; and
- CT/MR Frame Type and acquisition/timing attributes to the corresponding
  classic CT or MR image attributes.

For PET, the PET Series and PET Image attributes an Enhanced PET image keeps
elsewhere are derived when the frame lacks them: Units from a PET Rescale
Type (else NONE), Counts Source (EMISSION), Series Type (DYNAMIC with several
Temporal Position Index values, GATED with cardiac or respiratory
synchronization, else STATIC; IMAGE), Number of Slices and Number of Time
Slices from the temporal positions, Corrected Image from the Enhanced PET
Corrections flags, Decay Correction (NONE), Series Date and Time from the
acquisition, Frame Reference Time from Frame Reference DateTime, Image Index,
Acquisition Date and Time and Actual Frame Duration from Frame Content. The
Unassigned Converted Attributes of a Legacy Converted source are the classic
images' own values and take precedence (Image Type among them), the source's
own Conversion Source Attributes are not flattened, Enhanced-only leaves the
classic IOD does not list (Pixel Presentation, Frame Content, Frame Laterality,
PET correction flags, Rescale Type outside CT...) are dropped, and the Type 2
Attributes of the classic IOD's mandatory Modules come from the PS3.3 2026d
Module tables (projected by `cmd/iodmodulegen` into `internal/iodmodules`).

The output does not contain Number of Frames, Shared/Per-Frame Functional
Groups, Multi-frame Dimension, concatenation, representative-frame, or
frame-increment attributes. Remaining standard and private attributes from
the source top level, Shared Functional Groups, and the selected Per-Frame item
are preserved at the output top level when they do not conflict with a mapped
target-IOD attribute. Ambiguous collisions fail instead of silently choosing a
value. A source that places the same Functional Group Macro in Shared and
Per-Frame Functional Groups is rejected, as required by PS3.3 C.7.6.16.1.1.

Pixel provenance is preserved. A byte-equivalent frame does not become
`DERIVED` merely because its container changed: Image Type is populated from
the applicable Frame Type and remains `ORIGINAL`/`PRIMARY` when those source
semantics still apply. This primitive does not expose a pixel transformation
that would justify changing those provenance terms.

Each output contains Conversion Source Attributes Sequence (0020,9172) with the
source SOP Class UID, source SOP Instance UID, and one-based Referenced Frame
Number. It also appends the required Contributing Equipment Sequence item with
Purpose of Reference `(109106, DCM, "Enhanced Multi-frame Conversion
Equipment")` and Contribution Description `Classic Image created from Enhanced
Image`. Existing contributing-equipment items are retained. These
conversion references are distinct from the optional Source Image Sequence and
are not replaced by it.

The caller supplies the new SOP and Series Instance UIDs. A Query/Retrieve SCP
that uses this primitive for the alternative views defined by PS3.4 C.3.5 is
responsible for supplying deterministic UIDs across repeated retrievals; a
manual conversion workflow may instead create a new derived set on each run.

The output transfer syntax is always Explicit VR Little Endian. Native input is
normalized without changing sample values. Encapsulated still-image input is
decoded through the caller-provided registry; the converter does not perform a
lossy re-encode. Existing lossy-compression history attributes remain present
when the source pixel values were previously lossy. Conversion is bounded and
context-aware, and cancellation or any frame failure returns no partial output
set.

Current limits are deliberate:

- 8-bit or color Enhanced MR, XA/XRF, ultrasound, microscopy, video,
  JPIP-referenced pixels, Float Pixel Data, and Double Float Pixel Data are
  rejected as unsupported source profiles.
- Encapsulated sources require a registered decoder that can decode the complete
  declared frame layout. Missing codecs and ambiguous fragment layouts fail
  with typed pixel-data errors.
- Missing target Type 1 values, a Per-Frame Functional Groups count different
  from Number of Frames, malformed geometry, invalid UIDs, and functional-group
  attributes that cannot be represented without ambiguity are conversion
  errors. Required Type 2 attributes are emitted with an empty value when the
  source provides no value; the converter never invents clinical content.
- The primitive creates in-memory Part 10 objects. Filesystem staging, archive
  transactions, progress reporting, and user-interface policy belong to the
  application.

Unsupported:

- JPEG hierarchical modes, arithmetic-coded progressive modes,
  and metadata outside the documented Process 2/4 and 8/12-bit progressive
  subsets, JPEG-LS modes outside the
  qualified builtin Lossless subset, JPEG XL in the default registry, MPEG
  and HEVC frame decoding.
  JPEG-LS modes outside that subset, JPEG 2000/HTJ2K, and JPEG XL metadata plus
  encapsulated Pixel Data preservation remains available without an optional
  decoder adapter. JPEG XL decoder selection is tracked in
  `JPEGXL_DECODER_STRATEGY.md`.
- JPIP and SMPTE ST 2110 media decoding or streaming.
- Lossy encoder implementations and automatic transcoding during DIMSE
  negotiation. The API has a fail-closed lossy extension seam, but the default
  module ships built-in RLE Lossless, Deflated Image Frame Compression, JPEG
  Lossless Process 14/SV1 and JPEG-LS Lossless encoders. JPEG-LS Near-Lossless encode is not provided.
- Complete presentation pipelines such as presentation states, overlays,
  shutters, and display calibration. Reusable frame helpers cover
  modality/VOI LUTs and the native color layouts documented below; package
  `icc` validates ICC Profile (0028,2000) and converts RGB colours to sRGB
  (lut8, lut16, lutAtoB and matrix/TRC profiles), which `render` applies
  through `Frame.ColorTransform`.


## Classic to Legacy Converted Enhanced conversion

`multiframe.ConvertToLegacyEnhanced` converts the classic single-frame CT,
MR or PET images of one Series (one SOP Class, Study, Series, Frame of
Reference and Specific Character Set, equal Rows, Columns and pixel
representation, MONOCHROME2) into one Legacy Converted Enhanced CT, MR or
PET instance (PS3.3 A.70-A.72), the reverse of the conversion above. Frames
are ordered along the normal of their common Image Orientation (Patient), or
by Instance Number. Pixel Data is decoded and written native; Bits Stored is
widened losslessly to the 12 or 16 the Enhanced CT and PET Image Modules
allow, with samples sign-extended.

Attributes follow PS3.3 C.7.6.16.2.25: those of the IOD's Modules (from the
generated PS3.3 Module tables) that every image shares stay at the top level;
Pixel Spacing, Slice Thickness, Image Position and Orientation, the window,
the rescale, Image Type, Referenced Image, Source Image and derivation,
anatomy and Irradiation Event UID go to their Functional Group Macros
(Shared when all frames agree, Per-Frame otherwise, Plane Position and Frame
Content always Per-Frame); everything else, private data elements with their
creator in each Item, goes to the Unassigned Shared or Per-Frame Converted
Attributes Sequence, including the values the conversion replaces (Series
Number, Instance Number, Content Date and Time, Image Type). Each frame
references its image in the Conversion Source Attributes Sequence. Frame Type
Value 2 is PRIMARY, as the Enhanced IODs require; the classic Image Type is
kept in the Unassigned Attributes, so the round trip restores it. Frame
Anatomy is written when Body Part Examined holds a PS3.16 Annex L term (the
2026d table, projected by `cmd/iodmodulegen -bodyparts`) or an Anatomic
Region Sequence is present; Frame VOI LUT and Pixel Value Transformation,
mandatory for CT and PET, are completed from the frame's value range and the
modality (HU for CT, the PET Units) when the images lack them. Text a source
pads with NUL is written padded with a space.

A DICOM_Example CT series of 234 images, synthetic MR and PET series and the
classic images converted back from each validate with no dciodvfy error; the
round trip keeps pixels, geometry, rescale and Image Type, and the
remaining warnings are the sources' own and the classic Frame Type Value 3
(AXIAL) the Enhanced defined terms do not list.
## Functional groups and Concatenations

`object.FunctionalGroupItem` (and `FunctionalGroupItems` for macros with
several items) is the one functional group resolver of the module: the
macro of a frame comes from its Per-Frame Functional Groups item, otherwise
from the Shared Functional Groups; a macro in both is returned from the
per-frame item with `object.ErrFunctionalGroupConflict`, and
`FunctionalGroupConflicts` lists every such macro and frame (PS3.3 C.7.6.16).
Frame geometry, SEG (segment identification, plane position, derivation image
and the slice template, so shared macros now apply), Enhanced-to-Classic
conversion (conflicts and required macros), per-frame modality and VOI
transforms, NIfTI export scaling, Real World Value Mapping, Parametric Maps and
DICOMDIR records use it. Packages that copy or scan whole functional group
items (conversion flattening, ultrasound regions, dynamic timing, microscopy
tiles) read the items directly.

`multiframe.GroupConcatenations` sorts instances by Concatenation UID and
`multiframe.NewConcatenation` validates one Concatenation (PS3.3
C.7.6.16.2.2.4): the same SOP Class, Study, Series, Frame of Reference, SOP
Instance UID of Concatenation Source and Image Pixel description in every
instance; In-concatenation Numbers from 1 without gaps or repeats, up to
In-concatenation Total Number when present; each Concatenation Frame Offset
Number equal to the frames before it; one Per-Frame Functional Groups item per
frame. Incomplete or inconsistent sets are refused
(`ErrConcatenationIncomplete`, `ErrConcatenationInconsistent`). The resulting
`Concatenation` is a logical view that copies no pixels: `Locate` maps a
logical frame to its instance and instance frame, and `FunctionalGroupItem`
and `FrameGeometryAt` resolve functional groups over logical frames. Volume
assembly for display stays in the application.

With DCMTK 3.7.0 (opt-in `DICOMGO_DCMTK_INTEGRATION=1` and
`DICOMGO_DCMTK_CONCAT`, a program built from
`multiframe/testdata/dcmtk_concat.cc`), Concatenations that DCMTK's
ConcatenationCreator makes from a dicom-go Enhanced CT object load here with
every frame, plane position and pixel in place, and DCMTK's
ConcatenationLoader merges the same instances into the same frames. Unlike
dicom-go, DCMTK merges a set with a missing instance (first, middle or last)
after a warning that In-concatenation Total Number does not match, which
silently shifts the frames after the gap.

`multiframe.SplitConcatenation` writes a Concatenation from a large
multi-frame object read by a `pixeldata.FrameReader`, for archives with an
instance size limit, whole-slide images and Legacy Converted Enhanced
objects. `SplitOptions` bound each instance by frames (`MaxFrames`), by
stored frame bytes (`MaxPixelBytes`) or both; frames are taken in order, an
instance closes when the next frame would exceed a bound, and a frame larger
than the byte bound gets an instance of its own. Every instance
(`PlanConcatenation`) keeps the source attributes, Shared Functional Groups
and Instance Number, and gets a new SOP Instance UID (given or generated),
the Concatenation UID, SOP Instance UID of Concatenation Source (the
source's), In-concatenation Number and Total Number, Concatenation Frame
Offset Number, its Number of Frames and its slice of the Per-Frame
Functional Groups Sequence; Representative Frame Number stays, renumbered,
in the instance that holds that frame. Frames are copied as stored, without
decoding: native frames in the source byte order, encapsulated codestreams
in new Items with new offset tables (`object.WriteFileFrames`), pulled one at
a time while an instance is written, so a reader over deferred Pixel Data
holds one frame (`TestSplitConcatenationMemoryIsPerFrame`: under 6 MiB of heap
growth for 12 MiB of Pixel Data). Sources without the Multi-frame Functional
Groups Module, with a Per-Frame item count other than Number of Frames,
already part of a Concatenation, or with repeated or invalid UIDs are
refused (`ErrConcatenationSplit`). Splitting and regrouping with
`NewConcatenation` reproduces every source frame and functional group for
native and RLE sources; with DCMTK (`TestSplitConcatenationLoadsInDCMTK`,
opt-in as above) ConcatenationLoader merges the split back into the source
frames. Per-frame attributes outside the functional groups and frame
references in other objects are not rewritten.

## Write options: sequence and group lengths

Writing keeps its defaults (sequences and items with undefined length,
Group Length attributes written as they are). `WriteFileOptions` and
`WriteDataSetWithOptions` offer the common dcmconv choices:
`SequenceLength` writes undefined (`-e`), preserved or defined (`+e`)
lengths for sequences and items, and `GroupLength` preserves, recalculates
the present ones (`+g=`), creates one for every group (`+g`) or removes
(`-g`) Group Length attributes in the data set and in every item. File Meta
keeps its mandatory group length. Group lengths are measured by encoding the
group as it will be written, including nested items and Specific Character
Set, and deferred values are streamed once to be measured. Every combination
of the policies round-trips in Explicit and Implicit VR Little Endian and
Explicit VR Big Endian with correct group lengths at every level, and the
data set is byte-identical to DCMTK `dcmconv -F` for `+g`, `+g=` and `-g`
with `+e` and `-e`; `dcmdump` reads every output without warnings (opt-in
`DICOMGO_DCMTK_INTEGRATION=1`).

### File Meta Information

`WriteFileOptions.MetaMode` chooses how group 0002 is rebuilt, after DCMTK's
file write modes:

- `FileMetaUpdate`, the default, keeps the other File Meta elements, such
  as Source AE Title or Private Information.
- `FileMetaCreate` starts from the required elements only.
- `FileMetaPreserve` also keeps another implementation's identity.

`RebuildFileMetaWithOptions` applies the same choice. Update and Create
record dicom-go as the writer: Implementation Class UID
`1.2.826.0.1.3680043.10.543.1` and Implementation Version Name `DICOMGO`
(`object.ImplementationClassUID` and `ImplementationVersionName`, the
identity dicom-go also negotiates on associations). In every mode, Media
Storage SOP Class and Instance UID follow the data set and Transfer Syntax
UID follows the encoding written.


## UID generation

Every UID the library generates comes from `core` (PS3.5 B.2, ISO/IEC
9834-8, RFC 9562):

- `NewUID` is `2.25.` followed by a random version 4 UUID from crypto/rand
  (`NewUIDFromReader` for another source), with its version and variant
  bits set. That is at most 44 characters.
- `NameUID(parts...)` is deterministic: a version 8 name-based UUID from the
  first 128 bits of the SHA-256 of the length-prefixed parts. It gives
  derived objects stable UIDs, such as Dimension Organization, SR tracking
  and UPS transaction UIDs. `KeyedNameUID` uses HMAC-SHA-256 under a secret
  key; it backs de-identification's `NewKeyedUIDRemapper`.
- `NewUIDWithRoot(root)` appends random decimal digits to an organizational
  root, up to the 64-character limit and at most 39 digits. It refuses a
  root that is not a UID or leaves fewer than 20 digits (`ErrUIDRoot`).


## Typed element values

`object` builds elements from Go values (PS3.5 6.2); the setters on
`*Object` put them, and the `*Element` functions return them:

- `FormatDS` writes a Decimal String of at most 16 bytes: the shortest
  representation that reads back as the value when it fits, otherwise the
  most significant digits that fit in fixed or exponential notation (with
  the shortest exponent, such as `1.23456789012e17`) and that stay within the
  float64 range. NaN and infinities fail with `ErrInvalidElementValue`.
  `FormatIS` refuses values outside the signed 32-bit range. The formatting
  underneath follows C's printf, and is checked against the vectors of
  DCMTK's `ofstd/tests/tftoa.cc` (`testdata/upstream/dcmtk/ftoa_vectors.json`).
- `SetUS`, `SetSS`, `SetUL`, `SetSL`, `SetFL`, `SetFD` and `SetAT` encode in
  the object's `ValueByteOrder`, so values put into a Big Endian data set
  are Big Endian; `GetInts` and `GetFloats` read the binary VRs back in the
  same order as well as IS and DS.
- `SetDate`, `SetTime` and `SetDateTime` write DA, TM and DT from a
  `time.Time`, with a six-digit fraction only when the time has sub-second
  precision; DT carries the time's UTC offset and TM none.
- `SetStrings` and `SetPN` validate the values against the VR and the
  object's Specific Character Set, and store nothing when they fail.
  `SetSequence` puts items of undefined length.

## DCMTK value vectors

The value tables of DCMTK's unit tests (commit `8d68392d`) are imported as
data under `testdata/upstream/dcmtk/*_vectors.json` (provenance in
`THIRD_PARTY_PROVENANCE.json`) and replayed against dicom-go:

| DCMTK test | Checked with |
| --- | --- |
| `dcmdata/tests/tchval.cc` (checkStringValue) | `validation` (`core` tests) |
| `dcmdata/tests/tvrui.cc`, `tvrpn.cc`, `tgenuid.cc` | `core`, `personname`, `validation` |
| `dcmdata/tests/tvrds.cc`, `ofstd/tests/tatof.cc` | `object.GetFloats`, `object.FormatDS` |
| `dcmdata/tests/tvrdatim.cc`, `ofstd/tests/tofdatim.cc` | `dcmtime` |
| `dcmdata/tests/tmatch.cc` | `qrmatch.Match` |
| `dcmdata/tests/tfrmsiz.cc` | `pixeldata.Metadata.FrameSize`, RLE decoder |
| `dcmsr/tests/tsrcodvl.cc` | `sr.NewCode`, `sr.ValidCode` |

Every other vector agrees. Where DCMTK followed PS3.5 and dicom-go did not,
dicom-go was changed: validation now reports an AE value of only spaces,
a UR value outside the RFC 3986 characters or with a leading or embedded
space, a DA/TM/DT value with a leading space, and a UI value padded with
spaces or more than one NUL; `qrmatch` rejects DA matching values other than
`YYYYMMDD` unless `LenientDateQueries` is set; and `sr.NewCode` picks Code
Value, Long Code Value or URN Code Value as the Code Sequence Macro requires
(PS3.3 Table 8.8-1), with `sr.ValidCode` checking a code the same way.

The remaining divergences follow the standard (or a stated design choice)
and are listed, with these reasons, in the tests' divergence tables:

- ACR-NEMA formats. DCMTK reads the dates `YYYY.MM.DD` and times
  `HH:MM:SS.frac` (checkStringValue only on request, matching and OFTime
  always). PS3.5 6.2 DA and TM note that these formats are not DICOM
  conformant: validation and `dcmtime.ParseDA`/`ParseTime` reject them,
  `qrmatch` accepts the date form only with `LenientDateQueries`, and
  `dcmtime.ParseDate` reads it with `IsNEMA` set.
- ESC. DCMTK accepts ESC (1BH) in LO, SH, ST and UC values without an ISO 2022
  Specific Character Set (its own test notes that LO and SH should fail).
  PS3.5 6.1.2.5 and 6.1.3 reserve ESC for code extension escape sequences, so
  validation reports it.
- UIDs that are not object identifiers. DCMTK checks only digits and dots, so
  `0`, `99` and `5.6.7.8` pass. PS3.5 9.1 defines UIDs as ISO/IEC 8824 object
  identifiers, which have at least two arcs and a first arc of 0, 1 or 2.
- UID input correction. DCMTK removes leading and embedded spaces from UI
  input by default; dicom-go never rewrites a value silently and validation
  reports the spaces (PS3.5 6.2 UI, 9.1). Reading a space-padded UI returns it
  without the spaces, as before, while validation reports the padding.
- Time zone after a TM. OFTime accepts `121530+0100`; PS3.5 6.2 gives only DT
  a UTC offset, so `dcmtime.ParseTime` rejects it.
- Imprecise range bounds. For `-12` (TM) and `-2000` (DT) DCMTK takes the first
  instant of the period, so `120000.000001` and `20000102` do not match.
  dicom-go matches the whole hour or year: PS3.5 6.2 says trailing null
  components mean the value is not precise to them, and PS3.4 C.2.2.2.5
  matches values "prior to and including" the bound.
- Absent PN component groups. DCMTK reports a component group missing from
  the value as an error; PS3.5 6.2.1 lets trailing groups and their `=`
  delimiters be omitted, so `personname` returns an empty group.
- DS formatting. DCMTK writes 6 significant digits by default and refuses a
  value whose requested digits exceed 16 bytes. `FormatDS` writes the shortest
  string that reads back as the value and otherwise rounds to the digits that
  fit, in exponential notation where shorter, as PS3.5 6.2 DS permits.
  `GetFloats`, like DCMTK's atof, tolerates tabs and newlines around a value
  and reads NaN and infinities; validation reports those values.
- Frame size. DCMTK sizes Bits Allocated 12 as packed 12-bit samples and infers
  a YBR_FULL_422 frame as full size from the Pixel Data length. PS3.5 8.1.1
  allows Bits Allocated 1 or a multiple of 8 and PS3.3 C.7.6.3.1.2 defines
  YBR_FULL_422 as two samples per pixel, so `Metadata.FrameSize` follows the
  attributes; packed 12-bit data is read only with the opt-in
  `Unpack12BitPacked` recovery.

DCMTK-specific states without a dicom-go counterpart (the "complete" and
"enhanced encoding" flags of coded entries, the partial vector returned by
getFloat64Vector, the decodedBitsAllocated of native data and the codec
registration of tfrmsiz) are imported but not compared.

## KOS rejection note profile

`sr.NewKOSRejection`, its four title-specific wrappers and
`sr.ReadKOSRejection` construct and interpret the DCM titles 113001, 113037,
113038 and 113039. The strict profile uses one study, a new note Series/SOP
identity, explicit whole-instance IMAGE/WAVEFORM/COMPOSITE roles and exact
Current Requested Procedure Evidence. Quality notes require CID 7011 Document
Title Modifiers; other titles reject them. Optional text uses DCM 113012.
The reader checks VR/VM, known Storage class roles, UID conflicts, permitted
content, Evidence hierarchy, reference relationships and TID 2010. It returns
no partial interpretation after an error. Multi-study notes, partial-instance
references and content extensions are outside this profile.

Default caps bound references, visited elements/items, depth, reasons,
description and metadata before extraction and cloning; overrides can only
lower them. Nonmaterialized metadata fails explicitly before backing reads,
while unused root bulk is ignored. Stable caller sources are borrowed and
output metadata is detached. Existing common-module inheritance preserves
patient/study context and explicit source UTC offsets for new timestamps;
historical DA/TM is not reinterpreted. No pixel, Frame of Reference or SR
completion/verification fields are added. Synthetic fixtures, scoped IOD
checks, LE/BE round trips, limits, ownership and concurrent callers qualify
these library APIs. Archive rejection/deletion, instance selection, the mkkos
CLI and full IOCM PACS/clinical acceptance are outside this implementation.
See [KOS IOCM notes](./KOS_IOCM.md).

## Common modules of derived objects

`object.InheritCommonModules(dst, src, opts)` writes the modules that let an
archive file a derived object with its patient and study: the Patient and
Patient Study, Clinical Trial Subject and Study and General Study attributes
and the Frame of Reference, copied from a source instance; General and
Enhanced General Equipment naming the creating software (dicom-go by
default, never the source's scanner); and Instance Creation Date and Time in
the source's Timezone Offset From UTC. Type 2 attributes neither object has
are written empty and a Study Instance UID is generated when neither has one.
Copies are deep, text is decoded from the source's Specific Character Set
and encoded in the destination's when written (a destination without one
takes the source's), and what the destination already has wins unless
`Overwrite`.

The writers of `seg`, `surfaceseg`, `gsps`, `rtstruct`, `vps`, `sr` (measurement reports,
KOS and other SR documents), `encapdoc` (PDF, CDA, STL, OBJ, MTL) and `encapimage` (Secondary
Capture and VL Photographic) take an `object.DerivedCommon` (a source
instance and the equipment) and apply it; IODs without a Frame of Reference
module (GSPS, SR, encapsulated PDF, CDA and MTL, Secondary Capture) do not
inherit it.
Without a source they still write the empty Type 2 attributes, the
equipment and the creation date. dicom3tools `dciodvfy` reports no error in
these modules for objects written from a clean source image, beyond defects
the source itself carries.

SR sequence items are written in ascending tag order (PS3.5 7.1); items used
to keep the order they were built in, which dicom3tools rejects.

SR documents, KOS and measurement reports list every image and segmentation
they reference in the Current Requested Procedure Evidence Sequence (PS3.3
C.17.2.3). A reference without its Study or Series Instance UID takes them
from `Common.Source` when that is the referenced instance; otherwise the
writer returns `ErrInvalidDocument` or `ErrInvalidMeasurementReport` instead
of leaving the reference out of the evidence, as it did for SR documents and
measurement reports (KOS already failed). A document without a Series
Instance UID (Type 1) gets one derived from its SOP Instance UID. The
Referenced Performed Procedure Step Sequence (SR Document and Key Object
Document Series) and the Performed Procedure Code Sequence (SR Document
General) are written empty, and Referenced Segment Number is US. Frame numbers
are written as given: callers reference frames of multi-frame images only,
since the Image Reference Macro has Referenced Frame Number (Type 1C) for
Multi-frame Images alone. The RT Structure Set writes Series Number,
Operators' Name, Structure Set Date and Time, ROI Generation Algorithm and
ROI Interpreter (Type 2) empty when unknown. With the opt-in
`TestWrittenObjectsPassDCIODVFY` (`DICOMGO_DCIODVFY`), dicom3tools `dciodvfy
-new` (snapshot 20260901) reports no error for these writers' objects and
the encapsulated Secondary Capture and VL Photographic images.

Text values a source pads with NUL instead of a space (PS3.5 6.2, every text
VR but UI) are copied without that padding, so the writer pads them with a
space; derived objects no longer repeat a defect dciodvfy rejects.

## Building images from modality values

Package `imagebuild` creates classic image instances from buffers of
modality values (Hounsfield units, MR signal or any real-world quantity) and
their patient-space geometry: `Build(geometry, slices, opts)` returns one CT
Image, MR Image or Secondary Capture Image instance per slice, numbered
from 1, in one new series with one Study Instance UID. Values are stored as
16-bit integers through the inverse rescale, in the manner GDCM's rescaler
chooses the smallest fitting type: integral values that fit signed or
unsigned 16 bits are stored as they are with slope 1, and other values span
the range as unsigned 16 bits with slope (max-min)/65535, so a value reopens
within half the slope; `Options.Rescale` fixes it. CT and MR carry the Image
Plane module (Pixel Spacing, orthonormal Image Orientation, Image Position,
Slice Thickness) and the Frame of Reference, inherited from the source or
generated; Secondary Capture records no geometry, and Rescale Type (Type 1C
for it) defaults to US. Image Type defaults to DERIVED\SECONDARY with the
third value CT and MR require (AXIAL, OTHER); MR, which has no Modality LUT
module, omits an identity rescale. Patient Position (Type 2C) and the CT and
MR acquisition attributes are copied from a source of the same modality and
otherwise written with their Type 2 or defined defaults; Laterality is only
copied, since it must be absent for unpaired body parts. The patient and
study modules come from `Options.Common`, and `Options.Derivation` records
the derivation codes (CID 7203 multiplanar and curved multiplanar
reformatting, maximum and minimum intensity projection, spatial resampling)
and the source images. dicom3tools `dciodvfy` reports no error for CT, MR
and Secondary Capture instances built from a clean source.

`BuildSecondaryCaptureRGBContext` builds one single-frame Secondary Capture
Image Storage instance from an NRGBA or RGBA display image. It owns the
resulting interleaved RGB8 pixels, writes Explicit VR Little Endian with
Planar Configuration 0 and even-length OB Pixel Data, and omits patient-space
geometry and quantitative rescale. The caller supplies an opaque background
and an explicit Burned In Annotation value. Nonzero image origins, subimages
and padded strides are supported; malformed layouts and excessive dimensions
are rejected before pixel allocation, with a default limit of 16 Mi pixels.
Common patient/study modules and derivation references use the existing
`object.DerivedCommon` and `derivation` APIs. Synthetic round-trip fixtures
pass strict encoding validation and the library's PS3.3 IOD value checks;
DCMTK parsing is separate evidence. The RGB builder has no new `dciodvfy`
acceptance result, color-profile conversion or clinical display qualification.

## Geometric image derivation

`seriesderive.TransformImage` composes crop, resize, flips and right-angle
clockwise rotation for the documented classic single-frame CT/MR native16
MONOCHROME1/2 profile. It retains Study/Frame of Reference and native transfer
syntax, computes IPP/IOP/spacing from the inverse pixel-centre map, allocates new
Series/SOP identities and appends provenance. Resize retains the physical edges
of the cropped FOV; nearest and exact-rational bilinear operate in stored units.
Nearest preserves padding; bilinear with padding fails explicitly. Unsupported
IOD/pixel/geometry profiles, invalid identities and finite admission limits
produce no partial result. Source buffers remain independent of returned data.

Strict VR/VM/File Meta and scoped IOD value checks run before and after derived
Part 10 serialization. Informational/manual IOD conditions remain unevaluated.
Synthetic patient-coordinate and arbitrary-precision sample oracles cover 192
operation/SOP/syntax combinations and separately reopened files; two BE fixtures
also render in DCMTK with the expected dimensions. This is not exhaustive IOD,
target-PACS or clinical qualification. Per-operation payload limits are not an
aggregate/RSS budget; cancellation is observed between rows/stages, while legacy
contextless cloning/IOD/UID callbacks finish their step. See
[geometric derivation](./GEOMETRIC_DERIVATION.md) for the exact admitted domain,
metadata policy, ownership, rounding and limits.

## Quadric mesh decimation

`mesh.DecimateQuadric` collects adjacency in two request-owned stamped arrays
and reusable slices. Topology/link and face-flip checks, boundary preservation,
quadric errors and stopping controls retain their meaning. Arrays use eight
bytes per input vertex; heaps, other scratch and outputs remain allocated.
Equal-cost ordering and arbitrary-input bit identity are not guaranteed.

Five alternating process pairs over three asymmetric synthetic closed/open
meshes measured median time reductions of 9.3–10.8% on one Apple M4 Pro host;
bytes/op were essentially stable and allocation counts fell by less than 1%.
Closed32 timing ranges overlap. Five outputs per case/version passed fixed
geometric/topology bounds, with matching metric medians/ranges in that corpus.
These results do not establish a universal speedup or clinical equivalence.
VTK volume-preservation and attribute-error options were assessed as separate
algorithm/data-model changes and are not implemented by this storage change.
See [decimation scratch storage and evidence](./MESH_DECIMATION_MEMORY.md).

## Discrete mesh curvature and scalar export

`mesh.VertexCurvatures` returns owned float64 mean, Gaussian, ordered principal
curvatures and mixed areas, with one flag entry per vertex. Cotangent weights
and mixed Voronoi areas operate on promoted float32 patient-space positions;
oriented face normals determine the sign. Outward convex mean/principal values
are positive, in mm^-1, with Gaussian in mm^-2. Boundary and unused vertices
are masked; a negative principal radicand is explicitly flagged and clamped
without changing mean or Gaussian values. Invalid/non-manifold geometry fails
without partial output. No welding, repair or global self-intersection test
is performed.

Finite count and managed-array budgets precede allocation; context checks
cover validation, topology sorting and geometry. The conservative reservation
is not an allocator-overhead or RSS ceiling. PLY writes finite double vertex
attributes; GLB writes finite float32 custom SCALAR accessors with optional
units, without rescaling scalar values through the position transform. New
attributes are validated before output; twelve baseline hashes preserve
default PLY/GLB serialization bytes.

Analytic spheres/cylinders, closed-form and integrated geometry checks,
ownership/cancellation/topology cases and independent binary readers qualify
the local synthetic profile. Float32 coordinate precision limits refinement;
no arbitrary-surface convergence, external-viewer or clinical acceptance is
claimed. See [curvature and scalar export](./MESH_CURVATURE.md).

## CPR paths

A `render.CPRPath` centerline is a centripetal Catmull-Rom spline (knot
intervals |ΔP|^0.5) through the control points, so it passes through every
control point with a continuous tangent and forms no cusp or
self-intersection within a span on unevenly spaced points; the end spans
leave along the end chords (reflected phantom points), and a two-point path
is its straight segment. Construction integrates the arc length
(Gauss-Legendre) and precomputes a table of rotation-minimizing frames by
double reflection (Wang et al. 2008), so `PointAt`, `TangentAt` and
`FrameAt` are O(log n) lookups and every frame depends only on the path and
its arc length. `FindCPRAssistedPath` searches a volume-axis grid (1 mm by
default, coarsened to fit the node budget) between consecutive waypoints. By
default it solves the Eikonal equation there by first-order fast marching
(`render.FastMarching`, after ITK's FastMarchingImageFilter) and descends the
arrival time from the second waypoint with half-step subvoxel moves along
the interpolated upwind gradient (after SpeedFunctionToPathFilter), finishing
by a discrete steepest descent if the continuous one stalls. The speed is the
intensity similarity to the waypoints times (d + h), d being the distance
in mm to dissimilar grid nodes, which pulls the route onto the lumen's
medial axis; or, with `SpeedVolume`, a response such as vesselness raised to
`SpeedExponent`. The grid axes are treated as orthogonal. `CPRPathDijkstra`
keeps the earlier 26-neighbour graph search on a 2 mm grid. The route is
resampled as a polyline and, with `SmoothingMM`, low-pass filtered while
keeping the waypoints fixed.

`roi.VolumeMask.Thin` computes a 3D curve skeleton by Lee, Kashyap and Chu's
directional thinning (as in ITK's BinaryThinningImageFilter3D: arc ends kept,
Euler characteristic and 26-connectivity preserved, sequential re-check), so a
tube becomes a one-voxel 26-connected curve on its medial axis about a radius
short of each rounded end. `roi.PruneSkeleton` drops redundant staircase voxels
and then spurs shorter than a length in mm; `roi.SkeletonPath` returns the
shortest skeleton route between two points or the longest geodesic of the
largest component.

`render.RenderCPRScalar` samples a CPR view like `RenderCPR` but returns the
modality values with a validity mask, the spacing of columns and rows, and
the patient-space plane of a transverse section, for quantitative export.
Enhanced (multi-frame) image IODs are not built yet.

## Attribute paths and edits

Package `attrpath` addresses nested attributes with paths in the style of
DCMTK dcmodify (implemented from its documented behaviour, no DCMTK code):
segments separated by `.`, each a keyword, a tag `(gggg,eeee)` or a private
attribute `(gggg,"creator",xx)` resolved through the data set's own creator
reservation, with `[n]` or `[*]` selecting sequence items; every segment but
the last needs an item selector. Paths are bounded (4096 bytes, 64 segments,
item index at most 2^20) and fuzzed (`FuzzParse`).

`Find`, `Create`, `Insert`, `Modify` and `Remove` have dcmodify semantics:
Create and Insert add missing sequences, items up to the requested index and
private creator blocks (at most `MaxCreatedItems`, 1024 by default, per
edit); a wildcard only reaches existing items; Modify changes existing
attributes only; Remove deletes attributes or items. A new attribute takes
its VR from `Options.VR`, the existing element or the dictionary (private
ones through `LookupScopedEntry`, e.g. the curated catalog), and values are
converted per VR (backslash-separated, numeric and AT parsing) and checked by
strict validation, except for value multiplicity. `ParseScript` reads an
ordered edit script (`insert`, `modify`, `remove`, `create`, and
`-if-present` variants). The DCMTK `tpath` vectors are tests, and
`cmd/dcmodify` gives the same data set as DCMTK dcmodify 3.7.0 for a set of
insertions, nested item creation, modifications and removals (opt-in
`DICOMGO_DCMTK_INTEGRATION=1`).

`findscu -k` accepts paths (`ReferencedSeriesSequence[0].SeriesInstanceUID=`
or `(0008,0050)=A1`) next to plain keywords, and `deid.Options.Edits` runs an
edit script after de-identification, reporting the count in
`Report.EditedAttributes`.

## Encapsulated documents

`encapdoc` writes the five Encapsulated Document IODs:

| Builder | SOP Class | Content check | IOD-specific attributes |
|---|---|---|---|
| `NewPDF` | Encapsulated PDF | — | SC Equipment (Conversion Type WSD); Modality DOC |
| `NewCDA` | Encapsulated CDA | parses as XML | HL7 Instance Identifier (0040,E001), given or read from the `ClinicalDocument` `id` (`root^extension`). Explicit values must agree with the document ID when it is available. |
| `NewSTL` | Encapsulated STL | binary STL whose triangle count matches its length, or ASCII `solid`…`endsolid` | Frame of Reference, given or inherited; Manufacturing 3D Model |
| `NewOBJ` | Encapsulated OBJ | UTF-8 text without NUL | Frame of Reference; Manufacturing 3D Model; ICC Profile |
| `NewMTL` | Encapsulated MTL | UTF-8 text without NUL | Manufacturing 3D Model; ICC Profile |

- The 3D models use Modality M3D. Measurement Units Code Sequence defaults
  to UCUM `mm`, and Burned In Annotation defaults to NO.
- OBJ and MTL carry an sRGB ICC Profile unless the caller gives one. PS3.3
  2026d lists that module as User optional there, but dicom3tools
  `dciodvfy` requires it.
- The five IODs pass `dciodvfy`, and `validation/iod` agrees with it on
  every removed Type 1 or 2 attribute.

`Extract` returns the document of any of them. The value is padded to an
even length, and Encapsulated Document Length (0042,0015), when present,
gives the exact length. Without that attribute:

- a PDF ends after its last `%%EOF` line;
- a binary STL keeps the length its triangle count declares;
- the text formats drop their trailing NUL bytes.

`ParseCDANarrative` adds a bounded inert presentation of CDA section narrative:
title, nested sections, paragraphs, lists and simple table rows. It does not
validate clinical entries, templates, signatures or authorship. XML, narrative
and formatted text have separate finite limits; malformed, absent or unsupported
content returns an explicit error without a partial model. External resources,
DTDs and active content are refused. Original extraction bytes remain available
independently of narrative readability. See [CDA narrative limits](./CDA_NARRATIVE.md).


## Encapsulating existing JPEG images

`encapimage.FromJPEG` wraps a JPEG interchange-format file in a Secondary
Capture (default) or VL Photographic Image object without decoding it, in
the manner of DCMTK img2dcm (implemented from the standard; the upstream
DCMTK tool is an oracle only). The frame header selects the transfer syntax: SOF0 → JPEG
Baseline, SOF1 (8 or 12 bits) → JPEG Extended, SOF3 → JPEG Lossless SV1 when
every scan uses predictor 1, otherwise Process 14. Progressive frames (SOF2)
map only to the retired 1.2.840.10008.1.2.4.55 and are rejected unless
`AllowRetiredProgressive` is set; hierarchical, arithmetic-coded, JPEG-LS,
four-component and DNL-sized frames are rejected.

The Image Pixel attributes come from the frame header: one component is
MONOCHROME2; three are RGB when the Adobe APP14 transform flag is 0 or the
component identifiers are R, G, B (and always for lossless, which rejects a
declared YCbCr transform), otherwise YBR_FULL_422 whatever the chroma
sampling, as PS3.3 C.8.12.1.1.1 requires for VL images and img2dcm writes.
Lossy objects carry Lossy Image Compression 01, the ratio and ISO_10918_1;
lossless ones 00. VL Photographic takes only what the VL Image Module allows
(PS3.3 C.8.12.1.1.1-2): 8-bit samples, and for lossy color YBR_FULL_422.
Frames of other precisions (12-bit Extended, lossless above or below 8 bits)
and lossy RGB frames (Adobe transform 0, which cannot be relabeled
YBR_FULL_422 without re-encoding) are refused with
`ErrUnsupportedComponents`; Secondary Capture takes them, and lossless RGB
remains RGB. Pixel Data holds one fragment with an empty Basic Offset
Table; an odd stream gets one trailing zero byte.

APPn segments other than the JFIF APP0 and Adobe APP14 headers (Exif, GPS,
XMP, ICC, MPF, Photoshop and vendor segments), COM segments and every byte
after EOI are removed by default because they can hold personal data;
`KeepAPPn` and `KeepComments` keep them. A complete ICC profile is copied to
ICC Profile (0028,2000) unless `DiscardICCProfile` is set. `Report` names the
removed segments by marker, a fixed identifier list and size, never by
content, and lists the generated (2.25) UIDs. The modules written are
Patient, General Study, General Series (Modality OT or XC), General
Equipment, SC Equipment (Conversion Type WSD) or Acquisition Context (empty),
General Image, Image Pixel, VL Image (Image Type DERIVED\SECONDARY by
default) and SOP Common. Laterality (Type 2C in General Series) is written
empty (unknown) unless `Laterality` gives R or L: nothing records the Body
Part Examined that could show it is not needed, and dciodvfy requires it
then.

Against DCMTK 3.7.0 (opt-in `DICOMGO_DCMTK_INTEGRATION=1`, synthetic fixtures
made with libjpeg-turbo cjpeg): dcmdump reads every object without warnings,
dcmdjpeg decodes all of them (lossless ones to the samples of the built-in
decoder), and for baseline, extended and progressive input every attribute
img2dcm writes is present with the same Image Pixel values and the same
bitstream. img2dcm also removes the JFIF APP0 and the Adobe APP14 and does
not write a Modality for Secondary Capture. dicom3tools `dciodvfy -new`
(snapshot 20260901) reports no error for the Secondary Capture and VL
Photographic objects `FromJPEG` writes.

The retired JPEG transfer syntaxes 1.2.840.10008.1.2.4.52 to .66 (other than
.57) are now read and written as ordinary encapsulated Explicit VR Little
Endian data sets. The built-in set decodes the documented 8/12-bit progressive
profiles of .53 and .55 (see [Pixel Data Codecs](#pixel-data-codecs)); the others still
need a codec the built-in set does not provide.

## Siemens CSA headers and mosaic split

Package `siemens` is an independent implementation of the publicly
documented CSA layout (GDCM, DCMTK and nibabel serve as oracles; no code was
ported). `ParseCSA` recognises the SV10 and NOSV element layouts, byte-swapped
"VS01" blobs, the DATASET form (an Explicit VR Little Endian data set),
Interfile and markup text, and zeroed-out blobs. Element and item counts are
checked against the bytes that remain before anything is allocated, and
`CSALimits` bound the blob size and counts (fuzzed with `FuzzParseCSA`).
`ImageHeader` and `SeriesHeader` locate (0029,xx10) and (0029,xx20) under
"SIEMENS CSA HEADER"; `LookupOptions.AllowMissingCreator` (opt-in, reported
as `CreatorMissing`) accepts the fixed (0029,1010)/(0029,1020) left by some
anonymizers. `EncodeCSA` writes SV10. `ParseASCCONV` reads the ASCCONV
block of MrPhoenixProtocol (or MrProtocol) as an ordered key/value map, and
`Protocol.SliceArray` returns sSliceArray positions, normals and thickness.
On the gdcmData corpus the 14 SV10/NOSV headers (785 elements) match
`gdcmdump --csa` for names, VM, VR, SyngoDT, item counts and values, and the
DATASET and XML headers parse (opt-in `DICOMGO_GDCMDATA_ROOT`).

`CSAHeader.EmbeddedDatasetContext` and `EmbeddedDatasetsContext` explicitly
decode five allowlisted base64 fields as raw Explicit VR Little Endian datasets:
MRDiffusion, MR_ASL and the three documented fMRI sequence fields. Finite
encoded/decoded byte, dataset, node and depth limits apply; returned Objects
are detached and caller-owned. Existing CSA readers do not activate this path.
`mrdiffusion.ReadSiemensEmbeddedContext` extracts validated b-values and
patient-relative gradients from MRDiffusion, rejecting ambiguous or conflicting
structures. This does not implement general ASL/fMRI interpretation. See
[embedded dataset contracts and synthetic coverage](SIEMENS_EMBEDDED_DATASETS.md).

`multiframe.SplitMosaic` splits an Image Type MOSAIC image into classic
single-frame images with their own geometry; `MosaicGeometryOf` computes the
geometry alone. The slice count comes from CSA NumberOfImagesInMosaic, then
(0019,xx0A) of "SIEMENS MR HEADER", and only with
`ImagesFromAcquisitionMatrix` from the Acquisition Matrix (reported). Tiles
are laid out row-major on a ceil(sqrt(n)) grid. The first slice sits half the
mosaic/tile size difference from the mosaic Image Position along the row and
column directions, and the slices follow row x column at Spacing Between
Slices (else Slice Thickness). A CSA SliceNormalVector opposite to row x
column reverses the tile order; a missing or inconsistent one is reported.
When the ASCCONV slice array has one entry per slice, each slice centre is
checked against it (`mosaic-protocol-position-mismatch` above 0.01 mm).
Outputs get new SOP Instance UIDs, the caller's Series Instance UID, Instance
Numbers from 1, Slice Location along the normal, a Source Image Sequence
reference, and lose MOSAIC from Image Type, Smallest/Largest Image Pixel
Value and (unless `KeepPrivate`) private attributes. Compressed sources are
decoded through `DecoderRegistry`; sources that need decode recoveries are
decompressed first. The gdcmData `SIEMENS_MOSAIC_12BitsStored-16BitsJPEG.dcm`
gives the same 18 slices, origin, spacing and pixels as GDCM 3.2.5
SplitMosaicFilter (`multiframe/testdata/gdcm_mosaic_oracle.cxx`).

## United Imaging MR grids

`multiframe.UIHGridGeometryOf` and `SplitUIHGrid` support the known classic MR
VFRAME grid profile under `Image Private Header`, resolved through the creator's
actual private block. Count and frame-sequence cardinality define a rectangular
row-major grid. Each item must provide its own finite Image Position Patient;
common orientation, pixel spacing, private normal and advertised slice spacing
must be consistent. Descending positions and antiparallel normals preserve the
pixel/position pairing. Missing positions or ambiguous geometry are refused.


## Papyrus 3.0 files

Papyrus 3.0 files (ImplementationClassUID "PAPYRUS3.0") are Part 10 files
whose images are items of the private Image Sequence (0041,xx50) of the
"PAPYRUS 3.0" block, so a reader sees no top-level Pixel Data. Reading them
is unchanged; `papyrus.Convert` (and `cmd/pap2dcm`) is the explicit
conversion, returning one Part 10 instance per image with a report:

- The Image Sequence is taken as read (explicit VR files) or parsed from its
  UN value as Implicit VR Little Endian items through `RecoverUN`, within
  `Options.UNRecovery` and `MaxImages` (default 10000). A Number of Images
  (0041,xx15) that disagrees with the items is a warning.
- Each image keeps its own attributes and inherits the top-level ones it
  lacks. Group lengths, the Papyrus blocks and private attributes without a
  valid creator in the image are removed; other private blocks stay.
- Study, Series, Frame of Reference and SOP Instance UIDs that are missing,
  invalid, or under the reserved root 1.2.840.10008 are replaced by generated
  2.25 UIDs (one replacement per distinct value, one generated Series or
  Study UID for all images that lack one); repeated SOP Instance UIDs are
  made unique.
- An image whose SOP Class is not a current standard image storage class
  becomes `FallbackSOPClassUID` (default Secondary Capture, with Conversion
  Type WSD); missing Type 2 attributes of the mandatory modules of the
  target IOD are added empty. Other values are copied as they are, so
  legacy values (a two-digit-year date, free text in a CS) remain.
- Native pixel data is written Explicit VR Little Endian; encapsulated or big
  endian data keeps the source transfer syntax. The source is not modified.

The nine images of gdcmData `PET-cardio-Multiframe-Papyrus.dcm` convert,
decode, and carry the Pixel Data of GDCM 3.2.5 `gdcmpap3 --split`, which
keeps the reserved SOP Instance UID and the non-standard SOP Class
1.2.840.10008.5.1.4.1.1.70 of the source (opt-in `DICOMGO_GDCMDATA_ROOT`).

`siemens.ScrubCSA` returns an SV10 or NOSV header (byte-swapped VS01 too)
with its uninitialized bytes zeroed: after the NUL of each element name,
VR and item value, and the padding of each value. The result has the same
length and parses to the same elements; zeroed headers are returned as they
are, and the DATASET, Interfile and XML formats are refused. De-identification
applies it to every retained CSA header (see DEIDENTIFICATION). On the 14
SV10/NOSV headers of gdcmData every byte `gdcmclean --scrub` clears is
cleared, 13 results are identical, and one NOSV header also loses 40 bytes of
VR tails and value padding that GDCM keeps (they held leftover text).
`siemens.MedComHistoryOf` decodes MedCom History Information (0029,xx20 of
"SIEMENS MEDCOM HEADER"): UTF-16 LE strings, sections and "%&" records, which
record former patient names and identifiers. It matches `gdcmdump --medcom`.

## Vendor private blobs and manufacturer detection

Package `vendors` decodes, only when asked, private blobs of other vendors,
each with a bounded, fuzzed parser (`Limits`: blob size, decompressed size,
entry count) written from the layouts the public tools document (GDCM
gdcmdump as the oracle; no code ported):

- `GEProtocolDataBlockOf`: (0025,xx1B) of "GEMS_SERS_01", a length and a
  gzip stream of KEY "VALUE" lines or XML. A stream that ends before its gzip
  trailer, as scanners write it, decodes with `Truncated` set.
- `ElscintProtocolOf`: (01F7,xx26) of "ELSCINT1", counted groups of named
  entries with values (single and multiple-item groups), or the gzip text
  variant at byte 0x15F.
- `VEPRODataOf`: (0055,xx20) of "VEPRO VIF 3.0 DATA" or "VEPRO VIM 5.0 DATA",
  fixed-width fields whose data fields often hold patient demographics.
- `ElscintPixelDataOf`: pixel data Elscint stored in a private compression
  named by (07A1,xx11) of "ELSCINT1" in (07A1,xx0A), or in Pixel Data when
  the private element is absent and Pixel Data is not native-sized. It
  returns a separate native frame with its metadata, source tag and
  diagnostics (`elscint-private-pixel-data` and, when they apply,
  `elscint-compressed-standard-pixel-data` or
  `elscint-private-pixel-data-stored`); the object and its Pixel Data are
  never changed, so a caller chooses to show it. PMSCT_RLE1 (16-bit
  monochrome: 0xA5 count value byte runs, then 0x5A absolute samples and
  signed byte differences) and PMSCT_RGB1 (8-bit RGB: gray and color modes
  toggled by 0x81, 0x82 escapes, 0x83 repeats, byte differences; interleaved
  or planar output) were implemented from the observed behavior of GDCM's
  rle2img and pmsct_rgb1 examples (`DecodeElscintPMSCTRLE1`,
  `DecodeElscintPMSCTRGB1`). Unknown compressions, mismatched metadata and
  malformed or short streams are `ErrInvalid`; the stream and frame sizes
  are bounded by `Limits`. The gdcmData PMSCT_RLE1 file equals rle2img's
  output, and synthetic streams equal both examples' (opt-in
  `DICOMGO_GDCMDATA_ROOT`, `DICOMGO_GDCM_RLE2IMG`, `DICOMGO_GDCM_PMSCT_RGB1`).
  The ELSCINT1 LOSSLESS RICE compression is not decoded.

The two Protocol Data Blocks, the ELSCINT1 protocol (349 lines) and the two
VEPRO data of gdcmData equal `gdcmdump --pdb/--elscint/--vepro` (opt-in
`DICOMGO_GDCMDATA_ROOT`). Additional Philips SDS v8, Toshiba/Canon MEC CT3,
PMTF/MEC MR3, MEC framing and Canon PAS metadata profiles are decoded only
on request, under aggregate byte, entry and depth limits. Materialized
primitive values are charged by the larger of their stored length and
resident size; embedded File Meta tokens share the dataset entry budget.
These profiles have synthetic qualification, rather than scanner samples
from that corpus. Unknown variants and unsupported values are refused;
see [additional vendor headers](./VENDOR_ADDITIONAL_HEADERS.md).

`DecodePrivateHeaders` renders every header of an object as text for an
inspector, in the manner of gdcmdump: CSA Image and Series Header elements
(`--csa`; the DATASET layout lists its attributes, INTERFILE and XML their
text), the ASCCONV MR protocol (`--mrprotocol`), MedCom History by section,
the GE Protocol Data Block (`--pdb`), ELSCINT1 groups and items
(`--elscint`) and VEPRO fields (`--vepro`). `DetectPrivateHeaders` lists
the headers present through their private creators without decoding them;
each kind carries `MayIdentify` (VEPRO data and MedCom History hold patient
names), so applications warn before decoding. A header that is present but
does not decode keeps its error; rendered lines are bounded
(`PrivateHeaderOptions.MaxLines`, `Truncated`). The legacy kinds decode in
gdcmData (opt-in `DICOMGO_GDCMDATA_ROOT`); package `vendors/vendorfixture`
builds synthetic, PHI-free headers of every kind for tests.

`DetectManufacturer` names the manufacturer from Manufacturer (0008,0070),
ignoring case, periods and underscores and naming acquired lines by their
current owner (Acuson is Siemens, Toshiba is Canon, Hitachi and Aloka are
Fujifilm, Picker and Marconi are Philips), else from the Private Creators.
All evidence is listed, and `Conflict` reports evidence for more than one
manufacturer, as when an archive adds its own private blocks.

## Frame Rendering Helpers

Headless CPU iso-surface frames lease a prepared generation and reuse its
conservative 8-cubed min/max blocks. Skipping only occurs when a block's
maximum is below the iso-value; every skipped sample retains the original
position arithmetic, logical budget charge and cancellation stride. The
first-solid predicate and bisection remain unchanged. Synthetic images and
complete paired measurements, including the dense slowdown and preparation
cancellation limits, are in [iso-surface skipping](./ISO_SURFACE_SPACE_LEAP.md).
Twin's call-site guard rejects these CPU entry points for application 3D.

Scalar MPR slab options apply to orthogonal cached/fallback paths, gantry
tilt and oblique sampling. Zero spacing fraction retains the default step;
trapezoidal Average and Sum halve endpoint weights. Sum is a sample sum,
not an integral in millimetres. MIP/MinIP remain unweighted. Twin exposes
Standard/Fine spacing through its GPU requests in both frontends, without
CPU fallback or a GPU trapezoidal-weighting claim. See [MPR slab sampling
and qualification](./MPR_SLAB_SAMPLING.md).

The `pixeldata/frame` helpers and the example viewer can turn decoded frame
bytes into Go `image.Image` values for a small display-oriented subset:

| Samples Per Pixel | Bits Allocated | Pixel Representation | Planar Configuration | Photometric Interpretation | Output |
|---:|---:|---:|---:|---|---|
| 1 | 8 or 16 | 0 or 1 | absent | `MONOCHROME1`, `MONOCHROME2` | `*image.Gray` with rescale/window handling. |
| 3 | 8 or 16 | 0 | absent or 0 | `RGB` | `*image.RGBA` using interleaved RGB samples. |
| 3 | 8 or 16 | 0 | 1 | `RGB` | `*image.RGBA` using planar RGB samples. |
| 3 | 8 or 16 | 0 | absent, 0, or 1 | `YBR_FULL` | `*image.RGBA` after full-range YBR conversion. |
| 3 | 8 or 16 | 0 | absent or 0 | `YBR_FULL_422`, `YBR_PARTIAL_422` | `*image.RGBA` after native 4:2:2 expansion (or from decoded full-resolution samples) and full- or partial-range YBR conversion. |
| 3 | 8 or 16 | 0 | absent or 0 | `YBR_PARTIAL_420` | Decoded full-resolution samples of MPEG/HEVC objects only; native 4:2:0 is refused. |

Samples in 16-bit containers use Bits Stored (9–16 bits) and are scaled to 8
bits; `display.RenderColor16` keeps 16 bits per channel (`*image.RGBA64`, also
for PALETTE COLOR). Partial-range YBR uses the exact inverse of the PS3.3
C.7.6.3.1.2 matrix. DCMTK uses slightly different constants and differs by up
to two 8-bit steps; RGB and YBR_FULL at 12 and 16 bits match `dcm2img` within
one step (opt-in oracle). `display.ValidateColorPhotometric` rejects
YBR_PARTIAL_420 outside MPEG/HEVC and YBR_ICT/YBR_RCT outside JPEG 2000.
Native YBR_PARTIAL_422 frames are sized at two samples per pixel, like
YBR_FULL_422.

Color rendering does not apply VOI/window, modality rescale, presentation
states, overlays, ICC profiles, or display calibration. Signed color samples,
32-bit containers, planar 4:2:2, YBR_ICT/YBR_RCT pixel data, and PALETTE
COLOR through `pixeldata/frame` remain unsupported and return typed errors.
`pixeldata/display.RenderColor` separately supports PALETTE COLOR when callers
provide the required LUT metadata.

Modality, VOI, Presentation and palette color LUTs share one LUT Data
decoder (PS3.3 C.11.1.1.1, C.11.2.1.1, C.11.6.1.1, C.7.6.3.1.5). By default
it reads 8 to 16 bits per entry, 8-bit entries packed two per word (the first
in the low byte, the last padded when the count is odd, in either byte order)
and, as the standard's notes allow detecting from the value length, 8-bit
entries stored one per word with a zero high byte, reported as
`LUTAdjustmentOneEntryPerWord` in `LUT.Adjustments`. Other encodings are
refused with `ErrLegacyLUTEncoding` unless the reader is given
`display.LUTOptions{Legacy: true}` (`ModalityLUTFromObject`,
`VOIFromObject`, `ModalityLUTAt`, `VOIAt`, `VOIsAt`,
`PresentationFromObject`, `PaletteFromObject`); it then reads each one and
lists it in `LUT.Adjustments`: data longer or shorter than the entry count
(the entries present), packed 8-bit entries under another bits per entry,
bits per entry outside 8–16 (the depth of the largest entry), a 16-bit
descriptor whose entries are 8-bit values (read as 8 bits), an 8-bit
descriptor whose entries are 16-bit values (read as 16 bits), and entries
above the bits per entry (masked). A VOI LUT's output range is 0 to 2^n−1
(n its bits per entry) and is mapped to the display range as a whole, where
earlier versions stretched the range of its entries; palette and Presentation
LUT entries keep their top eight bits in 8-bit output and fill 16 bits in
16-bit output. `display.EncodeLUTData` writes the standard encoding, which
GSPS now uses for Softcopy VOI LUTs (8-bit entries were written one per
word). With DCMTK 3.7.0 (opt-in `DICOMGO_DCMTK_INTEGRATION=1`),
`dcm2pnm` renders VOI and palette LUTs in each encoding like dicom-go: palette
colors exactly, VOI output within one 8-bit step (DCMTK scales by 2^n rather
than 2^n−1). DCMTK reads packed, one-per-word and short data by default but
corrects a descriptor's depth only with `+Mc` (palettes and Modality LUTs) or
`+Mv` (VOI LUTs, the depth of the largest entry), which the legacy cases of the
oracle use.

The first value mapped of a LUT Descriptor is read as signed when the
descriptor's VR is SS or when the LUT's input may be negative, as DCMTK does:
for a Modality LUT or palette when Pixel Representation is 1 (PS3.3
C.11.1.1.1), and for a VOI LUT when the rescale of the stored range (Bits
Stored, Pixel Representation) reaches below zero, or with signed pixels and no
rescale, but never after a Modality LUT, whose output is unsigned (PS3.3
C.11.2.1.1). `ModalityLUTAt`, `VOIAt` and `VOIsAt` take the rescale from the
frame's Pixel Value Transformation macro. A US descriptor of -1024 on a signed
CT, earlier read as 64512, now maps as encoded. The Presentation LUT's values
are always unsigned (PS3.3 C.11.6.1.1). A constant VOI LUT renders its level
(scaled over 0 to 2^n−1). Signed pixels and pixels rescaled below zero, with
US and SS descriptors, and a constant LUT render like `dcm2pnm` within one
8-bit step (opt-in oracle).

Window Center/Width follow the VOI LUT Function (PS3.3 C.11.2.1.2,
C.11.2.1.3). A LINEAR window of width 1 is a threshold at c−0.5 (values up to
c−0.5 black, above it white), as in DCMTK; narrower LINEAR widths are not
valid, so `VOIFromObject`, `VOIAt` and `VOIsAt` skip a stored LINEAR window
below 1, like DCMTK, and the window functions treat a narrower width given by
a caller as 1. LINEAR_EXACT and SIGMOID accept any positive width. Narrow
LINEAR and SIGMOID windows on an 8-bit ramp render like `dcm2pnm +Ww`
(width 1 exactly, wider within one 8-bit step; opt-in oracle).

Grayscale modality and VOI transforms are resolved per frame.
`object.FunctionalGroupItem` returns a functional group macro for a
zero-based frame from the Per-Frame Functional Groups item, then the Shared
Functional Groups, and reports a macro present in both
(`object.ErrFunctionalGroupConflict`, using the per-frame item).
`display.ModalityLUTAt` reads Pixel Value Transformation (0028,9145),
`display.VOIAt` reads Frame VOI LUT (0028,9132), and `display.VOIsAt` lists
every stored window and VOI LUT with its explanation; each falls back to the
top-level attributes only when neither group has the macro, so single-frame
and classic multi-frame objects keep their behavior. `frame.ExtractFrames`
gives every frame its own rescale and window (`frame.RescaleAt`,
`frame.WindowAt`) before caller options.

Overlay planes (60xx) come from `display.OverlaysFromObject` for Overlay Data
(60xx,3000), and from `display.EmbeddedOverlays` for overlays kept in unused
bits of the decoded samples (Overlay Bits Allocated > 1 at Overlay Bit
Position, a retired encoding). Each `display.Overlay` carries its group, type
(G/R), subtype, label, description, Number of Frames in Overlay, Image Frame
Origin (`ForImageFrame` selects the plane of an image frame) and ROI area,
mean and standard deviation when present; Big Endian OW data is normalized.
The unpacked first plane matches GDCM's TestOverlay3 references on gdcmData.
Overlay attributes are untrusted: `Overlay.Validate` requires Overlay Rows and
Columns of 1-65535, an SS Overlay Origin, a US Image Frame Origin and Overlay
Data of at least Rows x Columns x Number of Frames in Overlay bits, checked
without integer overflow. `OverlaysFromObject` (and `PresentationFromObject`)
leave out a group that fails it and report `ErrInvalidOverlay` next to the
valid overlays; `EmbeddedOverlays` also refuses a Bit Position inside the
stored bits (Bits Stored / High Bit) and frame ranges beyond the decoded
samples. `Overlay.Composite` visits only the part of the plane that overlaps
both the image (after Overlay Origin) and the data, so burn-in work is bounded
by the image size; `Frame`/`ForImageFrame` and `Unpacked` refuse a frame the
data does not hold, so their allocations are bounded by the data length.
Transcoding to an encoded syntax refuses embedded overlays, whose bits
encoders do not keep, unless `TranscodeOptions.ExtractEmbeddedOverlays` moves
them to 1-bit Overlay Data and clears the sample bits
(`TranscodeReport.EmbeddedOverlaysExtracted`); native targets keep them.
A GSPS lists the overlays it displays in Overlay Activation Layer
(60xx,1001) and may define its own planes; `gsps.ActiveOverlays` applies
PS3.3 C.11.7: only activated groups are shown, from the state when it defines
the group, otherwise from the image.

Curves of the retired Curve module (50xx, PS3.3-2004 C.10.2) come from
`display.CurvesFromObject` for every group with Curve Data (50xx,3000): Curve
Dimensions, Number of Points, Type of Data, Curve Description and Label, Axis
Units and Labels, and Data Value Representation (US, SS, FL, FD, SL, decoded
in the data set byte order). A dimension whose Curve Data Descriptor
(50xx,0110) is 0000H is not stored and is computed from Coordinate Start and
Step Value. A group with Curve Dimensions outside 1-16, no Number of Points,
an unknown representation, a descriptor of the wrong length or Curve Data too
short for its points is left out and reported with `ErrInvalidCurve` next to
the valid curves; audio curves (no Curve Data) are not read. POLY (poly line)
and ROI (closed outline) curves with at least two dimensions in pixels (Axis
Units PIXL or absent) are image graphics: `Curve.ImagePoints` reads each
tuple as (column, row) with the top-left pixel at (1,1), the Overlay Origin
convention, and `Curve.Composite` draws one-pixel lines through the vertices,
closing ROI outlines, after clipping each segment to the image so the work is
bounded by the image size and the point count. Other types (TAC, PROF, HIST,
PHYSIO, ECG, PRESSURE and the like) are plots and are not drawn on the
image. The PHYSIO curves of gdcmData's GE DLX XA images match GDCM's
TestCurve2 point references.

`display.VOIsAt` lists every stored window and VOI LUT of a frame with its
explanation and VOI function. For images without a usable one,
`render.AutoWindowWith` offers four strategies: min/max (`AutoWindowMinMax`),
min/max ignoring the pixels equal to the extremes, a histogram window that
drops a fraction at each end, and any of them restricted to a region or a
mask (a region of interest). Values are counted per stored value with Pixel
Padding Value and Range Limit excluded (`render.Frame.PixelPadding`) and
converted through the rescale. The window covers the range inclusively:
center (min+max+1)/2 and width max−min+1 in stored units, so LINEAR maps the
minimum to black and the maximum to white. `AutoWindow` is the min/max
strategy; `render.PixelValueRange` returns the padding-free value range. With
a slope of 1 every strategy equals `dcm2img +Wm`, `+Wn`, `+Wh` and `+Wr`
(opt-in oracle test). DCMTK does not exclude padding, so the oracle images
carry none.

Display calibration follows the Grayscale Standard Display Function of PS3.14.
`display.GSDFLuminance` is Equation 1 and `display.GSDFJNDIndex` Equation 2.
`display.ParseCharacteristicCurve` reads a measured device curve: DDL to
luminance for monitors, or DDL to optical density for printers, with optional
ambient light, illumination and polynomial order. Points are interpolated with
a natural cubic spline, or with a least-squares polynomial when an order is
given. `DisplayCalibration.GSDFLUT` maps P-values of 1 to 16 bits to the DDL
whose luminance is closest to the GSDF target. The JND range runs between the
measured minimum and maximum luminance: ambient light is added for monitors,
and for printers the luminance is ambient + illumination·10^-OD, restricted
by Min/Max Density. With the DCMTK sample curves this matches
`dcm2img +Dm monitor.lut` and `+Dp printer.lut` for every 8-bit input. The
test is opt-in with `DICOMGO_DCMTK_INTEGRATION=1` and reads the curves from
the DCMTK installation; they are not vendored.

`DisplayCalibration.CIELABLUT` is the alternative perceptual calibration:
P-values are spaced equally in CIE 1976 lightness L* (`display.CIELABLightness`
and its inverse `CIELABRelativeLuminance`, with ε = 0.008856 and κ = 903.3)
from the L* of the minimum luminance relative to the maximum up to L* 100,
over the same luminance range and viewing conditions as the GSDF LUT. Each
P-value takes the DDL whose luminance is closest to its target, searching
forward through DDLs whose luminance increases and keeping the higher DDL on
a tie; with Min/Max Density a printer uses only DDLs inside that range. This
is DCMTK's CIELAB display function, and the opt-in oracle compares it with
`dcm2img +Dm monitor.lut +Dc` and `+Dp printer.lut +Dc`.
`DisplayCalibration.LUT` selects either function (`display.FunctionGSDF`,
`display.FunctionCIELAB`).

`display.RenderGray16` keeps the VOI output as 16-bit P-values, and
`Presentation.ApplyGray16` applies the presentation stages to them; the
shutter uses its P-value. Presentation LUT Shape LIN OD sets `Presentation.LinOD`
and a table built by `PrintDensity.LinODPresentationLUT` from the object's
Min/Max Density (hundredths of OD), Illumination and Reflected Ambient Light.
The defaults are 0.20–3.00 OD, 2000 cd/m² and 10 cd/m². Input values are
proportional to density, so 0 is Min Density. `LinODPresentationLUTFor`
spans a calibrated printer's own P-value range. Against `dcm2img +Dp
printer.lut +Pod` it is within one DDL. The default 8-bit rendering is
unchanged.

`render.StandardEncodedRenderer` accepts an already-decoded `render.Frame` and
uses this same grayscale modality/VOI and native color path before encoding
`image/png` or 8-bit Baseline `image/jpeg`. An optional source rectangle is
cropped before bilinear scaling. Requested width and height form a bounding box
that preserves aspect ratio; zero dimensions retain or derive the source
matrix. Source dimensions, requested dimensions, pixel count, and encoded
output bytes are bounded by `EncodedImageLimits`. Cancellation is checked
before rendering, for every resize row, and on every encoder write. Resource
lookup, transfer-syntax decoding, annotations, and ICC color management remain
caller-owned.

### Grayscale Softcopy Presentation State

`gsps.State` models the GSPS IOD: the Presentation Series and Presentation
State Identification attributes (Write defaults Instance Number 1, Content
Label UNNAMED and the creation date and time to now), image- and
frame-specific Displayed Areas, Softcopy VOI, display shutters, the Modality
LUT that replaces the images' own (rescale or table), the Presentation LUT
Shape or Presentation LUT Sequence table, graphic layers (order, recommended
grayscale and CIELab values, description), text objects with bounding boxes,
justification and anchor visibility, filled graphics and overlay activation.
Displayed Area corners are the pixels shown at the top left and bottom right
after rotation and flip, so rotated states list them in either order;
`DisplayedArea.ImageRegion` returns the region and `SpatialTransform.Corners`
the corners for a region. Write refuses layer names that are not CS values,
text over 1024 characters, fills of open graphics, rotations other than
multiples of 90 degrees, and VOI or Modality LUTs whose entries are not 8 or
16 bits (PS3.3 C.11.1.1.1 and C.11.2.1.1). Presentation Pixel Aspect Ratio 1:1
is written when a Displayed Area has neither spacing nor ratio.
`gsps.FromImage` builds the default state of an image as DCMTK `dcmpsmk`
does (whole image at its pixel spacing, its rescale or Modality LUT, first
VOI, shutter, overlay activation, INVERSE for MONOCHROME1); an opt-in test
compares both.

`gsps.Render` burns a state into a frame headless: modality, VOI and
Presentation LUT to P-Values, activated overlays, the display shutter,
rotation and flip, the Displayed Area (its part outside the image takes the
shutter presentation value, as DCMTK prints it) and the graphic and text
objects layer by layer. PIXEL annotations turn with the image, DISPLAY ones
stay on the displayed area; `Apply` now maps DISPLAY points back through the
rotation and flip the same way. Text uses a built-in 5x7 ASCII font unless
the caller provides a `gsps.Font`; INTERPOLATED graphics are Catmull-Rom
curves. Output is one pixel per image pixel: Presentation Size Mode, pixel
spacing and aspect ratio, which need the display device, are left to the
caller. Against DCMTK `dcmp2pgm` (opt-in oracle, five states covering rescale
overrides, windows, a VOI LUT, a Presentation LUT table and every rotation
and flip) the pixels agree within one level; dcmp2pgm burns neither
geometric shutters nor graphic overlays, so those are compared by unit
tests. `dcmpschk` accepts every generated state, and `dciodvfy` reports only
the Laterality it cannot know is not needed, as for `dcmpsmk`'s states.

## JSON

Supported:

- DICOM JSON marshal/unmarshal for the in-memory value types implemented by
  `core`.
- Reader/writer APIs and synchronous array callbacks, with cumulative limits
  and explicit partial-consumption semantics. One dataset may be retained in
  memory; this is not constant-memory streaming inside an individual value.
  DICOMweb QIDO/metadata servers validate and encode into bounded private
  staging before publishing; opt-in JSON client callbacks bypass the response
  cache and are not replayed after a callback or mid-body error. Existing
  resident/XML client APIs retain their contracts. See
  [JSON streaming and limits](DICOM_JSON_STREAMING.md).
- Person Name JSON components.
- Sequences.
- Inline binary values.
- `BulkDataURI` preservation.
- Opt-in defined-length primitive bulk selection by parsed VR, encoded length
  and complete sequence-item path on seekable, standard non-deflated sources.
  Deferred-source callbacks receive physical offset/length, effective source
  encoding and an owned path before payload replay. A nonempty URI avoids
  materialization; an empty URI retains the existing conversion/callback order.
  `MaxMaterializedValueBytes` can bound deferred fallback before allocation.
  Contextual VR discriminants remain inline, and legacy Pixel Data stream
  callbacks retain normalized bytes. See [bulk selection and reference
  contracts](BULK_DATA_DESCRIPTOR.md).
- `Value` arrays for AE, AS, AT, CS, DA, DS, DT, FD, FL, IS, LO, LT, PN,
  SH, SL, SQ, SS, ST, SV, TM, UC, UI, UL, UR, US, UT and UV.
- `InlineBinary` or `BulkDataURI` for OB, OD, OF, OL, OV, OW and UN.

Limitations:

- Source references require an open immutable source, preserved parsed backing
  and serialized replay. URI storage/lifetime, retrieval media types and charset
  conversion belong to the caller. Deflated source offsets and arbitrary
  mutations of aliased backing are outside this reference contract.
- `BulkDataURI` is not fetched while decoding. `object.ResolveBulkData`
  resolves the references of a decoded object afterwards through an explicit
  `object.BulkDataURIResolver` under per-value and total byte limits:
  `object.FileBulkDataResolver` reads `file:` URIs with optional `offset` and
  `length` query parameters inside a required root directory, and
  `dicomweb.Client` reads same-origin http(s) and relative references.
  Resolved values become native Little Endian bytes; encapsulated Pixel Data
  is not synthesized (stream it with `object.NewBulkDataWriteResolver`).
- `UN` values are preserved as bytes through `InlineBinary` or `BulkDataURI`;
  JSON `Value` arrays for UN are rejected instead of guessed. Two UN cases
  whose VR the standard fixes are exported by it: a Private Creator
  (gggg,0010-00FF) encoded as UN is written as LO
  when its bytes form a valid creator (Default Character Repertoire, space
  padding; otherwise it stays UN), and a UN whose value was parsed as a
  sequence (a private SQ in Implicit VR, or UN per CP-246) is written as SQ
  with its items.
- Text follows PS3.5 6.2: LT, ST, UT and UR hold one value in which a
  backslash is literal, leading spaces are kept and trailing spaces are padding;
  other text VRs split on backslash. Readers refuse several `Value` entries for
  those single-valued VRs and a backslash inside one entry of a multi-valued VR
  or PN group, and authoring refuses several components under LT/ST/UT/UR,
  because encoding would silently change the VM. An absent attribute has no
  key, an empty or padding-only one has no `Value`, and an empty component is
  `null` (PS3.18 F.2.5; pydicom 3.0.2 writes `""` there). Diagnostics report
  rules and sizes, not values.
- `InlineBinary` of OW, OF, OD, OL and OV values is Little Endian, as PS3.18
  F.2.7 requires, whatever the byte order of the object: the values of a Big
  Endian object are reversed by VR width (OW Pixel Data with Bits Allocated
  32 or 64 by sample), and `BulkDataURIFunc` and `PixelDataBulkDataURIFunc`
  receive the same bytes, the latter streamed. The object's own
  values are never modified: a held value is copied for the conversion only
  after `MaxInlineBinaryBytes` accepts it, and a value read from the source
  is reversed in its own buffer.
  Unmarshaling with a Big Endian `ByteOrder` or `TransferSyntax` reverses them
  back; the order is not inferred from the JSON. OB and UN bytes are kept.
  `SourceByteOrderInlineBinary` in `Options` and `UnmarshalOptions` keeps the
  object's own byte order instead, the former behavior.
- Unsupported or mismatched VR/value combinations return errors with the JSON
  path, VR and received JSON value type.

## Native DICOM Model XML

Supported:

- PS3.19 Native DICOM Model marshal/unmarshal for the in-memory value types
  implemented by `core`, including sequences, Person Name component groups,
  empty values, `InlineBinary`, and `BulkData` URI/UUID references.
- `EncodeContext` and `DecodeContext` convert directly between Objects and XML
  tokens without an intermediate DICOM JSON document. `Walk`/`WalkContext`
  deliver top-level attributes in document order without retaining an Object.
- The required namespace, `xml:space`, tag normalization, sequential value
  numbering, and omission of retired group-length elements.
- Finite element, value, nesting, binary, and aggregate input limits.
- DICOMweb QIDO and WADO metadata negotiation as
  `multipart/related; type="application/dicom+xml"`, with one model per part.

Limitations and safety policy:

- Walking retains one complete attribute (including its sequence items) and
  allocation metadata; callbacks can precede a later document error. Top-level
  private attributes expose normalized tags plus creator identity; nested items
  have allocated tags.
  Decode retains the final Object, and `encoding/xml` can buffer a complete
  character-data token. The legacy `BulkDataURIFunc` can materialize one complete
  Value Field before selecting a URI. Finite input/value limits are not a
  constant-memory or process-RSS guarantee; see
  [Native XML ownership, streaming and limits](NATIVE_DICOM_XML.md).
- DTDs and processing instructions are rejected; the decoder does not expand
  external entities.
- `BulkData` references are preserved without I/O by default. Rejection or
  bounded resolution requires an explicit caller policy.
- Resolving encapsulated Pixel Data requires an explicit transfer syntax; the
  decoder does not infer compressed syntax from XML metadata.
- The `InlineBinary` of OW, OF, OD, OL and OV values, and the data passed to
  `Options.BulkDataURIFunc`, are Little Endian whatever the byte order of the
  object. PS3.19 Table A.1.5-2 defers to the PS3.5 byte ordering rules, and a
  document carries no transfer syntax; PS3.18 8.7.3 excludes Explicit VR Big
  Endian from web services, and dcm4che 5.35.2 `dcm2xml` writes these values
  in Little Endian from a Big Endian file. A Big Endian value is reversed at
  the VR width, OW Pixel Data with Bits Allocated 32 or 64 at the sample
  width, as `pixeldata.TranscodeDataSet` and `dicomjson` do. Decoding with a
  Big Endian `ByteOrder` reverses `InlineBinary` and resolved `BulkData` back;
  `SourceByteOrderInlineBinary` in either direction keeps the object's byte
  order. Peers differ on OW Pixel Data with Bits Allocated 32 from a Big
  Endian file whose Value Field is `01 02 03 04`: this package writes the
  sample reversed, `04 03 02 01`, while dcm4che swaps 16-bit words, `02 01 04
  03`. DCMTK 3.7.0 `dcm2xml --native-format` writes OW, OF, OD, OL and OV in
  Big Endian whatever the file (`01 02 03 04` there, from the Big Endian and
  from its own Little Endian conversion, which it makes by 16-bit words), so
  its `InlineBinary` reads back byte-swapped.
- Missing or mismatched `keyword` attributes fail by default. A caller may
  explicitly enable missing-keyword tolerance for nonconforming producers.
- Documents declaring an encoding other than UTF-8 (DCMTK `dcm2xml` writes
  ISO-8859-1) are decoded through the IANA character set registry
  (`dicomxml.CharsetReader`); unknown encodings fail.
- A private data element needs a valid Private Creator for its
  `privateCreator`; creators encoded as UN are resolved from their bytes as in
  DICOM JSON. Elements of the odd groups 0001-0007, which PS3.5 7.8.1
  excludes from private groups, keep their tag without a creator. A private
  element at block offset 00 is written as `gggg0000` with its creator and is
  not mistaken for a group length on reading; a creator repeated in a group
  reads back to its first block, and a creator slot is never reused.
- Marshal rejects a private data element whose creator is missing or invalid
  (for example NUL padded) and one in a second block reserved by the same
  creator, which `gggg00ee` cannot tell apart. `Options.RecoverPrivate`
  (reported through `Options.PrivateRecovered`, which it requires) trims NUL
  padding from creators, omits elements still without a creator, and merges
  a repeated creator's elements into its first block, omitting those whose
  offset is taken there.
- Documents are XML 1.0, which cannot hold NUL, the other C0 controls except
  TAB, LF and CR (so not the FF or ESC that DICOM text may contain), U+FFFE,
  U+FFFF or invalid UTF-8. Marshal rejects a value, Person Name component or
  attribute (`keyword`, BulkData `uri`) holding one with `ErrInvalidXML` and
  its path (for example `.../DicomAttribute[@tag='00100020']/Value[1]` or
  `.../PersonName[1]/Alphabetic/GivenName`) instead of writing U+FFFD.
  `privateCreator` values cannot hold one: a creator must be a valid
  `dictionary.PrivateCreatorID`. `Options.RecoverText` (reported through
  `Options.TextRecovered`, which it requires) trims trailing NUL padding,
  which some producers write instead of spaces, from the last value of an
  element or the last component group of its last Person Name; a NUL
  elsewhere still fails. A padded Private Creator is recovered by
  `RecoverPrivate` instead. `dcm2xml -recover-text` applies it.

The Part 10 writer can materialize preserved `BulkData` references through an
explicit streaming resolver. It validates the declared source size, supports
references nested in sequences, pads according to the VR, and owns/closes each
resolved reader. It deliberately does not infer or synthesize encapsulated
Pixel Data.

## Conversion commands

`cmd/dcm2json`, `cmd/json2dcm`, `cmd/dcm2xml` and `cmd/xml2dcm` convert
between Part 10 files, DICOM JSON (PS3.18 Annex F) and the Native DICOM Model
(PS3.19 Annex A; not the DCMTK-specific XML schema), on top of `dicomjson` and
`dicomxml`:

- Text is written as UTF-8 and Specific Character Set is declared
  `ISO_IR 192`, as DCMTK dcm2json does, so readers do not mis-decode it;
  `-keep-charset` keeps the source declaration. `json2dcm`/`xml2dcm
  -charset TERM` re-encode text into a declared character set with
  `-unrepresentable fail|transliterate|discard` (default fail).
- `-meta` includes the File Meta Information so the transfer syntax, and
  with it encapsulated Pixel Data, round-trips; otherwise `-transfer-syntax`
  names it (default Explicit VR Little Endian).
- `dcm2xml -recover-private` applies `dicomxml.Options.RecoverPrivate`, and
  `-recover-text` `dicomxml.Options.RecoverText`; each prints its
  recoveries on stderr.
- `-bulk-dir DIR -bulk-threshold N` writes binary values of at least N bytes
  to files referenced by relative BulkDataURIs; `dcm2xml` writes OW, OF, OD,
  OL and OV values in Little Endian, inline and in those files. The reverse direction
  resolves only relative paths inside the document's directory; absolute
  paths, `..`, schemes and backslashes are rejected, symbolic links and
  junctions are evaluated before the containment check (a link leaving the
  directory or a missing file is rejected), and each file is bounded (1 GiB).
- `dcm2json`/`dcm2xml -bulk-file FILE -bulk-threshold N` instead append
  those values to one file and reference each with an absolute `file:` URI
  carrying `offset` and `length`. `json2dcm` and `xml2dcm` do not follow
  `file:` URIs; `object.ResolveBulkData` with `object.FileBulkDataResolver`
  reads them back.

A Part 10 -> JSON/XML -> Part 10 round trip gives the same encoded data set
(with `-keep-charset`, or `-charset` naming the source declaration), with and
without bulk data, for text in Latin-1, every numeric VR, AT, nested and
private sequences, and native and encapsulated Pixel Data. Against DCMTK
3.7.0 (opt-in `DICOMGO_DCMTK_INTEGRATION=1`): `dcm2json` output agrees
element by element and decodes to the same data set; DCMTK `json2dcm` reads
this JSON back to the same values (it cannot build encapsulated Pixel Data
from InlineBinary); `dcm2xml --native-format` output decodes to the same
values except for the binary ones, which it replaces by content-less UUID
BulkData references.

## Security Considerations

Parser limits are opt-in, except sequence nesting. Zero-valued numeric limits
mean unbounded reads, which is convenient for trusted batch processing but
inappropriate for untrusted inputs.


For untrusted files, set limits through `object.OpenFileWithOptions` or
`object.ReadFileWithOptions`:

```go
file, err := object.ReadFileWithOptions(r, object.ReadFileOptions{
	MaxElementBytes:               16 << 20,
	MaxDeformableVectorGridBytes: 64 << 20,
	MaxTotalBytes:                 128 << 20,
	MaxSequenceDepth:              32,
	MaxElements:                   100000,
	MaxFragments:                  10000,
	StrictReservedBytes:           true,
})
```

Relevant options:

- `MaxElementBytes`: caps allocation for a single element value. Within the
  cap, a value or native frame buffer grows as its bytes arrive unless the
  source's known remaining size already holds it, so a declared length that
  the peer never sends is not reserved.
- `MaxDeformableVectorGridBytes`: caps each Vector Grid Data OF value before
  allocation, including values nested in sequences.
- `MaxTotalBytes`: caps total bytes read.
- `MaxSequenceDepth`: caps nested sequence/item depth (default 128 frames, 64
  nested sequences; negative disables it).
- `MaxElements`: caps primitive element count per parser reader.
- `MaxFragments`: caps encapsulated Pixel Data fragment count.
- `StrictReservedBytes`: rejects non-zero explicit VR reserved bytes.
- `OddLengthPolicy`: controls handling of odd element lengths.

These limits may reject valid large DICOM objects. Tune them for the deployment
environment.

### Digital Signatures (PS3.15 Annex C)

`dsig.Verify` checks every Digital Signatures Sequence (FFFA,FFFA) item of a
data set and of its sequence items against the matching MAC Parameters
Sequence (4FFE,0001) item, and `dsig.Sign` adds a signature over the top-level
data set or, with `SignOptions.Item` (an attribute path selecting one existing
item, like DCMTK `dcmsign --sign-item`), over one sequence item, with the MAC
ID numbering, Data Elements Signed and profile checks of that item.
`RemoveSignature` and `RemoveAllSignatures` delete signatures (a MAC
Parameters item is kept while another signature uses it; empty sequences are
removed), and `cmd/dcmsign` lists, verifies, signs, timestamps and removes
signatures from the command line ([CLI tools](CLI_TOOLS.md#dcmsign-digital-signatures)). The MAC byte stream follows PS3.3 C.12.1.1.3.1.1: the Data Elements
Signed in ascending tag order, sequences and undefined-length items encoded
with the bare Item and Sequence Delimitation tags (no Value Length), then the
signature item's attributes other than the certificate, signature and
certified timestamp. Group lengths, Length to End, groups below 0008, UN
elements and sequences holding them, group FFFA, the MAC Parameters Sequence
and Data Set Trailing Padding are never signed.

| Aspect | Support |
| --- | --- |
| MAC Algorithm | RIPEMD160 (a local implementation checked against the published test vectors), MD5, SHA1, SHA256 (default when signing), SHA384, SHA512 |
| MAC Calculation Transfer Syntax | Implicit and Explicit VR Little Endian, and encapsulated syntaxes; encapsulated Pixel Data must be signed in the encapsulated syntax it is stored in (`ErrEncapsulatedMACSyntax`), as DCMTK requires. Big endian and deflated syntaxes are not supported. |
| Certificate Type | `X509_1993_SIG` |
| Signature | RSA PKCS #1 v1.5 with a DigestInfo (verified with and without NULL parameters), ECDSA (ASN.1, OB padding tolerated) |
| Attribute profiles | `CheckProfileContext` / `CheckProfileWithOptions` report bounded Base, Creator, Authorization and SR RSA attribute coverage; `SignOptions.Profile` and `ProfileOptions` refuse violated or incomplete coverage before publication. Legacy `CheckProfile` fails closed when Authorization context is unknown. |
| Certified timestamps | `CMS_TSP` (RFC 3161) created and verified, excluded from the MAC; see below |

`dsig.Verify` only proves that the data matches the signature and the key in
Certificate of Signer. `dsig.VerifyWithTrust` (and `VerifyCertificate` for one
signature) also decides whether that certificate is trusted, with
`crypto/x509` only, and reports it apart: `Result.Err` and `Valid()` for the
signature, `Result.TrustErr`, `Chains` and `Trusted()` for the certificate.
`TrustOptions` name the trust anchors (`Roots`; nil trusts nothing, the system
roots are never implied), untrusted `Intermediates`, `CRLs` (checked for every
chain certificate below the root, accepted only when signed by its issuer; a
listed serial revokes whatever the revocation date) with `RequireCRL` like
dcmsign `+cl`, the verification time (`Time`, the current time by default, or
`AtSignatureTime` for the signer-asserted Digital Signature DateTime) and a
`KeyUsagePolicy` (digitalSignature or nonRepudiation by default when the Key
Usage extension is present, optional Extended Key Usages). Failures match
`ErrCertificateUntrusted` and, when it applies, `ErrCertificateExpired`,
`ErrCertificateRevoked`, `ErrRevocationUnknown` or `ErrCertificateUsage`. OCSP
and fetching CRLs or intermediates are not supported.

Certified timestamps follow PS3.3 C.12.1.1.3 and RFC 3161: the
Certified Timestamp Type (0400,0305) is `CMS_TSP` and the Certified Timestamp
(0400,0310) holds the DER TimeStampToken, whose imprint is the hash of the
Digital Signature (0400,0120) value (without the OB padding byte of an odd
length ECDSA signature, as DCMTK does). `NewTimestampRequest` builds the
TimeStampReq (SHA-256 by default, SHA-1/384/512, optional policy, random nonce,
TSA certificate requested); a `Timestamper` answers it — `HTTPTimestamper`
posts `application/timestamp-query` (RFC 3161 3.4), `TimestamperFunc` wraps an
in-process or file-based TSA — and `TokenFromResponse` plus
`TimestampRequest.Check` refuse a rejection or a response to another request
(imprint, nonce, policy, missing requested certificate) before
`AddTimestamp` stores the token in the signature item (top level or sequence
item); `TimestampSignature` does all of it. The timestamp is outside the MAC,
so the signature stays valid. `VerifyTimestamp` reads the token with a minimal
DER-only CMS SignedData reader written from RFC 5652 (one signer with signed
attributes; RSA PKCS #1 v1.5, ECDSA or Ed25519; content-type and
message-digest attributes; an ESS signing-certificate v1 or v2 naming the TSA
certificate), requires the TSA certificate's critical Extended Key Usage of
`timeStamping` only (RFC 3161 2.3, as OpenSSL does), and checks that the
certified time lies within the Certificate of Signer validity; with
`TimestampOptions.Roots` it also chains the TSA certificate
(`ErrTimestampUntrusted`). `Verify` reports the token in `Result.Timestamp` and
`TimestampErr` (no TSA trust); `VerifyWithTrust` with
`TrustOptions.TimestampRoots` also requires a trusted TSA, and
`AtTimestampTime` verifies the signer certificate at the certified time, so a
signature timestamped while its certificate was valid stays trusted after it
expires. BER tokens, RSA-PSS token signatures and timestamp renewal (archive
timestamps) are not supported. A file signed here whose token `openssl ts
-reply` issued (`dsig/testdata/timestamp`, synthetic PKI, no keys) verifies
and is trusted at the certified time; with `DICOMGO_OPENSSL_INTEGRATION=1`
OpenSSL 3.6 answers requests built here and `openssl ts -verify` accepts the
tokens of the in-process test TSA, and with DCMTK too a query `dcmsign +ts`
writes, answered by OpenSSL and inserted with `dcmsign +t`, verifies here,
while files timestamped here are offered to `dcmsign --verify +tr`.
Attribute-profile checks use the complete PS3.15 2026d C.2/C.3/C.4 module lists,
including Enhanced MR/CT/PET/US, SEG, MRS, Raw Data and current waveform and
microscopy modules. PS3.3 module Includes, explicit local complements and
retired repeating Overlay/Curve lists are pinned by edition/source SHA;
unavailable catalogs or an unreviewed edition return `ErrProfileIncomplete`.
Present required attributes must be signable and signed; main-dataset scope,
existing identifiers, Raw private-data policy and SR/KOS observer/purpose
conditions are explicit in the [profile contract](DIGITAL_SIGNATURE_PROFILES.md).
Nested attributes are covered by their signed parent sequence, not fabricated
root AT tags. SR's purpose-5 candidate is structural coverage only.

Authorization requires `ProfileOptions.HumanVerifiedPaths`: nil is unknown,
while a nonnil empty slice is an explicit caller declaration of no additional
human-verified attributes. The library cannot attest that declaration's truth
or authority. Reports separate complete/needs-context/partial coverage from
violations, signature integrity, trust, timestamps and IOD validity. Defaults
bound metadata to 100,000 elements/items, depth 64, 256 issues and 256 declared
paths; SR sibling certificates and traversal have additional documented bounds.
Context cancellation or a limit returns partial coverage and an error.
These are logical limits, not measured RSS or clinical conformance.
Twin's inspectors display attribute coverage independently of integrity/trust;
they report Authorization verification context as unavailable and do not invent
a human declaration. A cryptographically invalid signature remains invalid
even when its metadata attribute coverage is complete.

With DCMTK 3.7.0 and OpenSSL 3.6 (opt-in `DICOMGO_DCMTK_INTEGRATION=1`),
signatures `dcmsign` creates with every MAC algorithm, tag subsets
(`-t`), a sequence item (`+si`), the Creator profile with a purpose code
(`+pc +sp`), encapsulated Pixel Data, Implicit VR files and two signatures in
one data set verify here; signatures made here (every MAC algorithm, native and
encapsulated Pixel Data, Implicit VR MAC syntax, the Creator profile, ECDSA)
pass `dcmsign --verify +rg` with the test CA trusted, `+rc` for the Creator
profile, signatures placed in sequence items one and two levels down
included; and both sides reject a file changed after signing. Samples
`dcmsign` signed with a trusted (through an intermediate), an untrusted, an
expired and a revoked certificate (`dsig/testdata/dcmsign-trust`, synthetic
PKI, no keys) all verify as signatures; `VerifyWithTrust` trusts only the
first, with the reason of each refusal, as `dcmsign --verify +cf +uf +cr +cl`
does.

## Network Security

`v0.1.0` networking uses plain TCP by default. The core UL APIs can opt into
TLS for association initiation and acceptance by setting
`ul.DialOptions.TLSConfig` and `ul.AcceptOptions.TLSConfig`. The caller supplies
the certificate roots, server certificates and verification policy through the
standard `crypto/tls` configuration.

### PS3.15 TLS Secure Transport Connection Profiles

`ul.ClientTLSConfig` and `ul.ServerTLSConfig` build `tls.Config` values for a
profile (`ul.TLSProfileOptions.Profile`, `ul.ParseTLSProfile`, which also
accepts DCMTK's option suffixes such as `8996` or `bcp195-ex`). Every profile
negotiates TLS 1.2 or 1.3 only, and the TLS 1.2 suites are restricted to the
list below; CBC, SHA-1 MAC, static RSA key exchange and NULL suites are never
offered. TLS 1.3 uses Go's AES-GCM and ChaCha20-Poly1305 suites, which every
profile allows (Go does not let them be configured).

| Profile | PS3.15 | TLS 1.2 suites offered | Required suites Go cannot offer |
| --- | --- | --- | --- |
| `bcp195-rfc8996` (default) | B.12 | ECDHE-ECDSA/RSA with AES-128/256-GCM (RFC 9325) | none |
| `bcp195-rfc8996-modified` | B.13 | ECDHE-ECDSA/RSA with AES-256-GCM, ChaCha20-Poly1305, AES-128-GCM | TLS_AES_128_CCM(_8)_SHA256 and TLS_ECDHE_ECDSA_WITH_AES_128/256_CCM(_8); optional Camellia and DHE suites |
| `bcp195-nd` | B.10 (retired) | ECDHE-RSA with AES-128/256-GCM | TLS_DHE_RSA_WITH_AES_128/256_GCM_SHA256/384 |
| `bcp195` | B.9 (retired) | as `bcp195-nd`, without fallback below TLS 1.2 | as `bcp195-nd` |
| `bcp195-extended` | B.11 (retired) | ECDHE-RSA/ECDSA with AES-128/256-GCM | the DHE suites; optional Camellia suites |

`TLSProfile.UnsupportedCipherSuites` lists the missing suites for a
conformance statement. Certificates below the profiles' key requirements (RSA
under 2048 bits, ECC under 256 bits, MD5/SHA-1 signatures) are refused both
when the configuration is built and in the peer's leaf certificate
(`ErrWeakTLSCertificate`).

Peer authentication (`ul.TLSPeerAuthentication`): the default
`TLSRequirePeerCertificate` makes a server require and verify client
certificates against `TrustedCAs` (the mutual authentication the profiles
require servers to support) and makes a client verify the server chain against
`TrustedCAs` or the system roots. `TLSVerifyPeerCertificateIfGiven` verifies an
optional client certificate; `TLSIgnorePeerCertificate` skips chain
verification, as DCMTK `--ignore-peer-cert`. Name verification is optional:
with `ServerName` the server certificate must match it (DNS or IP SAN) and the
name is sent as SNI; without it only the chain is verified, as DCMTK does.

`TLSProfileOptions.Revocation` optionally checks caller-supplied offline CRLs
after ordinary trust verification. Nil or `TLSCRLNone` without material retains
the previous behavior. `TLSCRLLeaf` checks the peer certificate;
`TLSCRLChain` also checks intermediates, excluding the trust anchor except when
it is the peer leaf. Missing, stale, inapplicable or unauthenticated evidence
for a certificate that must be checked fails closed. The restricted profile
accepts complete direct v2 CRLs and
refuses delta, indirect/scoped CRLs and `removeFromCRL`; it does not implement
complete RFC 5280 revocation processing or fetch CRLs/OCSP. See
[offline CRL policy](./TLS_CRL_POLICY.md) for issuer, signature, temporal and
conservative multi-list rules.

Construction admits at most 64 CRLs, 16 MiB aggregate DER and 100,000 entries;
aggregate structural admission precedes full x509 parsing. These are input
bounds, not an RSS guarantee. CRL DER and supplied trust pools are snapshotted.
`VerifyConnection` reverifies ordinary trust and CRLs using one sampled time on
full and resumed connections. `TLSRevocationError` exposes a reason and bounded
positions without certificate names, serials, contents or paths; ordinary
x509 trust errors remain distinguishable.

Synthetic localhost tests establish `DidResume` in TLS 1.2 and 1.3 before
revoked/unknown rejection on both peer callbacks. The separate opt-in
`TestTLSCRLDCMTKLocalInterop` verifies pinned DCMTK 3.7.0/OpenSSL 3.6.4 tools
and exercises C-ECHO in both directions with leaf/chain and valid/revoked
material. This is local interoperability evidence, not a PACS, live PKI or
native Windows GUI qualification.

With DCMTK 3.7.0 and OpenSSL 3.6 (opt-in `DICOMGO_DCMTK_INTEGRATION=1`), a
C-STORE with mutual authentication and certificates from a CA generated at test
time succeeds in both directions for every profile (`storescp`/`storescu +tls`
with `+pg`, `+pm`, `+py`, `+px`, `+pz`), and a dicom-go profile server refuses
DCMTK's `+pn` (NULL) and `+pa` (AES-CBC) profiles.

`storescu`, `storescp`, `echoscu`, `echoscp`, `findscu`,
`dicom-go-retrieve`, `mppsscu`, `stgcmtscu`, `upsscu`, `ianscu` and `qrscp`
enable TLS with `-tls-profile` and take `-cert`, `-key`,
repeatable `-ca` and `-require-peer-cert` (default true; an SCP then needs
`-ca`), plus `-tls-server-name` on requestors. Without `-tls-profile` they
use plain TCP. `dimse.RetrieveDialOptions.TLSConfig` secures C-MOVE and
C-GET associations.

User Identity negotiation can convey username, username/password and
token-oriented identity material, but the APIs do not authenticate those
credentials, authorize operations or write audit logs by themselves.

The requestor tools `storescu`, `echoscu`, `findscu`, `dicom-go-retrieve`,
`mppsscu`, `stgcmtscu`, `upsscu` and `ianscu` propose it with `-user-identity` (`username`, `username-password`,
`kerberos`, `saml` or `jwt`), `-username` and `-user-identity-response`
(positive response requested). The password or token is never a command-line
argument: it is read from `-user-identity-secret-file` (one trailing newline is
ignored) or from the environment variable named by
`-user-identity-secret-env`, and diagnostics never contain it.
`dimse.RetrieveDialOptions.UserIdentity` and
`dimse.CEchoVerificationOptions.UserIdentity` carry it on C-MOVE, C-GET and
C-ECHO verification associations.

The standalone DICOMweb CLI exposes bounded `verify`, `query`, `retrieve`,
`frames`, and `store` client operations. Every operation has explicit service
paths, timeout, response-body limit, and output destination. Basic passwords
and bearer tokens are read only from caller-named environment variables;
credential values are not accepted in command arguments or emitted in
diagnostics. HTTP authentication is rejected unless the caller explicitly sets
`-allow-insecure-auth`; HTTPS remains the production transport expectation.
This is client-side HTTP Authorization header support, not an authentication,
authorization, credential-storage, or audit service.

QIDO result output is not PHI-free and must be handled as clinical data. CLI
diagnostics redact credentials, query strings, identifiers, URLs, and local
paths. WADO output uses bounded part counts and bytes, owner-only temporary
files, atomic publication, and no overwrite. STOW accepts bounded DICOM Part 10
files and distinguishes complete success, warnings, and partial storage. The
normative operational contract and exit-code mapping are documented in
[`DICOMWEB_CLI.md`](./DICOMWEB_CLI.md).

Not supported:

- Server-side authentication or authorization policy.
- An audit record repository (ATNA messages are produced and sent, not
  stored or queried).
- Automatic DIMSE retry policy.
- Application-owned DICOMweb endpoint profiles, credential storage, job-level
  retry policy, job progress or archive import/export orchestration. The
  client itself offers an opt-in per-request `dicomweb.RetryPolicy`
  (exponential backoff with jitter, bodiless requests only), QIDO-RS
  offset/limit pagination that follows Warning 299 truncation notices, and
  an opt-in byte-bounded `dicomweb.ResponseCache`; see
  [`DICOMWEB_CLIENT.md`](DICOMWEB_CLIENT.md).
- Durable queues, scheduler persistence, metrics backend or service-wide retry
  orchestration.

Planned boundaries:

- TLS belongs in the core UL association transport as an opt-in client/server
  configuration. Plain TCP remains the default and never silently upgrades or
  downgrades. Misconfigured TLS handshakes return explicit client/server TLS
  errors. The CLI TLS flags stay separate from authentication and
  authorization policy.
- User Identity negotiation belongs in the UL association negotiation API rather
  than in DIMSE command helpers. SCUs set `ul.DialOptions.UserIdentity`; SCPs
  set `ul.AcceptOptions.UserIdentityHandler` to accept, reject or ignore
  identity items explicitly. Unsupported identity types, missing server policy
  and invalid requests fail predictably instead of creating an authenticated
  session by accident.
- Authentication, authorization, audit logging, credential storage and external
  identity-provider integration are deployment/application policy topics
  documented below.
- `net/audit` exposes structured local events to deployment code without
  importing a logging framework or emitting raw datasets/credentials by default.
  `cmd/echoscp` emits the first C-ECHO events; future service helpers should
  reuse the same event model.
- Reusable DICOMweb clients/helpers belong in this module next to the DIMSE
  network surface. Callers still own base URLs, TLS roots, bearer/basic
  credentials, site policy, retry scheduling, archive ingestion, receiver
  policy, operation history, UI summaries, auto-query and user-facing operation
  state.
- The current CLIs keep their trust and deployment warnings in focused command
  documentation rather than printing the same warning on every invocation.

Limitations/non-goals:

- The network stack is designed for trusted networks only.
- Server-side query enhancement is limited to PS3.4 C.2.2 matching in
  `qrmatch` plus optional fuzzy semantic matching of person names
  (`qrmatch.Options.FuzzyPersonNames` with a `personname/fuzzy` encoder:
  Soundex, Metaphone or Kölner Phonetik, applied to every word of every PN
  component), used when C-FIND negotiated it or QIDO-RS asked for
  `fuzzymatching=true`. PN values with wild cards keep literal matching.
- Role selection and raw SOP Class Extended Negotiation are implemented at the UL
  boundary; of the service-specific semantics C-FIND relational queries,
  combined date-time matching, timezone adjustment and fuzzy semantic
  matching are.
- Authentication, authorization, credential storage, external identity-provider
  integration and durable audit retention remain application/deployment policy.
  Optional audit formatting and syslog transport follow the narrower
  [clinical audit](CLINICAL_AUDIT.md) and legacy transport contracts above.

Use process-level controls, firewalls and trusted test peers when exercising the
network CLIs.

### Authentication, authorization, audit and PHI policy

User Identity negotiation is a wire-level association feature, not a complete
authentication or authorization system. Applications must decide whether
credentials are trusted, which AE titles or users may perform an operation, and
how failed or missing identity information should affect
deployment policy.

Authorization remains outside the core library. Network services should expose
enough metadata for deployment wrappers to make decisions, such as AE titles,
remote address, SOP Class UID, command type and status, but the library does not
own site access-control policy.

Audit event hooks in `v0.1.0` are local observer callbacks, not audit logging by
themselves. `net/audit.Event` carries association and service metadata such as
AE titles, remote address, SOP Class UID, command, status and error class while
avoiding raw datasets and credential material by default. Optional message
formatting, aggregation and syslog transport are documented in
[clinical audit](CLINICAL_AUDIT.md). Explicit PHI/query authorization, repository
trust/integration, durable retention and operational monitoring remain
deployment responsibilities; local observer success is not durable delivery.

Automatic PHI detection, network-traffic de-identification, and implicit
anonymization are explicit non-goals for the networking module. The opt-in
`deid` package is the reusable offline de-identification surface. Fixtures and
tests remain synthetic or scrubbed, and callers must validate the selected
profile/options and residual-risk report before sharing clinical data.

## Known Limitations

- Odd-length element handling is configurable but not a substitute for full
  dataset validation.
- Undefined length support is implemented for sequences/items and encapsulated
  Pixel Data. Defined-length `UN` values are preserved as raw bytes. Undefined-
  length `UN` values are rejected unless a dictionary resolves the element to
  `SQ`; arbitrary undefined-length primitive values are not a general supported
  interchange path.
- Character set handling covers the terms listed in
  `docs/CHARACTER_SETS.md`, including DICOM G0/G1 ISO 2022 designation and
  delimiter resets. Applications should still qualify deployment-specific
  character repertoires and fonts against their intended clinical languages.
- No automatic PHI/OCR detection or implicit network de-identification is
  provided; the explicit offline `deid` package is described in
  [`DEIDENTIFICATION.md`](DEIDENTIFICATION.md).
- SOP Class-specific required-attribute validation is opt-in. `iod.Validate`
  checks a data set against the IOD of its SOP Class with the Module and
  Macro rules generated from PS3.3 2026d (every IOD, Type 1/2 and the 1C/2C,
  conditional Include and conditional Module conditions it can formalize:
  1,253 of 1,997 1C/2C rows and 120 of 328 C Modules,
  `iod.ConditionCoverage`); the rest are reported as `ManualCondition` or
  decided by inference, so it is not a complete conformance check.
  The source-bound CT, MR and SEG rules and their tri-state operand handling
  are enumerated in [Validation](VALIDATION.md#source-bound-ct-mr-and-seg-conditions).
  `object.ValidateSOPClass`, caller-provided hooks and the narrow
  `object.RequiredAttributeRule` helper remain for application rules.
- Uniform VR/VM, dictionary, File Meta and lifecycle validation is opt-in;
  `iod.Options.CheckValues` additionally checks scoped PS3.3 Enumerated Values,
  extensible Defined Terms and the proven contextual Dimension Index Values
  count rule. `dcmvalidate -values` enables the same checks. The source is the
  pinned 2026d DocBook; unproved prose, predicates and references remain manual,
  and default IOD presence behavior is unchanged. Dictionary VR/VM stays in
  structural validation, where SQ VM is independent of Item count. Built-in structural
  and SR validation diagnostics omit dataset values; application hooks and
  custom rules are trusted code with direct access to supplied datasets. See
  [`VALIDATION.md`](VALIDATION.md).
- Deferred (lazy, seek-and-replay) value access covers defined-length
  primitive values and undefined-length encapsulated Pixel Data only. General
  `SQ` replay is an intentional non-goal: sequence and item values are always
  materialized in memory while reading. Lazily replaying nested sequences
  would require tracking item boundaries through arbitrary explicit- and
  undefined-length nesting and re-deriving transfer-syntax-specific framing on
  each replay, for a benefit limited to reducing peak memory on datasets with
  very large nested `SQ` metadata (e.g. bulky private sequences). Given that
  narrow payoff against the complexity and round-trip risk of a lazy `SQ`
  reader, this project keeps sequence materialization as the permanent
  behavior. Applications with memory-constrained nested-sequence
  workloads should read the affected sequence eagerly and discard it after
  use, or filter it out via a caller-provided element predicate.
- The `dsa` package implements the DICOM Mask Module (PS3.3 C.7.6.10) for
  XA/XRF and Enhanced XA/XRF. `ReadModule` parses the Mask Subtraction
  Sequence (0028,6100) with Subtraction Item ID, TID Offset (a zero-length
  value defaults to 1) and Mask Selection Mode, Recommended Viewing Mode,
  Number of Frames and the shared or per-frame Frame Pixel Shift functional
  group (0028,9415). `Module.Plan(frame, opts)` returns the frames averaged
  into the mask, the Contrast Frame Averaging window (the frame and the
  following ones) and the mask shift:
  - `AVG_SUB` averages all Mask Frame Numbers into one mask; they are not
    paired one-to-one with contrast frames. Without an Applicable Frame
    Range the item covers frames 1 to N - averaging + 1.
  - `TID` uses frame f - TID Offset as the mask; without a range it covers
    the frames whose mask exists.
  - `REV_TID` (range required) subtracts the TID Offset from the first
    contrast frame and decrements it by one per frame of the range.
  - Under TID and REV_TID the mask is averaged over the same number of
    frames as the contrast image (a time-interval difference of
    frame-averaged images, as ITK's FrameAverageVideoFilter followed by
    FrameDifferenceVideoFilter).
  - The Frame Pixel Shift entry whose Subtraction Item ID matches the item
    prevails over the module's Mask Sub-pixel Shift; with Contrast Frame
    Averaging the current (first) contrast frame's entry is used.
  - `Shift` keeps C.7.6.10.1.2's convention: a positive row offset moves
    the mask down and a positive column offset moves it left.
    `Shift.Displacement` converts to image x/y, `ApplyShift` shifts a
    modality frame bilinearly (edge samples repeated) and `AverageFrames`
    averages modality frames.
  - Mask motion beyond a shift, which the Mask Module cannot encode, is
    estimated for display: `RegisterRigid` (rotation and translation by the
    gradient difference similarity) and `MatchBlocks` (a local displacement
    field by block matching), applied with `ApplyRigid` and `ApplyField`.
  - Inconsistent inputs (odd, reversed or out-of-object ranges, missing
    Mask Frame Numbers or TID Offset, mask frames outside the object,
    averaging past the last frame, invalid shifts, unlinkable or conflicting
    functional groups, unknown operations) are typed errors.
    `PlanOptions.Recover` opts into the recovery documented on each error
    and lists it in `Plan.Recoveries`.
  - Mask Visibility Percentage (0028,9478) is not read; mask landmarking is
    left to presentation-state processing.

## Progressive stored-volume loading

Complete non-progressive packed16/float32 preparation fills disjoint slices
with the module's shared bounded helpers and joins them before canonical
publication. Progressive slice/band ordering remains unchanged. The ordered
`object.OpenFilesContext` batch API preserves read options and closes every
opened source on batch failure/cancellation; mutable streaming sinks are refused.
See [parallel loading](PARALLEL_VOLUME_LOADING.md) for ownership, cancellation
limits, byte equivalence and the local 48-file VTK comparison.

`render.Volume.PrepareSnapshotProgressiveWithOptionsContext` accepts
`VolumeDecodeOptions{SkipPreview:true}` to omit the derived averaged preview
before it is computed. It still decodes and reports original full-resolution
bands and installs the same canonical generation. The existing API and zero
options preserve the library's headless CPU preview. This option does not
disable regular-grid interpolation for irregular library volumes.

`VolumeDecodeProgress.Band` is borrowed for the decoding callback. Calling
`Retain` inside that callback creates a read-only `RetainedVolumeBandReader`
with captured dimensions, descriptor and backing-slice headers. No sample array
is copied, reduced or resampled by retention. The completed band is write-once;
retry uses another allocation, and source close does not overwrite retained
samples. Retaining any band retains that attempt's whole decode allocation
until all readers are dropped; bands from the same attempt share it. This is
an ownership contract, not a process-memory or GPU-residency bound.


## Architecture Notes

The optional [OpenJPEG lossless encoder](../examples/codec-adapters/jpeg2000/ENCODER_PROFILE.md)
registers only JPEG 2000 Part 1 `.90`, with explicit runtime preflight and a
bounded subprocess. The qualified encode subset is unsigned MONOCHROME1/2 or
RGB, 8..16 stored bits, on Windows/amd64 and Linux/amd64. The existing transcode
pipeline owns encapsulation and metadata. Independent native FFmpeg verification
checks every sample. The dependency-free
[pure-Go JPEG 2000 and HTJ2K encoder](JPEG2000_ENCODER.md) in
`pixeldata/jpeg2000` covers `.90`, `.91` and `.201`-`.203` without that runtime.

The [codify generator](CODIFY.md) emits bounded, compiled Go reproductions using
public object/core APIs. Structural substitutions are the default; faithful
values and binary inclusion require explicit policy. It is not a DICOM
serializer or an automatic anonymization/byte-exact reconstruction facility.

The [reference archive example](../examples/referencearchive/README.md) composes
safe Part 10 publication, detached metadata indexing, `qrmatch` and existing
DIMSE services. It supports the documented finite, native-syntax SC/CT/MR Study
Root subset, restart validation and explicit C-MOVE destinations. Storage
Commitment is opt-in with a durable transaction directory and callback routes
(see the example README); DICOMweb is out of scope. Its opt-in independent pynetdicom/pydicom
profile checks store/find/get/move, cancellation, all fixture pixel samples and
restart; this does not qualify a clinical archive or arbitrary peer/IOD.

C-GET and C-MOVE request parsing accepts every Command Data Set Type value
except 0101H as indicating an identifier, as specified by PS3.7 Tables 9.3-6 and
9.3-9. Builders retain their existing 0000H encoding. Regression tests include
the 0001H encoding used by the independent pynetdicom peer.

The module uses concrete Go structs, small interfaces and explicit registries.
The release documents only the implemented subset.

## Association configuration profiles

`ul.ParseAssociationConfig` and `ul.ReadAssociationConfigFile` loads the documented DCMTK-format transfer-syntax,
presentation-context and profile groups, including SCU/SCP roles and opaque
extended negotiation. Profiles are detached and compiled under finite
logical limits; acceptor indices are admitted before map allocation. Numeric
private UIDs remain supported. The parser intentionally rejects ambiguous,
unresolved or malformed entries. `storescp` and `storescu` require both
`-config-file` and `-profile`, load before bind/dial, and reject services,
roles and claims their adapters do not implement. A profile does not enable
implicit transcoding. Explicit StorePlan contexts must have canonical odd IDs;
nil keeps automatic planning. Original order and repeated SOP offers persist.

Unchanged official DCMTK profiles were compared through public APIs: requestor
128 contexts and acceptor 215/183 rules. Local real associations and C-STORE
RLE in both directions qualify the recorded synthetic cases. Seven usable
role intersections agree; two disjoint-role cases differ (Go context rejection,
DCMTK accepted context with NONE roles). Unsolicited extended replies are
omitted by the Go handler and refused by the Go requestor. These differences
are documented rather than represented as full DCMTK parity. Complete generic
profiles may be refused by the more restrictive Storage CLIs. No live-PACS or
clinical qualification is claimed. See [configuration scope and evidence](DCMTK_ASSOCIATION_CONFIG.md).

## Declarative network configuration

`net/netconf` provides an opt-in versioned JSON subset of Device, Connections,
Application Entities and local-role Transfer Capabilities. It validates unknown
fields, duplicate keys, references, installed inheritance, service/role catalogs
and finite JSON, nesting, scalar, entry and logical compiled-backing limits.
`CompileConnection` freezes the selected active acceptor connection; selection
is automatic only for a document declaring one Connection. It does not
implement LDAP, discovery, credentials or a service merely named by its UID.

`ul.CalledAERegistry` selects an immutable catalog after validated Called AE
parsing and before admission/negotiation. Titles compare case-sensitively after
DICOM padding normalization. Unknown identities receive Called AE rejection;
SOP, ordered transfer syntax and requestor role policy belong to the selected
AE. Conflicting global catalogs refuse construction. Nil retains legacy
single-AE behavior and the existing lifecycle, identity/TLS policy and limits.

`storescp -net-config` loads and validates the selected catalog and real TLS
transport before directory creation/bind. It refuses contradictory profile,
AE/address and syntax flags, unsupported services or local-SCU claims. Twin
uses the same compiler in its session and headless receivers while preserving
archive, service-toggle, calling/host/User Identity and transport constraints.
Received File Meta Source/Receiving AE, FIND Retrieve AE Title and outgoing
MOVE Calling AE retain the selected JSON identity, including its case.
Legacy send normalization remains the default; C-MOVE terminal accounting
continues to use correlated response/interruption metadata independently.

Qualification covers bounded synthetic loopback associations, separate AE
catalogs, services/roles/syntax refusals, selected identities, mixed-case MOVE,
cancellation, backend lifecycle, complete affected suites, race and vet.
It does not establish LDAP/full PS3.15 Device conformance, arbitrary PACS
interoperability or clinical validation. See [schema, defaults and limits](NETWORK_CONFIGURATION.md).

## Thin command profiles and structural HL7 XML

The twenty-two [small command tools](CLI_WRAPPERS.md) reuse the library's
MWL/MPPS/IAN, encapsulated document, SR/KOS, presentation, Basic Print and
other existing primitives. They provide finite command/file/network lifecycle
profiles rather than a second protocol implementation. `dcmftest` checks only
the Part10 marker; successful `termscu` negotiation does not establish remote
shutdown. Print and normalized-service smoke checks use synthetic loopback
peers; external service acceptance is separate.

`PolicyDirectoryModalityWorklist` adds opt-in complete-core filtering,
cooperative shared OS file locks and bounded raw Explicit LE query recording.
Completeness is the explicitly documented core profile; optional complex
profiles are refused. Request/IAN output quotas begin at a directory snapshot
and track this single publisher's lifetime, without accounting for concurrent
external mutations or claiming a process RSS limit. Windows lock code is
cross-built, while native Windows execution is outside this qualification.

The [HL7 tools](HL7_CLI.md) expose ER7/MLLP and the library's bounded structural
version-1 XML representation. This is not HL7 XML ITS. XML conversion reuses
ER7 models, escaping and charset rules; optional transport TLS and the
application's durable workflow remain distinct.

## Bounded image and video import

`encapimage.FromImages` and `img2dcm` support the finite [IMG2DCM profile](IMG2DCM.md):
opaque PNG/BMP/PNM/PGX or explicitly described raw native samples; compatible
single-/multi-frame SC or one 8-bit VL Photographic image; supported JPEG/JLS/J2K
encapsulation; and admitted AVC MP4/MPEG-2 PS Video Photographic containers.
The legacy `FromJPEG` contract remains unchanged. Native aggregate defined
Pixel Data length, padding and host-int limits are checked before allocation.

J2K lossless import requires the producer's explicit declaration plus the
reversible header constraints; parsing those headers does not establish
absence of prior truncation or qualify every entropy stream. Video preserves
the accepted container as one fragment with empty BOT, requires explicit
anatomic coding and uses actual encoded frame/timing metadata. Encapsulation
does not provide video playback or decoding in Twin. Source series/study
inheritance creates a new instance identity and atomic output refuses aliases
of input/source files. The guide records coding, cancellation, resource and
external-fixture limits.

## Scalar slab row traversal

Cached orthogonal and affine oblique thick scalar slabs compose rows with the
sample loop outside pixels. Coordinates, increasing sample order, endpoint
weights, validity and arithmetic remain unchanged; 7,560 finite synthetic
scalar/frame comparisons were bit-identical to the preceding loop. Non-affine,
geometry-free and separable-cubic paths retain their reference behavior.

A controlled 14-case warm-kernel campaign observed 12.25–28.85% lower median
time for the linear cases, with increased B/op and allocations. The fine
oblique cubic case showed overlapping/outlying measurements, so no robust
speedup is claimed for it. Separate test-only instrumentation observed up to
8,192 bytes of new row-buffer payload per request and 12,288 bytes across two
concurrent requests with three admitted workers; all owners and leases joined.
These are new-buffer capacities, not total scratch, allocator or process RSS.
See [slab row contracts and all measurements](MPR_SLAB_ROWS.md). No Twin caller,
GPU policy, application latency or native Windows qualification is implied.

## MOVE receiver and FIND command profiles

The MOVE/retrieve runner can explicitly bind a native-syntax Storage SCP before
its outgoing request. AE/SOP/UID checks, atomic Part 10 persistence and parser
limits reuse existing library services; receiver shutdown stops acceptance and
joins storage work before outgoing release. A blocked filesystem call can delay
joining, so drain deadlines are not a hard filesystem wall-time guarantee.

Retrieve and FIND admit bounded free keys and regular raw/Part 10/JSON query
files. MWL uses the existing neutral C-FIND Identifier path; count cancellation
drains to terminal, and extraction publishes bounded raw Explicit LE Identifiers
without invented SOP identity or File Meta. Query/extract counts, bytes, depth
and item budgets are finite; all integer/float/double Pixel Data and unresolved
bulk are refused as query keys. Software-peer checks against pinned local
DCMTK exercise MOVE storage, MWL, cancellation and extracted readback with
synthetic records; they are not target-PACS or complete DCMTK CLI parity proof.
See [MOVE/FIND details and qualified limits](CLI_MOVE_FIND.md).

## Writable QR software archive

`qrscp -write-root` is an explicit bounded local software archive profile.
It exclusively locks its storage, validates regular files and identities,
persists Part 10 bytes and indexed metadata, then publishes the catalog before
returning C-STORE success. Duplicate UIDs are refused; replay verifies recorded
metadata and SHA-256. Canonical orphan objects require explicit repair.
Failures after publication retain evidence and make the archive unavailable
until recovery rather than claiming that durable bytes were rolled back.

Optional Storage Commitment uses the existing transaction workflow and a
bounded durable CAS store, with allowlisted callback routing and explicit
retry/recovery. Same-association delivery requires a dedicated association with
exactly one accepted commitment context. Local sync/rename and process-kill
recovery checks do not prove hardware power-loss, network-filesystem durability,
clinical retention or replication. Windows storage has file sync and atomic
publication with cross-build qualification; no Windows directory-flush or
native execution claim is made. See [writable profile](qrscp-writable.md) and
[transaction store](STORAGE_COMMITMENT_FILE_STORE.md).

## Structure-bound SR context

Opt-in immutable metadata snapshots bind to one unchanged document with bounded
tree/content fingerprints and backing-storage checks. A stale snapshot is
rejected before validation or output; unsupported values and ambiguous identical
subtrees carrying different metadata fail closed. Header/lifecycle fields that
do not change item association retain their documented mutability. Rebinding
is explicit and requires already-correct caller metadata; it does not invent
stable identities or recover historical associations.

Context-aware text/HTML rendering presents the actual short, Long or URN code
and available scheme/version for title, concept, CODE value, units and numeric
qualifiers. URLs/URNs remain inert escaped data. Legacy no-context APIs retain
their behavior. The snapshot guard covers native FL/FD bit patterns, including
NaN payload changes; byte/item/depth limits are logical input bounds, not process
RSS guarantees. Full SR fixture checks and a pinned DCMTK presentation comparison
qualify this surface; DCMTK's refusal of scheme-free URNs is preserved as an oracle
limitation, while own fixtures cover the supported scheme-free form. See
[snapshot and rendering contracts](SR_CONTEXT_RENDERING.md).

### Physical and subpixel polylines

`roi.SimplifyPhysicalPolyline` reduces independent physical 3D piecewise-linear
paths; `SimplifyPixelPolyline` requires an explicit materialized float64 affine
plane. Original points/indices, open endpoints, protected duplicate samples and
implicit/explicit loop closure are preserved. Every chord is admitted by exact
rational finite-segment distance at the physical tolerance. Default simple
profile checks original/output exact segment intersections and refuses, without
claiming isotopy, knots, graphs, area or a later spline/CPR error bound. An explicit
unchecked profile reports its different contract. Points/work/structural byte
limits and cancellation return zero results on refusal.

The local synthetic 256/257-point campaign has four repeats, all 12 observations
per case/profile retained; exact arithmetic incurs significant cumulative
allocation/time costs. No speedup, RSS/memory saving or consumer/default adoption
is claimed. Owned output backing capacity is admitted conservatively against
input size; point reduction alone does not claim backing-memory reduction. See
[complete contracts, tests and measurements](./PHYSICAL_POLYLINE_SIMPLIFICATION.md).
Twin CPU geometric preparation does not authorize CPU 3D rendering or fallback.

## Physical Image Box presentation

Opt-in Image Box presentation carries Requested Image Size as width in mm,
Requested Decimate/Crop Behavior and explicit pixel aspect ratio. Resident
DS/CS/IS length, VR, VM, numeric grammar and bounds are checked before trimming;
the legal final whole-field padding of VM2 IS is handled explicitly. Opaque/nil
values and unsupported representations are refused.

`PhysicalServiceOptions` requires an explicit `PhysicalBackend` commitment to
honor the request and computed plan. Printable area, layout gaps and native pitch
are configured rather than inferred from film names. Known oversized FAIL or
DECIMATE with NONE magnification returns C603; B609/B60A acknowledge a deferred
crop/reduction plan without proving physical printed scale. Refusals preserve the
prior session; film updates replan all boxes atomically. Strict SCU handling of
ignored optional attributes stops before N-ACTION. Zero configuration retains
the existing 0107 warning profile.

Twin printer profiles distinguish known support, unknown and unsupported.
Both frontends share physical choices and a bounded preview using explicit
geometry. Software fixtures and independent peers qualify request/state/status
handling; no physical printer scale or native Windows execution was measured.
See [print contracts and peer limits](./PRINT_MANAGEMENT.md).

## Preserved SR numeric representations

SR numeric representation preservation: opt-in `ReadDocumentWithNumericRepresentations` and `BindNumericRepresentations` retain original DS bytes, finite FD bits and paired signed/unsigned fraction fields through the existing structural metadata snapshot. Source precedence FD > rational > DS and exactness provenance are explicit. Finite/coherence refusals preserve the caller Object and are distinct from generic DICOM validity; half-DS-quantum coherence is profile policy. Stale values/units/structure refuse output, with explicit rebind required. Full focused SR native/race/minGo122-external suites pass; DCMTK attribute fixture confirms FD/fraction and decimal spelling while its XML path normalizes outer DS spaces. No full IOD/PACS/clinical or performance claim. See [SR_NUMERIC_REPRESENTATIONS.md](./SR_NUMERIC_REPRESENTATIONS.md).

## Viewport reslice geometry

The reusable viewport affine planner admits finite logical/backing dimensions,
zoom, pan and rotation under 8192-pixel axis and 256 MiB output limits. It retains
patient-space field of view, pixel centres and stable source-grid coordinates;
planning performs no volume sampling or image composition. Twin consumes this
plan for GPU scalar reslicing before window/VOI presentation. Measured panes
default to viewport backing dimensions, while stored Balanced/High/Native/Custom
output overrides remain exact. Linear is the default; explicit Cubic uses the
existing GPU scalar kernel and fused Cubic reports a GPU limitation. Invalid exact
requests preserve the last accepted GPU frame. Accepted pixels retain their own
ROI/probe/export plan; reference axes track the current input while rendering is
pending. Every interaction and settled application 3D frame remains GPU-only.
Functional fixtures do not establish device throughput or visual acceptance;
hardware throughput and visual acceptance require separate evaluation.


## Explicit MLLP2 transport confirmations

`net/mllp` defaults to MLLP1. Explicit `MLLP2` selects the profile qualified
against dcm4che commit `acbbb85eee1a2fcc441eb2d8e65023fece2b40ec`:
`<VT><ACK 0x06 or NAK 0x15><FS><CR>` confirms a complete bounded frame, while
the following HL7 application response separately determines AA/AE/AR.
`TransportACKTimeout` bounds each confirmation, additionally capped by the
client's whole-exchange `ResponseTimeout` and context, and by the server's
write timeout for confirmation writes. Transport failures close and poison the
client connection; `ExchangeError` exposes phase and request transport knowledge
(not sent, unknown, acknowledged or rejected), with underlying causes available
through `errors.Is/As`. No automatic downgrade, resynchronization or replay is
performed. Transport ACK precedes application handling and establishes neither
durable publication nor a HIS commit. Endpoint selection and idempotent recovery
remain consuming application policy. See [HL7 v2 and MLLP](HL7_MLLP.md).

`hl7.Message.MarshalWithLimit` shares canonical ER7 escaping and charset rules
with the existing encoder and emits incrementally to a capped encoded-output
sink, including stateful charset flush. Positive output limits are required;
`hl7.ErrLimit` returns nil output. MLLP2 uses it for authored client messages and
handler responses before publication. Output allocation is bounded relative to
the ceiling and fixed transformer buffers; caller-owned input, traversal of
empty slices and RSS are separate. Existing `Marshal` and MLLP1 outgoing behavior
remain. Invalid writer progress cannot establish “not sent”; MLLP2 retains an
unknown request result and closes without replay.


## Native print LUT and annotations

Presentation LUT N-CREATE/N-DELETE and Basic Annotation Box N-SET are opt-in,
separately negotiated native objects. An explicit per-object backend commitment
is required; capture by MemoryBackend does not prove rendering or printing.
The finite grayscale LUT subset admits exclusive IDENTITY/LIN OD or aligned
256/4096-entry tables with 10–16-bit output. Film Box and Image Box references
are admitted, with Image Box precedence; Film Session references remain outside
this subset. Annotation Position and LO text are admitted separately under
finite count/byte/character limits, with ASCII or ISO_IR 192 only. Required
native requests stop before N-ACTION on ignored/rejected attributes. Native
objects and the physical presentation profile retain independent commitments;
unknown print outcome is never retried. Qualified Python/DCMTK software peers
exercise both directions within their declared service subsets; they establish
neither hardcopy output, optical calibration nor clinical acceptance.
See [native print scope and limits](PRINT_MANAGEMENT.md).


## Vector confidence connected growing

`roi.GrowVectorConfidenceConnected` consumes bounded scalar callbacks with explicit
channel IDs/units/normalization and identical physical grids/frame identity.
Unavailable/nonfinite encountered channels, unusable geometry, failed covariance
admission, resource exhaustion and cancellation return nil, without a published
partial mask. No dense vector-channel volume or inferred regularization is used;
binary visitation is dense. Defaults are unbiased sample covariance (`n-1`),
strict positive-definite Cholesky pivot admission and seed-expanded ellipsoid
radius. Population covariance (`n`) and normalized ridge are explicit choices.
The finite pivot profile is not a full condition-number guarantee and may admit
different channel orders differently near its boundary.

Synthetic centered-sum/direct-inverse and dense connectivity fixtures qualify
default sample behavior and explicit population/ridge profiles. The pinned public
ITK5.4.5 oracle qualifies exactly eight regular fixtures: one/two channels,
2D/3D and zero/two refits, with population selected, exact masks and absolute
mean/covariance tolerance 1e-12. Initial comparison with sample normalization failed
at the expected denominator difference and is preserved as diagnostic evidence;
it is not counted as successful parity. Arbitrary channels, singular/ridge,
registration accuracy, callback completeness outside encountered coordinates,
clinical suitability and Twin workflows are outside this external oracle claim.
See [vector confidence growing](./VECTOR_CONFIDENCE_GROWING.md).


## Detailed SR measurement context

The finite specialized measurement-context profile admits exact context roles,
value types and relationships, and three segmentation reference classes by
standard UID. Individual TID 1411 NUM items use an explicit INFERRED FROM RWVM
extension with a bounded finding: PS3.16 TID 1419 row 19 prescribes CONTAINS,
which conflicts with the PS3.3 SR relationship constraints. This profile does
not claim satisfaction of that row and does not alter the generic or catalog
rules. Unmodeled admitted tree content is retained and reported; unsupported
nested reference-macro optionals are refused with the source intact. Opaque raw
values remain bytes, and canonical output from big endian opaque input is
refused; semantic endian normalization is not promised. Local native, race,
minimum Go 1.22, vet and documentation checks, and bounded DCMTK XML
preservation, are fixture evidence, without clinical, PACS, whole-IOD or
performance-gain qualification. See the existing normative seam and reference
links in [measurement context and boundaries](SR_MEASUREMENT_CONTEXT.md).

## Incomplete Pixel Data inspection

`AllowIncompletePixelData` is a separate malformed-input inspection profile:
final root native bytes/complete encapsulated Items at physical EOF return with
an explicit partial error and historical counts/offsets. `DecodeRecovery.IncompleteData`
separately qualifies only complete byte-aligned native/JPEG/JPEG-LS frames, under
original metadata/input/expansion quotas, and updates Number of Frames in a new dataset structure with owned native Pixel Data
bytes; other materialized values retain ordinary borrowed backing. It never fills missing samples/entropy/EOI or invents derived identity.
Strict consumers and writers refuse diagnosed input; `AllowPartial` cannot turn
an odd incomplete tail into a completed sample through padding. Generic partial-SQ
retention/rewrite keeps its contract. Unsupported packing, syntax, framing/table
conflicts and count-dependent metadata fail explicitly. The two available GDCM
compressed fixtures match pinned payload MD5s with a separately selected existing
File Meta tolerance; three original native fixture acceptance items remain
unqualified. No default viewer adoption, clinical/PACS or performance claim is made.
See [complete-frame inspection and limits](INCOMPLETE_PIXEL_DATA.md).


## Masked physical rank filters

`imagefilter.MaskedRank` and `MaskedRankRegion` select the exact binary32 sample
at `floor(rank*(n-1))` from eligible regular-grid neighbors. Rank zero means
minimum; rank one means maximum; rank .5 chooses the lower median. Physical
box/ellipsoid membership uses the declared binary64 predicates without an
epsilon or silent kernel shrink. Clip and replicate border policies preserve
mask eligibility and replicated multiplicity. Full-source regional halos,
finite center/nonfinite policies, quotas, owned output and joined cancellation
are explicit. Empty windows remain invalid; excluded samples are not imputed.
Legacy Median, slab and shared worker primitives remain unchanged. A pinned
ITK 5.4.5 corpus qualifies 72 clipped cases and 24 explicitly padded replication
adapter cases, with all 12096 value-bit/validity entries exact; it does not assert
native ITK replication or general clinical equivalence. The single fixed local cost campaign
ran 144 rows across 16 profiles, three strategies and three repetitions with
five calls per row. All eight integer fixture medians were faster than forced
selection; their time-ratio geometric mean was 0.441896. All eight fractional
profiles are disclosed, including 0.34–14.95% overhead versus forced selection.
Shared-host archive I/O overlapped. These descriptive fixture measurements
establish neither statistical significance, universal speedup, process-memory
reduction nor clinical benefit.
See [masked rank semantics, resource ledger and measured scope](MASKED_RANK_FILTERS.md).

## WSI tile geometry

`microscopy.Read` admits implicit matrix placement for `TILED_FULL`
only. An unpartitioned volume must supply every position implied by the
matrix, tile size, encoded focal planes and optical-path sequence. A
concatenation member supplies a validated range of global frame ordinals.
The global path order stays intact when a shared path describes only that
member. Explicit tile positions on a full volume must agree with that
order. Single-frame label, thumbnail and overview images can have a
smaller extent than the total matrix.

For `TILED_SPARSE` and absent organization, each frame supplies its own
matrix position and the reader preserves encoded overlaps and order.
Focal plane positions are exposed in millimeters. Encoded full volumes use
Total Pixel Matrix Focal Planes and Spacing Between Slices in Pixel Measures,
while sparse volumes use the actual per-frame Z coordinates. Slide Z offsets
are converted from micrometers. Acquisition focus-stack count and distance
do not contribute to encoded plane positions.

`microscopy.Pyramid.Add` combines compatible full concatenation members
as one lazy resolution, including when they arrive out of order. An
incomplete group yields tiles only from present frame spans.
`ValidateConcatenations` verifies complete ordered coverage. Every tile
retains its original SOP and one-based local frame. [PS3.3 C.7.6.17.3](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.7.6.17.3.html) · [Pixel Measures](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.7.6.16.2.html#sect_C.7.6.16.2.1) · [Slide coordinates](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.12.14.html)

## WSI optical-path ICC

Optical Path Sequence profiles are resolved by frame/shared path identification
and TILED_FULL ordering, including non-numeric identifiers and concatenation
offsets. Root Image Pixel Description Macro ICC is never substituted. Identical
profile contents reuse a bounded ICC transform LRU. Missing, invalid or
unsupported RGB profiles retain stored colours with typed diagnostics; missing
or unknown path references and admission refusal prevent tile publication.
Profiles are limited to 1 MiB each, 8 MiB per instance and 32 MiB per resolver,
64 paths per instance, 4096 source instances and 4096 focal planes. The default
LRU admits 32 entries and 128 MiB conservative logical weight, not process heap.
Only existing RGB ICC transformations are supported; palette construction and
monitor profiling are outside this scope.

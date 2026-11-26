# RT family models and bounded template writers

`rtplan` supplies owned, reusable DICOM models. Twin consumes those models for
read-only inspection. The admitted profiles below use PS3.3/PS3.5; they do not
calculate dose, infer a source trajectory, verify delivery, or establish clinical
correctness. Common and optional modules outside the typed coverage remain
attributes of an explicit writer template.

The conventional reader also preserves typed block contours and exposes
per-control-point device geometry for the shared 2D aperture inspector. See
[Conventional RT Plan aperture inspection](RT_PLAN_APERTURE.md) for coordinates,
declared/inherited state, validation and authoring boundaries.

## API and SOP coverage

Use `ReadWithOptions(ctx, file, ReadOptions)` or
`ReadDatasetWithOptions(ctx, dataset, ReadOptions)` for plans,
`ReadTreatmentRecord`/`ReadTreatmentRecordDataset` for records, and
`ReadImage`/`ReadImageDataset` for RT Image. Legacy `Read`/`ReadDataset` retain
the conventional Plan return type. `Read` now admits the finite Ion profile and
still refuses records. `StrictIOD: true` runs generic VR/VM and
`validation/iod.Validate(CheckValues:true)` as well as the profile checks.

SOP suffixes below follow `1.2.840.10008.5.1.4.1.1`.

| SOP | Typed profile and round-trip properties | Explicit boundary |
| --- | --- | --- |
| `.481.5` RT Plan | Conventional photon template writing for STATIC fields and DYNAMIC gantry arcs with legacy jaws/MLC, fraction groups and retained machine references. Detailed temporary brachy HDR/MDR/LDR/PDR/MANUAL supports sources, setups, channels, CP weights/relative positions and optional patient positions/orientations. | Photon writing has the bounded profile below. Detailed brachy writing retains its existing profile. |
| `.481.8` RT Ion Plan | PROTON or single-species ION, NONE/MODULATED scanning. Species, beam identity, energy, VSAD, legacy devices, snout, range shifters/settings, inherited machine state, CP meterset, exact FL spots/weights/paintings, fraction/setup/tolerance/dose references. | MIXED_ION, UNIFORM/MODULATED_SPEC, enhanced devices and nonzero wedges, blocks, compensators, boli, lateral-spreading devices or range modulators are refused. No spot trajectory or aperture inference. |
| `.481.4` RT Beams Treatment Record | Session identification/status; specified/delivered meterset and time; sparse CP machine state, dates/times, legacy devices and references. | DEVICE/SIMULATION or absent content origin; USER salvage and accessory/correction/override structures are refused. |
| `.481.9` RT Ion Beams Treatment Record | Same origin policy; PROTON/ION, NONE/MODULATED, patient support type, recorded range shifters, delivered spots and machine state. | Same finite Ion/device restrictions. No patient Frame of Reference UID is invented. |
| `.481.6` RT Brachy Treatment Record | Recorded sources, setups/channels, source applicators, specified/delivered time, safe-position timestamps, delivered CPs, optional PDR pulse detail and references. | Same temporary implant/movement profile. Delivery can be interrupted, partial or resumed. |
| `.481.7` RT Treatment Summary Record | Declared treatment status/dates, fraction groups/statuses, measured/calculated dose values and reference lists. | Values are retained declarations, not delivery verification or dose calculations. |
| `.481.1` classic RT Image | Single native frame, unsigned MONOCHROME1/2, 8/16 allocated bits; DRR/PORTAL, NORMAL/NON_NORMAL, declared receptor geometry, plan/beam/fraction references, absent or one exposure with typed KVP/meterset and owned pixels. | Compression, multi-frame, enhanced devices, multiple exposures and advanced exposure accessories are refused. No projection-to-patient transform is synthesized. |

Brachy admits INTRACAVITARY, INTERSTITIAL, INTRALUMENARY and CONTACT techniques
and STEPWISE/FIXED movement. PERMANENT, INTRAVASCULAR, OSCILLATING and
UNIDIRECTIONAL are outside this profile. Sources have POINT/LINE/CYLINDER/SPHERE
type, isotope/half-life, reference date/time and air-kerma rate; DOSE_RATE_WATER
additionally requires Source Strength. One treatment-machine item with a
present Type 2 name is required. Its additional equipment attributes remain
in the template. Conditional source applicator/shield/accessory details outside
the table's typed properties remain template data; they are not a complete
clinical geometry model.

`Attribute[T]` distinguishes absent (`Present:false`), present empty
(`Present:true, Value:nil`) and a value (`Value(v)`). Optional new-family vectors
retain empty presence. Existing conventional pointer fields retain their older
API; the writer preserves their empty presence from the matching template item.
CP encoded flags distinguish local attributes from resolved inherited state.
Returned numeric slices and scalar pointers are independent of source buffers
and of another CP's resolved state.

## Conventional photon writing

`WritePlan` accepts a conventional RT Plan template and an owned `Plan` with
PHOTON beams. STATIC beams retain one aperture and machine configuration.
DYNAMIC beams describe gantry arcs with a fixed couch, isocentre, collimator
angle and nominal energy. Meterset and dose rate can vary along an arc, as can
the X/Y, ASYMX/ASYMY and MLCX/MLCY apertures. All control points carry their
effective inherited values in the model. The first meterset weight is zero,
weights are nondecreasing, and the final value agrees with the beam declaration.
Gantry movement needs the preceding segment's CW or CC rotation direction.
Symmetric jaw positions are checked.

The writer emits all varying gantry, rotation and dose-rate fields in every
control point. A beam-limiting device that changes anywhere in a beam is
encoded in every control point. A stationary device is encoded at the first
point. These rules preserve the numerical state of sparse source control
points while satisfying PS3.3's control-point presence rules. First-point
Type 2 couch coordinates and isocentre retain empty presence when unavailable.
No interpolation between points is supplied.

Fraction groups, beam numbers, CP indices, dose references, setup and tolerance
references are checked against their targets. Fraction and device ordering
is retained, and optional template attributes follow their keyed beam, CP,
device or reference identity. A source identity, private extension or machine
attribute is not moved to a different beam just because the arrays are reordered.
An edited/new instance with a prior APPROVED or REJECTED state is refused.
The caller can declare the new instance UNAPPROVED or omit Approval Status,
in which case prior review metadata is removed.

The current writer refuses mixed photon/Ion/brachy content, non-photon beams,
wedges, blocks, compensators, boli, applicators, general accessories, enhanced
beam-limiting-device definitions, specialized primary-fluence declarations,
gantry pitch, moving/tilted couches, rotating collimators, and variable nominal
energy. STATIC step-and-shoot sequences with changing apertures and non-arc
DYNAMIC beams need a separate delivery profile. These structures continue to
use the broader inspection reader where supported. All common template,
resource, syntax, signature and new-instance identity rules below apply.

The synthetic two-beam fixture contains a STATIC jaw/MLC field and a DYNAMIC
gantry arc with inherited jaw and final MLC positions. On Windows amd64,
DCMTK 3.7.0 `dcmdump`/`drtdump` and dicom3tools
`dciodvfy` 1.00.snapshot.20260927113651 read the source and the three native
syntax outputs without warnings or errors. Removing Beam Number produces a
DCMTK warning and a dicom3tools error. The tests also cover keyed reordering,
changed-instance identity, invalid references, unsupported modifiers, resource
limits, cancellation, source immutability and failure after partial
serialization through the existing atomic file writer.

Reproduce the fixture generation from this module with
`DICOMGO_RT_FAMILY_FIXTURE_DIR` set to an absolute destination and run
`go test ./rtplan -run '^TestExportPhotonPlanQualification$' -count=1`.
Read each resulting `photon-plan*.dcm` with `drtdump -v` and `dciodvfy`.
The files ending in `invalid-beam-number` are negative controls.

The profile follows
[PS3.3 C.8.8.14](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.8.14.html),
[control-point presence C.8.8.14.5](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.8.14.5.html),
[sequence item inheritance C.8.8.14.18](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.8.14.18.html),
[fraction references A.20.3](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_A.20.3.html),
and [rotation semantics C.8.8.14.8](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.8.14.8.html).

## Writer contract

`WritePlan`, `WriteTreatmentRecord` and `WriteImage` take context, a typed model
and `WriteOptions{Template, Limits, TransferSyntax, SOPInstanceUID}`. A matching,
materialized `*object.File` template supplies patient/study/common and unmodelled
modules. The result is independent and returned only after profile, VR/VM and
IOD checks; these APIs do not serialize to or publish a destination.

Template items follow semantic identity: beam/source/setup/channel number,
plan CP index, reference UID, device type and record session/CP identity.
Record keys include declared referenced CP index and date/time; partial delivery
indices need not start at zero. Duplicate or ambiguous keys fail. Optional
attributes stay with retained items when arrays are reordered; newly keyed
items need sufficient required data, and omitted model items are removed.
Sparse source CP encoding may become explicit resolved state. Byte-identical
encoding and full typing of every optional module are not promised.

An unchanged semantic round-trip may retain the instance UID. A controlled
semantic edit requires an explicitly supplied different, valid SOP Instance UID,
which cannot equal the Study, Series or Frame UID. Meta and dataset identity
agree. References to other instances are preserved rather than silently
relabelled. Common patient/Study modules come from the template; callers must
supply common modules consistent with their intended edits.

All three native syntaxes are supported: explicit LE, implicit LE and retired
explicit BE. Source byte order and file-meta syntax must agree. Existing
`internal/valueencode` word helpers normalize fixed-width RawValue data before
the independent bounded Object clone and before BE output. Unicode context is
retained. Unknown private FL retains numeric meaning across explicit syntaxes;
with implicit VR and no private dictionary it becomes opaque UN with preserved
LE bytes. A reader cannot infer the original VR of arbitrary UN bytes.

FL arrays preserve binary32 exactly. FL physical scalars reject values that are
not exactly representable as binary32, NaN or infinity. DS uses
`object.FormatDS` (16 characters); semantic comparison accepts its resulting
decimal representation. IS uses `object.FormatIS`. Encoding errors never become
silent empty required values.

## Structural checks and coordinate meaning

Unique sources/setups/channels, source references, sequence counts, contiguous
plan CP indices, finite values and cumulative budgets are checked. Brachy first
weight is zero, weighted values are nonnegative/nondecreasing, and the final CP
has the declared final weight. Empty interior weights remain unavailable.
Positive channel time needs positive final weight. STEPWISE CPs form dwell pairs
with equal relative positions; transit time can be included, so no zero transit
increment is imposed. PDR channel time describes one pulse; pulse count is
positive and repetition interval cannot be less than that time. The model does
not invent dwell times or timer resolution.

Brachy relative position is mm from the distal-most possible source position.
Optional 3D positions/orientation are patient coordinates/direction cosines.
Ion spot maps are IEC GANTRY X/Y pairs projected to the isocentric plane. Table,
gantry, beam-limiting-device and patient isocentre coordinates remain distinct.
Ion checks first Type 1/Type 2 machine state, inheritance, device dimensions,
nondecreasing/final meterset, first zero and exactly 2N positions/N weights with
positive paintings. Segment sum tolerance is
`sum(abs(weights))*2^-23 + ULP(current CMW) + ULP(next CMW)`; sums are accumulated
in binary64. This covers binary32 rounding and the represented DS endpoints.
Range-shifter settings remain machine codes; distance and water-equivalent
thickness are separate mm quantities.

Records retain specified versus delivered quantities. Partial/resumed records
can begin at nonzero delivered meterset or a nonzero referenced plan index.
Local declared meterset intervals and delivery endpoints are checked without
forcing full planned delivery or NORMAL completion. `ValidateReferencedPlan`
checks identifiers/counts only when the caller supplies the actual matching
Plan; absent external plans are unresolved. Date/time text, precision and
source timezone context remain declared; insufficient precision is not turned
into a chronological assertion.

RT Image uses Image Plane Pixel Spacing and RT Image Position in IEC X-RAY IMAGE
RECEPTOR coordinates. They are not generic patient PixelSpacing/IPP. Encoded
spacing/SAD/SID are positive, NON_NORMAL orientation is orthonormal when known,
and empty Type 2 geometry remains unavailable. Native pixel extent, unsigned
layout/high bit, one frame and the single trailing zero pad are validated before
copying. Model pixels exclude padding and use canonical LE, including OW8/OW16
sources encoded in BE. Changing pixels removes a stale Icon Image Sequence.
Other untyped optional image modules remain the explicit template's responsibility.

## Embedded dose-volume histograms

`rtdose.ReadEmbeddedDVHs(dataset)` reads the RT DVH module independently of
Pixel Data and dose-grid geometry. It requires a single RT Structure Set
reference and reads the dose units and dose type declared on each histogram.
The report retains each ROI's contribution and the stored dose-bin width
and volume pairs with their scaling. Invalid siblings produce a one-based
item position and a DICOM tag in the diagnostics. The remaining
histograms retain source order. The reader admits up to 1,048,576 bins
per item and 4,194,304 bins per result and checks the declared pair count
before allocating the decoded values. Empty optional statistics are
treated as absent.

`rtdose.Read` and `ReadDataset` continue to require valid native geometry
for point sampling and computed DVH. They expose diagnostics for rejected
embedded histograms and can use the grid's dose units and type for older
files that omit those fields inside an item. Use the referenced structure
set to interpret ROI numbers. See [DICOM PS3.3 C.8.8.4][rt-dvh-module].

[rt-dvh-module]: https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.8.4.html

## Limits, ownership and cancellation

Zero selects finite defaults; negative or overflowing limits fail.

| Limit | Default |
| --- | ---: |
| Metadata / primitive value | 64 MiB / 16 MiB |
| Logical model/template allowance / RT Image pixels | 128 MiB / 64 MiB |
| Elements/items / nesting depth | 100,000 / 32 (maximum configurable depth 128) |
| Beams / sources+setups+channels+related entities | 1,024 / 10,000 |
| CPs / spot-map pairs / diagnostic findings | 100,000 / 1,000,000 / 256 |

Admission traverses materialized SequenceValue containers and primitive lengths
before GetSequence, VM decoding, deep cloning or model/pixel copies. It counts
DAG occurrences separately, refuses cycles/duplicate attributes, and reserves
remaining metadata/value/model/count allowances before decoding. It includes
inherited leaf/jaw state and range-shifter state copies. The root snapshot is
charged before `Elements` allocates; six subsequent header/container copy stages
are reserved for metadata stripping, independent facades, endian conversion,
clone and validation. Writer models are
admitted cumulatively before inspecting the template, including exact cumulative
spot-map pairs. Meta and dataset share metadata/element/model admission; File
Meta accepts only group 0002. Effective allowances can reject earlier than an
individual configured maximum because other cumulative limits also apply.

Nil/nonzero backing metadata, deferred providers and arbitrary Value
implementations are refused without implicit reads or `EncodedLength` calls.
Plans/records ignore group 7FE0 and Waveform Data bulk without reading it.
They remove this bulk recursively before any generic Object facade or identity,
VR/VM or IOD helper, including SequenceValue graphs hidden beneath bulk tags.
Their writers never republish it. RT Image requires materialized native RawValue Pixel Data and refuses other
bulk layouts. Input Objects must already have been constructed safely and must
not be mutated concurrently while read/write APIs run. Independent readers use
separate Object facades/caches; returned models may be mutated independently.
A context-aware gate protects only the root `Elements` snapshot and its lazy
order compaction. It holds no lock during provider access, parsing or decoding.

Logical admissions are not measured heap/RSS ceilings. They conservatively
account decoded/model/template and diagnostics; caller input, parser caches,
Object/maps, bounded clone serialization and validator overhead can coexist.
Byte-order conversion creates temporary raw copies. A writer can retain input,
model, clone and output together; admission does not claim one buffer's size as
the whole peak. `Plan.ModelBytes` includes the file-meta allowance for file reads
and is used by Twin's separate retained-batch admission.

Context is checked before stages and in tree, sequence/CP/spot/model loops.
Cancellation returns nil results. Existing Object cloning and IOD validation
have no internal context interface; they remain finite and are checked before
and after the stage. No unbounded spool/output buffer is introduced. New-family
errors use fixed reasons plus tag paths/indices; conventional diagnostic wording
is retained for API regression. Unsupported/invalid/limit/unmaterialized errors
remain distinguishable, with no accepted partial model.

Writers validate the template's declared Transfer Syntax, file-meta identity and
raw byte order before normalizing its representation. A retained
MAC Parameters Sequence (4FFE,0001) or Digital Signatures Sequence (FFFA,FFFA)
at any retained metadata depth is refused before cloning. These APIs do not
verify signatures; read-only inspection remains available for signed metadata.

## Qualification and external limits

Synthetic complete fixtures cover all seven SOPs and all three native syntaxes,
HDR/PDR with multiple sources/setups/channels, other admitted brachy types with
FIXED/presence controls, PROTON MODULATED and ION NONE, native DRR/PORTAL 8/16,
real BE sources, UTF-8/ISO_IR 100, opaque attributes, source ownership and edits
requiring new identity. Generic VR/VM and IOD checks have no errors in accepted
fixtures; manual condition findings are retained in qualification JSON. Profile
checks and independent stored-sample/relationship assertions address the stated
coverage; these are not full arbitrary-IOD validation.


DCMTK 3.7.0+ DEV `dcmdump` reads the 53 positive files without diagnostics;
`drtdump` reads 41 without diagnostics and reports unsupported SOP for the 12
Beams/Brachy Treatment Record files despite exit zero. It checks module presence,
not the model's cross-references. Three missing-label controls produce warnings.
`dcm2img` renders 16 RT Image sources/outputs with exact independent PGM sample
oracles.


| Error category | Occurrences | Consulted PS3.3 2026d rule and fixture |
| --- | ---: | --- |
| PDR pulse counts/intervals | 96 | C.8.8.15/.22 require these fields for PDR; fixture setups declare PDR. |
| NOT_VERIFIED | 29 | C.8.8.21/.22/.26 include this enumerated verification status. |
| Gamma Source Strength | 16 | C.8.8.22 requires strength for non-gamma sources; these Ir-192 sources declare AIR_KERMA_RATE and Reference Air Kerma Rate. |
| PORTAL KVP/Meterset Exposure | 16 | C.8.8.2 requires these Type 2C fields for PORTAL; Image Type value 3 is PORTAL. |
| Modulated Scan Mode Type | 12 | C.8.8.25/.26 require specialization for MODULATED_SPEC; these beams declare MODULATED. |

The 57 warnings comprise 25 current Content Origin attributes, four current
Clinical Fraction Number attributes, 25 related tool IOD-inventory warnings and
three unrecognized explicit-VR private FL extensions with a retained synthetic
private creator. Conventional table-eccentric fields were removed from the
standard Ion fixtures; arbitrary template extensions remain outside the typed
standard model. The tool's build date does not establish its implemented
standard edition. Prior generations, including corrected fixture findings, are
preserved separately. No PACS, clinical delivery or native Windows execution is
claimed.

Primary references: [RT Ion Beams C.8.8.25](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.8.25.html),
[patient support macro C.8.8.28](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.8.28.html),
[brachy C.8.8.15](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.8.15.html),
[time C.8.8.15.6](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.8.15.6.html),
[beam records C.8.8.21](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.8.21.html),
[brachy records C.8.8.22](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.8.22.html),
[Ion records C.8.8.26](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.8.26.html),
[RT Image C.8.8.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.8.2.html),
[PS3.5 VRs 6.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_6.2.html)
and [UIDs chapter 9](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/chapter_9.html).

# Tractography Results

`tractography` reads, validates and writes stored DICOM Tractography Results
Storage (`1.2.840.10008.5.1.4.1.1.66.6`). It does not reconstruct tracks,
calculate statistics, load source images, render, or access files or networks.
The library API operates on Object values and explicit result models.

```go
results, err := tractography.Read(ctx, source, tractography.ReadOptions{})
if err != nil {
    return err
}
// Resolve UnknownOrigin explicitly before rewriting an unreferenced result.
output, err := tractography.Write(ctx, results, tractography.WriteOptions{})
```

`Validate(ctx, results, limits)` checks the stored model and retained metadata;
`Write` additionally requires a resolved source origin and validates the completed
IOD. A nil context means Background. Errors return nil Results/Object and do not
change input values. Categories are `ErrInvalidOptions`, `ErrInvalidData`,
`ErrInvalidReference`, `ErrUnsupportedValue` and `ErrLimitExceeded`. Cancellation
remains discoverable with `errors.Is(err, context.Canceled)`.

## Model and wire representation

A result has explicit SOP Instance, Study, Series and Frame of Reference UIDs,
series/instance numbers, Content Date/Time/Label/Description and track sets.
Track sets contain consecutive numbers starting at one, label/description,
anatomy and modifiers, diffusion model, optional diffusion acquisition, one or
more algorithms, tracks, point measurements and both statistic families.

Tracks have at least two patient-coordinate xyz triplets in mm (`[][3]float32`).
Color is present at set, track or individual-point level. A point-color list has
one CIELab uint16 triplet per point; multiple supplied valid color levels are
retained without an invented precedence. Each measurement has exactly one Values
item per track. Nil indices mean all points in track order; explicit one-based
indices and values have equal positive counts. Their arbitrary valid order is
preserved. Track statistics have one float32 scalar per track. Set statistics use
one float64 FD value, without narrowing to float32. No values are computed.

OF, OL and OW streams use restricted endian-aware codecs. Scalar UL/US/IS/FL/FD
use existing Object helpers after length/VR/VM checks. The writer emits independent
LE raw OF/OL/OW buffers so stream VRs retain VM 1 through the existing validators;
these are streams, not numeric VM arrays. Nested output attributes are sorted by
tag. A narrow FL NaN path preserves original float32 bits across helper widening.
IEEE NaNs, infinities and signed zero are retained. Downstream consumers can apply
their own finite-value policy. Full DA and valid partial TM strings are preserved;
precision is never invented. Text follows the declared Object charset on read
and uses UTF-8 on write; custom fallback text policies are not inherited.

`Code` preserves short SH, long UC or URN UR value form, coding scheme/version,
meaning, optional enhanced context encoding and one level of equivalent codes.
Algorithm family/name code and textual name/version/parameters/source are
supported. Registry context groups enforce registered non-extensible laterality;
extensible groups permit additional valid codes. Baseline CIDs do not forbid
alternate coding. CID 82 requires the UCUM scheme, but this is not a complete UCUM
expression parser. DCMR context-group version dates require explicit `+0000`.

## References and source origin

`DICOMImages` requires source image references. `OtherSource` requires no image
references. `UnknownOrigin` is returned when the encoded object has none, since
absence cannot establish how it was created; the caller must resolve that origin
before writing. `ReadOptions.RequireDICOMReferences` enforces references when the
caller independently knows DICOM images were used.

References carry SOP class/instance, study/series and optional frame/segment
lists. The writer creates a flat Referenced Instance Sequence and an exact common
instance hierarchy, including other studies. Exact complete duplicates are
removed in first-seen order. Conflicting identities, class, hierarchy or selectors
for the same instance are rejected; scopes are never silently unioned. Selectors
are positive and unique. Segment selectors require a segmentation class; known
single-frame CT/MR/Secondary Capture references cannot select frame numbers above
one. Referenced source existence, frame/segment counts, patient compatibility,
registration and clinical correctness remain caller responsibilities.

PPS references are optional and require the MPPS SOP class. No UID or frame of
reference is invented by this API. Common.Source study/frame identities, when
present, must agree with the explicit result. Source pixels are never copied.

## Metadata, ownership and signed content

`Results.Metadata` holds independent supported common/optional root IOD
attributes, including bounded content annotations, creator identification,
clinical-trial and specimen macros. It cannot override modeled root fields or
introduce attributes outside this IOD. Unknown/private extension fidelity is not
promised. Optional modules, when present, must satisfy their applicable required
attributes; existing IOD warning severity for a missing optional-module field is
not treated as sufficient acceptance.

Reader outputs own coordinate, color, measurement, index, statistic, reference and
metadata buffers. Writer outputs own all retained mutable values, including
numeric metadata kinds. BE raw metadata words are canonicalized to LE. Borrowed
input Objects, model buffers, options and equipment must remain stable during a
call. Other Object methods must not run during the initial snapshot; package calls
serialize that snapshot and thereafter use private facades without publishing
sequence caches on the source. There are no background goroutines or ownership
transfers. The caller retains its source provider and Close responsibility.

Reading signed content is inspection, not cryptographic verification. MAC and
Digital Signatures metadata can be retained in the independent read result.
`Write` refuses retained signed metadata with `ErrUnsupportedValue`, since
rewriting the data would invalidate signatures. A root signature is refused
before model/metadata copies; nested metadata is checked during bounded raw
preflight. The caller can explicitly prepare metadata without signatures using
Object APIs. Root signatures on Common.Source are not inherited into the derived
result; nested signed common metadata is refused. Synthetic signature tests do
not claim a valid certificate or verified signature.

Equipment explicitly supplied in Common overrides retained equipment. Retained
equipment must be complete; partial retained fields are not mixed with software
defaults. Otherwise existing DerivedCommon defaults create conformant enhanced
equipment. Common.Now can make creation date/time reproducible and must stay in
the DICOM year range, including any declared timezone conversion.

## Materialization and resource bounds

The profile accepts materialized values. Deferred/nil values with nonzero or
unknown length, unresolved BulkDataValue and unsupported payload kinds are
refused before lookup, GetSequence, cloning or provider reads. This includes
retained/common metadata. Ignored root Pixel, Float Pixel, Double Pixel and
Spectroscopy payloads are neither read nor copied. This is not lazy tractography
support. The caller can materialize input through existing bounded parser APIs.
Non-bulk attributes are preflighted even if outside the retained metadata profile.
Nested image bulk is outside retained macro fidelity and is not copied.
Ignored root bulk headers are removed from the independent snapshot before any
Object constructor receives it, including unexpected SequenceValue payloads.
Their original root-header reservation remains charged; source headers, backing
and provider stay available to the caller.

Zero fields select finite defaults. Positive overrides can only lower them;
negative values and values above the default ceiling are invalid.

| Limit | Default per operation |
| --- | ---: |
| MaxBytes | 512 MiB logical reservation |
| MaxValueBytes | 128 MiB per primitive/model buffer |
| MaxTextBytes | 8 MiB cumulative encoded/input text |
| MaxTrackSets | 1,024 |
| MaxTracks | 100,000 |
| MaxPoints | 8,000,000 |
| MaxValues | 16,000,000 OF measurement/statistic values |
| MaxReferences | 100,000 input references |
| MaxElements / MaxItems | 100,000 / 100,000 reserved structural slots |
| MaxSequenceDepth | 16 item levels from root depth zero |

Counts and reservations are cumulative across sets, source and retained metadata.
Shared/DAG items are charged on every visit; address sharing does not bypass a
limit. Model-generated code items are charged at their actual output depth.
Independent limits can bind before other advertised ceilings.

| Reservation | Conservative charge before allocation/helpers |
| --- | --- |
| Binary payload | 8 times encoded/model bytes |
| Text payload | 64 times bytes, plus 128 bytes per authored StringValue component |
| Element slot | 512 bytes |
| Item slot | 1,024 bytes |
| Source structure | Four element/item slots per visit for facade, retained clone, common clone and validation storage |
| Initial root snapshot | Additional 512 bytes per root element, with root count checked first |
| Generated structure | Bounded macro/track/reference slot reservations; references reserve four items each |

Actual slice lengths and checked width/count arithmetic precede uint32 VL
conversion and make/copy. Primitive header length, word width, VR/kind, xyz/CIELab
shape and sequence cardinality are strict. Structural quotas precede duplicate
maps, private facades and sequence clones. Cancellation is checked during visits,
reference scans and bounded copy/conversion chunks. Existing synchronous IOD and
common helpers are bounded and checked before/after; they do not provide an
interruptible inner-loop context API.

MaxBytes is a conservative request-local reservation, not measured RSS. Already
allocated caller data, static tables and allocator/runtime overhead are not a
claimed resident-memory bound. No performance or RSS reduction is claimed.

## Qualification and limits

The standard-authored fixture has two sets, three unequal tracks, nine points,
two algorithms, all color levels, all-point and subset measurements, both
statistic families, FD precision and references across two studies. Its own
literal-tag/byte builder is independent of the factory. Golden OF/OL/OW bytes,
Explicit/Implicit LE and BE reads, IEEE bits, IOD/encoding, malformed input,
limits/DAG/empty items, zero-read deferral, ownership, cancellation and concurrent
calls are tested. The complete fixture preserves segmentation selectors [1,3]
through all library round trips.

An explicitly supplied external public-API harness qualifies against DCMTK
3.7.0 development at commit `8d68392db66cd69e13dc878dc9889681062ad479`.
The native libdcmtract target was built from that pinned local source. No upstream
implementation or fixture is bundled or translated. Activation uses
`DICOMGO_TRACTOGRAPHY_ORACLE`; absent activation skips only the external test.

The pinned reference has four observed limits: TrcMeasurement.get rejects valid
indices, loadDataset loses SeriesNumber before writeDataset, and writeDataset
removes ReferencedSegmentNumber and empty Laterality. The harness uses public getValues/getType/getUnits
and an explicit getSeries().setSeriesNumber from the input. A variant referencing
the entire segmentation instance matches every modeled family and both study
hierarchies bidirectionally. Common metadata is non-equivalent because empty
Laterality is dropped; no integral IOD round trip is claimed. The full profile is separately asserted
**non-equivalent** because selectors [1,3] are lost; it is not claimed as a safe
external round trip. Earlier failed observations remain qualification evidence.

The authored fixture explicitly supplies empty series Laterality (0020,0060);
no R/L value is inferred from track-set modifiers. The initial external dciodvfy
missing-Laterality observation is retained separately from final qualification.
Final dciodvfy checks of both authored Go inputs exit zero, with an unknown-
laterality warning. Reference rewritten outputs still fail that external check
with only missing Laterality.
General Series laterality and other clinical conditions depend on caller facts
and supplied metadata; the writer does not insert Laterality automatically.

IOD validation is scoped to the existing 2026d module/value rules plus package
invariants, not exhaustive clinical validation. Informational source-origin/PPS
conditions may require facts absent from the dataset; SourceOrigin and optional
PPS contracts expose them explicitly. No PACS, CI, native Windows, reconstruction,
rendering or clinical acceptance is claimed.

Primary authority: [PS3.3 A.78.3](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_A.78.3.html),
[C.8.33](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.33.html),
[C.8.33.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.33.2.html),
[C.8.33.3](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.33.3.html),
[Code Sequence Macro](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_8.8.html),
[Common Instance Reference](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.12.2.html)
and [PS3.5 VRs](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_6.2.html).

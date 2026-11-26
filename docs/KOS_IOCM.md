# KOS rejection notes

`sr.NewKOSRejection` and four named wrappers construct in-memory Key Object
Selection documents with DCM titles 113001 (quality), 113037 (patient safety),
113038 (incorrect worklist entry), and 113039 (retention expired).
`sr.ReadKOSRejection` recognizes and validates this deliberately strict profile.
These functions perform no filesystem, network, archive rejection, or deletion.
The `mkkos` CLI and IOCM receiver workflows are separate features.

```go
note, err := sr.NewKOSQualityRejection(ctx, sr.KOSRejectionOptions{
    SOPInstanceUID: "1.2.3.100",
    StudyInstanceUID: "1.2.3.10",
    SeriesInstanceUID: "1.2.3.101",
    References: []sr.KOSRejectionReference{{
        ValueType: sr.ValueImage,
        StudyInstanceUID: "1.2.3.10", SeriesInstanceUID: "1.2.3.11",
        SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.3.12",
    }},
    QualityReasons: []sr.CodedEntry{sr.FromCode(dcm.MotionBlur)},
    Common: object.DerivedCommon{Source: source},
})
```

Supply explicit valid UIDs for the note and references. All references must be
in the note's study; its series must differ from every referenced series and its
SOP Instance UID must differ from every target. Identical references collapse
in first occurrence order. A repeated SOP Instance UID with a different class,
study, series, or content type is an error. The caller declares IMAGE, WAVEFORM,
or COMPOSITE according to the referenced IOD; the API checks recognized patient
Storage SOP Classes but does not open the target or infer its role from modality.
COMPOSITE is for objects other than image and waveform IODs. References always
cover whole SOP instances; frames, segments, and channel selectors are rejected.
Known Study/Series/SOP Class/SOP Instance UID role collisions are rejected;
global uniqueness beyond these supplied identities remains the caller's duty.

The root CONTAINER has the selected title. Quality notes require at least one
HAS CONCEPT MOD CODE item named Document Title Modifier (113011), using a member
of the library's vendored CID 7011. Other note kinds reject quality modifiers.
An optional nonempty description uses Key Object Description (113012). Target
items use CONTAINS. Current Requested Procedure Evidence identifies exactly
the same SOP instances, classes, studies, and series as the content. The reader
rejects missing, extra, conflicting, partial, or by-reference evidence/content.
An evidence series may carry Retrieve URL (0008,1190), the one optional
attribute of the Hierarchical Series Reference Macro accepted (CP-2595, used by
IHE MADO): `KOSRejectionReference.RetrieveURL` writes it and reads it back on
each reference of the series. It must be a URI without spaces or control
characters, and every reference of a series carries the same value, empty
included.
Code recognition uses value and scheme, allowing localized meanings.

The profile validates TID 2010 and KOS by-value relationships. It accepts basic
short coded entries, an optional coding scheme version, and optional DCMR TID
2010 identification. Additional code context, observers, image libraries,
language subtrees, nonempty procedure-step references, multi-study notes, and
other content extensions are outside this profile and are rejected. A different
SOP Class/title returns `sr.ErrNotKOSRejection`; malformed supported notes return
an error. Every reader error returns `sr.KOSRejectionNote{}`, with no actionable
partial interpretation. Successful interpretation is metadata, not authority
to discard instances. Selection of incorrect/derived instances and retention
policy remains the application's responsibility.

The factory reuses common Patient, Study, equipment, and instance-creation
metadata through `object.DerivedCommon`, supplies existing Type 2 defaults, and
omits Frame of Reference and SR completion/verification attributes. Content and
instance-creation timestamps use CreatedAt, then Common.Now, then time.Now. A
valid source Timezone Offset From UTC controls their local date/time; year
rollover and half-hour offsets are supported. Series/instance numbers default
to 1 and this profile accepts positive decimal IS numbers.

All inherited metadata must be materialized: any non-bulk nil payload with a
positive or unknown encoded length, unavailable sequence, or unsupported value
representation is rejected before a helper could access backing IO. Neither
factory nor reader supports lazy metadata materialization. Unused root Pixel
Data, Float/Double Float Pixel Data, and Spectroscopy Data are ignored, including
deferred bulk; nested bulk is rejected or charged as ordinary materialized
metadata. No pixels are inherited. Source, options, and their reachable payloads
must remain stable during the call. Concurrent calls through these APIs use
private sequence/charset facades and can share a stable source; callers must
avoid concurrent mutation or other source APIs that modify its caches.

The factory returns owned metadata structures and values, including inherited
sequence items and typed arrays. The reader returns owned slices and immutable
strings; it does not expose the source Object or generic SR Document. Errors
preserve inputs and return nil/zero. Both functions require a nonnil context and
check cancellation during traversal and validation; no cancelled note is
returned. Generic inheritance itself has no context, so cancellation can wait
for an already bounded copy to finish.

`KOSRejectionLimits` has finite defaults: 10,000 references, 50,000 visited items,
200,000 visited elements, depth 32, 16 quality reasons, 65,536 description bytes,
and 1,048,576 metadata bytes. Zero uses the default; positive overrides only lower
it, and negative or above-default overrides fail. Structural visits count DAG
repetitions; cycles stop at the depth cap. Raw sequences, items, payload sizes,
and VM are checked before sequence facades, charset decoding, validators, or
inheritance allocate. UTF-8 decoding reserves up to four bytes per encoded text
byte, and the factory conservatively reserves generated headers and structure,
so low budgets can reject inputs whose final serialized size would fit. Caps
are logical bounds rather than promises of exact heap usage or peak RSS.
Limit failures wrap `sr.ErrResourceLimitExceeded`.

The tests include an independently assembled fixture, all four titles, three
reference families, explicit/implicit little endian and explicit big endian
round trips, scoped library IOD/common-module checks, source ownership,
conflicting evidence, VM/VR errors, bounded DAG/empty-item inputs, zero backing
reads for deferred metadata, ignored deferred pixels, and shared-source races.
These checks do not demonstrate an IOCM PACS, external validator certification,
or complete IHE actor conformance.

Authority: [DICOM CID 7010](https://dicom.nema.org/medical/dicom/current/output/chtml/part16/sect_CID_7010.html),
[CID 7011](https://dicom.nema.org/medical/dicom/current/output/chtml/part16/sect_CID_7011.html),
[TID 2010](https://dicom.nema.org/medical/dicom/current/output/chtml/part16/sect_TID_2010.html),
[KOS IOD](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_A.35.4.3.html),
[KOS Series](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.17.6.html),
[KOS Evidence](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.17.6.2.html),
[COMPOSITE macro](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.18.3.html),
and [IHE RAD TF Vol. 2, RAD-66](https://www.ihe.net/uploadedFiles/Documents/Radiology/IHE_RAD_TF_Vol2.pdf).
DICOM pages inspected were 2026d; IHE was Revision 23 Final, August 8, 2025.
dcm4che's documented `mkkos --title` behavior was a clean-room behavioral
reference only; no code, algorithms, or fixtures were copied.

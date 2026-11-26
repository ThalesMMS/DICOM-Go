# HL7 ADT patient and visit attributes

`hl7.ParseADT` reads seven trigger events over the existing ER7 parser. The
library returns typed operations and independent, sparse DICOM attributes. The
application decides how to resolve identities, update its catalog, or apply an
attribute set to an existing dataset.

| Trigger | Meaning | Typed result | HL7 2.5.1 structure |
| --- | --- | --- | --- |
| A01 | Admit / visit notification | `Patient`, `Visit` | ADT_A01 |
| A04 | Outpatient registration | `Patient`, `Visit` | ADT_A01 |
| A08 | Patient information update | `Patient`, optional `Visit` | ADT_A01 |
| A28 | Add person information | `Patient` | ADT_A05 |
| A31 | Update person information | `Patient` | ADT_A05 |
| A40 | Merge patient identifier lists | Ordered `Merges` with PID and MRG-1 identifiers | ADT_A39 |
| A47 | Correct patient identifier list | `Change.Current` (PID-3), `Change.Prior` (MRG-1) | ADT_A30 |

Every event requires a populated PID-3. A01/A04 also require one PV1 with a
populated PV1-2 class. A28/A31 describe a person; their pseudo PV1 class N does
not create an encounter. The implementation does not create a visit for either
person event, even if a sender includes other PV1 fields. A08 keeps its existing
acceptance of an absent or empty PV1; a populated encounter PV1 is now typed.

A47 requires one PID followed by one MRG with populated MRG-1. It is a distinct
identifier correction: `Patient`, `Visit` and `Merges` remain empty. No supplied
name or other demographics are applied by the correction mapper. Unlike A40,
it does not instruct a receiver to merge two patient records. Conflicts with a
current identity, unknown prior identities, transactionality and persistence
are decisions for the consuming application. A40 retains its existing support
for ordered, repeated PID/MRG groups.

This is a typed subset parser, not a complete HL7 message-profile validator.
The third MSH-9 component, EVN presence and unmapped optional segments are not
validated against an IHE static definition. The event comes from MSH-9.2, with
the existing EVN-1 fallback if no trigger is supplied. EVN-2 is retained as
`RecordedDateTime`.

## Patient mapping

Call `adt.Patient.DICOMAttributes(primaryIndex)` to explicitly choose the
primary PID-3 repetition. The result is a new `object.Object` and does not modify
the message, the typed model, or a stored DICOM object.

| HL7 source | DICOM output |
| --- | --- |
| Selected PID-3 CX.1 | Patient ID (0010,0020), LO |
| CX.4 namespace | Issuer of Patient ID (0010,0021), LO |
| CX.4 universal ID / type | Issuer of Patient ID Qualifiers Sequence (0010,0024), with (0040,0032) UT and (0040,0033) CS |
| CX.5 identifier type | Identifier Type Code (0040,0035), CS, in the qualifier item |
| Other populated PID-3 repetitions | Other Patient IDs Sequence (0010,1002); each item has its own ID and issuer |
| First PID-5 XPN | Patient's Name (0010,0010), PN, `Family^Given^Middle^Prefix^Suffix` |
| PID-7 date | Patient's Birth Date (0010,0030), DA |
| PID-8 | Patient's Sex (0010,0040), CS, using existing `DICOMSex` behavior |

CX.5 is not assigned to Type of Patient ID (0010,0022), whose TEXT/BARCODE
semantics differ. A universal issuer ID and its type must be supplied together
when mapping. Identifiers with no ID, including a field sent as HL7 null, cannot
be mapped as current or prior patient identifiers. `DICOMPatientIDs` and issuer
matching remain available independently of the attribute mapper.

The first name remains the existing model's projection; other name repetitions,
FN subcomponents beyond the family surname, XPN representation/type metadata
and birth time are not projected. Full PID-3/MRG-1 identifier repetitions are
retained. PID-7 can retain a partial birth date in the HL7 model; a mapper refuses
it rather than inventing a month or day for modern DICOM DA.

For A47, call `adt.Change.DICOMCurrentAttributes(primaryIndex)`. It maps only
current identifiers; prior identifiers remain lookup keys and never become
additional current identifiers. For an A40 merge, use `Merges` to reconcile the
surviving and prior identities explicitly.

## Visit mapping

Call `adt.Visit.DICOMAttributes()` only when `Visit` is non-nil. Its sparse
object is separate from the patient object.

| HL7 source | DICOM output |
| --- | --- |
| First PV1-19 CX.1 | Admission ID (0038,0010), LO |
| PV1-19 CX.4 HD | Issuer of Admission ID Sequence (0038,0014), with local namespace (0040,0031) UT, universal ID (0040,0032) UT and type (0040,0033) CS |
| First PV1-3 PL.1 / PL.2 / PL.3 | Current Patient Location (0038,0300), LO, joined with `/`, with trailing empty components removed |
| PV1-4 | Route of Admissions (0038,0016), LO, the source's local value |
| First PV1-8 XCN name | Referring Physician's Name (0008,0090), PN |
| PV1-44 TS.1 date / time | Admitting Date (0038,0020), DA, and Admitting Time (0038,0021), TM |
| Explicit PV1-44 UTC offset | Timezone Offset From UTC (0008,0201), SH |

PV1-44 retains its local wall-clock date/time, fractional precision and offset.
A date-only value emits no time. Partial year/month dates are refused during
mapping. Leap-second spelling and fractions are preserved without passing the
value through a formatter that would normalize them. An explicit offset is
emitted alongside the admitting pair; an unspecified offset does not imply UTC
or the current machine's timezone. Before combining this sparse visit object
with another dataset, reconcile its offset: (0008,0201) also affects other
DA/TM and implicit-offset DT values in that dataset.

Patient class, hospital service, the location facility HD and discharge TS stay
in `Visit`. They are not coerced into unrelated institution/patient-state
attributes, universal code translations, or retired discharge tags. A facility
identifier is not automatically the name of the imaging institution. This
mapping is an explicit library policy for the listed subset, not a claim that
all local HL7 table values are universal DICOM codes.

## Empty fields, nulls and validation

An absent or empty HL7 demographic/visit field means no update and emits no
corresponding element. HL7 null `""` emits a zero-length element; a null visit
number also emits an empty issuer sequence. A supplied unknown sex maps to an
explicit empty DICOM sex, preserving existing `DICOMSex` semantics. An absent
sex must not silently clear a stored sex. Clearing means zero-length data in
this attribute set; the caller decides whether to remove a stored value.

All objects use Specific Character Set ISO_IR 192. Parsing/unescaping and the
MSH-18 decoder are reused unchanged; `Object.SetStrings` provides the shared
DICOM VR repertoire/length validation. An unescaped literal DICOM value
separator, or a PN component containing a literal `^` or `=`, is refused rather
than changing VM or name semantics. Conversion errors return a nil object and
an `hl7.Error` naming the source field, without copying its value. A later
failure does not expose a partial attribute set.

Missing/misordered segments use code 100, missing required IDs/class use 101,
and mapping failures use 102. Unsupported message/event codes retain 200/201
with rejection. `ACKFor` converts these to AE or AR with ERR location. The new
single-patient events reject duplicate PID/PV1 segments; the existing A08
parser's first-segment selection and A40 group behavior remain intact.

These sparse objects carry no SOP identity, pixels or complete IOD. Applying
an identifier with a different issuer must explicitly replace or clear stale
issuer attributes; simply overlaying sparse attributes is not an identity
reconciliation algorithm.

## Evidence and references

`go test ./hl7 ./net/mllp` exercises synthetic messages for all five new events,
A08/A40 regression behavior, ACK errors, repeated identifiers, Unicode and
custom separators, empty/null distinctions, mapper refusals, date/time
precision, source ownership, and concurrent independent mapping. Patient and
visit attributes are encoded and parsed in explicit LE/BE and implicit LE.
These checks do not establish acceptance by a live ADT source or PACS, full
IHE profile conformance, or clinical validation.

The implementation is clean-room from primary specifications:

- [IHE ITI-30 Patient Identity Management](https://profiles.ihe.net/ITI/TF/Volume2/ITI-30.html), including A28/A31, pseudo PV1 and A47 correction semantics.
- [IHE ITI-31 Patient Encounter Management](https://profiles.ihe.net/ITI/TF/Volume2/ITI-31.html), including A01/A04/A08 encounter events.
- [DICOM PS3.3 Patient Identification](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.2.2.html), [Visit Identification](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.3.2.html), [Visit Admission](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.3.4.html), and [Timezone Offset From UTC](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.12.5.html).

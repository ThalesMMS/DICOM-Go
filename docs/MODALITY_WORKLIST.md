# Modality Worklist SCU and SCP

`net/dimse` implements the Modality Worklist Information Model - FIND SOP
Class (`1.2.840.10008.5.1.4.31`) as a reusable, single-level C-FIND profile.
It supplies typed query builders, a blocking callback-streamed SCU, a
synchronous-yield SCP, exact SOP Class routing for `ServeAssociation`, and an
optional deterministic in-memory matcher. It does not supply a RIS/HIS,
persistent worklist, HL7 scheduling, or inferred clinical values.

## Public integration surface

- `ModalityWorklistFindPresentationContext` proposes Explicit and Implicit VR
  Little Endian.
- `BuildModalityWorklistIdentifier` preserves absent, present-empty, and
  present-valued keys.
- `ModalityWorklistClient.FindWithOptions` owns one operation on a borrowed
  established association. It streams each pending Identifier to a synchronous
  callback, sends C-CANCEL for local cancellation or match limits, and drains a
  final response before returning the association as reusable.
- `ServeModalityWorklistCFindWithOptions` serves one request directly.
- `NewModalityWorklistCFindRouter` installs MWL beside an optional hierarchical
  `CFindHandler` without changing `AssociationSCPOptions`.
- `ModalityWorklistHandler` receives a borrowed request and synchronously
  yields borrowed candidate objects. The core projects each candidate to the
  requested return keys before encoding it; providers need not materialize the
  complete result set.
- `NewInMemoryModalityWorklistHandler` clones its synthetic input records and
  yields matching records in input order. Production applications should
  implement `ModalityWorklistHandler` over their own bounded RIS/PACS query.

The association remains caller-owned. A clean final response permits Release
or another serialized operation. Failure to send C-CANCEL or drain the final
response is reported as uncertain association state and follows the configured
operation error policy.

## Matching and return-key profile

The typed profile covers the following Table K.6-1 attributes:

| Attribute | Tag | Matching implemented | Return behavior |
|---|---:|---|---|
| Patient's Name | `(0010,0010)` | Single/wildcard; literal and wildcard PN matching are case-insensitive by declared implementation policy | Requested value must be supplied by the provider |
| Patient ID | `(0010,0020)` | Case-sensitive single value | Requested value must be supplied |
| Patient's Birth Date | `(0010,0030)` | DA single/open/closed range | Requested value must be supplied, including explicit zero length when unknown |
| Patient's Sex | `(0010,0040)` | Case-sensitive single value | Requested value must be supplied, including explicit zero length when unknown |
| Accession Number | `(0008,0050)` | Case-sensitive single value | Requested value must be supplied, including explicit zero length when unknown |
| Requested Procedure ID | `(0040,1001)` | Case-sensitive single value | Requested value must be supplied |
| Requested Procedure Description | `(0032,1060)` | Case-sensitive single value | Requested value must be supplied and non-empty; the alternative Requested Procedure Code Sequence is available separately |
| Scheduled Procedure Step Sequence | `(0040,0100)` | Sequence matching with zero or one query item; each match contains exactly one item | Requested nested attributes are projected; a zero-item or empty-item Universal Return Key returns the complete selected item |
| Scheduled Station AE Title | `(0040,0001)` | Case-sensitive single value, VM 1 | Requested value must be supplied |
| Modality | `(0008,0060)` | Case-sensitive single value, VM 1 | Requested value must be supplied |
| SPS Start Date | `(0040,0002)` | DA single/open/closed range | Requested value must be supplied |
| SPS Start Time | `(0040,0003)` | TM single/open/closed range | Requested value must be supplied |
| Scheduled Performing Physician's Name | `(0040,0006)` | Single/wildcard with the same case-insensitive PN policy | Requested value must be supplied, including explicit zero length when applicable |
| SPS Description | `(0040,0007)` | Case-sensitive single value | Requested value must be supplied and non-empty unless a requested, valid Scheduled Protocol Code Sequence supplies the alternative |
| SPS ID | `(0040,0009)` | Case-sensitive single value | Requested value must be supplied |
| Scheduled Station Name | `(0040,0010)` | Case-sensitive single value | Requested value must be supplied, including explicit zero length when applicable |
| SPS Location | `(0040,0011)` | Case-sensitive single value | Requested value must be supplied, including explicit zero length when applicable |

When both SPS Start Date and Start Time contain ranges, the matcher treats them
as one continuous interval, from the lower date/time boundary through the upper
date/time boundary. A single Date or Time value continues to match independently
even when its paired attribute is a range. A present zero-length value is Universal Matching and a
return-key request; an absent attribute is neither matched nor returned. An
Identifier without a non-universal matching key produces no matches and final
Success.

`TimezoneOffsetFromUTC` is accepted only with a DA or TM key, validated in the
DICOM `+/-HHMM` range, and carried into responses that return SPS Start Time or Study Time. The in-memory matcher treats candidate DA/TM values as being in
that declared offset when the candidate omits its own value; a candidate with a
different explicit offset does not match. It does not convert between offsets.

`SpecificCharacterSet` declarations use the library character-set registry.
The SCU includes the caller-supplied declaration in its Identifier, so the
dataset writer encodes textual `StringValue` query keys with that repertoire;
it interprets response text through the normal `object` character-set APIs.
The SCP decodes raw request keys under the request declaration before matching,
and the in-memory matcher independently decodes provider candidates under each
candidate's declaration. Raw textual response values are validated under that
candidate declaration, and `(0008,0005)` is projected only when returned text
needs a non-default repertoire, including ISO 2022 escape sequences. Neither
role replaces or infers a missing declaration.

The raw Identifier parser recognizes Scheduled Protocol Code Sequence
`(0040,0008)` as an in-model optional return key that this matcher does not use
for matching. It is returned when the provider supplies it. If it is absent for
a particular match, that response uses `FF01`; otherwise pending responses use
`FF00`. Each returned item follows the optional-meaning coded-entry macro:
exactly one Code, Long Code or URN Code Value is present; Coding Scheme
Designator is required for Code/Long Code and optional for URN; Code Meaning is
optional. The remaining Table K.6-1 attributes (for example Patient Transport
Arrangements, Current Patient Location, Medical Alerts, Allergies, Pregnancy
Status, Patient's Weight and Size, plus Pre-Medication, Requested Contrast Agent
and SPS Status inside the Scheduled Procedure Step) remain return-only. An
empty value, or a lone `*`, requests the attribute. An absent provider value
produces `FF01`; non-universal matching remains refused with `A900`. Attributes
outside the declared profile are also rejected, unless
`ModalityWorklistParseOptions.AcceptUnknownReturnKeys` admits their universal
return-key form. That option does not enable matching of unknown attributes.

### Optional standard matching keys

The following `ModalityWorklistQuery` fields extend the typed builder, raw
Identifier parser, in-memory/directory matcher and response projection:

| Field | Tag | Matching |
|---|---:|---|
| `IssuerOfPatientID` | `(0010,0021)` | Case-sensitive single/wildcard |
| `StudyDate` | `(0008,0020)` | DA single/open/closed range |
| `StudyTime` | `(0008,0030)` | TM single/open/closed range |
| `StudyInstanceUID` | `(0020,000D)` | Single UID; UID lists are not exposed |
| `RequestingPhysician` | `(0032,1032)` | Single/wildcard; existing case-insensitive PN policy |
| `ReferringPhysicianName` | `(0008,0090)` | Single/wildcard; existing case-insensitive PN policy |
| `AdmissionID` | `(0038,0010)` | Case-sensitive single/wildcard |
| `RequestedProcedurePriority` | `(0040,1003)` | Case-sensitive single/wildcard |

All scalar query keys require VM 1. `MWLReturnKey()` and a lone `*` on wildcard
keys are universal; their absence on a record does not exclude that record and
is reported with `FF01`. Matching Patient ID together with its Issuer prevents
records with the same ID in a different assigning domain from matching.
Study Date and Study Time ranges are independent, including when both are
present: combined datetime matching has not been negotiated. The existing SPS
range policy is unchanged. Timezone offsets are carried into returned time
attributes without converting between different explicit offsets.

`MWLSequenceKey` preserves absence (`Present: false`), a zero-item universal
request (`MWLSequenceReturnKey()`), an empty-item universal request, and one
item containing matching and/or return keys (`MWLSequenceMatch(item)`). The
builder and parser detach validated items. The item is a `core.DataSet` with a
bounded, sequence-specific attribute allowlist:

| Query sequence | Supported item matching attributes |
|---|---|
| `RequestedProcedureCodeSequence` `(0032,1064)` | Code Value `(0008,0100)`, Coding Scheme Designator `(0008,0102)`, Coding Scheme Version `(0008,0103)`, Code Meaning `(0008,0104)`, Long Code Value `(0008,0119)`, URN Code Value `(0008,0120)` |
| `IssuerOfAccessionNumberSequence` `(0008,0051)` and `IssuerOfAdmissionIDSequence` `(0038,0014)` | Local Namespace Entity ID `(0040,0031)`, Universal Entity ID `(0040,0032)`, Universal Entity ID Type `(0040,0033)` |
| `IssuerOfPatientIDQualifiersSequence` `(0010,0024)` | Universal Entity ID `(0040,0032)`, Universal Entity ID Type `(0040,0033)`, Identifier Type Code `(0040,0035)` |
| `OtherPatientIDsSequence` `(0010,1002)` | Patient ID `(0010,0020)` with single-value matching, Issuer `(0010,0021)`, and nested Issuer of Patient ID Qualifiers Sequence |

Supported string item keys use case-sensitive single/wildcard matching except
Patient ID. Unimplemented standard macro attributes can still be requested
universally, but matching their values is refused. Multiple query items,
unsupported VRs, duplicate item attributes and unsupported nesting are refused.
Every non-universal key in a query item must match **the same candidate item**;
recursive keys keep that rule at each level. Only matching items and requested
nested attributes are projected, in provider order. A zero-item or empty-item
request returns the complete sequence. Missing optional requested keys produce
`FF01`; the SCP does not fabricate values. This matching profile is not a
complete coded-entry or issuer-macro IOD validator.

### Explicit veterinary extension

`ModalityWorklistQuery.Veterinary` selects a Standard Extended profile permitted
by PS3.4 K.6.1.2 note 1. Veterinary matching on raw SCP requests requires
`ModalityWorklistSCPOptions.Parse.EnableVeterinaryMatching: true`. With the
zero-value options, non-universal veterinary matching remains refused;
`AcceptUnknownReturnKeys` alone continues to enable only universal return keys.

`ModalityWorklistVeterinaryQuery` supports Patient Species Description
`(0010,2201)`, Patient Breed Description `(0010,2292)`, Patient Sex Neutered
`(0010,2203)`, Responsible Person `(0010,2297)`, Responsible Person Role
`(0010,2298)` and Responsible Organization `(0010,2299)`. PN uses the existing
case-insensitive policy; other strings use case-sensitive single/wildcard
matching. Patient Species Code Sequence `(0010,2202)` and Patient Breed Code
Sequence `(0010,2293)` use the code-item profile above. Breed Registration
Sequence `(0010,2294)` supports Registration Number `(0010,2295)` and nested
Breed Registry Code Sequence `(0010,2296)`, preserving same-item matching and
projection at both levels.

The authority for these rules is [PS3.4 Table K.6-1 and its extension note](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_K.6.html),
[worklist matching/return-key types](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_K.2.2.html),
[wildcard matching](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_C.2.2.2.4.html),
[range matching](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_C.2.2.2.5.html)
and [sequence matching](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_C.2.2.2.6.html)
(edition 2026d at implementation). DCMTK is an independent behavioral oracle;
its implementation is not copied.

Scheduled Station AE Title is VM 1-n in a record: a query value matches any of
the record's values, and every value is returned.

An Identifier without a non-universal matching key produces no matches by
default. `ModalityWorklistSCPOptions.AllowUniversalQuery` passes it to the
handler, and `InMemoryModalityWorklistOptions.UniversalQueryMatchesAll` makes
the in-memory handler answer it with every record, as PS3.4 universal matching
and wlmscpfs do.

Fuzzy PN, Empty Value Matching, Multiple Value Matching, cross-timezone
conversion, and SOP Class Extended Negotiation of those behaviors are not
implemented.

## Statuses, cancellation, and bounds

- `FF00`: pending match; requested optional keys were supplied.
- `FF01`: pending match; an optional return key was unavailable.
- `0000`: matching complete, without a final Identifier.
- `FE00`: matching terminated after C-CANCEL.
- `A700`: configured resource or match limit exceeded.
- `A900`: Identifier does not match the MWL SOP Class/profile.
- `C000`: provider/output failure, with a closed PHI-free Error Comment.

Zero-valued limits use finite defaults. The SCP defaults to 10,000 matches,
4 MiB per Identifier/response, 256 Identifier element/item nodes at depth 4,
and 1,024 response element/item nodes at depth 8. The SCU uses the same 4 MiB,
1,024-node, depth-8 response bounds and a one-second final-response drain
timeout. New matching sequence trees are admitted before constructing item
facades or cloning: at most 256 element/item nodes, depth 4 and 4 MiB of
aggregate primitive values. Builders with optional keys also apply these
aggregate Identifier bounds. Candidate matching sequences are bounded at
1,024 nodes, depth 8 and 4 MiB before decoding their values. Response projection
continues to charge the configured output element, value-byte and depth budgets.
These are finite input/output bounds, not a process RSS guarantee. Responses
are structurally checked and encoded into a bounded discard writer before any pending response bytes are committed. Provider yields are
serialized through the entire command/dataset pair, and the yield callback is
invalidated before the final response is sent.

## Examples and independent interoperability

Run the synthetic SCP and typed SCU in separate terminals:

```sh
go run ./examples/modalityworklist-scp -listen 127.0.0.1:11112 -aet MWLSCP
go run ./examples/modalityworklist-scu -address 127.0.0.1:11112 \
  -called MWLSCP -patient-name 'SYNTHETIC*' -date 20260810
```

The opt-in pynetdicom gate exercises Go as both SCU and SCP, including multiple
pending results and the single nested SPS item:

```sh
DICOMGO_PYNETDICOM_INTEGRATION=1 \
DICOMGO_PYTHON=/path/to/python \
go test ./net/dimse -run '^TestModalityWorklist(SCU|SCP)AgainstPynetdicom$' -count=1 -v
```

## DCMTK worklist fixtures

DCMTK's fictitious example database (`dcmwlm/data/wlistdb`, ten records) and
its thirteen example queries (`wlistqry`), imported under
`testdata/upstream/dcmtk` with provenance, are read by a Go reader of DCMTK's
dump format (`internal/dicomtest.ReadDCMTKDump`), together with six queries
written for dicom-go (`net/dimse/testdata/mwl_queries`: multi-valued AE,
modality, date and time ranges, PN wildcard, Patient ID). Offline, the
in-memory handler and the SCP projection must return the responses recorded in
`net/dimse/testdata/dcmtk_mwl_responses.json`. The opt-in oracle converts the
dumps with DCMTK 3.7.0 `dump2dcm` (and checks the Go reader against it), serves
the database with `wlmscpfs`, and requires `findscu` to receive the same
responses from wlmscpfs and from the dicom-go SCP for all nineteen queries;
`DICOMGO_UPDATE_GOLDEN=1` rewrites the golden file from wlmscpfs:

```sh
DICOMGO_DCMTK_INTEGRATION=1 go test ./net/dimse -run TestDCMTKWorklistOracle -count=1 -v
```

Responses are compared by their non-empty attributes, because wlmscpfs
returns requested attributes a record lacks as empty values while this SCP
omits them with `FF01`. The divergences from wlmscpfs, following DCMTK's
`twlmatch` and `twlallret` tests, are:

- Veterinary matching is an explicit Standard Extended profile in this
  implementation. DCMTK 3.7.0+DEV supports the compared veterinary keys, while
  the local released 3.7.0 binary does not support all of them. The default Go
  parser still refuses non-universal veterinary queries unless enabled.
- Return keys outside Table K.6-1: wlmscpfs drops them with `FF01` by default
  and fills them with `--enable-all-return`; this SCP refuses them with `A900`
  by default and returns them when present with `AcceptUnknownReturnKeys`.
- A requested attribute the record lacks is omitted with `FF01`; wlmscpfs
  returns it empty (and an absent Pregnancy Status as 4, unknown).
- A query without a non-universal matching key returns nothing unless
  `AllowUniversalQuery` and `UniversalQueryMatchesAll` are set.

### Optional-key loopback qualification

`TestDCMTKMWLOptionalMatchingOracle` builds three synthetic records and runs
17 queries through DCMTK `findscu` against both `wlmscpfs` and the Go SCP, and
through the typed Go SCU against `wlmscpfs`. It compares complete non-empty
response fingerprints and expected match counts, including issuer conflicts,
open/closed temporal ranges, physicians, admission, priority and veterinary
nested codes. `wlmscpfs -nse` disables automatic query-sequence expansion;
`-dfr` permits deliberately small synthetic worklist files. The full profile
was qualified with DCMTK 3.7.0+DEV on local loopback. Released 3.7.0 passed the
10 scalar cases, but its veterinary behavior differs; use the qualified DEV
profile for the complete test:

```sh
DICOMGO_DCMTK_INTEGRATION=1 DICOMGO_DCMTK_BIN=/path/to/dcmtk-dev/bin \
DCMDICTPATH=/path/to/dicom.dic \
go test ./net/dimse -run '^TestDCMTKMWLOptionalMatchingOracle$' -count=1 -v
```

Requested Procedure Code/issuer qualifier/Other Patient IDs sequence matching
is covered by synthetic offline tests, not claimed as DCMTK feature parity.
The local comparisons do not qualify modalities, live PACS/RIS databases or
clinical workflows. The original 19-query fixture oracle remains separate.

## Directory-backed worklist

`NewDirectoryModalityWorklistHandler(root, options)` serves worklist files in
the layout of DCMTK `wlmscpfs`: one subdirectory of `root` per AE Title, each
holding one item per `.wl` file (extension compared without case,
configurable). The called AE Title of the association
(`ModalityWorklistRequest.CalledAETitle`) selects the directory, or
`DirectoryModalityWorklistOptions.AETitle` fixes one. Matching is the
in-memory handler's, in file name order, so the fixtures above answer the same
through files as from memory.

- A file is a Part 10 file or a bare data set in Explicit or Implicit VR
  Little Endian (told apart by the first element's VR bytes), as `dump2dcm`
  and `wlmscpfs` write and read them.
- The directory is listed on every query. Files whose size or modification
  time changed are read again, removed files are dropped, unchanged files are
  served from memory.
- A file over `MaxFileBytes` (1 MiB) or that is not a readable data set is
  skipped and reported once to `OnFileError` (the path and the reason, never
  values). A directory with more than `MaxFiles` (10000) worklist files fails
  the query with `A700` Out of Resources.
- An AE Title that is empty, contains `..`, a path separator, `:` or NUL, or
  has no directory (including a symbolic link to one) fails with `C000` Unable
  to process (`ErrModalityWorklistUnknownAETitle`). Symbolic links and
  subdirectories inside an AE directory are never followed. `AETitles` lists
  the AE Titles with a directory, for the application's association policy.

The opt-in DCMTK oracle above also serves `wlmscpfs`'s own `.wl` files through
the directory handler and requires `findscu` to receive the same responses.

The Study Root and Patient Root C-FIND SCPs accept a lower-case
QueryRetrieveLevel by upper-casing it, as DCMTK's `tqrlevel` test expects
(PS3.5 defines CS values in upper case, so this is leniency), and answer a
missing, unknown or oversized level with `A900` (Identifier does not match
SOP Class, PS3.4 C.4.1.1.4) and an Error Comment naming Query/Retrieve Level
(0008,0052). `C000` is kept for failures of the provider itself. DCMTK
3.7.0 `dcmqrscp` also answers a missing level with `A900` but an unknown one
with `C000`.

The examples use synthetic values only. Networking is plain TCP unless the
caller supplies UL TLS configuration; authentication, authorization, audit
retention, and deployment policy remain application responsibilities.

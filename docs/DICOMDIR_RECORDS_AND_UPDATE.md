# DICOMDIR record schemas, append and refresh

The `dicomdir` package authors 34 standard Directory Record Types: PATIENT,
STUDY and SERIES, plus 31 file-bearing types. A recognized SOP Class must have
an explicit mapping in `record_types.go`; unknown SOP Classes and PRIVATE
records are refused. This is a Directory Information/selection-key contract,
not validation of every referenced source IOD or certification of every
PS3.11 application profile. Existing `WriteOptions.Profile` constraints still
apply when selected; an empty Profile writes a profile-neutral directory.

## Standard record matrix

The factual keys/types come from [PS3.3 Annex F](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/chapter_F.html),
including the [directory model](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.3.2.2.html)
and [Directory Information Module](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.4.html).
The implementation was qualified against PS3.3 2026d. Required Type 1 keys
must be nonempty; Type 2 keys must exist and may be empty. Conditional keys
use source/SOP conditions during extraction and available directory context
during strict import. VR/VM and bounded nested sequences are checked.

| Record type | Parent | Definition |
| --- | --- | --- |
| `PATIENT` | Root | [F.5.1](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.html) |
| `STUDY` | PATIENT | [F.5.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.2.html) |
| `SERIES` | STUDY | [F.5.3](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.3.html) |
| `IMAGE` | SERIES | [F.5.4](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.4.html) |
| `RT DOSE` | SERIES | [F.5.19](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.19.html) |
| `RT STRUCTURE SET` | SERIES | [F.5.20](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.20.html) |
| `RT PLAN` | SERIES | [F.5.21](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.21.html) |
| `RT TREAT RECORD` | SERIES | [F.5.22](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.22.html) |
| `PRESENTATION` | SERIES | [F.5.23](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.23.html) |
| `WAVEFORM` | SERIES | [F.5.24](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.24.html) |
| `SR DOCUMENT` | SERIES | [F.5.25](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.25.html) |
| `KEY OBJECT DOC` | SERIES | [F.5.26](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.26.html) |
| `SPECTROSCOPY` | SERIES | [F.5.27](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.27.html) |
| `RAW DATA` | SERIES | [F.5.28](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.28.html) |
| `REGISTRATION` | SERIES | [F.5.29](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.29.html) |
| `FIDUCIAL` | SERIES | [F.5.30](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.30.html) |
| `HANGING PROTOCOL` | Root | [F.5.31](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.31.html) |
| `ENCAP DOC` | SERIES | [F.5.32](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.32.html) |
| `VALUE MAP` | SERIES | [F.5.34](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.34.html) |
| `STEREOMETRIC` | SERIES | [F.5.35](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.35.html) |
| `PALETTE` | Root | [F.5.36](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.36.html) |
| `IMPLANT` | Root | [F.5.37](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.37.html) |
| `IMPLANT ASSY` | Root | [F.5.38](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.38.html) |
| `IMPLANT GROUP` | Root | [F.5.39](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.39.html) |
| `PLAN` | SERIES | [F.5.40](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.40.html) |
| `MEASUREMENT` | SERIES | [F.5.41](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.41.html) |
| `SURFACE` | SERIES | [F.5.42](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.42.html) |
| `SURFACE SCAN` | SERIES | [F.5.43](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.43.html) |
| `TRACT` | SERIES | [F.5.44](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.44.html) |
| `ASSESSMENT` | SERIES | [F.5.45](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.45.html) |
| `RADIOTHERAPY` | SERIES | [F.5.46](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.46.html) |
| `ANNOTATION` | SERIES | [F.5.47](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.47.html) |
| `INVENTORY` | Root | [F.5.48](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.48.html) |
| `WF PRESENTATION` | SERIES | [F.5.49](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_F.5.49.html) |

Root-level file records receive no invented patient/study/series. Clinical
records retain Patient/Study/Series roles and coherent SOP/meta identity.
`InventMissingKeys` retains the existing common-key invention policy; it does
not invent clinical codes, labels, dates or required Type 1 leaf data. Missing
Type 2 source fields become empty directory fields, including HP procedures.

**Surface Scan interpretation:** F.4 has a SURFACE SCAN row and F.5.43 defines
it for Surface Scan IODs, but F.4's enumerated SERIES children omit it. This
package follows those IODs' patient/study/series placement. Its schema and
byte-offset tests pass. The available external `dciodvfy` rejects the record
as an unknown enumeration; external acceptance is therefore not claimed.

## Conditions and included macros

Content Identification uses [PS3.3 10.9](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_10.9.html).
SOP references validate class/instance pairs, required hierarchy and item
cardinality. PRESENTATION validates required Referenced Series for SOP Classes
with the Presentation State Relationship Module and two Blending items when
present. WF PRESENTATION validates series relationships, homogeneous waveform
SOP Class within a series and annotation SR references; source Structured
Waveform Annotation requires Referenced Instance Sequence.

SR/KO include the root title code and only the root's HAS CONCEPT MOD targets,
with TEXT/CODE Document Content Macro data; descendant Content is excluded.
VERIFIED SR must be COMPLETE and have Verifying Observer entries. The most
recent Verification DateTime retains an effective source timezone offset.
Truncated precision is preserved; overlapping unequal precision intervals or
mixed offset/unknown-offset values that cannot establish chronology are
refused. These rules use [PS3.5 DT](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_6.2.html)
and [PS3.3 timezone context](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.12.html).

Spectroscopy copies Referenced Image Evidence Sequence (0008,9092) when
present, validating Study -> Series -> referenced SOP pairs from the
[Hierarchical SOP Instance Reference Macro](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.17.2.html).
It does not derive an unrelated acquisition-contrast condition from Image Type.
ENCAP DOC copies directory title/MIME/concept fields and the CDA identifier
when applicable; Encapsulated Document, Pixel Data and spectroscopy bulk data
are excluded from new directory selection keys.

HP checks modality/anatomic-region conditions, conditional laterality, Type 2
procedure sequences and user identification. INVENTORY checks the
[Scope of Inventory Macro](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.38.2.html),
extended matching mechanism fields and sequence cardinality. Each UID-list
item contains exactly one valid UID, including for an attribute with VM 1-n.
Universal Match (`*`) is refused; selective wildcards such as `A*` are allowed.
This validates stored directory scope structure, not query execution semantics.

Code sequences use [Table 8.8-1a](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_8.8.html)
for Code Value/Long Code Value/URN Code Value representation and check required
scheme/context-extension fields. Older chapter 8.1 prose has a discrepant
short-URN wording; this implementation follows Table 8.8-1a explicitly.
It does not consult external coding-scheme registries or CID membership, infer
whether a Coding Scheme Version is necessary to disambiguate a registry, or
verify certificates/signatures in optional reference macros. Generic VR/VM
checks on those retained optional values are not cryptographic validation.

## Owned values, append and refresh

`FileRecord.SelectionKeys()` returns a deep-owned snapshot: sequence arrays,
raw buffers and string slices are independent. Strings are decoded Unicode;
retained numeric bytes are Little Endian, including keys read from Explicit
VR Big Endian. The writer encodes text with each record's character-set context;
imported records and nested non-ASCII keys receive independent UTF-8 declarations.
Source objects/files are never edited. Duplicate selected attributes are refused.

`ParseFileSetWithOptions` strictly imports the offset graph and verifies current
sources. `PrepareFileSetUpdate(ctx, path, ids, options)` instead imports the
owned graph first, then returns an independent candidate and report, or nil
candidate/zero report on failure:

- `FileSetAppend` preserves current selection keys and transfer syntax for every
  existing physical file. Changed or missing existing sources fail; new selected
  File IDs append in caller order.
- `FileSetRefresh` permits selection keys, common parent keys and transfer
  syntax to change for selected existing files. SOP Class/Instance identities
  remain fixed. Unselected files must still agree with their references. New
  selected files append; all legacy aliases of a refreshed physical file follow.
- Existing file-set UID/ID/descriptor are preserved. Conflicting explicit options
  fail. Study/Series/SOP/File-set UID roles and parent keys remain consistent,
  including empty imported entity records. Opaque optional/private keys on
  supported imported standard records remain owned; refreshed conditional
  schema keys are replaced or removed instead of keeping stale values.

No implicit file deletion, copying or renaming occurs. A caller may explicitly
use the existing FileSet removal API before a rewrite; retired aliases/now-empty
previously populated ancestors are then omitted, while originally empty entity
records can be retained. PRIVATE record hierarchies are refused.

`WriteDICOMDIR` rebuilds coherent next/lower links, root first/last links and
MRDR targets in two passes. Every offset must land on an actual encoded Item;
old absolute offset numbers are not preserved after layout changes. Sources and
candidate revision are rechecked before bytes are written. `CommitDICOMDIR`
also checks the imported directory's held-file identity, size/mtime and SHA256
immediately before atomic replacement, then syncs the directory. The imported
baseline is immutable: after a successful publication, prepare/import again
for another update. A post-replacement sync failure returns the populated
write report with `ErrTransaction`; earlier failures do not publish a directory.
A plain io.Writer can still receive partial bytes if its I/O fails; atomic root
publication requires CommitDICOMDIR.

`WriteOptions.PublicationGuard` lets an application revalidate authorization
and additional source identities after staged bytes are synced and parsed.
It runs before the final root, retained DICOMDIR and destination checks.
Returning an error preserves the existing DICOMDIR. `WriteDICOMDIR` has no
filesystem publication and does not invoke this callback.
Concurrent commits in one process are serialized per directory identity at
this final publication boundary, including aliases of the same directory.
The callback must not call `CommitDICOMDIR` recursively for that directory.

`WriteOptions.MaxBytes` is inclusive for each encoding pass and for the
verified output. A smaller budget returns `ErrResourceLimit` before
publication. An update revalidates its previous DICOMDIR within the captured
baseline extent, so it can publish a shorter index at the new exact size.

## Explicit retired MRDR compatibility

Current DICOM retired MRDR. Compatibility follows the public historical
[PS3.3 2004 directory definitions](https://dicom.nema.org/MEDICAL/Dicom/2004/printed/04_03pu3.pdf)
(pages 812-813 and 833-834), not current-standard authoring. Both import/update
`LegacyMRDR` and `WriteOptions.LegacyMRDR` must be explicit. New FileSets author
direct references; the API does not create new MRDR alias groups.

A logical file record has MRDR Directory Record Offset (0004,1504), SOP
Class/Instance/Transfer Syntax UIDs and no direct Referenced File ID. Its
physical MRDR target has File ID, Number of References and no SOP UID fields
or next/lower links. Aliases must agree on physical identity, parent selection
keys, defined leaf keys and related SOP classes. Counts are recomputed from
live logical references and targets relocate coherently. Mismatched counts,
cycles, missing/orphan targets, direct+indirect references, ambiguous inactive
hierarchies and incompatible alias identities are refused. Zero-count inactive
MRDRs and inactive ordinary leaf records can be safely omitted; reserved
nonzero in-use flags are treated as active and emission uses FFFF.

`Records`/Statistics count physical files once. Preparation/Write reports expose
logical reference and MRDR record counts separately; `References` returns
logical aliases and resolves their targets, retaining its documented best-effort
behavior. It does not make physical MRDR records appear as clinical objects.
The writer reports a retired-compatibility diagnostic for emitted MRDRs.

## Bounds and cancellation

Zero values select finite defaults: 100,000 physical files, 400,000 directory
records, 4 MiB per record's selected keys, 64 MiB aggregate key accounting,
10,000 key elements/controls per record and depth 32 (configurable up to 128).
`WriteOptions.MaxBytes` and update `MaxBytes` default to 64 MiB per encoded/imported
DICOMDIR. Existing scan/path/index bounds still apply; source parsing has a
finite extent and selective-token bound. Requested values and containers are
admitted before materialization; per-record admission is capped by remaining
aggregate balance. Planned record counts and logical alias key multiplication
are checked before constructing rewrite graphs or cloning alias payloads.

Aggregate accounting conservatively includes retained imported payloads and
selection snapshots, admitted source keys and planned logical aliases; profile
key copies and icon payload allowances consume the remaining balance. Imported
snapshots remain retained for the FileSet lifetime, including after explicit
removal. Accounting is logical payload plus fixed element/item allowances,
not a process RSS ceiling. Existing common index metadata, decoded text caches,
parser containers, transient canonical/graph copies, profile/source bulk decoding,
output datasets and the two encoded passes can coexist. Caller-created Records/
SelectionKeys snapshots add caller-owned copies. The encoded writer has its own
byte cap; profile/icon source decoding keeps its existing separate resource model.

Context cancellation is checked during metadata token traversal, ownership,
source operations, graph validation, encoding and before publication. Some
bounded identifier-only planning/merge traversals run between context checks;
no claim is made of instant cancellation inside a single metadata decoder or
filesystem operation. Public FileSet mutations synchronize admission; a changed
revision invalidates a prepared write. Errors/diagnostics do not echo source
attribute values or UIDs.

## CLI and reproducible qualification

```sh
# Create from existing portable File IDs beneath a media root.
dcmgendir ./MEDIA
# Append only the selected new file; all existing references are still checked.
dcmgendir -append -file NEW00001 ./MEDIA
# Refresh the selected existing source's directory keys and transfer syntax.
dcmgendir -update -file EXIST001 ./MEDIA
# Explicitly preserve an imported historical MRDR graph.
dcmgendir -append -legacy-mrdr ./MEDIA
```

`-append` and `-update` are mutually exclusive. Omitting `-file` uses the bounded
root scan. `-update -skip-invalid` is refused; append skip-invalid can omit only
new scan candidates, never missing/changed imported references. Replacement
file-set identity flags are refused in update modes. `-o` writes a separate
output instead of atomic root publication. Existing profile/icon flags remain.

`go test ./dicomdir ./cmd/dcmgendir` covers all 34 directory record schemas,
root/clinical placement, conditional/nested keys, owned charset/BE values,
limits/duplicates, append/refresh refusal, changed imported-directory fingerprint,
MRDR aliases/inactive records and an independent Explicit-VR byte-offset oracle.
The 34 schema sources are deliberately not full source-IOD fixtures. Six
separate complete CT/GSPS/SR/KO/RTSTRUCT/PDF synthetic sources pass the library's
strict VR/VM and `validation/iod` checks. Export them explicitly with:

```sh
DICOMDIR_1328_FIXTURE_DIR=/tmp/synthetic-dicomdir \
  go test ./dicomdir -run '^TestMixedDirectoryCompleteSourceIODs$' -count=1
```

External qualification used dicom3tools `dciodvfy` snapshot
`1.00.20260901072548`, not DCMTK: six complete sources plus their DICOMDIR
returned exit 0 with zero errors/warnings. DCMTK `dcmmkdir` 3.7.0+DEV successfully
opened/re-wrote that directory with `+A` and `+U`; the library reimported all six
files. Its rewritten directory also passed dciodvfy with one optional ImageType
warning. These are local fixture interoperability checks, not PACS/media certification.


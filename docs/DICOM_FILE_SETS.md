# DICOM file-set authoring

The `dicomdir` package has three deliberately separate surfaces:

- the compatibility reader (`References`, `ReferencedPaths`) extracts active
  references from existing DICOMDIR datasets;
- `FileSet` indexes files that already exist below a media root, validates
  their selection keys, supports bounded recursive scans, queries, removal,
  and statistics;
- `WriteDICOMDIR` and `CommitDICOMDIR` author or transactionally publish a
  Basic Directory IOD for a validated `FileSet`.

The author never copies, renames, or rewrites referenced instances. Existing
paths must already be valid PS3.10 File IDs: one to eight components, each one
to eight characters from `A-Z`, `0-9`, and `_`. Absolute, drive-qualified,
UNC, device, traversal, symlink, case-colliding, and hard-link aliases are
rejected. `DICOMDIR` is reserved for the single root directory file.

## Building and publishing

```go
set, err := dicomdir.NewFileSet(root, dicomdir.Options{
    FileSetID: "MEDIA_01",
})
if err != nil {
    return err
}
report, err := set.Scan(ctx, dicomdir.ScanOptions{
    Policy: dicomdir.EntryReject,
})
if err != nil {
    return err
}
_, err = dicomdir.CommitDICOMDIR(ctx, set, dicomdir.WriteOptions{})
```

`EntryReject` stops at the first non-indexable entry. `EntrySkip` records a
bounded, value-free diagnostic and continues. Direct `Add` always returns its
error. Metadata indexing and selection-key extraction use finite parser limits;
pixel, document and spectroscopy bulk are excluded from the new directory keys.

Authoring applies 34 standard record schemas: PATIENT/STUDY/SERIES and 31
file-bearing types with explicit SOP mappings. Clinical records use their
patient/study/series hierarchy; the six non-patient kinds live at the root.
Required and conditional keys/macros, strict import, append/refresh and explicit
retired MRDR compatibility are described in
[record schemas and updates](./DICOMDIR_RECORDS_AND_UPDATE.md). Unknown SOP
Classes and PRIVATE record authoring are refused.

For the IMAGE profile, Patient ID, Study ID, Study Date, Study Time, Series
Number and Instance Number are Type 1 keys. Image objects often leave them
empty, and de-identified files usually lack them. A file missing any of them
is refused with `ErrMissingRequiredKey` unless `Options.InventMissingKeys` is
set (as DCMTK `dcmgpdir +I`). The option derives each missing value from the
file alone, so it is stable across runs:

- Patient ID and Study ID come from digests of the Study Instance UID.
- Series Number and Instance Number come from numbers derived from their
  UIDs.
- Study Date and Time come from the Series, Acquisition or Content Date and
  Time. When those are absent too, they are `19000101` and `000000`.

`FileRecord.InventedKeys` lists the invented keys. Reading a DICOMDIR takes
the values it records for keys its files lack. Trailing NUL or space padding
is removed from record text values.

## Application profiles and icons

`WriteOptions.Profile` names a PS3.11 Media Storage Application Profile.
Every referenced instance is read once (large values deferred) and checked
before any byte is written; a `*ProfileError` (`ErrProfileViolation`) lists
each violation as a value-free `Diagnostic` with its source index, and
`FileSet.ValidateProfile` runs the same checks as a preflight.

| Profile | Transfer syntaxes | Other rules and keys |
| --- | --- | --- |
| `STD-GEN-CD` (also DVD-RAM, BD) | Explicit VR Little Endian | Patient ID, Study Date/Time, Study ID, Series and Instance Number Type 1; Image Type and Referenced Image Sequence copied when present (PS3.11 D.3.3). |
| `STD-GEN-DVD-JPEG`, `STD-GEN-USB-JPEG` | ELE, JPEG Lossless SV1, Baseline, Extended | As STD-GEN-CD plus the Table H.3-2 keys: Patient's Birth Date and Sex, Institution Name/Address and Performing Physician when any subordinate instance has them, Rows and Columns, and Frame of Reference, synchronization, Number of Frames, acquisition time, Image Position/Orientation and Pixel Spacing (also from the Shared Functional Groups) when present. |
| `STD-GEN-DVD-J2K`, `STD-GEN-USB-J2K` | ELE, JPEG 2000 lossless-only and lossy | As the JPEG variants. |
| `STD-XA1K-CD`, `STD-XA1K-DVD` | XA: JPEG Lossless SV1 (DVD also Baseline, Extended); SC: ELE | Only XA and SC; XA up to 1024x1024 with 8, 10 or 12 bits stored and Modality XA; SC 8-bit unsigned MONOCHROME2 without overlays; the Table B.3-2 keys, Image Type required for XA, and a 128x128 icon in every IMAGE record. |

`WriteOptions.Icons` adds an Icon Image Sequence to every IMAGE record
(size `IconSize`, 1 to 128, default 64; the XA profiles fix 128). The icon is
rendered from the Representative Frame Number, or the frame one third of the
way through a multi-frame image (PS3.11 B.3.3.2), after decoding compressed
Pixel Data with `WriteOptions.Decoders` (default: the built-in decoders).
Grayscale frames use their VOI window or an automatic one, MONOCHROME1 is
inverted, and RGB, YBR and PALETTE COLOR images are reduced to luminance. The
image is scaled by area averaging to fit the square with its aspect ratio
kept, the rest black, so Pixel Aspect Ratio is 1:1; the icon is 8-bit
unsigned MONOCHROME2 with Pixel Data in OB (PS3.3 F.7). An image that cannot
be rendered fails the write with `ErrIconUnavailable`, or, with the opt-in
`BlankIconOnFailure`, gets a black icon reported in `WriteReport.Diagnostics`.

Against DCMTK 3.7.0 `dcmmkdir` (opt-in `DICOMGO_DCMTK_INTEGRATION=1`, profiles
`-Pgp`, `-Pdv`, `-Pfl` and `-Pxa`): `dcmdump` reads each DICOMDIR without
warnings, `dcmmkdir +U` loads it, checks every file against the profile and
rewrites it with the same records and values apart from its own icon pixels,
and every key `dcmmkdir` writes for the same files is present. dcmmkdir
renders icons from the first frame and stretches non-square images; this
writer follows the PS3.11 frame recommendation and keeps the aspect ratio.

## Encoding and transaction guarantees

The writer uses `object.WriteFile` with Explicit VR Little Endian. It first
encodes fixed-width zero offset placeholders, reads the actual Item offsets,
encodes again with First/Last/Next/Lower offsets, then reads the result back.
The second pass must have identical Item positions and a fully reachable,
acyclic hierarchy. Active references are compared with the File ID, ancestor
keys, SOP Class/Instance, related general SOP Class UIDs, and Transfer Syntax
of the referenced source.

`CommitDICOMDIR` creates a private temporary file in the media root, flushes
and closes it, performs strict read-back, revalidates source and destination
identity, and atomically replaces the root `DICOMDIR`. A failed preflight or
write never replaces an existing valid directory. Source identity and
selection metadata are checked again immediately before publication.

Optional descriptor File IDs must resolve to a regular, non-symlink member of
the file-set. The File-set UID is stable across parse/update and is encoded as
the Media Storage SOP Instance UID, not as a SOP Common element in the Basic
Directory dataset.

## Diagnostics, limits, and interoperability

Errors and diagnostics intentionally omit filesystem paths, DICOM values, and
UIDs. `Diagnostic.SourceIndex` is the correlation boundary for callers that
are allowed to show a local path. Default scan and parser budgets are finite;
callers may lower them through `dicomdir.Limits` and `index.Options`.

The regular test suite validates multi-patient hierarchy, exact offsets,
deterministic encoding, malformed graphs, stale sources, charset round-trips,
portable paths, transaction safety, and source immutability. An independent
pydicom gate is available when Python and pydicom are installed:

```sh
DICOM_GO_PYDICOM_DICOMDIR=1 go test ./dicomdir -run PydicomInterop -count=1 -v
```

The implementation follows DICOM PS3.3 Annex F (Directory Information) and
PS3.10 sections 8.2, 8.5, and 8.6 (file-set structure, File IDs, and DICOMDIR).

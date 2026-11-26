# DICOM De-identification

The `deid` package has two deliberately separate surfaces:

- `PlanBasicProfile` / `ApplyBasicProfile` and
  `CloneFileWithBasicProfile` are the strict, data-driven APIs.
- `AnonymizeObject` / `CloneAnonymizedFile` are compatibility helpers retained
  for existing applications. They do not by themselves assert a PS3.15 profile.

Neither surface is an automatic PHI detector. There is no OCR, semantic text
classification, or automatic burned-in annotation detection.

`AssessVisualPHIRisk` is a read-only, metadata-only helper for release
preflight. It normalizes Burned In Annotation and Recognizable Visual Features
to absent, present, or unknown without echoing malformed source values. It does
not inspect pixels and is not OCR.

## Which API to choose

For a new release or export workflow, use `CloneFileWithBasicProfile`. It
creates a detached Part 10 file, applies the current PS3.15 table recursively,
rebuilds File Meta, and leaves the source untouched. Treat its `ProfileReport`
as one input to a release decision, not as a compliance certificate.

| Need | API | De-identification guarantee |
|---|---|---|
| Produce a new file for controlled release | `CloneFileWithBasicProfile` | Recommended current Basic Profile path; selected options and residual risks are reported. |
| Preview exact actions before changing an in-memory data set | `PlanBasicProfile`, inspect `Report`, then `Apply` | Same strict engine; `PlanBasicProfile` does not mutate the source. Prefer cloning when producing an exported file. |
| Make an independent structural copy | `object.CloneFile` / `object.CloneFileWithOptions` | None. Every identifier and pixel is copied. |
| Change only Study, Series, and SOP Instance UIDs | `CloneWithRemappedHierarchyUIDs` | None. This is referential-identity plumbing, not anonymization. |
| Maintain an existing limited anonymization workflow | `CloneAnonymizedFile` / `AnonymizeObject` | Compatibility behavior only; it does not assert the PS3.15 Basic Profile. |
| Release with authorized re-identification (for example a clinical trial) | `CloneFileWithBasicProfile` with `EncryptedAttributes`; `Reidentify`, `CloneFileReidentified` or `ReidentifyPath` with a recipient key | Same profile; the originals are encrypted for the recipient certificates (see [Reversible de-identification](#reversible-de-identification-encrypted-attributes-sequence)). |
| De-identify a file too large to hold in memory | `BasicProfilePath` (or `AnonymizePath` for the compatibility helper) | Same result as the clone API written with `WriteFile`; Pixel Data is copied from the source instead of held, and the destination is replaced atomically. Pixel cleaning or burned-in masking reads the file whole. |

For path operations on Deflated Explicit VR Little Endian, metadata-only
processing inflates the dataset into a private temporary replay file. The
inflated dataset is capped at 2 GiB. The compressed dataset is capped at
2 GiB plus 1 MiB for stream framing. A header-only pass bounds serialized
metadata to 128 MiB and applies a structural token budget before reading
values. The parser defers top-level integer, Float and Double Float Pixel
Data from the replay file. The writer
recompresses the modified dataset in the original transfer syntax, preserving
the uncompressed Pixel Data bytes. The replay file is removed before
publication and on error. `WithMetadataPath` lets a synchronous preflight
borrow the same source without publishing an output file.
[PS3.5 A.5](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_A.5.html)

Do not select a helper because its name contains “clone” or “anonymized”. Select
it from the guarantee required by the destination. In-place APIs are useful for
controlled transformations, but a detached clone is safer at an archive/export
boundary because failed processing cannot partially change the source.

## Safe Basic Profile quickstart

Create one `UIDRemapper` for the complete export transaction, not one per file.
That preserves references between instances while replacing source UIDs. Start
with no retention options and add an option only when the destination policy
requires it and its dependencies (cleaner, registry, or date policy) are
available.

```go
uids := deid.NewUIDRemapper() // share across every file in this export
options := deid.DefaultBasicProfileOptions()

clone, report, err := deid.CloneFileWithBasicProfile(
	ctx, source, options, uids,
)
if err != nil {
	return err
}
if !report.Complete || len(report.ResidualRisks) != 0 {
	return fmt.Errorf("de-identification requires review: %v", report.ResidualRisks)
}

// Write clone to a neutral, restrictive, atomic destination only after the
// application's independent metadata/pixel validation succeeds. source was
// not modified.
```

The compilable [`ExampleCloneFileWithBasicProfile`](../deid/example_test.go)
uses a synthetic, non-PHI file and verifies that the source name and UID are
unchanged. The strict behavior is exercised more broadly by the
[`profile tests`](../deid/profile_test.go) and synthetic CT, MR, US, SR, SEG,
RT, waveform, encapsulated-document, and enhanced-multiframe
[`fixture tests`](../deid/profile_fixtures_test.go).

For files containing Pixel Data, the default report is intentionally
incomplete until the application deals with pixel and recognizable-feature
risk. Selecting `ProfileOptionCleanPixelData` requires a `PixelCleaner`;
selecting `ProfileOptionCleanRecognizableFeatures` requires a
`VisualFeaturesCleaner`. Those callbacks are application-owned attestations,
not built-in OCR or face detection. A workflow must review
`BurnedInAnnotation`, overlays, graphics, encapsulated documents, free text,
and any other content relevant to its modalities before release.

## Normative table and provenance

The strict planner uses 655 patterns generated from DICOM PS3.15 2026b Table
E.1-1. The generated projection records:

- standard edition: `PS3.15 2026b`;
- official source URL in `GeneratedProfileSourceURL`;
- canonical SHA-256 in `GeneratedProfileProjectionSHA256`;
- row count in `GeneratedProfileRowCount`.

`cmd/deidtablegen` reads a caller-supplied official XHTML/XML document without
network access. Generation tests recompute the canonical projection and reject
duplicate patterns, unknown action codes, or provenance drift. No tag or VR is
inferred from the synthetic dictionaries of reference projects.

When the Clean Structured Content Option is selected, the planner also uses
211 rows generated from PS3.15 2026b Table E.3.4-1. Its key is the complete
`Code Value + Coding Scheme Designator + Value Type` tuple because the same
concept can require different actions for IMAGE, WAVEFORM, COMPOSITE, or TEXT.
`GeneratedStructuredContentSourceURL`,
`GeneratedStructuredContentProjectionSHA256`, and
`GeneratedStructuredContentRowCount` expose the independently reproducible
provenance. The selected source and checksum are copied into the report.

Retired `SRT`, `SNM3`, and `99SDM` concept codes are mapped to the current
`SCT` concepts through the generated PS3.16 2026b Annex O projection required
by Section 8.3. The 30 PHI-free aliases cover every current SCT concept used by
the pinned E.3.4-1 table. `GeneratedRetiredCodeSourceURL`,
`GeneratedRetiredCodeProjectionSHA256`, and `GeneratedRetiredCodeRowCount`
expose that provenance, which is also recorded in the report.

The planner models `X`, `Z`, `D`, `C`, `U`, `K`, `Z/D`, `X/Z`, `X/D`,
`X/Z/D`, and `X/Z/U*`. Conditional actions use an injected
`AttributeRequirementResolver`. If it is absent, the default chooses a
conservative IOD-preserving result (Type 1 dummy/UID and Type 2 zero length)
without retaining the original value. Set `RequireResolvedConditional` to
reject that fallback.

## Planning and atomic application

`PlanBasicProfile` walks the complete data set and sequence tree on a detached
copy. It runs all selected policies and callbacks, constructs a redacted
`ProfileReport`, and records a source fingerprint. The source object is not
modified. `BasicProfilePlan.Apply` first checks cancellation and the source
fingerprint, then applies only already-validated changes. A stale or previously
applied plan returns `ErrStaleProfilePlan`.

The report contains tags, sequence-item indexes, action/reason codes, aggregate
counts, the normative edition/checksum, selected options, and residual-risk
codes. It never contains original or replacement values, UIDs, dates, private
creator strings, callback errors, or filesystem paths. Callbacks receive data
and are trusted in-process code; they must not log or retain PHI.

The following limits are mandatory and checked before source mutation:

- nesting depth;
- elements and sequence items;
- total materialized value bytes;
- action records and serialized report bytes;
- explicit pixel masks.

Deferred values are counted from their encoded length. Value-independent
actions such as remove, zero, or dummy remain stream-safe; actions that must
inspect a value (UID remapping, date shifting, or a cleaner callback) fail with
`ErrDeferredValueUnavailable` before mutation. Callers must explicitly
materialize those bounded inputs.

All six extension callback types use the same fail-closed guard. After a
callback returns or panics, context cancellation takes precedence over its
result; otherwise returned errors and panic values become the redacted
`ErrProfileCallback`. Failed outputs are cleared, and successful pixel-region
results are defensively copied before use. Callback error and panic text is not
copied into public errors or reports.

## Explicit options

The zero option set is the Basic Application Confidentiality Profile. The
following additive options map to the 2026b Table E.1-1 columns or explicit
cleaning attestations:

| Option | Policy |
|---|---|
| Clean Descriptors | Table `C` actions require an `ElementCleaner`. |
| Clean Structured Content | Applies Table E.3.4-1 by concept and Value Type across Content, Acquisition Context, and Specimen Preparation Step Content Item Sequences. `X`, `D`, `K`, and `C` are supported; primitive `C` actions require a cleaner. Generated PS3.16 aliases cover retired SRT-family forms of current SCT concepts; every remaining unclassified code fails closed with `ErrUnclassifiedStructuredContent` and does not produce a plan. |
| Clean Graphics | Table graphics actions require a cleaner where content must be replaced. |
| Retain Full Dates | Retains only attributes marked `K` in that column. |
| Retain Modified Dates | Requires one injected deterministic day shift per plan; DA/DT precision and DT timezone are preserved. |
| Retain Patient Characteristics | Applies only that normative option column. |
| Retain Device Identity | Applies only that normative option column. |
| Retain Institution Identity | Applies only that normative option column. |
| Retain UIDs | Retains only UIDs marked `K`; other instance/reference UIDs remain remapped. |
| Retain Safe Private | Requires an immutable, checksummed creator registry; unknown private data is removed. |
| Clean Pixel Data | Requires a caller cleaner, or is recorded by an attested `PixelRedaction` that covers every frame (see below); `BurnedInAnnotation=NO` is written only after success. |
| Clean Recognizable Visual Features | Requires a caller cleaner; `RecognizableVisualFeatures=NO` is written only after success. |

Full Dates and Modified Dates are mutually exclusive. Table option overrides
are evaluated in the normative column order. Every selected option is included
in the report and in De-identification Method Code Sequence using CID 7050.

## UIDs, dates, private data, and pixels

A caller should share one `UIDRemapper` across every file in one export so
equal source UIDs stay equal and distinct source UIDs remain distinct. Empty or
unusable source UID components are minted independently and counted as
unresolved; original UIDs never enter the report.

`NewUIDRemapper` mints random version 4 UUID UIDs (`core.NewUID`).
`NewKeyedUIDRemapper(key)` replaces each UID with `core.KeyedNameUID(key,
uid)` instead, an HMAC-SHA-256 name-based UID. Every remapper with the same
key and every run then give the same replacement, so batches de-identified
separately keep their cross-references without a stored mapping table.
Without the key a replacement reveals nothing of its source. Treat the key
as a secret of the de-identifying site: anyone holding it can confirm a
guessed source UID. UIDs minted for absent attributes stay random.

Modified Dates uses a trusted `DateShiftPolicy`. The callback sees a detached
object and returns one day offset for the whole plan, preserving relative
intervals. The offset and original dates are not reported. Time-only values are
retained because a day shift does not alter local clock time.

Safe-private rules are keyed by private group, creator string, and relative
element number. This follows the creator when its encoded block is relocated.
Each rule also requires the exact encoded VR and VM; “Other” VRs and Sequences
have VM 1 regardless of payload size. Each registry requires a version and
SHA-256 provenance digest. Unknown creator data, shape mismatches, unmatched
elements, and unused creator declarations are removed.

A retained Siemens CSA Image or Series Header (0029,xx10/xx20 of "SIEMENS
CSA HEADER"), whether kept by a safe-private rule or by a `PrivateTagPolicy`
of `AnonymizeObject`, is scrubbed with `siemens.ScrubCSA`: the bytes CSA
writers leave uninitialized (after the NUL of each name, VR and value, and
value padding), which can hold leftovers of other memory such as patient
names, become zero while the header reads the same. Headers in the DATASET,
Interfile or XML formats have no such bytes and are kept as they are; a
header that does not parse is removed. `ProfileReport.CSA` and `Report.CSA`
count scrubbed headers, cleared bytes and removed headers; the profile
records the reasons `safe-private-csa-scrubbed` and
`safe-private-csa-unscrubbable`. On the gdcmData corpus the scrub clears
every byte `gdcmclean --scrub` (GDCM 3.2.5) clears; it also clears the
fourth VR byte and value padding, which GDCM keeps.

### Operator-attested pixel redaction

`PixelTextCandidates` offers optional local proposals for visual review of
MONOCHROME or RGB preview frames. It groups contrasting small components in
the upper and lower quarters, returns stored-pixel rectangles, and leaves
the source image untouched. It accepts at most 1,048,576 pixels, returns at
most 64 proposals and checks context cancellation throughout the scan.
It performs no OCR and cannot determine whether a region contains identifying
information. Empty or accepted proposal lists never create
`PixelRedactionAttestation`. Callers retain responsibility for inspecting
all frames and selecting the final regions.

`BasicProfileOptions.PixelRedaction` removes burned-in annotation from
operator-specified rectangles (stored pixel coordinates; `Frame` is zero-based
or `PixelRegionAllFrames`). Every sample in a rectangle becomes `Fill` (zero by
default; it must fit Bits Stored and Pixel Representation). The engine uses
`pixeldata.RedactDataSet`:

- native little-endian Pixel Data is redacted in place and keeps its transfer
  syntax; big-endian native becomes Explicit VR Little Endian;
- encapsulated Pixel Data is decoded, redacted and re-encoded in the source
  transfer syntax with a registered *lossless* encoder, after which the result
  is decoded again and must equal the redacted frames exactly; without such an
  encoder (or for lossy sources such as JPEG baseline or JPEG-LS near-lossless)
  the output is native Explicit VR Little Endian and keeps Lossy Image
  Compression `01`. `pixeldata.RedactionOutputLosslessOnly` fails instead of
  storing native. Redaction never re-encodes lossily.

Because the transfer syntax may change, encapsulated redaction runs only in
`CloneFileWithBasicProfile` and `BasicProfilePath`; the Object-level APIs
return `ErrPixelRedactionRequiresFile`. A dry run over deferred Pixel Data
(`PlanBasicProfile` on a file read with `DeferPixelData`) validates geometry
and reports the outcome, but its plan cannot be applied.

`Attestation` (`AttestPixelRedaction(regions, fill)`) binds the operator's
confirmation to the exact rectangles and fill through an order-independent
digest; a mismatching attestation fails with
`ErrStalePixelRedactionAttestation`. Only an attested redaction that covers
every frame writes Burned In Annotation `NO` and adds code 113101 (Clean Pixel
Data Option) to De-identification Method Code Sequence; De-identification
Method also gains "Operator-attested pixel region redaction". An unattested
redaction or uncovered frames still change the pixels but keep the source
Burned In Annotation, record "Unattested pixel region redaction" when
applicable, and add the residual risks `pixel-redaction-not-attested` or
`pixel-redaction-frames-not-covered` (so `Complete=false`). Redaction cannot be
combined with `ProfileOptionCleanPixelData`/`PixelCleaner`. Reports carry only
counts, transfer syntax UIDs and booleans (`ProfileReport.PixelRedaction`).

### Pseudonym templates

`BasicProfileOptions.Pseudonymizer` (`NewPseudonymizer(template, secret)`)
replaces the zeroed Patient's Name and Patient ID (Table E.1-1 action `Z`
permits a dummy value) with rendered templates. Tokens are `{counter[:N]}`, a
sequence number in first-seen order within one Pseudonymizer, and
`{hash[:N]}`, upper-case hexadecimal HMAC-SHA-256 of the original identity
under a caller-held secret of at least 16 bytes, deterministic for every
Pseudonymizer with the same secret. Each non-empty field needs a token;
literals are printable ASCII without `\`, braces or `=` (and no `^` in Patient
ID); rendered values are limited to 64 characters. The identity is Issuer of
Patient ID plus Patient ID, else Patient's Name plus Birth Date
(`PatientPseudonymIdentity`). Two identities rendering the same value fail with
`ErrPseudonymCollision` instead of being merged. The Pseudonymizer keeps only
keyed digests of identities; reports count `PatientPseudonyms` and De-identification
Method records "Template pseudonyms for patient name and ID", never values.

### Reversible de-identification (Encrypted Attributes Sequence)

`BasicProfileOptions.EncryptedAttributes` makes a de-identification reversible
for authorized recipients, such as the coordinating centre of a clinical trial
(PS3.15 E.1.1 and Annex C, PS3.3 C.12.1.1.4.1). After the plan is computed,
the original value of every top-level attribute that the profile removed or
replaced is copied into the single item of a Modified Attributes Sequence
(0400,0550). A changed attribute inside a sequence stores the whole original
top-level sequence. That Encrypted Attributes Data Set is encoded in Explicit
VR Little Endian and encrypted as CMS EnvelopedData (RFC 5652) for each
certificate in `Recipients`. It is appended as a new item of the Encrypted
Attributes Sequence (0400,0500), whose Encrypted Content Transfer Syntax UID
(0400,0510) is `1.2.840.10008.1.2.1` and whose Encrypted Content (0400,0520)
holds the DER ContentInfo.

- Key transport is RSAES-OAEP with SHA-256 and MGF1-SHA-256 by default.
  `KeyTransportRSAPKCS1v15` serves recipients whose software predates OAEP.
  Each recipient needs an RSA key of at least 2048 bits. A Key Usage extension,
  when present, must allow key encipherment. Recipients are named by issuer
  and serial number, or by Subject Key Identifier with `SubjectKeyIdentifier`.
- Content encryption is AES-256-CBC by default, with AES-128-CBC or
  AES-192-CBC selectable. A fresh content key and IV are drawn from
  `crypto/rand` for every object. No key or plaintext appears in the report.
- Pixel Data (and Float/Double Float Pixel Data) is never stored, so pixel
  cleaning and redaction are not reversed. The source Specific Character Set
  is stored with the originals. The original values of the markers the profile
  writes (Patient Identity Removed, De-identification Method and its Code
  Sequence, Longitudinal Temporal Information Modified) are always stored.
  De-identification Method gains "Encrypted Attributes Sequence for authorized
  re-identification".
- `ProfileReport.EncryptedAttributes` counts the recipients and stored
  attributes and names the algorithms and transfer syntax.
- Certificates are trusted as given. The caller decides who is authorized and
  whether a certificate is current. The implementation is a clean-room,
  standard-library-only CMS writer and reader (`crypto/*`, `encoding/asn1`).

`Reidentify(ctx, obj, key)` reverses the most recent reversible
de-identification addressed to a `RecipientKey`. That key is a certificate plus
an `*rsa.PrivateKey` or any RSA `crypto.Decrypter`. `ParseRecipientKey` reads
PEM or DER, PKCS #8 or PKCS #1, unencrypted keys. Reidentify copies the originals back,
removes the markers the source did not have, and removes that Encrypted
Attributes item, or the sequence when no item remains. The item for the key is
the last one naming its certificate. Layered de-identifications therefore peel
in reverse order, and items for other recipients stay. `CloneFileReidentified`
also rebuilds File Meta, and `ReidentifyPath` streams Pixel Data like
`BasicProfilePath`. `DecryptEncryptedAttributes` returns the originals without
changing the object. Nothing changes on error.

Without a recipient private key, nothing is recoverable. A recipient who is not
named fails with `ErrRecipientNotFound`. A key that does not match its
certificate fails with `ErrRecipientKey`. A wrong key behind a certificate that
names the recipient fails with `ErrEncryptedAttributesDecrypt`, the same error
as a padding failure. Decrypted content is untrusted because any certificate
holder can produce it. It is parsed with bounded limits, must hold exactly one
Modified Attributes item, and may not restore File Meta or item delimiter tags.
The reader also accepts BER (OpenSSL's streamed output),
RSAES-OAEP with SHA-1/384/512 and Triple-DES CBC content written by older
de-identifiers. Key agreement, KEK and password recipients are not supported.
`dcmanon -encrypt-for` and `dcmanon -reidentify` expose both directions on the
command line.

### Compatibility masker

The compatibility pixel masker supports ordinary native byte-aligned layouts.
Packed pixels and subsampled YBR layouts fail with
`ErrUnsupportedPixelRedaction`; bounds are overflow-safe and no partial pixel
mutation occurs after validation failure. Encapsulated or semantically cleaned
pixels require `PixelRedaction` or a caller-owned cleaner in the strict profile
API.

The strict engine walks nested Sequences recursively. Standard attributes that
are not listed in the pinned confidentiality table are preserved, so callers
must review application-defined or newer attributes that fall outside that
table. Private creators and private data are removed by default. Retaining
private data requires `ProfileOptionRetainSafePrivate` and an immutable,
checksummed `SafePrivateRegistry`; an unknown creator, element, VR, or VM is
still removed. Dates follow the selected normative column: the default profile
removes/zeros them as directed, Full Dates retains only the table's `K` rows,
and Modified Dates requires one deterministic `DateShiftPolicy` for the plan.

## File, DICOMDIR, and application boundary

`CloneFileWithBasicProfile` replaces the Part 10 preamble, rebuilds File Meta
from the transformed data set, and leaves the source untouched. A generic `D`
action on a Sequence requires an IOD-aware `DummyValueProvider`; the engine
never invents a structurally invalid empty macro. A DICOMDIR is a multi-file
graph, so the single-file planner rejects it with
`ErrDICOMDIRPolicyRequired`. Applications must plan referenced-file renames,
all referenced instances, and directory-record updates as one transaction.
The library does not copy media trees, update catalogs, or publish files.

The underlying `object.CloneFile` operation is bounded by default and streams
the writer directly into the parser instead of retaining a second encoded copy
of the complete file. Its defaults cap individual values, Pixel Data, total
encoded bytes, nesting, elements, sequence items, and fragments.
`object.CloneFileWithOptions` lets applications choose lower limits. A limit
failure returns before a clone is exposed and never mutates the source; mutable
payload buffers in a successful clone are detached from the source.


## Validation and claims

Synthetic tests cover the action codes, recursive references, stable UIDs,
date shifting, safe-private creator relocation, pixel-layout failures,
preamble/File Meta rebuilding, cancellation, resource limits, and report
redaction. CT, MR, US, SR, SEG, RT, DICOMDIR, waveform, encapsulated-document,
and enhanced-multiframe fixtures exercise the same planner.

These capabilities do not justify “HIPAA compliant” or “fully conformant”
claims. Callers still own IOD conditions, site policy, free-text review,
application-private rule provenance, pixel/visual cleaning validation,
DICOMDIR transaction integrity, and release-specific independent interop.

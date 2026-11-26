# Digital Signature attribute-profile coverage

`dsig.CheckProfileContext` checks the attribute coverage of PS3.15 Annex C.
It does not validate signature bytes, certificate trust, RFC 3161 timestamps,
IOD Type/VM/value requirements, signatory identification, or human authority.
Call `Verify`/`VerifySignature`, trust and timestamp APIs, and `validation/iod`
separately. A `complete` report means the bounded attribute evaluation
completed; violations still return an error and appear in `Issues`.

## Normative inputs and module coverage

The normative inputs are [PS3.15 C.1](https://dicom.nema.org/medical/dicom/current/output/chtml/part15/chapter_C.html),
[C.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part15/sect_C.2.html),
[C.3](https://dicom.nema.org/medical/dicom/current/output/chtml/part15/sect_C.3.html),
and [C.4](https://dicom.nema.org/medical/dicom/current/output/chtml/part15/sect_C.4.html),
reviewed as edition **2026d**. The current links can change; the pinned official
DocBook SHA256 values are:

- PS3.15: `1eb2a9724822476b3f1f3de23d15ae655a4c7f6e9d32bf16f2d9ef76d7d8437c`.
- PS3.3: `3ae695b956ba57c490933a49220b515aeaedb5ece86f05c4bf7e94ee0fbcc6ef`.
- Retired Curve table: official [PS3.3-2004 PDF](https://medical.nema.org/MEDICAL/Dicom/2004/printed/04_03pu3.pdf),
  C.10-2, printed pages 667–668, SHA256
  `c962627e181f5a7a6d7188924aa215e30f489ee50d2a38c184b54f1699be587f`.

The checker reuses the top-level module/macro Includes from
`internal/iodmodules`, pinned to the PS3.3 SHA above. A missing/empty required
module catalog or a new source edition fails with `ErrProfileIncomplete`;
updating the IOD catalog alone does not silently redefine signature profiles.
Two local primary-table complements cover Enhanced XA/XRF Image and Waveform
Presentation State Relationship. Repeating Overlay and retired Curve groups
have explicit element lists, for even groups `6000..601E` and `5000..501E`.
Unknown elements, odd groups and groups beyond these ranges are not blanket
requirements.

C = Creator, A = Authorization, S = SR. This list requires signing attributes
**when present**, regardless of whether the module appears in the object's IOD
module table. It does not invent absent optional attributes. Signing a sequence
covers its item contents; nested child tags are not added to root AT values.

| Module or family | PS3.3 table(s) | Profiles |
|---|---|---|
| General Equipment | C.7-8 | C, S |
| General Image | C.7-9 | C, A |
| Image Pixel | C.7-11a | C, A |
| Overlay Plane / retired Curve | C.9-2 / 2004 C.10-2 | C, A |
| Graphic Annotation | C.10-5 | C, A |
| SR Document General / Content | C.17-2 / C.17-4 | C, A / C, A, S |
| Waveform / Annotation | C.10-9 / C.10-11 | C, A |
| Multi-frame Functional Groups | C.7.6.16-1 | C, A |
| Enhanced MR Image | C.8-79 | C, A |
| MR Spectroscopy instance | C.8-102 | C, A |
| MR Spectroscopy Pulse Sequence / Data | C.8-103 / C.8-106 | A |
| Raw Data | C.19-1 | C, A |
| Enhanced CT Image | C.8-114 | C, A |
| Enhanced XA/XRF Image | C.8.19.2-1 | C, A |
| Segmentation Image | C.8.20-2 | C, A |
| Encapsulated Document Series | C.24-1 | C |
| Encapsulated Document | C.24-2 | A |
| X-Ray 3D Image / Enhanced PET Image / Enhanced US Image | C.8.21.1-1 / C.8.22-3 / C.8.24.3-1 | C, A |
| Surface Segmentation / Surface Mesh | C.8.23-1 / C.27-1 | C, A |
| Structured Display / Image Box / Annotation | C.11.16-1 / C.11.17-1 / C.11.18-1 | C, A |
| Generic Implant Template family | C.29.1.1-1 through C.29.1.5-1 | C, A |
| Implant Assembly Template / Implant Template Group | C.29.2-1 / C.29.3-1 | C, A |
| Point Cloud / Enhanced Mammography Image | C.27.5-1 / C.8.31-1 | C, A |
| Tractography Results Series / Results | C.8.33-1 / C.8.33-2 | C |
| Volumetric Graphic Annotation | C.11.28-1 | C, A |
| Microscopy Bulk Simple Annotations | C.37.1-2 | C |
| Waveform Presentation State Relationship / Structured Waveform Annotation / Textual Waveform Annotation / Displayed Waveform Segment / Montage Activation / Waveform Presentation Montage | C.39.1-1 through C.39.6-1 | C |

Interpretations are explicit: C.3's plural “MR Spectroscopy modules” covers
C.8.14.1, C.8.14.2 and C.8.14.4; Creator's singular reference covers C.8.14.1.
“Implant Template Module” resolves to all five Generic Implant Template modules
in C.29.1. The two Encapsulated lists remain distinct. Attributes shared by
another required module can still be required by both profiles; the table is
not an IOD whitelist or an exemption.

Creator and Authorization require existing, signed SOP Class/Instance and
Study/Series UIDs. Creator also requires Instance Creation Date/Time when
present. A fabricated public signed-tag list cannot satisfy an absent UID.
The Base RSA algorithm/certificate/key checks are preserved; full timestamp,
trust and application obligations remain outside attribute evaluation.

### Raw Data policy

Creator-Version UID `(0008,9123)` present activates a conservative Raw Data
superset: all private creators and private data attributes in that dataset
must be signed. The library cannot infer a vendor's private raw-data payload.
This explicit superset can require unrelated private attributes. Private group
lengths and structurally forbidden groups are excluded; required UN data or a
required sequence containing UN fails as `required-unsignable`, since that
requirement cannot be fulfilled by the supported MAC stream. Private data in
other datasets is not generically required by this rule.

## Authorization declaration and migration

`CheckProfile(sig, ProfileAuthorizationRSA)` now fails closed with
`ErrProfileIncomplete`: the legacy call has no human verification context.
Use the additive options API only with a real caller declaration:

```go
report, err := dsig.CheckProfileContext(ctx, signature,
    dsig.ProfileAuthorizationRSA, dsig.ProfileOptions{
        HumanVerifiedPaths: []string{
            "ReferencedStudySequence[0].ReferencedSOPInstanceUID",
        },
    })
```

Nil `HumanVerifiedPaths` means unknown. A **nonnil empty slice** explicitly
declares that there are no additional human verified attributes. The library
never assumes this declaration and does not attest its truth or the signer's
clinical authority. `CheckProfileWithOptions` is the background-context
wrapper. `SignOptions.ProfileOptions` supplies the same declaration for atomic
preflight; failed profile checks leave the object unchanged. Creator,
Authorization and SR require main-dataset signatures; Base can still sign
sequence items. Pending signatures carry the selected item path before checks.

Paths use the existing `attrpath` grammar and must select one existing
attribute, with no wildcard or terminal item selector. Private creator paths
are resolved to canonical hexadecimal tags; report diagnostics contain only
fixed codes, tags, numeric indexes and canonical paths, never dataset values
or private creator strings. Signed parent sequences satisfy nested coverage.

`dcmsign` sign and verify accept repeatable `-human-verified-path PATH` or the
mutually exclusive `-human-verified-empty`, with Authorization profile selected.
Neither is implied by `-profile auth` or `-require-profile auth`. The CLI labels
results **attribute-profile coverage**, and refuses incomplete required
coverage. Crypto/trust/timestamp results remain separate. In particular, the
CLI's verify pass still fails an invalid signature; coverage never makes its
signature bytes valid.

## SR document conditions

SR profile signatures require the main dataset and an SR or KOS Storage SOP
Class. Verifying Observer Sequence has at most one item. Evidence sequences,
Predecessor Documents and Observation DateTime are required **if present**;
IOD validation determines whether their presence/value is otherwise mandatory.
Each signature requesting this profile has a purpose code.

For `VERIFIED`, at least one structurally eligible main-dataset candidate has
purpose `(5, ASTM-sigpurpose)`, covers the normal SR requirements plus SOP
Instance UID, Verification Flag and Verifying Observer Sequence, and includes
VerificationDateTime **inside the observer item**. The signed observer sequence
covers that nested attribute; no root `(0040,A030)` is fabricated. An additional
author/Creator signature can coexist without independently being purpose 5.
The pending candidate is evaluated together with existing candidates before
publication. All occurrences of Referenced SOP Instance MAC Sequence are
checked for the allowed MAC algorithm metadata.

`SRVerificationCandidate` records **structural coverage only**. A purpose code,
readable RSA certificate and nonempty signature blob do not prove integrity
or trust. Verify the candidate bytes and certificate independently. Tests
explicitly corrupt a candidate's signature bytes while retaining structural
coverage, and assert that `Verify` rejects its integrity.

## Resource and qualification limits

Default evaluation limits are 100,000 elements **and sequence items**, depth
64 and 256 report issues. Caller bounds may be stricter; depth cannot exceed
64. Human declarations are limited to 256 paths, each with the existing
4096-byte/64-segment grammar. SR sibling parsing is bounded to 256 signature
items and 256 MAC items, 64 KiB per candidate certificate and 1 MiB total
candidate certificate bytes. Context cancellation and any limit failure return
partial coverage and an error. Synthetic cyclic metadata graphs terminate at
the depth fence. Inputs are borrowed read-only; callers must not mutate them
during evaluation. These are metadata/logical bounds, not an RSS measurement.

Independent hard-coded fixtures cover every listed module/table/alias, Includes,
C/A asymmetries, Raw private/UN rules, parent coverage, SR/KOS conditions,
atomic signing, limits, cancellation and deterministic reports. Tiny fixtures
exercise metadata and crypto contracts; they are **not valid clinical/IOD
qualification datasets**. No external implementation code or image corpus was
copied. No performance, clinical, target-PACS or full application conformance
claim follows from these tests.

Opt-in local interop uses DCMTK dcmsign v3.7.0+DEV, OpenSSL 3.6.4, tool SHA256
`0b21f93c3e704e3e31fb6a2d414cca9df3400a05f2e84bd0242320df16870e62`.
The source reference is `8d68392db66cd69e13dc878dc9889681062ad479`; the binary's
build commit is not independently proven. With its source dictionary loaded,
Go signatures agree on positive and missing-subset negative fixtures for
Enhanced MR/CT, SEG, MRS, Raw private and Functional Groups; a verified SR with
nested observer DateTime passes. The local oracle accepts omissions of Curve
TypeOfData and Waveform Presentation Montage that the pinned current normative
checker rejects. Those are documented divergences, not reasons to weaken our
rules. `+pc/+pa/+pr` can silently add attributes while signing, so negatives
are signed here with `ProfileNone` before oracle verification.

```sh
DCMDICTPATH=/path/to/dcmtk/dcmdata/data/dicom.dic \
DICOMGO_DCMTK_INTEGRATION=1 DICOMGO_DCMTK_BIN=/path/to/bin \
go test ./dsig -run '^TestDCMTKProfile' -count=1 -v
```

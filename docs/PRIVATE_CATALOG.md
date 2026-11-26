# Complete opt-in private dictionary DATA

`dictionary/privatecatalog` imports complete pinned DATA files from DCMTK and
GDCM. Its Go normalization and lookup code are independently implemented. This
provider extends the existing private-creator resolver only when a caller passes
it explicitly. `dictionary/curated` keeps its 294 reviewed records; the standard
dictionary and default reader behavior remain unchanged.

```go
catalog, err := privatecatalog.New(privatecatalog.Options{})
if err != nil {
    return err
}
dict := dictionary.Chain{localDictionary, catalog, std.Dictionary}
reader := parser.NewReader(input, transfer.ImplicitVRLittleEndian,
    parser.ReaderOptions{Dictionary: dict})
```

Import `github.com/ThalesMMS/dicom-go/dictionary/privatecatalog` together with the
existing dictionary, std, parser and transfer packages. A local overlay placed
first keeps precedence. The package performs no external file access, network
access or writes; it reads only embedded DATA and notices. There is no global
registration or package initialization that changes another reader.

Select one source with `Options{Sources: []privatecatalog.Source{privatecatalog.DCMTK}}`
or `privatecatalog.GDCM`. Empty source selection uses both. Unknown/duplicate
source identifiers are errors. Source ordering does not choose a winning VR.

## Source and entry provenance

| Source | Pin / version | DATA SHA-256 | Rows |
| --- | --- | --- | ---: |
| DCMTK | `8d68392db66cd69e13dc878dc9889681062ad479`, `3.7.0+` | `d3907ad1c14b2a9bfb434099e0d1775246be63b58f5d06b57c6ec5a53645fe12` | 2,837 |
| GDCM | `2cd05d13222b175e791b5ae3c08feeabcc1ed1f0`, `v3.2.11-68-g2cd05d132` | `d3744ac3678d4c089742d4cfc30616301f59da0ad528708f2a51e4332ef8fea4` | 10,519 |

The complete files are byte-identical copies at
[`data/private.dic`](../dictionary/privatecatalog/data/private.dic) and
[`data/privatedicts.xml`](../dictionary/privatecatalog/data/privatedicts.xml).
[`sources.json`](../dictionary/privatecatalog/data/sources.json) records upstream
path, URL, commit, file hash, version, row count and notice identifiers. DCMTK has
132 creator identifiers; GDCM has 450. These are source counts, not the number of
unambiguous effective definitions. No GDCM generated C++ table is imported.

`Records()` returns every one of the 13,356 DATA rows, including duplicates,
opaque definitions and quarantines. A record preserves literal creator, group,
element, VR, VM and name tokens, its physical source line, exact row text, row
SHA-256, allowed VRs and disposition/reason. `Sources()` resolves its source
identifier to pinned metadata and license URLs. This reports upstream provenance,
not independently verified authorship or correctness of every vendor definition.

`Summary()` accounts for all rows by source and row disposition. `Lookup(creator,
tag)` reports the resolved entry, disposition, VM conflict flag and all matching
candidates. A row can be individually valid and participate in a VR conflict at
lookup; its literal record is retained. Unsupported/normatively invalid rows are
visible in `Records()` and `Summary()` but do not become fallback matches.

## Scoped matching and conservative resolution

[DICOM PS3.5 2026d 7.8](https://dicom.nema.org/medical/dicom/2026d/output/chtml/part05/sect_7.8.html)
governs the odd private group domain, LO creator reservations, movable blocks and
independent scopes in sequence items. Existing reader scopes enforce these rules;
the catalog cannot infer a creator from a tag, parent item or attribute name.
Case, internal spaces and punctuation remain significant, with the existing
creator helper trimming ASCII space padding.

- DCMTK two-digit elements are block-relative offsets. Full elements at or above
  `1000` are fixed to that actual element and verified creator. `ByPrivate` does
  not generalize a fixed definition to another block; `ByPrivateTag` uses the
  actual element. Odd group/element ranges retain their bounds and parity through
  the existing immutable `.dic` matchers.
- GDCM `xxNN` and `00NN` are relative offsets. `60xx` and `70xx` group patterns
  intersect the valid odd private group domain; they cannot match even standard
  overlay/curve groups. Patterns remain bounded matchers rather than expanded
  tag tables. The 11 `10xx` rows have unclear creator/data semantics and remain
  `unsupported-pattern`, rather than becoming a broad LO definition over payload.
- Gather all applicable exact, range and fixed candidates. There is no silent
  source-priority or specificity rule to hide a differing VR. Equal exact VRs
  resolve to that VR while retaining every origin. A differing VR, contextual
  token or known corpus contradiction resolves to UN. VM-only disagreement keeps
  an otherwise unanimous exact VR but makes the entry VM empty and sets
  `VMConflict`. Display names are deterministic presentation metadata, with all
  originals available in candidates. `ByTag` and `ByKeyword` always miss; private
  metadata never becomes a global absolute-tag or keyword overlay.
- All standard source VRs, including SQ/OB/OW/OF, are usable when unambiguous and
  normatively valid. A `?` name does not invent a keyword or disqualify an exact
  VR. Upstream UN stays opaque. The reader's existing resource and sequence limits
  still apply. Explicit-VR headers remain authoritative.
- `px`, `ox`, `US_SS` and `OB_OW` occur in four rows. Their alternatives remain
  inspectable, but the current private resolver transports only an exact
  `Entry.VR`. They remain UN; no unrelated standard attribute is used to guess a
  vendor-private representation. No parser/core contract was expanded.
  Fixed contextual definitions are quarantined only at their actual element;
  moving a relative definition from another source to a different block does
  not make the fixed row apply there.
- The eight known contradictions in
  [`CURATED_PRIVATE_CATALOG.md`](CURATED_PRIVATE_CATALOG.md) remain
  `known-corpus-vr-conflict`: SIEMENS MED `0021/11`; SIEMENS CT VA0  COAD
  `0019/90,92,93,96`; PHILIPS MR/PART `0019/00`, `0021/00,10`. The complete source
  rows are retained. A separately reviewed local overlay can be placed earlier
  in the chain.
  DCMTK's fixed PHILIPS MR/PART `0019/1000`, `0021/1000` and `0021/1010` rows
  are quarantined at those actual elements even with DCMTK-only selection.
  They do not follow a moved block; GDCM's relative forms remain quarantined
  wherever their creator reserves a block.

The current inputs contain 422 cross-source exact-relative VR disagreements,
111 VM disagreements (497 keys with either), and 12 internal GDCM duplicate-key
VR disagreements. Normative quarantine includes 10 forbidden-group DCMTK rows,
13 forbidden-group GDCM rows and six GDCM creators exceeding the existing 64-byte
LO limit. Contextual and quarantine records preserve reasons and provenance.

These dictionaries describe vendor private data; they are not clinical ground
truth. A recognized private definition does not authorize retention during
de-identification. The complete catalog covers substantially more source data
than the corpus-qualified curated subset; use that narrower package when its
review profile fits the caller's needs.

## Limits and ownership

`DefaultLimits()` defines these finite construction ceilings:

| Limit | Ceiling |
| --- | ---: |
| Bytes per embedded source | 4 MiB |
| Total normalized input, including equivalent-definition recovery | 8 MiB |
| Bytes per source/normalized line | 4,096 |
| Total DATA records, including duplicates | 65,536 |
| Pattern DATA records, including quarantines | 4,096 |
| Duplicate normalization partitions per source | 64 |

`Options.Limits` may lower ceilings. Zero uses the ceiling; negative values or
values above it return `ErrOptions`. Size/count checks precede buffer, record and
partition allocations. Identical tag fields are partitioned before loading
through `dictionary.ParseDIC`, preserving alternatives instead of silently using
its last-definition-wins behavior. Equivalent definitions overridden by that
loader are recovered within the same normalized-input budget. No full tag-domain
or wildcard cartesian expansion occurs.
The relative block marker `xxNN` is an offset definition, not a wildcard pattern
for this budget; group wildcards and repeating/unsupported element patterns count.

Any constructor failure returns a nil catalog. Limits are classified as
`ErrLimitExceeded`; corrupt pins/metadata are `ErrSource`. Expected unusable
upstream rows are catalog records rather than fatal load errors. The default
inventory bounds detailed candidate lists and detached snapshots. Readers keep
their own request/dataset reservation scopes and budgets.

Catalog storage is immutable. `Records()`, `Sources()`, `Summary()` and detailed
`Lookup()` return caller-owned slices/maps, including nested VR and notice
slices. Concurrent lookups share no mutable state. No accumulating lookup report
is retained. Holding a returned snapshot does not allow changing a subsequent
resolution.

## Aggregate comparison with dcm4che


The first exact-relative input-key comparison uses 8,031 rows with exact groups;
five wildcard-group rows are separately accounted for. DCMTK raw DATA keys match
2,242 rows, GDCM 5,300, and their union 5,457 (5,453 distinct keys / 241 creators).
Of the union's present rows, 4,496 have a single exact source VR agreeing with the
comparison VR, 399 have conflicting source VRs, 394 are opaque/contextual, and
168 have a single exact source VR differing from the comparison VR. Presence and
agreement measure dictionary overlap, not clinical correctness; comparison
disagreements never supply catalog overrides.

The final provider was measured at blocks `10`, `80` and `FF` for the same 8,031
exact-relative rows (8,016 have normatively valid private groups):

| Actual provider outcome | Block 10 | Block 80 | Block FF |
| --- | ---: | ---: | ---: |
| Matching candidate present | 5,448 | 5,448 | 5,448 |
| Resolved exact VR | 4,645 | 4,647 | 4,647 |
| Exact VR agrees with comparison | 4,477 | 4,479 | 4,479 |
| Exact VR differs from comparison | 168 | 168 | 168 |
| Conflicting VR / UN | 399 | 398 | 398 |
| Opaque upstream UN | 394 | 394 | 394 |
| Contextual / UN | 2 | 1 | 1 |
| Known corpus contradiction / UN | 8 | 8 | 8 |
| Invalid private group request | 15 | 15 | 15 |
| Missing definition | 2,568 | 2,568 | 2,568 |

Fixed source definitions explain why movable-block outcomes can differ; they are
not generalized to another block. 4,477 rows agree at all three sampled blocks.
VM conflict flags occur in 110 / 109 / 109 matches respectively. The five
wildcard-group comparison rows were separately evaluated over every valid
matching odd group and the same three blocks: 1,920 lookups, with 1,152 resolved
and 768 VR conflicts. All five rows have candidates somewhere; three agree in
every matching valid group/block and two remain conflicting. No wildcard DATA
is expanded in catalog storage.

The final row inventory contains 12,566 individually resolved rows, 730 opaque
rows, four contextual rows, 16 source rows for the eight known corpus keys, 29
invalid definitions and 11 unsupported `10xx` rows. Individually valid rows can
still conflict at lookup. These aggregate measurements use the qualified provider
and preserve UN for ambiguity; they do not claim that agreement establishes the
correct VR for a real vendor file.

The input-set SHA-256, calculated over sorted filename,
NUL, bytes, NUL, is
`f211e7411950ac045907a421d67be446747f30255f22e3b0f48c942831ce362e`;
the Maven mapping SHA-256 is
`b4dde6d9c526e33d7b056c9642a78bfff5e35846693fbcb836b1b03be056a57f`.

## Notices and updating DATA

DCMTK's applicable BSD-3-Clause notice is retained in
[`LICENSE.DCMTK`](../dictionary/privatecatalog/LICENSE.DCMTK). GDCM's BSD-3-Clause
notice and partly converted dicom3tools BSD-style notice are retained in
[`LICENSE.GDCM`](../dictionary/privatecatalog/LICENSE.GDCM) and
[`LICENSE.dicom3tools`](../dictionary/privatecatalog/LICENSE.dicom3tools).
`LicenseNotice()` exposes the complete applicable notices for binary
distribution. Retain attribution, conditions and disclaimers; no endorsement by
OFFIS, GDCM authors, CREATIS or PixelMed Publishing is implied. See
[`THIRD_PARTY_NOTICES.md`](../THIRD_PARTY_NOTICES.md).

Update only from exact upstream files and notices under their own licenses.
Record new commits, versions, URLs, hashes and row counts in `sources.json`;
preserve byte-identical DATA and all rows. Review new shapes/VR tokens and
conflicts, maintain known corpus quarantines, and update the reuse inventory
before merge. Recalculate aggregate coverage locally without importing dcm4che
material. Qualify source accounting, implicit-VR/default behavior, limits,
ownership and race tests; a source update is not merely a new checksum.

Synthetic acceptance fixtures cover GE, Siemens, Philips and Toshiba attributes
outside the curated subset; movable creator blocks; default UN fallback; scoped
defined/undefined-length private SQ; conflict/opaque payload preservation;
explicit-VR authority; local overlay precedence; construction limits and
detached/concurrent reads. They establish library behavior, not live vendor,
PACS or clinical validation.

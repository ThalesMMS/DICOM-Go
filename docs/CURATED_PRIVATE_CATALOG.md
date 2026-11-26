# Optional reviewed private catalog

`dictionary/curated` supplies 294 CT/MR/CR definitions through the existing
[private creator resolver](PRIVATE_CREATORS.md): 246 from DCMTK's `private.dic`
and 48 from GDCM's `privatedicts.xml` ([GDCM source](#gdcm-private-dictionary-1146)). It has no registration side
effects, network access or dependency from the default parser. Its records are
immutable; queries and detached snapshots support concurrent reads.

## Provenance and bounded coverage

The source is [DCMTK 3.6.9 private.dic](https://github.com/DCMTK/dcmtk/blob/ac002900cab167509881e5b837cdef5dcb07cd37/dcmdata/data/private.dic),
commit `ac002900cab167509881e5b837cdef5dcb07cd37`. Every record in
[`catalog.json`](../dictionary/curated/catalog.json) carries creator, odd group,
byte offset, VR, VM, upstream name, project, version, commit, source URL, complete
source SHA-256, line and exact row, license URL, restrictions and verification
status. Generated `Origin` also carries the exact row's SHA-256 (without newline).
`Name` and `Keyword` preserve the reviewed upstream token.

The initial 21 entries are listed below; offsets are
relative to the creator's reserved block and all have VM `1`:

| Creator | Group | Offset | VR | Name / keyword |
| --- | --- | --- | --- | --- |
| GEMS_ACQU_01 | 0019 | 02 | SL | NumberOfCellsInDetector |
| GEMS_ACQU_01 | 0019 | 03 | DS | CellNumberAtTheta |
| GEMS_ACQU_01 | 0019 | 04 | DS | CellSpacing |
| GEMS_ACQU_01 | 0019 | 23 | DS | TableSpeed |
| GEMS_ACQU_01 | 0019 | 27 | DS | GantryPeriod |
| SIEMENS CSA HEADER | 0029 | 08 | CS | CSAImageHeaderType |
| SIEMENS CSA HEADER | 0029 | 09 | LO | CSAImageHeaderVersion |
| SIEMENS CSA HEADER | 0029 | 10 | OB | CSAImageHeaderInfo |
| SIEMENS CSA HEADER | 0029 | 18 | CS | CSASeriesHeaderType |
| SIEMENS CSA HEADER | 0029 | 19 | LO | CSASeriesHeaderVersion |
| SIEMENS CSA HEADER | 0029 | 20 | OB | CSASeriesHeaderInfo |
| Philips MR Imaging DD 001; PHILIPS MR IMAGING DD 001 | 2005 | 20 | SL | NumberOfChemicalShifts |
| Philips MR Imaging DD 001; PHILIPS MR IMAGING DD 001 | 2005 | 2D | SS | NumberOfStackSlices |
| Philips MR Imaging DD 001; PHILIPS MR IMAGING DD 001 | 2005 | B0 | FL | DiffusionDirectionRL |
| Philips MR Imaging DD 001; PHILIPS MR IMAGING DD 001 | 2005 | B1 | FL | DiffusionDirectionAP |
| Philips MR Imaging DD 001; PHILIPS MR IMAGING DD 001 | 2005 | B2 | FL | DiffusionDirectionFH |

The final five rows represent ten entries: both spellings occur explicitly in
upstream. No general case folding or inferred creator aliases are applied.
## Corpus-driven additions


| Creator | Group | Entries | VRs |
| --- | --- | ---: | --- |
| FDMS 1.0 | 0009 | 8 | CS, LO, SH, ST, UL |
| FDMS 1.0 | 0019 | 9 | CS, IS, LO, SH |
| FDMS 1.0 | 0021 | 4 | CS, IS |
| FDMS 1.0 | 0025 | 3 | US |
| FDMS 1.0 | 0029 | 5 | CS, US |
| FDMS 1.0 | 50F1 | 2 | CS |
| Philips Imaging DD 001 | 2001 | 39 | CS, DS, FL, IS, LO, SH, SL, SS |
| Philips MR Imaging DD 001 | 2005 | 3 | CS, SH |
| SIEMENS CM VA0  ACQU | 0019 | 5 | LO, LT |
| SIEMENS CM VA0  CMS | 0009 | 4 | DA, LT, TM, UL |
| SIEMENS CM VA0  CMS | 0011 | 3 | DA, DS, TM |
| SIEMENS CM VA0  CMS | 0019 | 5 | DS, IS, LT |
| SIEMENS CM VA0  CMS | 0021 | 15 | CS, DS, IS, LT, US |
| SIEMENS CM VA0  CMS | 0029 | 4 | LT |
| SIEMENS CM VA0  CMS | 0051 | 1 | LT |
| SIEMENS CM VA0  LAB | 0009 | 2 | LT |
| SIEMENS MED | 0021 | 2 | DS, US |
| SIEMENS MR VA0  COAD | 0019 | 39 | DS, IS, LT, UL, US |
| SIEMENS MR VA0  GEN | 0019 | 32 | DS, IS, LT, UL |
| SIEMENS MR VA0  GEN | 0021 | 13 | DS, IS, LT, US |
| SIEMENS MR VA0  RAW | 0021 | 21 | DS, IS, LT |
| SPI RELEASE 1 | 0009 | 4 | LO, LT, US |
| SPI RELEASE 1 | 0011 | 1 | LO |
| SPI-P Release 1 | 0009 | 1 | LO |

The effect on the corpus: private attributes read as UN fell from 2660 to 1856
(Implicit VR files: from 1705 to 901) out of 10667, and the gdcmData sweep with
`-curated-private` gives the same status for every file as the baseline.

Rejected candidates (creator, group, offset, VR, upstream name, private.dic
line, corpus files, reason):

| Creator | Group | Offset | VR | Name | Line | Files | Reason |
| --- | --- | --- | --- | --- | ---: | ---: | --- |
| SIEMENS MED | 0021 | 11 | DS | Target | 1888 | 6 | explicit VR in the corpus contradicts the dictionary (!OB x3,DS x3) |
| Philips Imaging DD 001 | 2001 | 5F | SQ | StackSequence | 1222 | 5 | SQ: a dictionary sequence would change how implicit VR data is parsed |
| Philips MR Imaging DD 001 | 2005 | 83 | SQ | Unknown | 1305 | 5 | SQ: a dictionary sequence would change how implicit VR data is parsed |
| SIEMENS MR VA0  COAD | 0019 | D5 | IS | 2DOversamplingLines | 1968 | 5 | no reviewed name upstream |
| SIEMENS MR VA0  COAD | 0019 | D6 | IS | 3DPhaseOversamplingPartitions | 1969 | 5 | no reviewed name upstream |
| Philips MR Imaging DD 005 | 2005 | 02 | SQ | Unknown | 1311 | 4 | SQ: a dictionary sequence would change how implicit VR data is parsed |
| FDMS 1.0 | 0021 | 80 | OB | EquipmentTypeSpecificInformation | 277 | 2 | opaque VR OB adds nothing over the UN fallback |
| FDMS 1.0 | 0009 | 0C | OW | FilmUID | 256 | 2 | opaque VR OW adds nothing over the UN fallback |
| FDMS 1.0 | 0009 | 05 | OW | ImageUID | 252 | 2 | opaque VR OW adds nothing over the UN fallback |
| FDMS 1.0 | 0009 | 06 | OW | RouteImageUID | 253 | 2 | opaque VR OW adds nothing over the UN fallback |
| PHILIPS MR/LAST | 0019 | E3 | DS | Unknown | 1401 | 2 | no reviewed name upstream |
| SIEMENS CT VA0  COAD | 0019 | 90 | DS | OsteoOffset | 1655 | 2 | explicit VR in the corpus contradicts the dictionary (!OB x2) |
| SIEMENS CT VA0  COAD | 0019 | 92 | DS | OsteoRegressionLineSlope | 1656 | 2 | explicit VR in the corpus contradicts the dictionary (!OB x2) |
| SIEMENS CT VA0  COAD | 0019 | 93 | DS | OsteoRegressionLineIntercept | 1657 | 2 | explicit VR in the corpus contradicts the dictionary (!OB x2) |
| SIEMENS CT VA0  COAD | 0019 | 96 | IS | OsteoPhantomNumber | 1659 | 2 | explicit VR in the corpus contradicts the dictionary (!OB x2) |
| SPI-P Release 1 | 0009 | 16 | LO | Unknown | 2559 | 2 | no reviewed name upstream |
| SPI-P Release 1 | 0009 | 18 | LO | Unknown | 2560 | 2 | no reviewed name upstream |
| SPI-P-Private_CDS Release 1 | 0021 | 40 | IS | Unknown | 2762 | 2 | no reviewed name upstream |
| SPI-P-Private_ICS Release 1 | 0019 | 31 | LO | Unknown | 2766 | 2 | no reviewed name upstream |
| SPI-P-Private_ICS Release 1 | 0029 | 67 | LO | Unknown | 2776 | 2 | no reviewed name upstream |
| SPI-P-Private_ICS Release 1 | 0029 | 0F | SQ | Unknown | 2768 | 2 | SQ: a dictionary sequence would change how implicit VR data is parsed |
| SPI-P-Private_ICS Release 1 | 0029 | 1B | SQ | Unknown | 2770 | 2 | SQ: a dictionary sequence would change how implicit VR data is parsed |
| SPI-P-Private_ICS Release 1 | 0029 | 21 | SQ | Unknown | 2772 | 2 | SQ: a dictionary sequence would change how implicit VR data is parsed |
| SPI-P-Private_ICS Release 1 | 0029 | 4C | SQ | Unknown | 2775 | 2 | SQ: a dictionary sequence would change how implicit VR data is parsed |
| SPI-P-Private_ICS Release 1;5 | 0029 | 50 | CS | Unknown | 2806 | 2 | no reviewed name upstream |
| SPI-P-Private_ICS Release 1;5 | 0029 | 55 | CS | Unknown | 2807 | 2 | no reviewed name upstream |

Other sources evaluated: DCMTK `diconde.dic` defines ten private creators, none
of which occurs in the corpus, so nothing was taken from it. GDCM's private
dictionary is imported from its XML source, as described next.

## GDCM private dictionary

The second source is GDCM's
[`Source/DataDictionary/privatedicts.xml`](https://github.com/malaterre/GDCM/blob/2cd05d13222b175e791b5ae3c08feeabcc1ed1f0/Source/DataDictionary/privatedicts.xml),
commit `2cd05d13222b175e791b5ae3c08feeabcc1ed1f0` (`v3.2.11-68-g2cd05d13`),
file SHA-256 `d3744ac3678d4c089742d4cfc30616301f59da0ad528708f2a51e4332ef8fea4`:
10,519 `<entry/>` rows, one per line, with owner, group, element, VR, VM and a
free-text name (`gdcmPrivateDefaultDicts.cxx` is generated from it).
`HarvestedPrivate.xml` was not used: its rows have no names and are mostly UN.

`cmd/privatecataloggen -import-gdcm` reads a local copy of that file (it refuses
any other SHA-256) and writes
[`catalog_gdcm.json`](../dictionary/curated/catalog_gdcm.json) for the keys
listed in [`gdcm_selection.tsv`](../dictionary/curated/gdcm_selection.tsv);
nothing is copied by hand. Each record keeps the creator, group, offset
(`xxNN`, or `00NN`, which GDCM also uses for block-relative elements), VR, VM,
the upstream display name as `Name`, a keyword derived from it (ASCII letters
and digits, each word capitalised: "Table Speed in mm/rot" gives
`TableSpeedInMmRot`), and the line, exact row and file hash. The importer and
the generator refuse a selected key with no row or several rows, repeating
groups, names with `?` (GDCM's uncertain names, 3,624 rows are just `?`),
`<internal` placeholders or a leading digit, and SQ, OB, OW and UN
definitions. A key defined by both DCMTK and GDCM records is a conflict.


| Creator | Group | Offset | VR | GDCM name | Line | Files | Reason |
| --- | --- | --- | --- | --- | ---: | ---: | --- |
| SIEMENS MED | 0021 | 11 | DS | Target | 7737 | 6 | explicit VR in the corpus contradicts the dictionary (!OB x3,DS x3) |
| Philips Imaging DD 001 | 2001 | 5F | SQ | Stack Sequence | 6118 | 5 | SQ: a dictionary sequence would change how implicit VR data is parsed |
| SIEMENS MR VA0  COAD | 0019 | D5 | IS | 2D Phase Oversampling Lines | 8744 | 5 | name does not yield a keyword (leading digit) |
| SIEMENS MR VA0  COAD | 0019 | D6 | IS | 3D Phase Oversampling Part | 8745 | 5 | name does not yield a keyword (leading digit) |
| FDMS 1.0 | 0021 | 80 | OB | Equipment Type-Specific Information | 2663 | 2 | opaque VR OB adds nothing over the UN fallback |
| FDMS 1.0 | 0009 | 0C | OW | Film UID | 2636 | 2 | opaque VR OW adds nothing over the UN fallback |
| FDMS 1.0 | 0009 | 05 | OW | Image UID | 2632 | 2 | opaque VR OW adds nothing over the UN fallback |
| FDMS 1.0 | 0009 | 06 | OW | Route Image UID | 2633 | 2 | opaque VR OW adds nothing over the UN fallback |
| PHILIPS MR/PART | 0019 | 00 | DS | Field of View | 5811 | 2 | explicit VR in the corpus contradicts the dictionary (!IS x1,DS x3) |
| PHILIPS MR/PART | 0021 | 00 | IS | Scan Date | 5929 | 2 | explicit VR in the corpus contradicts the dictionary (!DA x1,IS x3) |
| PHILIPS MR/PART | 0021 | 10 | IS | Image Type | 5936 | 2 | explicit VR in the corpus contradicts the dictionary (!TM x1,IS x2) |
| SIEMENS CT VA0  COAD | 0019 | 90 | DS | Osteo Offset | 7274 | 2 | explicit VR in the corpus contradicts the dictionary (!OB x2) |
| SIEMENS CT VA0  COAD | 0019 | 92 | DS | Osteo Regression Line Slope | 7275 | 2 | explicit VR in the corpus contradicts the dictionary (!OB x2) |
| SIEMENS CT VA0  COAD | 0019 | 93 | DS | Osteo Regression Line Intercept | 7276 | 2 | explicit VR in the corpus contradicts the dictionary (!OB x2) |
| SIEMENS CT VA0  COAD | 0019 | 96 | IS | Osteo Phantom Number | 7278 | 2 | explicit VR in the corpus contradicts the dictionary (!OB x2) |


| Creator | Group | Entries | VRs |
| --- | --- | ---: | --- |
| FDMS 1.0 | 0009 | 1 | ST |
| PHILIPS MR/PART | 0019 | 5 | IS |
| PHILIPS MR/PART | 0021 | 5 | IS |
| Philips Imaging DD 001 | 2001 | 9 | DS, IS, SH |
| Philips MR Imaging DD 001 | 2005 | 4 | DS, FL |
| TOSHIBA_MEC_CT3 | 7005 | 24 | CS, DA, DS, LO, LT, SH, UI, UL, US |

The effect on the corpus: private attributes read as UN with the catalog fell
from 1856 to 1802 (Implicit VR files: from 901 to 847) out of 10667. The other
accepted pairs are named but keep their stored VR in this corpus: the
`TOSHIBA_MEC_CT3` attributes are written as explicit UN, which the reader keeps
(the catalog gives callers the name and the VR to decode them), and the
implicit VR files that hold the `PHILIPS MR/PART` group 0019 attributes reserve
that creator twice in the group, which the resolver refuses as conflicting.

GDCM's `privatedicts.xml` bears Copyright (c) 2006-2011 Mathieu Malaterre, and
GDCM's private dictionaries were partly converted from dicom3tools. The
[notice](../dictionary/curated/LICENSE.GDCM) keeps GDCM's `Copyright.txt`
(BSD-3-Clause) and `Source/DataDictionary/COPYRIGHT.dicom3tools` (Copyright (c)
1993-2006 David A. Clunie DBA PixelMed Publishing, BSD-style); both are returned
by `curated.LicenseNotice()` after the DCMTK notice.

`reviewed-upstream` means the definition was checked against this pinned project
source. It is not validation by the manufacturer for every scanner or software
release. There is no numerical coverage target or imported synthetic vendor-tag
generator. Unknown/UN definitions, repeating groups and fixed-block-only entries
are excluded. CSA values remain opaque OB; this package does not decode CSA blobs.

The selected DCMTK definitions use the release's BSD-3-Clause terms.
The [complete applicable notice](../dictionary/curated/LICENSE.DCMTK) preserves
copyright, redistribution conditions and disclaimer; together with the
[GDCM and dicom3tools notices](../dictionary/curated/LICENSE.GDCM) it is also
available via `curated.LicenseNotice()`. Preserve them in source and binary
distributions.

## Explicit use and auditing

```go
catalog, err := curated.New()
if err != nil { return err }
dict := dictionary.Chain{catalog, std.Dictionary}
file, err := object.ReadFileWithOptions(source, object.ReadFileOptions{
    Dictionary: dict,
})
if err != nil { return err }
defer file.Close()
record, found := catalog.Lookup("GEMS_ACQU_01", 0x0019, 0x02)
// record.Definition and record.Origin are detached values.
```

Imports are `dictionary/curated`, `dictionary`, `dictionary/std` and `object`
under `github.com/ThalesMMS/dicom-go`. Supply the dictionary when reading implicit
VR data; inspection cannot retroactively decode raw UN values. Actual block
assignment comes from each dataset/item's own creator reservation, following
[PS3.5 7.8](https://dicom.nema.org/medical/dicom/2026c/output/chtml/part05/sect_7.8.html).
Explicit VR remains authoritative. Unknown definitions preserve the resolver's
UN/raw fallback. No default dictionary, charset or privacy rule is changed.

Put a caller-owned overlay first: `dictionary.Chain{local, catalog, std.Dictionary}`.
`Lookup` reports only this catalog's origin; when an earlier overlay wins, audit
the effective definition at that provider. `ByTag` and `ByKeyword` return no match
because these definitions have no absolute tag until a creator reserves a block.
`Entries()` returns a detached slice. A known definition does not authorize
retention during de-identification; the existing basic profile removes private
creators and values, including known CSA OB data.

## Offline regeneration and verification

From the library module:

```sh
go run ./cmd/privatecataloggen
go run ./cmd/privatecataloggen -check
go generate ./dictionary/curated
go run ./cmd/privatecataloggen -check -verify-upstream /path/to/pinned/private.dic
go run ./cmd/privatecataloggen -check -verify-gdcm /path/to/GDCM/Source/DataDictionary/privatedicts.xml
go run ./cmd/privatecataloggen -import-gdcm /path/to/GDCM/Source/DataDictionary/privatedicts.xml
```

Generation reads `catalog.json` and `catalog_gdcm.json` (repeat `-source` to
name others). `-verify-upstream` checks the DCMTK records and `-verify-gdcm` the
GDCM records against the exact upstream file hash and every selected line,
and both reject competing block-relative definitions. `-import-gdcm` rewrites
`catalog_gdcm.json` from the pinned `privatedicts.xml` and `-select`
(`gdcm_selection.tsv` by default), then regenerates the Go records. Obtain those
files separately from the pinned commits; the generator never downloads them.
With `DICOMGO_REFERENCE_ROOT` naming the reference checkouts (`dcmtk/`,
`GDCM/`), `TestPrivateCatalogMatchesPinnedUpstreams` re-imports the GDCM source
byte for byte and verifies both sources from `git show` of the pinned commits.
Ordinary generation checks the committed reviewed source and recorded rows;
it does not claim to authenticate an arbitrary replacement source against the
remote project. Adding entries requires source and license review, not merely
setting a verification label.

Generation sorts by creator/group/offset and has no timestamps. `-check` compares
bytes without writing. Validation rejects duplicate/conflicting keys, duplicate
JSON fields (including case variants), unknown fields, trailing documents,
noncanonical keys, invalid/UN VR, unsupported VM syntax and incomplete provenance.
The initial source format supports fixed positive VM, ordered ranges, `N-n` and
`N-Nn`; numeric bounds are at most one million. Input is bounded to 2 MiB, nesting
to 16 and records to 4096 per source. The source format accepts reviewed DCMTK
`private.dic` rows (name equal to the keyword token) and GDCM `privatedicts.xml`
rows (display name and derived keyword); each record's row must match its
definition field by field. Invalid input leaves existing output intact; replacement follows
successful validation and formatting through a temporary file in the same folder.

Tests regenerate byte for byte, reverse input order, verify source drift and
rejected conflicts, import a synthetic GDCM excerpt, exercise all 294 definitions in blocks `10`, `1F`, `80`, `FF`,
and cover item scopes, overlay precedence, unknown/raw fallback, privacy removal
and concurrent immutable snapshots. Fixtures contain synthetic values only.

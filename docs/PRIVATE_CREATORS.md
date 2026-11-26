# Private creator resolution

`dictionary.PrivateCatalog` is an optional, immutable catalog supplied by the
caller. Its identity is `(creator, odd group, byte offset)`, independently of the
block reserved in a particular dataset. The separate, opt-in
[`dictionary/curated` package](CURATED_PRIVATE_CATALOG.md) distributes a bounded
selection of reviewed definitions with per-entry provenance and licensing,
taken from DCMTK's `private.dic` and GDCM's `privatedicts.xml`. GDCM-sourced
entries carry GDCM's display name as `Name` and a keyword derived from it.

The implementation follows [PS3.5 2026c Section 7.8.1](https://dicom.nema.org/medical/dicom/2026c/output/chtml/part05/sect_7.8.html)
and the [LO definition](https://dicom.nema.org/medical/dicom/2026c/output/chtml/part05/sect_6.2.html).
Creators occupy elements `0010..00FF` and reserve data elements `xx00..xxFF` in
the same group. Valid private groups are odd, excluding `0001`, `0003`, `0005`,
`0007` and `FFFF`. A creator is LO/VM1 in the Default Character Repertoire,
regardless of Specific Character Set. Only leading/trailing ASCII SPACE padding
is removed for matching. Case, internal spaces and punctuation remain significant;
empty identifiers, backslash, NUL, controls, escapes, non-ASCII and values longer
than 64 bytes are rejected. The same creator may reserve blocks in different
groups, but cannot reserve two blocks within one group.

## Opt-in parsing and inspection

```go
catalog, err := dictionary.NewPrivateCatalog([]dictionary.PrivateEntry{
    {Creator: "ACME", Group: 0x0011, Offset: 0x01, VR: core.VRUS,
        Keyword: "AcmeCounter", Name: "Acme counter", VM: "1"},
})
if err != nil { return err }
dict := dictionary.Chain{catalog, std.Dictionary}
file, err := object.ReadFileWithOptions(source, object.ReadFileOptions{
    Dictionary: dict,
    MaxPrivateCreators: 4096,
    MaxPrivateDiagnostics: 128,
    RejectInvalidPrivateCreators: true,
})
if err != nil { return err }
defer file.Close()
// With (0011,001F) = "ACME", this resolves the catalog's offset 01.
entry, err := file.Dataset.ResolvePrivateEntry(core.NewTag(0x0011, 0x1F01))
```

`DataDictionary` is unchanged. `PrivateDataDictionary` adds `ByPrivate`;
`PrivateCatalog.ByTag` and `ByKeyword` intentionally return no match because an
unbound definition has no absolute tag. `ByPrivate` returns a block-relative
`Entry.Tag`; `LookupScopedEntry` substitutes the actual dataset tag. `Chain`
preserves its configured order across static overlays and creator catalogs. A
matching static entry, including UN, wins over later dictionaries. Existing
chains containing only ordinary dictionaries keep their existing behavior.

The implicit-VR reader resolves verified definitions before parsing the value,
including private SQ. Every root dataset and sequence item has a fresh scope;
there is no inheritance from parent items or reuse between siblings. The same
block can therefore mean different VRs in different items. Explicit VR remains
authoritative even if it disagrees with the catalog. Inspection resolves names
and keywords without changing stored VRs. A later summary dictionary cannot
retroactively decode an opaque implicit-VR value; supply the catalog when reading
to enable that interpretation.

Parsing is single-pass in wire order, as required by ordered DICOM datasets.
Creators must precede the attributes they reserve. Late creators or conflicts do
not retroactively reinterpret already emitted tokens. A private definition is
never inferred from byte patterns, an assumed `0x10` block, another group or a
parent dataset. Without a verified definition, defined-length values remain raw
UN bytes. Existing undefined-length UN sequence grammar and preservation remain
available. Reserved/forbidden odd-group tags receive no heuristic definition.

### Vendor-specific readers

Package `realworld` reads the Philips MR private scale only when a caller
asks (`PhilipsScaleAt`): Rescale Intercept/Slope (2005,xx09/xx0A, creator
`Philips MR Imaging DD 005`) and Scale Slope (2005,xx0E, creator
`Philips MR Imaging DD 001`), definitions taken from GDCM's private
dictionary. The creator is matched exactly (in either Philips spelling),
including creators encoded as UN; a creator reserved
twice is ambiguous and ignored. The values are never applied to display or
the Modality LUT. `FloatingPointValue` implements the Philips floating point
value described by Chenevert et al. (Transl Oncol 2014).

### JSON and XML export

In Implicit VR every Private Creator is read as LO (PS3.5 7.8.1), with or
without a private dictionary, so its block resolves and an Explicit VR
transcode writes it as LO. `dicomjson` writes a Private Creator encoded as
UN as LO when its bytes are a valid creator, and a private
sequence read as UN as SQ with its items. `dicomxml` resolves each private
data element's creator the same way, keeps elements of the non-private odd
groups 0001-0007 under their own tag, and refuses, unless the caller opts in
to `RecoverPrivate` and receives each recovery, an element without a valid
creator or in a second block of a repeated creator: the Native DICOM Model
names a private element by creator and offset only. The recovery trims NUL
padding from creators, omits orphans and merges repeated blocks into the
first. Measured on gdcmData (178 strictly readable files): JSON exports all
but the four strict-charset cases; XML exports the same set with the
recovery (8 files need it), and every exported file reads back with the
same sequence items.

## Errors, bounds and mutation

Missing and unknown creators/definitions produce typed diagnostics and raw UN.
An invalid creator leaves its block unusable. Duplicate creator slots or the
same creator in two blocks make the entire affected group ambiguous; other
groups remain independent. `RejectInvalidPrivateCreators` turns invalid or
conflicting reservations into read errors; unknown definitions remain permitted.
`PrivateDiagnostics` on Reader and `ParsedPrivateDiagnostics` on Object return
detached snapshots with tags, offsets, enclosing Item offsets and typed errors,
without creator strings or attribute values. The Object report describes the
original parse and is not rewritten after edits.

The default budget is 4096 reservations per dataset/item and 128 diagnostics per
read. Negative limits fail; zero selects defaults. Budget exhaustion aborts
parsing. Diagnostics are capped and expose a truncation flag. Combine these
limits with ordinary element, depth and total-byte limits for untrusted input.
`PrivateReservationsFromElements` checks one already materialized scope and
returns the first `DefaultMaxPrivateIssues` issues (128), retaining all invalid
and conflicting state even after that list fills. A resource error returns no
usable scope. Construct reservation scopes with `NewPrivateReservations`.

Object preserves duplicate-slot ambiguity from original input even though its
ordinary element map uses last-wins semantics. Editing an unrelated creator
does not erase that ambiguity. Replacing/removing the affected slot explicitly
repairs it. Item facades retain independent mutation behavior. Catalogs support
concurrent reads; Reader, Object and mutable reservation scopes require external
synchronization when the same instance is used across goroutines. Independent
operations may share one immutable catalog.

With an active catalog, selective readers must materialize creator elements.
Streaming lifecycle hooks may observe creators, but cannot skip, defer, filter
or replace their tag/VR/value while dependent attributes are still being parsed.
Completed-item or sequence transformations and later authoring still carry the
caller's responsibility to keep reservations consistent. No automatic remapping
or reinterpretation occurs. Seek-based replay rebuilds reservation scopes when
it reparses, while direct recorded-value replay leaves scope state unchanged.

## Authoring

`Object.ReservePrivateCreator(group, creator)` reuses an existing unambiguous
reservation or inserts an LO/VM1 creator at the first unused block `10..FF`.
A block containing orphaned private data is occupied. Conflicts, malformed
reservations and exhaustion return errors without modifying the object.
`Object.PrivateTag(group, creator, offset)` returns a tag from an existing
reservation; it never creates one. Put the value using its verified VR and call
`ValidatePrivateReservations` before serialization. These helpers do not move
existing attributes or renumber blocks. Generic writing preserves explicit VRs
and raw values; it does not promote UN based on a catalog. Typed creator strings
are encoded using the Default Character Repertoire, including in UTF-8 datasets.

Private catalog entries are not a de-identification allowlist. Existing removal
and caller-attested safe-private policies remain unchanged. A known name or VR
does not establish that an attribute is safe to retain.

## Runtime dictionaries in DCMTK .dic format

`dictionary.ParseDIC` and `dictionary.LoadDICFile` load a site dictionary
(DICONDE, DICOS or local attributes) at run time from DCMTK's text format,
without regenerating or widening the curated catalog. The parser is an
independent implementation of the documented grammar; no DCMTK code or
dictionary file is embedded.

Each non-comment line has the fields Tag, VR, Name, VM and Version, separated
by tabs (runs of tabs count as one separator; spaces around fields are
trimmed). VM and Version may be omitted and default to `1` and `DICOM`.
Blank lines and lines whose first non-blank character is `#` are ignored.
Entry lines must be printable ASCII; CRLF line ends are accepted.

| Tag form | Meaning |
| --- | --- |
| `(gggg,eeee)` | One absolute tag (hexadecimal, four digits each). |
| `(gggg-hhhh,eeee)` | Range of even values only, as in `(6000-60FF,0010)`. |
| `(gggg-o-hhhh,…)`, `-e-`, `-u-` | Odd, even or all values in the range. |
| `(60xx,eeee)`, `(0028,04x0)` | PS3.6-style nibble pattern (an extension). Group patterns match even groups only; element patterns match every value. |
| `(gggg,"CREATOR",ee)` | Private definition relative to the block the creator reserves (offset `00`–`FF`). |
| `(gggg,"CREATOR",eeee)` | Private definition bound to one data element (`eeee` ≥ `1000`). |

The element part may also be a range. Private definitions need odd groups
from `0009` (a group range must use `-o-`), a valid LO creator (compared
exactly after trimming spaces, as for `PrivateCreatorID`), and an element that
is either an offset or a data element; the forbidden odd groups `0001`–`0007`
are refused. The VR is one of the 34 standard VRs or a DCMTK alias:
`xs` (US or SS), `ox`/`px` (OB or OW), `lt` (US, SS or OW) and `up` (UL).
Aliases keep their alternatives through `VRSpecByTag`, with the same default VR
as the generated standard dictionary. `na` is accepted only for group `FFFE`
items and delimiters, which have no VR. DCMTK-internal tokens such as `pi`,
lower-case VRs and unknown codes are refused with `ErrDICVR`. VM accepts `1`,
`1-3`, `1-n`, `2-2n`, `3-3n`, `n` and `2n` (the last two as `1-n` and `2-n`);
`DICEntry` reports the minimum, maximum (`-1` when unbounded) and step. A
`RETIRED_` keyword prefix is removed and, like a Version containing `retired`,
sets `Entry.Retired`. The Name field is the keyword; `Entry.Name` is derived
from its CamelCase spelling.

A later line with the same tag field overrides the earlier one (counted in
`DICReport.Overridden`). Lookups prefer an exact tag over any range, then the
narrowest matching range, then the later line; fixed-element private
definitions precede block-relative ones. `ByTag`, `ByKeyword` and
`VRSpecByTag` serve absolute definitions only. Private definitions resolve
through `ByPrivate` (block-relative) or the optional `PrivateTagDictionary`
interface, which `LookupScopedEntry`, the Implicit VR reader and object
inspection use once a dataset scope has verified the creator. A file without
private definitions does not activate private-creator resolution.

```go
site, report, err := dictionary.LoadDICFile("site.dic", dictionary.DICOptions{})
if err != nil { return err } // *DICLineError names the line and field
dict := dictionary.Chain{site, std.Dictionary}  // the file overrides the standard
// dictionary.Chain{std.Dictionary, site} only adds attributes the standard lacks.
// Several files: dictionary.Chain{laterFile, earlierFile, std.Dictionary}.
file, err := object.ReadFileWithOptions(source, object.ReadFileOptions{Dictionary: dict})
```

Strict mode, the default, fails on the first malformed line with a
`*DICLineError` wrapping `ErrDICSyntax` or `ErrDICVR`. `DICOptions.Tolerant` is
an explicit opt-in that skips malformed lines and reports each in
`DICReport.Skipped` with its line number, field and reason; callers should
surface that report. For example, DCMTK's own private dictionary contains
entries in the forbidden groups `0003` and `0005`, so it loads only in tolerant
mode, with those lines reported. Resource limits are fatal in both modes and
wrap `ErrDICResourceLimit`: file size (16 MiB, checked before reading by
`LoadDICFile`), line length (4096 bytes), accepted entries (65536), repeating
entries (4096, since ranges are scanned per lookup) and skipped lines (1024).
Zero selects these defaults and negative values fail with `ErrDICOptions`.
Diagnostics quote at most 64 bytes of the offending text. The loaded dictionary
is immutable and safe for concurrent lookups.

Tests parse the vendored standard source at run time and match every
generated standard entry, VR and contextual VR alternative. `FuzzParseDIC`
checks that arbitrary input stays within the limits, that tolerant mode fails
only on resource limits and that every accepted definition is reachable
through its lookup; its seeds are in `dictionary/testdata/fuzz/FuzzParseDIC`.

## Qualification

Local tests cover dynamic blocks, group/creator isolation, nested private SQ and
empty items, implicit LE and explicit LE/BE, explicit-VR precedence, PS3.18
JSON/XML preservation, malformed/duplicate reservations, bounded diagnostics,
authorship collisions, replay, selective/lifecycle guards, charset and concurrent
independent reads. A bounded parser fuzz target exercises malformed input.


```sh
go test -race ./dictionary ./parser ./object
go test ./parser -run '^$' -fuzz '^FuzzPrivateCreatorScopes$' -fuzztime 15s
go test ./dictionary -run '^$' -fuzz '^FuzzParseDIC$' -fuzztime 60s
```

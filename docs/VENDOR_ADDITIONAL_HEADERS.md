# Additional private header decoders

The metadata-only `vendors/headers` package decodes Philips SDS, Toshiba CT Private Data 2,
Toshiba MEC MR3 Original Data, PMTF/MEC embedded datasets and Canon PAS XML.
`DetectPrivateHeaders` resolves each tag through the actual private creator
reservation. These formats can contain personal information; decoding remains
an explicit request. Importing an object does not decode these headers.

## Known layouts

| API / kind | Creator, group and offset | Representation |
|---|---|---|
| `PhilipsSDSOf` / `philips-sds` | `Philips MR Imaging DD 002`, 2005:32 | Protocol Data SQ; each item's own creator locates 37=name, 39=type, 44=blob, 47=boolean |
| `DecodePhilipsSDS` | Protocol Data blob | Version 8 at byte 24, directory count at 28; 50-byte descriptors; float32/int32/uint32 or 81-byte strings |
| `ToshibaCT3Of` / `toshiba-ct3` | `TOSHIBA_MEC_CT3`, 7005:10 | Two implicit LE UL wrapper elements, values 12 and payload length, then embedded DICOM |
| `ToshibaMECMR3Of` / `toshiba-mec-mr3` | `TOSHIBA_MEC_MR3`, 700d:08 | Six or more counted groups, numerical record key/type, payload length and five-word signature |
| `PMTFOf` / `pmtf`, `canon-pmtf`, `toshiba-pmtf` | `PMTF INFORMATION DATA`, `CANON_MEC_MR3`, `TOSHIBA_MEC_MR3`, 0029:01; Toshiba also 02 | SQ items with creator-bound 0029:90 blobs; reverse the entire byte string before reading explicit VR LE DICOM |
| `CanonPASOf` / `canon-pas` | `CANON_MEC_MR3^10`, 700d:19 | One well-formed UTF-8 XML document |

Every sequence item resolves its own creator; a creator block in its parent is
never reused as if it were a reservation in the child. Toshiba PMTF offsets 01
and 02 are independent detected locations, including when both are present.
The decoded text and structures are owned; parsing does not change the source
object or reverse a blob in place.

SDS metadata names/types use the item's declared DICOM character set, including
inheritance from its parent. SDS v8 field names and strings admit the known
ASCII subset. Names must terminate within their 34-byte descriptor field;
payload spans must lie after the complete descriptor directory. Unknown
versions/types and Philips direct-int/direct-string records return
`ErrUnsupportedHeader`. The latter are TODO variants in the reference tool;
no undocumented encoding is inferred here.

CT3 accepts a Part 10 file with its declared syntax, or raw explicit VR little
endian data. Raw implicit/big-endian formats have no encoding declaration in
this envelope and are not guessed. PMTF accepts only the reversed raw explicit
VR little endian dataset, and uses the normal DICOM charset decoder for text.
Binary DICOM values are shown as bounded hexadecimal previews.

MEC MR3 records retain numeric group/key/type, without a copied proprietary name
map. Known types cover finite float32/float64, signed and unsigned integers,
booleans, ASCII datetime and fixed structures, Unicode strings and STR40 arrays.
`DecodeMECMR3WithOptions` supports explicit `TextEncoding` values `UTF-8`
(default), `Shift-JIS` and `EUC-JP`; no encoding heuristic is used. Unsupported
or invalid text stays an error instead of replacement characters. The legacy
`ISO8859-1` envelope marker is preserved as metadata: the reference tool decodes
that payload using guessed Japanese encodings, so its label alone is not taken
as evidence of the actual encoding. ASCII/UTF-8 is accepted by default; other
bytes require explicit encoding selection. The separate character-set record
is displayed as uninterpreted hexadecimal bytes and does not change decoding
policy. Unknown types are reported through `ErrUnsupportedHeader`.

PAS accepts one optional final DICOM NUL pad and retains the UTF-8 XML text.
Multiple roots, malformed XML, unsupported declarations, DTD/directives,
undefined entities and excessive depth are refused. The XML decoder performs
no external entity or network retrieval.

## Limits and consumers

The existing `vendors.Limits` facade and `headers.Limits` defaults remain 16 MiB stored bytes, 16 MiB
rendered/decoded text and 100000 entries/values/items. Sequence subtrees are
checked before item facades are built. Primitive budgets use the larger of the stored header length and the supported resident value size, measured without narrowing to uint32 or copying payloads. Short/inconsistent headers cannot hide larger values; unresolved or opaque value representations are refused before their methods are consulted. Direct blob lookup APIs also require a concrete resident raw value and apply this byte preflight before copying it, including when the caller supplies a missing or misleading header length. Embedded datasets count all parser tokens, including empty containers, before
constructing an object. Part 10 File Meta is preflighted before its object reader: only primitive group 0002 elements are accepted, unexpected sequence/item containers are refused immediately, and all meta primitives (including group length) consume the same entry budget as the dataset.
Declared deflated datasets are inflated into a buffer capped by both byte limits
before token preflight. Exactly one NUL pad after an odd-length DEFLATE stream
is accepted (PS3.5 A.5); other trailing bytes are refused. XML attributes consume
the entry budget in addition to tokens. Sequence
nesting and XML nesting are capped at 64 levels. Aggregate sequence input and
output limits cover all items of a decoded header. These are logical work and
value limits, not bounds on the application's resident memory or caller-owned
input. `PrivateHeaderOptions.MaxLines` additionally truncates displayed lines.

`dcmdump -decode-additional-private-headers file.dcm` explicitly selects the decoded header
view. It refuses JSON, selectors, value export and offsets in the same command.
Malformed/unsupported headers are shown with their individual error and produce
exit status 1; output failures are propagated. Default dumps are unchanged. Legacy Siemens/GE/ELSCINT/VEPRO headers remain in
the combined `vendors` API and both Twin inspectors; this CLI view selects the
new formats only. The facade delegates to the metadata-only implementation,
preserving error identities and the reusable public decoding APIs. No decoder
is duplicated, and dcmdump does not import image codecs or DICOM networking.
Both Twin inspectors use their existing shared `privateheaders` service and
show the new decoded kinds after the existing confirmation. No private-header
capability is duplicated in either frontend.

## Provenance and verification

The implementation is independently authored Go. The clean-room behavioral
reference is the local GDCM tree's `Applications/Cxx/gdcmdump.cxx` and public
MEC MR3 interfaces/types. Its algorithms, implementation and proprietary field
name dictionary were not ported. DICOM parsing, charset handling and object
ownership use this library's existing primitives.

Synthetic fixtures independently encode the known layouts and invented values.
Tests cover decoder success, relocated creator slots, item-local reservations,
malformed framing/spans/signatures/XML, explicit Japanese encoding, limits,
unchanged input, CLI output/errors and both inspector surfaces. They establish
these format contracts; they are not scanner interoperability, arbitrary
private-format coverage or clinical validation. No native Windows execution,
performance claim or CI pipeline is required or added.

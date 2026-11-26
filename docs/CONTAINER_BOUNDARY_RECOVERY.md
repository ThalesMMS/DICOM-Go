# Sequence and item boundaries


## Strict reading (default)

The reader checks every child against the innermost enclosing defined-length
container before reading it:

- an item or sequence whose declared length exceeds the bytes left,
- a primitive value or Pixel Data fragment that would cross the end,
- an element header that would cross the end (header reads are fenced at the
  container end), and
- an undefined-length item or sequence still open when its container ends

all fail with `parser.ErrContainerOverrun` inside a `*parser.ParseError` with
the offset. No byte outside the container is consumed as its content.
Previously such inputs failed later, after reading elements that followed the
sequence as if they were part of the item; the error is now earlier and
structured, and well-formed input reads exactly as before.

## Recovery (opt-in)

`parser.ReaderOptions.RecoverContainerOverruns`, or
`object.ReadFileOptions.RecoverContainerOverruns`, repairs only what can be
delimited safely:

| Anomaly | Recovery | Reason |
|---|---|---|
| Defined-length item longer than what its container has left | Read to the container end | `item-overruns-container` |
| Defined-length sequence longer than its container has left | Read to the container end | `sequence-overruns-container` |
| Item header whose body lies entirely outside the container | No item is recorded | `item-header-at-container-end` |
| Undefined-length item or sequence open at the container end | Closed there | `missing-delimiter-at-container-end` |

`object.ReadFileOptions.AllowImplicitVRFileMeta` is a separate opt-in for
File Meta Information written in Implicit VR Little Endian (a group 0002 tag
not followed by a known VR, as some GE, Theralys and early GDCM writers did).
Group 0002 is then read with the file meta dictionary on every read path,
including transfer syntax recovery without a preamble, and `File.Meta`
reports one `implicit-vr-file-meta` recovery (path `(0002,xxxx)`, offset of
the first meta element, bytes consumed). Writing always emits conforming
Explicit VR Little Endian File Meta Information with a recomputed group
length.

`object.ReadFileOptions.AllowZlibDeflate` reads a Deflated Explicit VR Little
Endian data set that starts with a zlib header (RFC 1950: CM 8, window at most
32 KiB, no preset dictionary, valid FCHECK) instead of the raw deflate stream
PS3.5 A.5 requires, as DCMTK does. The Adler-32 trailer is verified, and the
data set reports one `zlib-deflate-wrapper` recovery (path `(dataset)`, offset
of the compressed data set). Raw deflate is read unchanged with the option on,
and writing always emits raw deflate.

`RecoverImplicitVRElements` (parser and `object.ReadFileOptions`) handles
data sets that mix Explicit and Implicit VR, typically explicit public and
implicit private elements. When the two VR bytes of an Explicit VR Little
Endian element are not a known VR, they and the next two bytes are read as an
Implicit VR length and the VR comes from the dictionary. The switch is taken
only if that length fits the enclosing defined-length container and the
source, never for Pixel Data, and at most `MaxImplicitVRElements` times
(default 4096, then `ErrMaxImplicitVRElements`). Each switch is reported with
reason `implicit-vr-element`, the element's path and offset. A wrong but known
VR is kept as written (DCMTK's default); preferring dictionary VRs is a
separate option.

`RecoverDelimiterAnomalies` (parser and `object.ReadFileOptions`) repairs
sequence framing from broken encoders, capped by `MaxDelimiterRecoveries`
(default 4096, then `ErrMaxDelimiterRecoveries`):

| Anomaly | Recovery | Reason |
|---|---|---|
| `(FEFF,00E0)`, `(FEFF,0DE0)`, `(FEFF,DDE0)` inside a sequence or item | Read as byte-swapped Item/delimiter tags; an item opened this way is read as Explicit VR Big Endian and its numeric values are stored in the stream's byte order | `swapped-item-tag` |
| Item or Sequence Delimitation Item with a nonzero length | Accepted; no value is consumed | `delimiter-length-ignored` |
| Delimiter with no open undefined-length item or sequence (for example after a defined-length sequence), or an item header nested in an item that ends exactly at that item's end | Skipped | `orphan-delimiter-skipped` |
| `(FFFE,0000)` group length (UL, 4 bytes) inside a sequence | Skipped | `item-group-length-skipped` |

Depth, element and byte limits apply as without recovery.

`VRRecovery` (`parser.VRRecoveryOptions`, also on `object.ReadFileOptions`)
holds independent repairs of VR headers and selected malformed value lengths.
Each applied repair is a boundary recovery with the element path, the recorded
offset, `DeclaredVR` and `UsedVR`:

| Flag | Repair | Reason |
|---|---|---|
| `MapOXToOW` | The non-standard VR `OX` on Pixel Data is read as OW | `ox-pixel-data-read-as-ow` |
| `PreferDictionaryVR` | A valid explicit VR of a public element that differs from its single dictionary VR is replaced by the dictionary VR (DCMTK `dcmPreferVRFromDataDictionary`) | `dictionary-vr-preferred` |
| `PreferDictionaryLengthFieldSize` | The length field is read with the 2- or 4-byte size of the dictionary VR (DCMTK `dcmPreferLengthFieldSizeFromDataDictionary`) | `dictionary-length-field-size` |
| `CorrectPrivateULLength` | A private UL declared with value length 6 is read with length 4 (Siemens Leonardo; GDCM's workaround) | `private-ul-length-6-read-as-4` |
| `ShortLengthUN` | UN written by GDCM 1.x with a 16-bit length: nonzero reserved bytes are the length, or a 32-bit length that cannot fit the source is re-read as 16-bit | `short-length-un` |
| `CorrectGETextLength` | Public string-like VR in Implicit VR Little Endian declared with VL 13 but structurally proven to contain 10 bytes | `ge-text-length-13-read-as-10` |

`CorrectGETextLength` requires ten printable ASCII bytes and an ordered, known
public tag at +10, with no known public tag at the declared +13 boundary.
Manufacturer and Institution Name are excluded. A fixed lookahead of at most
82 bytes validates chains of at most four malformed text fields ending at a
normal defined, even-length header; it never treats VL=13 as sufficient proof.
Source, container and configured byte/element limits still apply. Ambiguous,
private, unknown or truncated boundaries remain refused, and lookahead bytes
are restored for both seekable and non-seekable readers. The diagnostic records
the absolute value offset and lengths 13/10, without the text. This repair is
included in the explicit `object.ReadFileRecovering` VR-repair ladder attempt;
strict reading remains unchanged.

Other value lengths that are not a multiple of the VR size are still read as
raw bytes without change; the CP-246 UN recovery (`RecoverUN`) and the odd
length policy are separate.

`SequenceRecovery` (`parser.SequenceRecoveryOptions`, also on
`object.ReadFileOptions`) holds independent repairs in the style of DCMTK's
parser flags and GDCM's NOSHADOWSEQ reading, each reported as a boundary
recovery:

| Flag | Repair | Reason |
|---|---|---|
| `UndefinedLengthOBOWAsSQ` | OB/OW other than Pixel Data with undefined length is read as SQ when its content starts with an Item tag (DCMTK `dcmConvertUndefinedLengthOBOWtoSQ`) | `undefined-length-ob-ow-read-as-sq` |
| `VOILUTSequenceOWAsSQ` | VOI LUT Sequence written with a VR other than SQ is read as SQ (DCMTK `dcmConvertVOILUTSequenceOWtoSQ`) | `voi-lut-sequence-read-as-sq` |
| `ReplaceWrongDelimiter` | A Sequence Delimitation Item where an undefined-length item must end is read as an Item Delimitation Item, and vice versa (DCMTK `dcmReplaceWrongDelimitationItem`) | `wrong-delimiter-replaced` |
| `PrivateSequenceAsBytes` | A defined-length private SQ is parsed from a buffered copy first; if it does not parse, it is kept as a UN value with its original bytes | `private-sequence-kept-as-bytes` |

Implicit VR private elements with an undefined length are always read as
sequences (VR UN, PS3.5 6.2.2), which is what DCMTK's
`dcmReadImplPrivAttribMaxLengthAsSQ` enables. No repair consumes bytes outside
the enclosing container, and depth limits apply unchanged.

Content that still does not fit fails exactly as in strict mode: a value, a
fragment or a header crossing the (possibly clamped) end is never truncated,
padded or invented, and a clamped item whose last element ends past the
container is refused. Limits (element bytes, elements, depth, total bytes),
cancellation and deferred values work as without recovery.

Every repair is listed, with no values, by `Reader.BoundaryRecoveries` and
`Object.BoundaryRecoveries`: the structural path of the repaired child (for
example `(0004,1220)[51]`), the absolute value offset, the declared length
(-1 when undefined), the bytes that were available, and the reason. The list is
capped by `MaxBoundaryRecoveries` (default 128) with a truncation flag. It is
taken when the read completes, so anomalies found at the very end of a data set
are present. An empty list means the data set parsed without anomalies; a
non-empty one means the result is a recovery, not a conforming file.

## Partial reads (opt-in)

`object.ReadFileOptions.AllowPartialRead` keeps what was read when the data
set cannot be read to the end (a truncated value, header, open sequence or
encapsulated Pixel Data fragment, or any other structural error after the
file meta). The reader returns the File built from completed elements and
sequence item prefixes, together with a `*object.PartialReadError` that matches
`object.ErrPartialRead`, `object.ErrDataSet` and the original cause. The
primitive value or encapsulated Pixel Data open at the failure is dropped
whole. Interrupted SQ/item prefixes are retained only when they contain
completed components, and are explicitly marked incomplete by `Path` and
`Containers`; no truncated value is ever exposed. Context cancellation is never
turned into a partial read.

`File.PartialRead()` reports the offset and tag of the failure (when the parser
knows them), complete/root-entry counts, structural path, incomplete containers
and the error. See [partial sequence reads](./PARTIAL_SEQUENCE_READ.md) for
retention, source-reference and explicit-rewrite details. A partial File is
never valid: `object.WriteFile` refuses it with `object.ErrPartialFile` unless
`WriteFileOptions.AllowPartial` confirms it, and pixel data decompression
refuses it outright. The same behavior applies to seekable and non-seekable
sources. `parser.Reader.ReadDataSetPartial` exposes the same result at the
parser level.

## Evidence


pydicom and amrshadid/go-dicom accept such files silently or with a log
warning; here acceptance is opt-in and reported. That difference is deliberate
and is not a claim about the standard.

## Terminal Pixel Data inspection

The separate [incomplete Pixel Data profile](INCOMPLETE_PIXEL_DATA.md) may retain
actual final root native bytes or complete encapsulated Items at physical EOF,
with an explicit partial error. It does not recover nested Pixel Data, container
fences, work limits, cancellation or arbitrary I/O failures. Boundary/VR/sequence
repair switches cannot be combined with this profile. Generic partial sequence
reads continue to drop interrupted Pixel Data atomically. Complete image frames
require a second explicit bounded decompression normalization.

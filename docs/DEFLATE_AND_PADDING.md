# Dataset Deflate level and trailing padding

`object.WriteFileWithOptions` and `object.WriteDataSetWithOptions` accept
`DeflateLevel *int` and `Padding PaddingOptions`. Zero options preserve the
previous compression level and existing padding. These controls affect
serialization, not image-frame compression, pixel samples or identity.

```go
level := 6
err := object.WriteFileWithOptions(destination, file, object.WriteFileOptions{
    DeflateLevel: &level,
    Padding: object.PaddingOptions{
        Policy:        object.PaddingCreate,
        Alignment:     512,
        ItemAlignment: 32,
        MaxAddedBytes:  16 << 20,
    },
})
```

The file's transfer syntax must be Deflated Explicit VR Little Endian for an
explicit level. Nil selects Go's `flate.DefaultCompression`; explicit values
0 through 9 select stored blocks through best compression. Other values, or
an explicit level for another syntax, fail before the Part 10 prefix is
written. Each writer snapshots the pointed-to value at entry. Callers must
not mutate options concurrently with entry. There is no process-wide level,
global compressor pool or level for encapsulated Deflated Image Frames here.

PS3.5 A.5 requires one raw RFC1951 stream for the entire Explicit VR Little
Endian dataset. File Meta is uncompressed. A single NULL byte follows the
stream if its encoded size is odd. Level selection preserves this rule;
there is no zlib header or Adler-32 trailer. Levels need not produce strictly
monotonic sizes, and different implementations need not produce identical
compressed bytes.

## Padding policies and domains

`PaddingPolicy` and `PaddingOptions` are shared with `parser` through aliases
in `object`:

| Policy | Behavior |
| --- | --- |
| `PaddingPreserve` (zero) | Keep existing (FFFC,FFFC) values and create none. |
| `PaddingRemove` | Omit existing (FFFC,FFFC) recursively. |
| `PaddingCreate` | Replace existing padding and append an OB element of NULL bytes only when alignment requires one. |

Padding leaves the source Object, nested DataSets and deferred values intact.
It changes neither Pixel Data Item boundaries nor the Basic/Extended Offset
Tables. Existing insignificant padding values need not be zero under Preserve;
newly created values are zero.

`Alignment` counts the logical Part 10 preamble, magic, File Meta and dataset.
For `WriteDataSetWithOptions`, its origin is dataset offset zero because there
is no Part 10 prefix. `ItemAlignment` counts only the Item Value: its 8-byte
Item Header and any 8-byte Item Delimiter are excluded. A sequence item's
value includes its nested sequences and their headers/delimiters. Empty Item
Values are already aligned and receive no padding.

With Deflate, both alignments count the encoded DICOM bytes **before
compression**. Padding is an element inside the compressed dataset. The
physical compressed file size is not guaranteed to be an alignment multiple;
the writer neither searches for a compressed size nor adds arbitrary bytes
after the stream. The A.5 NULL remains independent of dataset padding.

Alignments must be zero or even uint32 values (2 through 4294967294). Nonzero
alignments require Create. Zero disables creation in that domain; Create with
both zero removes old padding and creates none. The calculation includes the
padding OB header and any newly generated Group Length FFFC header/value. OB
Value Length is even and defined; overflow of its 32-bit representation or
its Group Length is an error.

## Bounds, streaming and Group Length

`MaxAddedBytes` is a shared budget for all generated padding in the write,
including its headers/values and associated generated FFFC Group Length.
Zero selects `DefaultMaxPaddingBytes` (16 MiB); negative values fail before
output. The writer reserves the complete addition before its first header,
then writes zeros in at most 4096-byte chunks. It never allocates a buffer the
size of an alignment. Existing padding retained by Preserve is not charged
as newly added data. The budget limits added padding, not source dataset size.

Undefined items continue to stream deferred/bulk values. Top-level padding
counts bytes as they are written and does not replay/decode frames to measure
their size. Defined items and sequences now calculate a bounded metadata layout and emit
directly, with generated padding included in declared lengths. Pure length
planning does not spend the actual write's padding budget. See
[defined sequence streaming](DEFINED_SEQUENCE_STREAMING.md) for source metadata,
limits, hook phases and the qualified normalization/isolation exceptions.

Group Length Preserve keeps received values unchanged, including any stale
FFFC Group Length when padding changes. Recalculate/Create produce the encoded
length of the new padding element; Remove omits Group Length. Create and
Recalculate omit an old FFFC Group Length if no padding is needed or the
domain's alignment is zero. PaddingRemove omits padding and lets the selected
Group Length policy handle the remaining attributes. Create together with
recalculated/created Group Length rejects other attributes in the reserved
FFFC group, beyond (FFFC,0000)/(FFFC,FFFC), rather than emitting a duplicate or
misordered Group Length.

Defined deferred primitive values can be measured from their header and
declared length. An undefined encapsulated value now requires recorded encoded-length metadata
when Group Length or a defined ancestor needs its size; it is not replayed
for measurement. Reader/frame providers expose that metadata; unsupported
custom providers are refused explicitly. FrameSource is pulled once even
with Group Length. Generic object/parser writes can fail after partial
output. A caller requiring cancellation can supply a context-aware Writer;
these APIs do not add a context parameter.

Low-level parser callers writing a complete dataset can use
`PrepareDataSetElements`, write its returned elements, and then call
`WriteDataSetPadding` with the encoded Value length, prefix length and original
elements. Nested items are handled by Writer itself. Invalid padding options
are rejected by `ValidatePaddingOptions` and by writes.

## TranscodePath and dcmconv

`pixeldata.TranscodeOptions.WriteOptions object.WriteFileOptions` configures
only final `TranscodePath` serialization. All path routes apply it: equivalent
copy, streaming encode/decode, serial/parallel and materialized fallback.
`TranscodeFile`, `TranscodeDataSet` and other materialized APIs return objects;
their caller chooses serialization with `object.WriteFileWithOptions`.
WriteOptions is ignored by those APIs and does not configure their codecs.

TranscodePath snapshots the level and preamble before work, validates level
and padding before opening input, and keeps its existing context, finite
limits, private staging, verification and publication behavior. Added padding
also counts toward `MaxOutputBytes`. Failures/cancellation preserve an existing
destination and clean up private files. Existing bounded spool routes in serial transcode decoding remain; this writer
change does not refactor those pipeline choices. Default serial decoding keeps
its one-frame route. No extra source decode is needed for padding alone.

The CLI accepts:

```text
dcmconv -ts deflated -compression-level 0 \
  -padding create -pad-file 512 -pad-item 32 \
  -max-padding-bytes 16777216 -o output.dcm input.dcm
```

`-compression-level -1` keeps the default; `0..9` requires `-ts deflated`.
`-padding preserve|remove|create` defaults to Preserve. `-pad-file`/`-pad-item`
default to zero and use the logical domains described above.
`-max-padding-bytes 0` selects 16 MiB. Help states these domains explicitly.

## Qualification and references

Focused complete tests for parser/object/pixeldata/dcmconv passed under current
Go, race and Go 1.22.12; vet and whitespace checks passed. New tests cover all
levels and nil, raw Deflate roundtrip/A.5 tail, empty streams and write errors,
defined/undefined/preserved item lengths, alignment sweeps, UN implicit item
encoding, charset, shared budget, Group Length measurement, source immutability,
deferred values, frame-source replay, path routes and publication failures.

An isolated DCMTK 3.7.0+ DEV build with zlib 1.2.12 accepted 37 generated files
with `dcmdump -E -Ep` and `dcmconv +te`. These include all ten levels plus nil,
defined/undefined nested items, native/Deflate padding and Group Length
policies. Every non-padding/non-Group-Length primitive value, including Pixel
Data, was identical after reconversion. An independent Python raw-Deflate/
Explicit LE walker checked logical alignment, Item Value lengths, zero padding,
FFFC Group Length and the mandatory final NULL. Source serialization was
identical before/after the campaign.

The preliminary 30-cell DCMTK behavior probe confirmed Item Value and logical
Deflate alignment. Some peer outputs omitted the final A.5 NULL on an odd raw
stream; dicom-go retains the normative rule. DCMTK also did not emit FFFC Group
Length in these probe outputs, even with `+g`; our FFFC lengths were therefore
checked independently, not declared byte-identical to the peer. This is
serializer/fixture qualification, not complete IOD conformance or PACS proof.

- [PS3.5 A.5: Deflated Explicit VR Little Endian](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_A.5.html)
- [PS3.5 7.5: Item and Sequence encoding](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_7.5.html)
- [PS3.10 7.2: Data Set Trailing Padding](https://dicom.nema.org/medical/dicom/current/output/chtml/part10/sect_7.2.html)
- [DCMTK public dcmconv options](https://support.dcmtk.org/docs-353/dcmconv.html), confirmed against current public CLI declarations and measured behavior.


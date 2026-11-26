# Bulk selection and source-reference JSON export

`ReadFileOptions.BulkDataDescriptor` and `parser.ReaderOptions.BulkDataDescriptor`
add selection by parsed VR, encoded length and complete attribute path. A nil
descriptor preserves existing threshold/tag, Pixel Data and Waveform policies.
Rules are combined by OR; they add to those policies. There are no implicit
standard-tag defaults in a descriptor.

```go
descriptor := &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{
    {VR: core.VROB, Threshold: 1 << 20},
    {VR: core.VROW, Threshold: 1 << 20},
    {Threshold: 1024, Path: []parser.BulkDataPathStep{
        {Tag: core.NewTag(0x0008, 0x1115), ItemIndex: -1},
        {Tag: core.NewTag(0x0042, 0x0011), ItemIndex: -1},
    }},
}}
file, err := object.OpenFileWithOptions(filename, object.ReadFileOptions{
    BulkDataDescriptor: descriptor,
    MaxElementBytes: 128 << 20, MaxPixelDataBytes: 256 << 20,
    MaxTotalBytes: 512 << 20, MaxSequenceDepth: 32, MaxElements: 100000,
})
if err != nil { return err }
defer file.Close()
err = dicomjson.EncodeContext(ctx, destination, file.Dataset, dicomjson.Options{
    MaxMaterializedValueBytes: 8 << 20,
    Limits: dicomjson.Limits{MaxJSONBytes: 16 << 20, MaxElements: 100000,
        MaxSequenceDepth: 16, MaxSequenceItems: 10000},
    BulkDataReferenceURIFunc: func(ctx context.Context, ref object.BulkDataReference) (string, error) {
        // Application-owned storage/URI policy; this receives no payload bytes.
        return referenceURIFor(ctx, filename, ref)
    },
})
```

`referenceURIFor` is an application function, not a library API. It must give the
resource the lifetime and representation required by its consumers. Closing the
input after export does not create persistent storage behind that URI.

## Selection contract

`Threshold` is nonnegative, in bytes, and matches strictly `length > Threshold`.
Empty VR matches all eligible VRs; empty path matches all paths. A nonempty path
matches the whole path: each ancestor SQ tag plus zero-based item index, then the
leaf tag with index `-1`. Ancestor index `-1` is a wildcard; a concrete reference
always contains actual indexes. A root path consists of its leaf alone.

Readers validate at most 1024 rules and 65 path steps per rule before copying
any rule/path. They snapshot caller slices. Changing the descriptor later does
not change an active reader; concurrent mutation during construction is outside
the contract. Negative thresholds, ineligible specified VRs, invalid indexes,
control tags and excessive rule/path counts fail with
`parser.ErrInvalidBulkDataDescriptor`.

Eligible VRs are those that permit BulkDataURI in
[PS3.18 F.2.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part18/sect_F.2.2.html):
DS, FL, FD, IS, LT, OB, OD, OF, OL, OV, OW, SL, SS, ST, SV, UC, UL, UN, US, UT and UV.
Only defined-length primitive values are selected. SQ containers, parsed UN
sequences, fragment Item values and Private Creator reservations stay inline.
File Meta stays materialized even when dataset options contain a descriptor.
Existing `DeferPixelData` can additionally provide a reference to an undefined
length encapsulated Value Field.

Pixel Representation, Bits Allocated and LUT Descriptor stay inline in every
descriptor scope, including nested items, because contextual VR interpretation
needs them. Independent legacy deferral options or validation hooks retain their
own policies; a hook that explicitly defers required metadata must supply its
interpretation policy.
Selection uses the parser's header VR. With Implicit VR, contextual US/SS starts
with the dictionary's US alternative; object parsing can then resolve it to SS
from the nearest Pixel Representation scope. The callback and JSON fallback use
that final VR. A rule specifying SS alone does not match an implicit provisional
US header; use an attribute path or the header VR to select that attribute.
Relabeling is accepted only for the recognized implicit US-to-SS contextual case.

An explicit descriptor requires an `io.ReadSeeker`, a standard non-deflated
encoding and no frame sink. Nonseekable inputs fail before payload reads;
deflated inputs are rejected because inflated positions are not original file
offsets. Legacy recovery encodings are also refused for descriptor selection:
their normalization can depend on metadata selection would otherwise defer.
Existing legacy deferral policies continue to work, including original-byte
references and normalized Pixel Data callback/fallback bytes.

Source extent, container, odd-length, element count, element/Pixel Data/vector
byte limits and total-byte limits still apply before deferral. Deferral disables
the full-source slurp optimization, including values encountered late in a file.
It does not make malformed lengths acceptable or remove traversal budgets.

## Reference and lifetime contract

`Object.DeferredBulkDataReference(path)` returns metadata only for an eligible,
recorded deferred occurrence in that root object. Materialized values and
preexisting external `core.BulkDataValue` URIs are not offered to the new callback.
Duplicate source tags within the same item fail. The JSON exporter retains the
root provider while traversing item facades; callers cannot attach a deferred SQ
from another source with identical numeric offsets and obtain a valid reference.
The provider retains original ancestor item identities only for nested deferred
bulk. Top-level-only deferral allocates no identity map. Path changes and foreign
item arrays are rejected; normal facades and their local COW changes retain the
root's original binding. Empty items are not represented by a nil identity.

The reference exposes tag, final VR, concrete owned path, physical `Offset`,
encoded `Length`, original `ByteOrder`, effective `ExplicitVR` and source transfer
syntax UID. Offset is relative to `SeekStart(0)` of the original source, even when
reading starts after a prefix or at a Part 10 dataset offset. It is not the
parser's `ValueLocation.ValueOffset` coordinate. Length excludes the element
header and includes BOT/fragment Item headers and sequence delimiter for an
encapsulated Pixel Data reference. `NormalizationWidth` describes the reversal
unit needed for this object's canonical fallback bytes; zero means none.

`CopyBulkDataValueTo(ctx, path, writer)` streams original encoded bytes and
restores the source cursor. CP-246 implicit Little Endian values inside a Big
Endian object retain their original source order in the reference; fallback
normalizes them before using the object's configured JSON interpretation.
The existing Pixel Data stream callback retains its canonical byte contract.
Its normalization uses bounded chunks and at most one pending numeric unit.

Sources are borrowed by `ReadFile`/`ReadDataSet`; `OpenFile`/`OpenDataSet` retain
their owned handles while deferred values remain. Keep the source open and
immutable, and serialize access during parsing/export/replay. A metadata lookup
checks seek availability without reading payload bytes, but cannot detect
same-size source edits or caller mutations of aliased dataset backing. Preserve
parsed metadata, payload and dataset backing while using source references.
The identity map rejects replacing SQ with a different source's item arrays;
it does not detect arbitrary in-place edits to an original array or importing a
top-level nil-valued element from another source. Such edits do not transfer that
source's provider: the retained provider belongs to the original root. Do not
use deferred references after those mutations. The original item bindings
remain reachable with the provider; `Object.Close` releases it. `File.Close`
closes its handle with the existing replay-error behavior. The callback receives
an owned path but no source ownership, file name, handle or serving mechanism.

An offset is not automatically a compliant HTTP bulk resource. For textual or
numeric bulk, the application must implement the appropriate retrieval media
type and charset/value conversion; charset-dependent JSON text is UTF-8 under
[PS3.18 F.2.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part18/sect_F.2.2.html).
The reference describes encoded source bytes, not preconverted JSON text.

## Empty URI, limits and cancellation

A nonempty reference URI completes the element without replaying its bytes.
Callback errors preserve their cause. Returning an empty URI tries the existing
Pixel Data stream callback, then the byte callback/InlineBinary or JSON Value
conversion as applicable. Numeric and text Value fallbacks retain their existing
behavior; they do not call the historical byte callback. Text uses the current
item's inherited charset and `TextOptions` without mutating the dataset.

`Options.MaxMaterializedValueBytes` limits each deferred fallback before
allocation or replay, including numeric and text VRs. Zero preserves historical
unlimited behavior; negative values are invalid. Set a finite value for untrusted
inputs or callbacks that may return empty URI. Existing InlineBinary and total
binary limits are also charged before replay when InlineBinary is the fallback;
accepted reference/stream URIs do not consume those binary budgets. Replay
allocates one payload buffer only after validating the declared size. Text/numeric
JSON conversion and the serialized output have additional costs governed by the
conversion limits. Context is checked before/after callbacks and between replay
writes; it cannot interrupt an arbitrary blocking source Read or callback.
Discard partial writer output after any error.

## Verification and opt-in memory campaign

Focused synthetic tests cover VR boundaries, OR/path/wildcard selection, descriptor
snapshotting, source prefixes, repeated nested tags, source identity, closed
sources, limits/truncation, signed implicit scopes, inherited Latin-1 text,
CP-246, legacy Pixel Data normalization, external URIs, empty fallback and
cancellation. The standard and dcm4che's
[BasicBulkDataDescriptor](https://github.com/dcm4che/dcm4che/blob/acbbb85eee1a2fcc441eb2d8e65023fece2b40ec/dcm4che-core/src/main/java/org/dcm4che3/io/BasicBulkDataDescriptor.java)
were behavior references; no implementation code was copied.

`TestBulkReferenceMemoryCampaign` is opt-in through
`DICOM_BULK_CAMPAIGN_MODE=reference|bytes` and `DICOM_BULK_CAMPAIGN_BYTES`.
It creates a bounded synthetic file containing one deferred bulk value in SQ,
then exports five identical JSON documents. File creation/parsing is outside the
timed/allocation interval. Both modes emit the same URI and JSON bytes; the byte
callback must materialize the payload while the reference callback uses metadata.
Process RSS includes fixture setup and runtime overhead. This is a local export
comparison, not HTTP transport or live-server validation.


| Encoded bulk | Callback | Allocated bytes/export | Process peak RSS |
| --- | --- | ---: | ---: |
| 8 MiB | Existing bytes callback | 8,427,443 | 43,499,520 B |
| 8 MiB | Source reference | 3,962 | 17,874,944 B |
| 64 MiB | Existing bytes callback | 67,147,022 | 152,780,800 B |
| 64 MiB | Source reference | 3,789 | 17,645,568 B |

At 64 MiB, median allocation bytes fell 99.994% and process peak RSS fell 88.45%.
Reference export allocation stayed approximately constant when the bulk grew
eightfold. RSS is a whole-process high-water mark: the bytes mode at 64 MiB
ranged from 152,436,736 to 220,086,272 B, while the reference mode ranged from
17,629,184 to 17,858,560 B. Fixture setup, GC/runtime and file-buffer overhead are
included in RSS; the allocation interval excludes setup and parsing. These
numbers show the accepted-URI export saving for the controlled local fixture.
They do not measure bulk serving, fallback materialization or transport.

# Native DICOM XML conversion

`dicomxml` reads and writes the PS3.19 Native DICOM Model with the PS3.18
`application/dicom+xml` media type. `DecodeContext` reads XML tokens and builds
DICOM values directly. `EncodeContext` traverses an `object.Object` and writes
attributes directly. Neither path builds or serializes an intermediate DICOM
JSON document.

`Marshal`/`Unmarshal` remain the byte-slice convenience APIs; `Encode`/`Decode`
accept a writer/reader. The context variants check cancellation during traversal,
reader/writer operations, and binary chunks. A blocking caller-provided `Read`,
`Write`, resolver or callback still needs to cooperate with cancellation.
An encoder error can leave partial XML in the writer; discard that output.

## Values and compatibility

Text is decoded from the XML declaration through `CharsetReader`; Native Model
text becomes Unicode `core.StringValue`, while `SpecificCharacterSet` and
`TextOptions` stay with the resulting Object. Encoding uses the Object's text
accessors, including inherited sequence character sets. PN alphabetic,
ideographic and phonetic groups, empty values, literal text delimiters, DS/IS
lexical validation, AT byte order, and exact SV/UV integers are preserved.
Floating-point spellings retain the prior conversion's decimal rules and
nonfinite values. Sequences retain item order and dataset tags are sorted.

`InlineBinary` and `BulkData` of OW, OF, OD, OL and OV values are Little
Endian. A Big Endian object's values are reversed while they stream into the
base64 encoder, at the VR width or, for OW Pixel Data with Bits Allocated 32
or 64, at the sample width; the legacy `BulkDataURIFunc` receives a reversed
copy. Decoding with a Big Endian `ByteOrder` reverses them in place, taking
Bits Allocated from the same data set or item (`Walk` sees it only when it
precedes Pixel Data). `SourceByteOrderInlineBinary` keeps the object's byte
order in both directions.

Private data retains its `privateCreator` identity and normalized XML tag.
Decoding allocates creator blocks at dataset completion, so a creator may follow
its data and occupied creator slots remain reserved. Encoding's private/text
recovery remains opt-in and reported through the existing callbacks. XML 1.0
character restrictions and strict namespace, keyword, VR and value-form checks
remain in force.

`PreserveBulkData` performs no I/O. `RejectBulkData` rejects the reference when
encountered. `ResolveBulkData` closes the resolver's reader and bounds returned
bytes; ordinary Decode resolves after constructing the Object, with allocated
private tags. Encapsulated Pixel Data requires an encapsulated transfer syntax
and is parsed through the existing DICOM parser, without a JSON/base64 bridge.

## Consume attributes without retaining an Object

```go
err := dicomxml.WalkContext(ctx, reader, dict, opts, func(a dicomxml.Attribute) error {
    // Consume a.Element.Value here, then discard or retain it as needed.
    // a.PrivateCreator identifies normalized private tags in a.Element.Header.
    return consume(a)
})
```

`Walk`/`WalkContext` deliver top-level attributes in XML order. The callback owns
the delivered value. Top-level private data uses the normalized `gggg00ee` tag
from XML plus `PrivateCreator`; it cannot have its final block assigned before
later creator declarations are known. Nested sequence items already have their
own allocated tags. Under `ResolveBulkData`, the resolver sees that normalized
tag for top-level private references, and allocated tags inside sequence items.

The walker retains tag/creator metadata to check duplicates and allocation at
the end. A sequence attribute still materializes its complete items before the
callback. Callbacks may precede a later XML or duplicate-tag error: only a nil
return from Walk confirms completion. A callback error stops reading and remains
accessible through `errors.Is` and the enclosing `*dicomxml.Error` path.

## Memory and limits

The existing finite `Limits` apply to source XML bytes, individual values,
inline/total binary bytes, dataset elements, sequence depth and items. Private
creators synthesized from `privateCreator` count toward the final element
budget; encapsulated fragments share the item budget. Bounds are checked before
accepting another attribute/item or binary chunk. Errors retain XML paths and
sentinels, including reader errors and context cancellation. The source reader
may have a small tokenizer read-ahead; a one-byte probe distinguishes an exact
`MaxXMLBytes` boundary from overflow.

Encoding inline binary uses fixed base64 buffers and 32 KiB source chunks;
typed binary values use a 4 KiB work buffer. The legacy `BulkDataURIFunc` accepts
a complete `[]byte`, so enabling it can still materialize one deferred or
fragmented Value Field before the callback decides whether to emit a URI.
Returning a URI preserves the existing offload policy without charging the
inline binary budget.

Decoding stores the final Object, whose size remains proportional to its
content. It retains per-dataset allocation metadata and one attribute under
construction. Inline binary is decoded through 32 KiB/24 KiB work buffers into
its final bytes, without accumulating a second complete base64 string. Go's
`encoding/xml` still buffers a complete XML token, including a large contiguous
`CharData` token; the source-byte limit bounds the input, but this is not a
constant-memory SAX parser or a hard process-RSS limit. Resolved bulk content
and encapsulated parser output also require their resulting value bytes.

## Local measurements

The package includes `BenchmarkNativeXMLStreaming` and an opt-in process memory
probe. Its synthetic metadata case has 4,000 SQ items with LO and maximum UV
values. Its inline case has an 8 MiB OB Value Field. No patient data or external
peer is used.

```sh
go test ./dicomxml -run '^$' -bench '^BenchmarkNativeXMLStreaming$' -benchmem
go test -c -o /tmp/dicomxml.test ./dicomxml
DICOMXML_MEMORY_PROBE=Decode/inline /usr/bin/time -l /tmp/dicomxml.test \
    -test.run '^TestNativeXMLMemoryProbe$' -test.count=1
```

The process probe separately records input XML bytes, logical primitive-value
bytes, operation time, allocations, sampled heap peak, and retained heap after
GC. `/usr/bin/time -l` supplies the process high-water RSS on macOS. RSS includes
fixture construction and runtime overhead; retained heap includes allocator and
cache effects, and the sampled peak can miss short spikes. These measurements
do not establish a deployment memory bound or a universal performance ratio.

Example observations:

| Operation | Wall time, before → after | Allocated bytes, before → after | Process peak RSS, before → after |
| --- | ---: | ---: | ---: |
| Decode metadata | 60.76 → 21.73 ms | 100.41 → 15.72 MB | 50.94 → 31.78 MB |
| Decode inline 8 MiB | 138.31 → 90.49 ms | 250.77 → 41.96 MB | 157.58 → 86.21 MB |
| Encode metadata | 23.31 → 9.25 ms | 34.36 → 11.04 MB | 44.50 → 30.85 MB |
| Encode inline 8 MiB | 35.61 → 17.66 ms | 67.20 MB → 9.88 KB | 94.63 → 60.75 MB |

MB/KB above are decimal; the inline payload is 8,388,608 bytes. Metadata XML is
1,995,098 bytes and its primitive values total 1,184,000 bytes. Inline XML is
11,185,031 bytes. Decode's retained heap delta after GC was approximately
2.16 MB for metadata and 8.40 MB for inline binary, versus 2.08 MB and 8.43 MB
before. Thus the reduction is in conversion work, not elimination of the final
Object. Sampled transient heap (peak minus post-GC retained heap) changed from
20.70 to 6.62 MB for metadata Decode and from 111.88 to 25.17 MB for inline
Decode. These samples are lower bounds on transient peaks, not independent
measurements of every allocation's lifetime. Other workspace work could run on
the host; this short comparison is not a controlled system-wide ranking.

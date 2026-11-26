# DICOM JSON streaming

The streaming APIs preserve the existing DICOM JSON object conversion rules
while avoiding retention of an entire result array. They are an addition to
`Marshal`/`Unmarshal` and the resident DICOMweb client methods; the resident APIs
and XML negotiation remain available.

## Object encoder and decoder

```go
encoder := dicomjson.NewEncoder(writer, dicomjson.Options{OmitGroupLength: true})
err := encoder.EncodeContext(ctx, obj)

err = encoder.EncodeArrayContext(ctx, func() (*object.Object, error) {
    // Return one object, or io.EOF after the last object.
    return nextObject(ctx)
})

decoder := dicomjson.NewDecoder(reader, std.Dictionary, dicomjson.UnmarshalOptions{})
obj, err = decoder.DecodeContext(ctx)
err = decoder.DecodeArrayContext(ctx, func(obj *object.Object) error {
    return consumeObject(ctx, obj)
})
```

`EncodeContext` writes exactly the compact or pretty bytes produced by
`MarshalContext`, including escaping, property order and `omitempty`, without
an added newline. Array encoding writes one top-level JSON array. Conversion
uses the same charset, private implicit VR, numeric byte order, bulk-data
callback and encapsulated Pixel Data rules as the resident object API.

The decoder reads one dataset at a time and rejects trailing JSON values or
malformed array endings. Each top-level dataset must be a JSON object. An empty
dataset is `{}`. Duplicate top-level tag keys are rejected, including equivalent
case or whitespace spellings. Errors for an invalid member shape or duplicate
tag include its zero-based array position.

`DecodeDatasetsContext` delivers the represented `dicomjson.Dataset` shape,
retains numeric tokens as `json.Number`, and validates VR-specific representation
before invoking the callback. `DecodeRawDatasetsContext` also retains numbers and
uses the ordinary JSON conversion into `Element` fields. It bounds JSON bytes
and cumulative top-level elements and leaves additional VR and sequence
validation to its caller. Object decoding reconstructs binary numeric fields
and encapsulated pixels according to the explicit conversion options.

Callbacks and producers run synchronously. Earlier successful callbacks remain
consumed when a later dataset, limit, callback, cancellation or trailing JSON
fails. There is no replay or rollback, and there are no reader/worker goroutines.
Callers requiring atomic application changes must stage their own consumption.
The APIs are not safe for concurrent use; inputs must not be mutated during
conversion. They never close the supplied writer/reader. An arbitrary blocked
reader or callback must cooperate with cancellation; context checks alone cannot
interrupt an uncooperative `io.Reader`.

Limits apply cumulatively to an array: JSON bytes, elements, sequence items and
binary values. Sequence depth uses the existing finite default. A successful
callback cannot be delivered for a dataset whose consumed JSON bytes exceed the
configured byte limit. A decoder may read ahead up to the admitted byte limit
plus one detection byte. Limits bound admitted input and traversal, not exact
process RSS. Zero limit fields preserve the resident API's historical unlimited
behavior except for sequence depth; applications should set finite limits.

Streaming retains at most one represented/converted dataset plus decoder/output
buffers. One dataset, value array or inline binary field can still be large;
this is not constant memory independent of the largest dataset. Object encoding
still materializes the converted representation for that single dataset. The
existing PixelDataBulkDataURIFunc supports offloading deferred pixels without
collecting their complete Value Field; bulk URIs are not fetched automatically.

## Represented Dataset writer

`EncodeDatasetContext(ctx, writer, dataset, DatasetOptions)` validates and writes
compact metadata directly, without constructing a binary DICOM object or
marshaling/reparsing the entire dataset. `ValidateDatasetContext` applies the
same checks without publication. Numeric validation reuses the resident VR
parser's exact integer widths, float forms and DS/IS rules. SQ items use bounded
recursive traversal, and generic map field presence/order is preserved.

For admitted non-nil datasets, the bytes equal `encoding/json.Marshal(dataset)`,
including typed versus generic-map field order and HTML/string escaping. A nil
top-level dataset is encoded as `{}`.
`DatasetOptions.MaxValues` additionally bounds cumulative attributes plus Value
entries, including typed-slice admission before normalization; failure is
`ErrMaxValuesExceeded`. `Limits` bounds normal conversion resources. An optional
`ValidateElement` callback can apply application policy before each attribute
is emitted. `TransferSyntax` permits validation of encapsulated Pixel Data.

Any encoder error can leave a prefix in the writer. These APIs do not promise
atomic publication; use private staged output when that is needed.

## DICOMweb server and opt-in client

QIDO-RS and JSON WADO metadata now validate VR-specific represented metadata
while writing directly into the existing private response spool. Invalid or
oversized later datasets, backend failures and cancellation still fail before
HTTP body publication whenever publication has not started. MaxJSONValues and
the conservative value estimate restart for each dataset, as in the legacy
server. The complete array is bounded by MaxResponseBytes in the spool and
MaxResults in the handlers. Per-dataset
value/depth/node limits, strict base64 and service-local BulkDataURI checks remain
in force. A failed append remains failed even if a backend ignores its yield error.
The spool is finalized before HTTP copying and is removed after
success or failure. This preserves the prior complete-response publication
policy rather than exposing a partial JSON array on backend failure.

The new JSON-only client methods are `SearchStudiesStream`,
`SearchSeriesStream`, `SearchInstancesStream`, `StudyMetadataStream`,
`SeriesMetadataStream` and `InstanceMetadataStream`. Each takes a synchronous
`func(Dataset) error` callback and returns `Response` headers/status with no
resident Body. They use the existing transport/authentication/retry policy and
bypass ResponseCache. Retry may occur before response consumption under the
existing HTTP policy; delivered callbacks are never replayed after a body or
callback failure. The HTTP response body is closed on every return.

`StudyMetadataRefsStream` and `SeriesMetadataRefsStream` share that contract
but hand each callback the `InstanceRef` that `StudyMetadata` and
`SeriesMetadata` return for the dataset, parents inherited from the request
and identity checks included. A caller can follow a large listing arriving,
for example to bound it by the time between instances rather than as a whole.
They admit the JSON listings the resident methods admit: the body is bounded
as a whole by MaxBodyBytes, and the 1,000,000 elements and 1,000,000 sequence
items apply to each instance, not to the listing, so a study whose instances
hold more than that in all is still listed.

A 200 response with an empty body, with or without a JSON Content-Type, is an
empty array to every streaming method, as to the resident ones. A body that
stops arriving, cut by the connection or by a deadline, fails the stream as a
request failure (`ErrorKindRequestFailure`) with the transport cause and the
response status in the error, as it fails the resident methods; only a body
that arrives and cannot be decoded is a decode failure.

Streaming requires the JSON metadata representation to be configured and
accepts the existing generic JSON/text media aliases. XML-only configuration is
refused before the request. Existing resident methods still negotiate JSON/XML.
Configured body/part limits apply, and the smaller of MaxMetadataParts (10,000
by default) and ResponseLimits.MaxParts bounds JSON callbacks in these new
streaming methods. Traversal is additionally
bounded to 1,000,000 attributes and sequence items with the library's finite
sequence depth. Callback errors remain discoverable by errors.Is/errors.As;
public DICOMweb errors sanitize their text.

The represented streaming decoder has a stricter admission contract than the
raw resident JSON client: it validates tags, VR-specific values, unknown element
fields and conflicts among Value/InlineBinary/BulkDataURI before each callback.
The resident client stages the complete array as Element structs with the same
top-level object and unique-tag checks. Its validation covers JSON syntax and
dataset structure.

## Qualification

Synthetic tests cover byte equivalence for compact/pretty/array output, private
UN/SQ, byte order, charset, bulk callbacks and encapsulated pixels; callbacks
before EOF; late failures, cumulative limits and cancellation; JSON stream
HTTP scopes, cleanup, response cache bypass and no replay after consumption;
and response spool failure before publication. A 10,000-dataset fixture checks
byte/hash equivalence with the former structural-validation + Marshal + full
Unmarshal server pipeline.

`BenchmarkMetadataJSON10K` compares that preserved legacy server pipeline with
the validated writer using 10,000 synthetic QIDO STUDY datasets; it measures
per-dataset encoding, without the response spool or HTTP handler.
`BenchmarkMetadataJSONClient10K` compares resident JSON array decoding with
validated streaming callbacks and a resident-plus-VR-validation control. The
control calls `ValidateDatasetContext` for each decoded dataset: it applies the
same VR rules to this valid fixture but also emits JSON to `io.Discard`, while
the streaming decoder suppresses validation output. Resident struct decoding
also loses unknown fields and empty-field presence before validation. The
control therefore measures the cost of adding VR policy to resident decoding;
it does not isolate streaming overhead or reproduce every structural rejection
of the streaming decoder.

`BenchmarkStudyMetadataSpool10K` generates one synthetic CT study/series with
10,000 distinct instances and 26 attributes per instance, including a Pixel Data
BulkDataURI. The current cell calls the real metadata preparation handler; the
test-only legacy JSON route preserves its spool policy and replaces only the
append with the former structural-validation + Marshal + full Unmarshal path.
Both cells generate one dataset at a time, create/finalize/copy/remove the spool
and write the complete response to `io.Discard`. They exclude server construction,
the common routing/authentication middleware and network transport. A functional
crosscheck compares the complete array's byte count and SHA-256 against the full
`ServeHTTP` path without retaining the body in a ResponseRecorder.

Process RSS qualification is opt-in via `DICOMGO_JSON_STREAM_RSS` (the original
per-dataset/client cases) or `DICOMGO_STUDY_JSON_RSS` (the study preparation/spool
cases), with a separate process per mode. The study RSS cases hash the output;
the timed benchmark discards it. Compare the two modes within each case.
Server construction is outside the operation timer but inside process RSS.
Report allocation, latency and process RSS separately: cumulative allocated
bytes are not retained heap, HeapAlloc at return is not post-GC retention, and
a process high-water mark is not an exact memory bound.

### Recorded local measurements

The October measurements below precede per-tag duplicate detection. A separate
Windows amd64 check with Go 1.26.4 and `-cpu 1` used synthetic arrays of 10,000
datasets with 17 attributes each. Validated stream decoding took 231–263 ms
and allocated about 172 MB per array with duplicate detection, compared with
190–203 ms and about 138 MB in the earlier reader. These short runs measured
local decoder work without network I/O.


| Case (10,000 datasets/instances per operation) | Median ms/op | Median B/op | Median allocs/op |
|---|---:|---:|---:|
| Study metadata: legacy preparation/spool | 875.231 | 1,077,103,888 | 13,024,817 |
| Study metadata: current preparation/spool | 436.212 | 560,193,648 | 8,530,300 |
| QIDO writer: legacy full reparse | 535.071 | 674,337,384 | 7,963,877 |
| QIDO writer: current validated writer | 136.040 | 64,037,712 | 4,160,072 |
| Client: raw resident array | 84.804 | 97,253,624 | 963,453 |
| Client: streaming callbacks, original campaign | 173.997 | 150,208,112 | 2,563,857 |
| Client: resident array plus VR validation, control campaign | 224.996 | 159,734,912 | 5,093,469 |
| Client: streaming callbacks, paired control campaign | 175.161 | 150,208,144 | 2,563,857 |

For the same study fixture, preparation/spool time fell 50.16% and allocated
bytes fell 47.99%. All 10,000-instance arrays had 12,565,573 bytes and SHA-256
`22855d3387bc27bd2a8563e1810168f36112ddef8e537049a01724564b001cff`, including
the full-handler crosscheck. RSS was 34.53–36.13 MiB for legacy preparation and
33.61–34.39 MiB for current preparation. The spool keeps both server paths
bounded; this fixture does not show a large server RSS reduction.

The original client comparison used 110.42–121.30 MiB RSS for resident decoding
and 33.52–33.83 MiB for streaming. Streaming took 105.17% more time and allocated
54.45% more total bytes than raw resident decoding. In the additional paired
control, streaming took 22.15% less time than resident decoding plus public VR
validation. The contract and control differences described above remain
material: this establishes a memory/cost tradeoff, not a client speedup over
the raw resident API measured in that campaign.

Normative authority: [DICOM PS3.18 F.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part18/sect_F.2.html)
(2026d as consulted), whose multiple-result representation is one top-level JSON
array, and its object/VR representation rules. Implementation is clean-room.
The synthetic/loopback checks do not qualify live PACS or clinical workflows.

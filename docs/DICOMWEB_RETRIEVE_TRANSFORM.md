# Prepared retrieval, URI service and bulk ranges

`net/dicomweb.Server` provides three independent opt-ins: `RetrieveTranscode`,
`EnableWADOURI`, and `EnableBulkDataRanges`. They retain deny-by-default
authorization, finite budgets, cancellation and redacted errors. Their zero
values preserve backend-driven WADO-RS and sequential bulk retrieval.

## WADO-RS preparation

`RetrieveTranscodePolicy` supplies explicit `pixeldata` codec registries and a
finite target allowlist (at most 64 entries; default Explicit VR Little Endian,
EVRLE). The handler requests as-stored Part 10 from `RetrieveBackend`, stages it,
checks actual Study/Series/SOP Instance and File Meta identities, then prepares
the negotiated representation with `pixeldata.TranscodePath`. Output identity
and actual syntax are checked too; backend declarations cannot replace these checks.

Binding requires a single complete canonical UI value and exactly one top-level
occurrence of dataset Study/Series/SOP Instance/SOP Class and File Meta SOP
Instance/SOP Class/Transfer Syntax. An additional selective `parser.Reader`
scan checks original occurrences before Object indexing can hide duplicate tags.
The selective pass bounds UID headers before consuming their values; the initial
File Meta read is independently bounded by element and total-source budgets.
This pass skips pixel values
and never decodes pixels; seekable non-deflated values can be skipped by seek,
while Deflated datasets need another bounded incremental inflation/consumption.
Every prepared input/output incurs this structural scan. Nested item UIDs are
outside the route binding scope. This is not complete IOD validation.

These reads, and the one `SourceOutputBound` makes, bound a single value by
the object's size and the policy's input limit, not by `MaxMetadataBytes`:
they keep values above their inline threshold out of memory, and the
values they hold count towards the total they may read. An object delivered
as stored with a 65 MiB Encapsulated Document or private value is therefore
delivered whole, as by a server without the policy; `SourceOutputBound`
does not read such an object, so a lower element limit would have refused
it only once a multipart response had begun. For the same reason these
reads do not count Pixel Data fragments, which they skip: an object
delivered as stored is never decoded, and a conversion is bounded by
`TranscodeLimits.MaxFragments` in `TranscodePath` and in `SourceOutputBound`.
The element count and sequence depth limits of the policy still apply to
every prepared read.

An object of at most 8 MiB is staged in memory, and written to the spool
directory only when it is converted; a larger one is staged in a file of a
private job directory. Staged bytes held in memory are bounded by
`MaxConcurrentJobs`. The parsers read a staged object through a 64 KiB
buffer, so their reads follow the object's size, not its element and item
headers, and each read of the staged object still passes the read-lease
checks.

Conversions need the allowlist and necessary codecs. Encoded targets require a
lossless encoder and output decoder for verification. Acceptable stored syntax
passes through byte-for-byte without encoder/allowlist membership, including
stored lossy objects. New lossy encoding, UID changes, forced re-encoding,
Secondary Capture, recovery/rewrite hooks and verification opt-out are refused
at construction. Write/fragment/planar options apply only to conversions.
Invalid options are rejected even for pass-through. No global codec registry is
installed and no derived-instance publication is implemented.

When a client accepts the stored syntax and a conversion equally, as with
`transfer-syntax=*`, the first allowlisted target wins unless
`PreferStoredSyntax` is set, which passes the stored bytes through.
For objects, omitted syntax selects EVRLE with the stored-lossy exception. For
frames, concrete MIME selects its own default syntax. A syntax wildcard is
scoped to the requested MIME family; `image/*` leaves available encoding open.
Quality/specificity retain `q=0` exclusions. Implicit Little Endian and retired
Explicit Big Endian can be inputs but cannot be output encodings: even the
zero policy, without codecs, delivers them as EVRLE. Unavailable
conversion returns `406`, rather than claiming every source is decodable.

The backend receives an as-stored source request whose `Accept` asks for any
syntax. `RetrieveRequest.AcceptsSource(uid)` evaluates the client's own
preferences against the policy for an object stored in `uid`, so a backend
can refuse an undeliverable collection before its first part. It errs only
towards refusal: the stored-lossy exception counts for a syntax that is lossy
by itself (JPEG Baseline and Extended, video), not for a lossless-capable
syntax whose Lossy Image Compression attribute is read later, so a backend
that checks every member never starts a multipart response that negotiation
would cut short. A member can still fail on its content, such as a codestream
its decoder refuses. On a request that is not prepared, it equals
`AcceptsOriginal`.

The server enforces `MaxPartBytes`, `MaxResponseBytes` and the conversion
output limit only while it writes, after the response has begun, and a
conversion can deliver several times the stored bytes. A backend that checks
a collection against those limits before its first part therefore budgets
`RetrieveRequest.SourceOutputBound(ctx, uid, storedSize, open)` rather than
the stored size. It returns the stored size, without calling `open`, when the
server delivers the stored bytes. When it may convert the object it opens it,
reads it up to its Pixel Data and bounds the result by the decoded samples
(the stored value of native Pixel Data) plus three times every other byte,
which covers re-encoding each header in Explicit VR with undefined-length
sequences, plus 4 KiB for the File Meta Information; a Deflated source is
inflated to count its bytes. It answers `ErrResourceLimit` for an object
above the conversion input or whose bound exceeds the conversion output, and
`ErrUnsupported` where `AcceptsSource` is false. Like `AcceptsSource` it errs
only towards refusal.

The conversion also refuses an object on limits derived from its content:
Number of Frames, Rows × Columns × Number of Frames and the ratio of the
decoded bytes to the compressed input, for the whole object and for each
frame (`TranscodeLimits.MaxFrames`, `MaxPixels`, `MaxExpansionRatio`); the
frames and pixels of an object with Icon Image Sequences are counted over
the image and its icons together, as its conversion counts them. A
constant image can compress a thousandfold, so a few hundred stored bytes
can exceed the ratio. `pixeldata.CheckTranscodeDecodeLimits` applies these
limits before anything is decoded, with the same inputs and arithmetic as
the conversion, and both `SourceOutputBound` and the delivery of each object
call it. When the limits refuse the selected conversion the object is
delivered in its stored syntax if the client accepts that syntax too, as it
would be if no decoder were configured, and `SourceOutputBound` returns the
stored size; otherwise it answers `ErrResourceLimit` (`413`) before the
first part, and a single-instance request answers `413` as well.
See [PS3.18 transfer syntax](https://dicom.nema.org/medical/dicom/current/output/chtml/part18/sect_8.7.3.4.html)
and [bulk media defaults](https://dicom.nema.org/medical/dicom/current/output/chtml/part18/sect_8.7.3.3.2.html).

| Representation | MIME / syntax | Profile |
|---|---|---|
| Part 10 | `application/dicom`; known Web Services syntax | Stored pass-through or configured lossless conversion |
| Native frame | `application/octet-stream`; EVRLE (default), Encapsulated Uncompressed EVRLE (explicit) | Raw samples |
| Compressed frame | `image/dicom-rle` / RLE; `image/jls` / JPEG-LS Lossless; `image/jpeg` / JPEG Lossless SV1; `application/x-deflate` / Deflated Image Frame Compression | Builtin lossless conversion and codestream extraction tested |
| Other image codecs | `image/jp2`, `image/jpx`, `image/jphc`, `image/jxl` | Default lossless syntax mapped; compatible supplied codecs required; unavailable codecs return `406` |
| Dataset Deflate | Part 10 / Deflated Explicit VR Little Endian | Object conversion tested; not a frame MIME encoding |
| Video | Stored Part 10 may pass through | Prepared frames refuse video; no video encoding/frame extraction |

Frames use `FrameReader.RawFrame`, not decoded-frame APIs: compressed Item
payloads are joined without DICOM Item headers; native samples remain native,
and requested order is retained. Video has one representation containing all
frames, so it is not emitted once per selected image frame. See
[PS3.18 compressed bulk data](https://dicom.nema.org/medical/dicom/current/output/chtml/part18/sect_8.7.3.3.2.html).

Parsing a staged copy reads it a few bytes at a time, so the read lease is
validated before its first read and then once per 256 KiB read rather than
for every read; cancellation, an earlier denial and expiry still stop the
next read, and every response chunk is checked as it is written.

Single-instance objects/frames are staged through backend completion before
publication. Late backend errors or duplicate instances cannot publish a
successful complete instance. Collections prepare/publish one instance at a
time; later errors terminate multipart, whose prior bytes cannot be recalled.
Network failure can truncate any committed response. There is no study-wide
transaction or durable HTTP-delivery promise.

## WADO-URI profile

`EnableWADOURI` enables `GET /wado` under the deployment mount. Authorization
receives `OperationRetrieveWADOURI` and the original request before backend work.
Required query fields are case-sensitive `requestType=WADO`, `studyUID`,
`seriesUID`, `objectUID`, once each with valid UIDs. Accept is mandatory (`406`
when absent). Optional `contentType` must agree with positive header preferences
and cannot override `q=0`. DICOM/rendered representation classes cannot mix.

Application/DICOM returns one Part 10 file. `transferSyntax` is one query UID,
default EVRLE; wildcard query syntax and MIME syntax/charset parameters are
refused. Content-Type is plain `application/dicom`; File Meta carries actual
syntax. URI-only enablement can prepare native inputs with default options;
compressed conversion requires registries through `RetrieveTranscode`. It does
not enable prepared WADO-RS unless that policy is explicitly supplied.

Rendered retrieval maps positive `frameNumber`, paired positive rows/columns,
paired windowCenter/windowWidth, imageQuality and annotation (or compatibility
alias imageAnnotation) into the existing bounded injected Renderer. DICOM
rejects rendering options; rendered output rejects transferSyntax. Recognized
anonymize=yes, charset/Accept-Charset, normalized region and paired Presentation
State UIDs return `501`. Unknown/duplicate/empty parameters, invalid grammar and
forbidden combinations return `400`; unavailable media/syntax is `406`; absent
backend/renderer capability is `501`. This is a partial URI profile, not the
complete rendered/document/media surface. See
[PS3.18 URI service](https://dicom.nema.org/medical/dicom/current/output/chtml/part18/chapter_9.html),
[representation parameters](https://dicom.nema.org/medical/dicom/current/output/chtml/part18/sect_9.4.html),
[rendering parameters](https://dicom.nema.org/medical/dicom/current/output/chtml/part18/sect_9.5.html),
and [Accept negotiation](https://dicom.nema.org/medical/dicom/current/output/chtml/part18/sect_8.7.5.html).

## Bulk byte ranges

`EnableBulkDataRanges` handles one range on direct application/octet-stream GET
when Size is known and explicit `BulkDataPart.ReaderAt` addresses an immutable
logical representation identical to Reader. Offsets are value-relative, not
backing-file offsets. The existing owning Reader keeps backing storage alive and
is closed exactly once; ReaderAt is borrowed, not separately closed or inferred.
Token authorization, media and total-size checks precede reads.

Closed, open-ended and suffix ranges return `206` with exact Content-Length and
Content-Range. End/suffix values safely clamp. Unsatisfiable ranges return `416`,
`bytes */size`, no body. Policy ignores malformed/multiple (including repeated
fields)/unknown-unit ranges and returns full `200`; empty values also return
full `200`. This adapter supplies no representation validator, so any nonempty
If-Range causes full `200`; malformed empty/repeated If-Range fields also take
this conservative full-response path. See [RFC 9110](https://www.rfc-editor.org/rfc/rfc9110.html).

Unknown size, missing ReaderAt, multipart negotiation or disabled ranges retains
full sequential retrieval. Direct fallback advertises Accept-Ranges:none;
known random-access direct values advertise bytes. Ranges do not apply to Part
10, rendered, metadata or multipart framing. Small ranges cannot bypass total
size caps. Short ReaderAt data/I/O failure truncates the declared-length stream.

## Resources and lifetime

MaxConcurrentJobs defaults to two, clipped to server concurrency, maximum 64;
admission is cancellable before source reads. Jobs use private mode-0700
directories and mode-0600 files. Input/output, metadata, elements/depth/fragments,
frames, time and HTTP output have finite caps; transcode defaults are clipped to
server limits. Frame lengths and their sum are checked before raw allocation;
multipart overhead is capped by the response spool. Existing pipeline controls
bound managed buffers; some layouts/syntaxes use a bounded whole-object fallback.
These are not RSS bounds: codec workspace/runtime are outside them. No
performance or memory improvement is claimed.

Disk retains input, transcode work/output spools and staged HTTP responses.
Codec work is bounded by jobs; finished HTTP spools/copying remain bounded by
MaxConcurrentRequests/MaxResponseBytes. Collections do not retain a whole study
in RAM. Range uses SectionReader and a 32-KiB buffer, without prefix discard.
Codec registries are borrowed, immutable for server lifetime, context-aware and
safe for concurrent requests/jobs. Option values, targets, preamble and Deflate
level are frozen at construction. Backend callbacks yield synchronously and
must honor context. Native filesystem calls/blocking ReaderAt cannot be forcibly
interrupted; context checks surround I/O.

Cleanup runs on success/error/cancellation. Startup scavenging recognizes owned
response files and inactive retrieval directories, inspects at most 17 child
entries, and removes only bounded flat recognized regular files. Unknown
children, symlinks and same-process active jobs are preserved. Existing parent
spool inventory/age/quota rules apply. Use a private spool root with immutable
backing/parents; cross-process active ownership is not coordinated. Cleanup
errors are redacted; after commitment they cannot replace the HTTP status.

Synthetic fixtures and real loopback HTTP clients qualify actual Part 10
syntax/UID/sample equality, encoded frame bytes/order, URI translation,
refusals, cancellation, late errors, cleanup and Range ownership/framing.
Injected rendered fixtures prove translation, not rendering fidelity. They do
not prove live PACS/third-party viewer interoperability, arbitrary codecs,
clinical correctness, native Windows or complete PS3.18 conformance.

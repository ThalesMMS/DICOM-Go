# DICOMweb client response policy

The `net/dicomweb` client validates response media types and applies finite
limits before returning buffered WADO-RS parts or delivering streamed parts to
a callback. The zero-valued `dicomweb.Options` remains usable and selects the
defaults below.

## Defaults

| Option | Zero-value behavior | Scope |
| --- | --- | --- |
| `Options.MaxBodyBytes` | 512 MiB | Complete HTTP response body |
| `ResponseLimits.MaxParts` | 100,000 | WADO-RS multipart parts |
| `ResponseLimits.MaxPartBytes` | Effective `MaxBodyBytes` | Each single or multipart representation |
| `ResponseLimits.MaxPartHeaderBytes` | 64 KiB | Raw MIME headers for each part, before parsing/allocation |
| `ResponseLimits.MaxMultipartDepth` | 1 | The single `multipart/related` layer defined for these resources |
| `Options.MaxMetadataParts` | 10,000 | Metadata datasets in WADO-RS, DICOM XML, and streaming QIDO-RS |

`MaxMetadataParts` and `ResponseLimits.MaxParts` are applied together using
the lower limit for those metadata calls. DICOM XML also applies the shared
part byte and header limits. Negative limits and malformed configured media types
are invalid configuration, except that the established `MaxBodyBytes <= 0`
behavior continues to select 512 MiB. WADO-RS responses are flat: nested multipart is
unsupported, and `MaxMultipartDepth` accepts only zero (the default) or one.

The default part-byte limit follows the effective body limit. Existing callers
that raise `MaxBodyBytes` therefore continue to accept a single object up to
that configured ceiling. Large study retrievals may require raising the total
body limit even when each individual instance is small.

## Connection source port

A nil `Options.HTTPClient` selects a shared client like `http.DefaultClient`
whose transport, a clone of `http.DefaultTransport`, connects again, up to
three more times within the request context, when a connect fails before
reaching the server because the source port the system chose cannot be used
(address in use, as with Windows Tcpip event 4227 under ephemeral port
pressure, or no buffer space). A refusal by the server and every other error
fail at once, and no request byte has been sent when a connect is repeated.
An `HTTPClient` supplied by the caller keeps its own dial; set its
transport's `DialContext` to `ul.SourcePortRetryDialContext(nil)` for the same
retry. The UPS-RS event channel dial does the same when its transport has no
`DialContext`.

## Redirect policy

Basic authentication always requires certificate-verified HTTPS and redirects
within the original origin. BulkData retrieval also always requires
same-origin redirects. An origin consists of scheme, hostname and effective
port. Guarded redirects reject a changed origin or HTTP Host, URL user
information, and opaque URLs, including changes made by a caller's
`CheckRedirect` callback. They stop after at most ten requests per redirect
chain and leave the supplied HTTP client untouched.

The zero-valued `Options.RedirectPolicy` delegates redirects to the configured
HTTP client for other requests. Use `RedirectPolicyAuthenticated` to enforce
same-origin redirects for static and dynamically sourced bearer credentials as
well. Use `RedirectPolicySameOrigin` to enforce the same policy for anonymous
requests, such as a remote preview. A cache shared by clients with different
origin requirements never supplies a buffered response fetched without the
guard to an origin-bound client.

### WADO-URI single-instance streaming

Set an explicit Endpoint.WADOURIPath below Endpoint.BaseURL and call
Client.RetrieveWADOURIInstanceStream with a complete study, series and SOP
instance UID. The client requests one Part 10 instance with plain
application/dicom, omits the transferSyntax query parameter for the default
Explicit VR Little Endian syntax and requires same-origin redirects even for
anonymous requests. It accepts a full HTTP 200 response with the requested
media type. The ordinary body and part byte limits apply.

The callback runs during the request and receives the payload as a bounded
reader. Stage it in the callback and verify the UIDs and actual Transfer
Syntax UID from Part 10 before publication. Publish only after the stream
method returns successfully. The URI retrieval profile covers instance
payloads. [DICOM PS3.18 §9.4](https://dicom.nema.org/medical/dicom/current/output/chtml/part18/sect_9.4.html)

## Accepted representations

Object retrieval accepts a single `application/dicom` representation or
`multipart/related` whose declared type and every part are
`application/dicom`. Study and series retrieval require multipart responses.

### WADO-RS transfer syntax preferences

When `RetrieveOptions.TransferSyntaxUIDs` contains more than one distinct
preference, the first GET advertises the full list in the supplied order.
On HTTP 406, object and frame retrieval close that response before issuing
a GET with the leading syntax removed from `Accept`. Each subsequent 406
advances through the remaining preferences. The client returns the final
406 after exhausting the list and adds no implicit wildcard; include
`"*"` to allow the server to select a transfer syntax.

While preferences remain, 400, 415, 500 and 501 advance the same way. Some
servers use them for a representation they cannot produce: Orthanc's
DICOMweb plugin answers 500 when it cannot transcode to the requested
syntax. The answer to the last preference is returned with its own status.

The optional `RetryPolicy` applies independently to HTTP attempts for each
header value. Negotiation ends when the response returned by that policy
has another status. Only the selected response reaches the buffered decoder
or the callback.
A callback error or an incomplete multipart body ends the operation with
the error from that response. Buffered object cache entries use the URL
and the initial request's full `Accept` value.
[DICOM PS3.18 §8.7.7](https://dicom.nema.org/medical/dicom/current/output/chtml/part18/sect_8.7.7.html)

### Multipart and metadata response validation

Multipart response parsing verifies the complete delimiter line before
ending a part. A line such as `--boundary--extra` or `--boundary <text>`
remains in the payload, including its preceding line break. An actual
closing delimiter is required even when the last payload bytes resemble
one. Delimiter lookahead is bounded to 4 KiB of line bytes. Spaces and
tabs before the line ending, LF-only framing selected by the first
delimiter, and a closing delimiter at transport EOF are supported.
[MIME delimiter syntax](https://www.rfc-editor.org/rfc/rfc2046.html#section-5.1.1)

Frame retrieval requires `multipart/related`. Parts may use the DICOMweb frame
media types supported by the package:

- `application/octet-stream` and `application/x-deflate`
- `image/jpeg`, `image/jls`, `image/jp2`, `image/jpx`, `image/jphc`,
  `image/jxl`, and `image/dicom-rle`
- `video/mpeg`, `video/mp4`, and `video/h265`

When a frame part supplies a `transfer-syntax` parameter, its media type must
match that transfer syntax.

Metadata negotiation remains controlled by `MetadataMediaTypes`. DICOM JSON
uses `application/dicom+json`; the existing `application/json` and `text/plain`
aliases remain accepted for legacy servers when JSON was requested. DICOM XML
uses `multipart/related; type="application/dicom+xml"` with homogeneous
`application/dicom+xml` parts.

QIDO-RS and WADO-RS metadata JSON responses contain one array of dataset
objects. The buffered and streaming readers reject null datasets and
duplicate tag keys at the dataset root. The JSON layer accepts empty objects
and preserves null values inside attributes. WADO-RS metadata applies the UID
checks below. Buffered metadata calls return no datasets if a later array
member is malformed.

`ResponseLimits.AllowedMediaTypes` optionally narrows these
operation-specific sets. It cannot enable a representation that is invalid for
the requested resource. Because the option belongs to the client, include all
representations needed by that client's object, frame, and metadata calls.

### WADO-RS metadata identity

Each metadata dataset must belong to the requested study, series, or instance.
The client requires valid instance UIDs and checks every supplied parent UID
against the request when that parent is in scope. For JSON compatibility, it
uses the requested parent when the response has no value for that parent.
Only the reference-returning methods expose inferred values. Raw and streaming
datasets retain their received fields. XML also requires each part to agree
with its instance Content-Location. Instance-level metadata rejects an
unexpected SOP Instance UID and responses containing multiple datasets.
Study-level responses reject an SOP Instance UID associated with conflicting
series and accept repeated datasets for the same identity.

Use `StudyMetadataDatasetsWithURL` to resolve a study metadata BulkDataURI
with `Endpoint.BulkDataURLFromMetadata`. The returned base is the final request
URL and follows the metadata across cache hits. Keep it confined to URI
resolution because its path and query may contain sensitive values.

An identity failure satisfies `errors.Is(err, dicomweb.ErrInvalidMetadataIdentity)`.
Its cause names the attribute tag for field failures and keeps UID values private.
Buffered methods return no datasets or references after such a failure.
Streaming callbacks may already have consumed earlier valid datasets.
[DICOM PS3.18 §10.4](https://dicom.nema.org/medical/dicom/current/output/chtml/part18/sect_10.4.html)

## Streaming frames

`RetrieveFrames` remains the compatibility API and returns all requested frame
payloads in memory. It now collects over the streaming core, avoiding the
former extra allocation for the complete multipart response. Use
`RetrieveFramesStreamWithOptions` when the consumer can process one frame at a
time:

```go
stop := errors.New("enough frames")
err := client.RetrieveFramesStreamWithOptions(ctx, ref, []int{1, 20, 40}, opts,
    func(frame dicomweb.FramePartStream) error {
        if err := consumeFrame(frame.FrameNumber, frame.Reader); err != nil {
            return err
        }
        if frame.FrameNumber == 20 {
            return stop
        }
        return nil
    })
if err != nil && !errors.Is(err, stop) {
    return err
}
```

The callback is synchronous. Its `Reader` is valid only until the callback
returns and must not be retained. Returning `nil` lets the client drain any
unread bytes under the configured limits and advance to the next frame.
Returning an error stops without draining the remaining response, closes the
HTTP body, and preserves the callback error for `errors.Is`. The client also
closes the response on cancellation, HTTP errors, and decode failures.

A successful return validates the terminal multipart boundary and requires one
part for every requested frame. A callback can therefore have observed valid
earlier frames before a later truncation or count mismatch is reported. Stage
side effects until success when atomic consumption is required. An intentional
early stop cannot validate bytes the caller chose not to consume; the response
is closed instead.

## Rendered and thumbnail resources

`RetrieveRendered` and `RetrieveRenderedStream` fetch the PS3.18 rendered
resources of a `RenderTarget`: a study, a series, an instance, or selected
frames. `RenderedOptions` sets the acceptable media types (default
`image/jpeg`; listed in preference order with descending `q` values) and the
`viewport`, `window`, `quality`, `annotation` and `iccprofile` query
parameters, which are validated before any request. Study, series and
multi-frame targets request and require `multipart/related`; an instance or
single frame accepts one representation. Accepted media types are the
rendered images (`image/jpeg`, `image/png`, `image/gif`, `image/jp2`,
`image/jph`, `image/jphc`, `image/jxl`), video (`video/mpeg`, `video/mp4`,
`video/h265`), `text/html`, `text/plain` and `application/pdf`.

`RetrieveThumbnail` returns exactly one image representation; its
`ThumbnailOptions.Viewport` accepts only width and height. `RenderedURL` and
`ThumbnailURL` build the same URLs without performing a request.

Several media types are sent in one `Accept` field with quality values. When
the server answers 400 or 415 to that field, the client asks for each media
type alone, in order of preference: Orthanc's DICOMweb plugin refuses a
rendered or thumbnail request whose `Accept` names more than one media type.

## Bulk data

`RetrieveBulkData` and `RetrieveBulkDataStream` fetch a `BulkDataURI` taken
from QIDO-RS or WADO-RS metadata. `Endpoint.BulkDataURL` resolves relative
references against the base URL and refuses any reference whose scheme,
host or port differs from the endpoint, or that carries user information, so
credentials are never sent to another origin. Responses may be one
representation or `multipart/related`, with `application/octet-stream` or
the compressed pixel media types, under the usual response limits.

Use `Endpoint.BulkDataURLFromMetadata(metadataURL, bulkURI)` when a relative
reference is rooted at the request that supplied it. Supply the absolute
metadata request URL. The helper validates both origins and returns an
absolute URL for the ordinary retrieval, resolver or byte range methods.
[DICOM PS3.18 §10.4.1.1.5.1](https://dicom.nema.org/medical/dicom/current/output/chtml/part18/sect_10.4.html#sect_10.4.1.1.5.1)

`Client.RetrieveBulkDataRange(ctx, uri, offset, length)` requests one exact
closed byte range of a direct `application/octet-stream` value. Offset is
zero-based within the representation. Success requires HTTP 206, one exact
`Content-Range`, identity encoding and the requested body length. A known
total must exceed the inclusive end. An unknown total (`*`) is accepted.
The range and response limits apply before body allocation. Redirects stay
on the endpoint origin and retries keep the range. Range reads bypass the
response cache.

An ignored range (200), shortened partial response or inconsistent framing
returns an error with no data. Use the ordinary BulkData retrieval method
when the application explicitly needs the whole value. HTTP errors such as
416 retain their status classification. [RFC 9110 §14.4](https://www.rfc-editor.org/rfc/rfc9110.html#section-14.4)

`Client` implements `object.BulkDataURIResolver`. Combine it with
`object.FileBulkDataResolver` to resolve every reference of a decoded
metadata object:

```go
resolvers := object.BulkDataURIResolvers{
    "": client, "http": client, "https": client,
    "file": object.FileBulkDataResolver{Root: bulkDir},
}
err := object.ResolveBulkData(ctx, obj, resolvers, object.ResolveBulkDataOptions{
    MaxValueBytes: 64 << 20,
    MaxTotalBytes: 512 << 20,
})
```

The client buffers each value and stops with `object.ErrBulkDataTooLarge` as
soon as it exceeds the smaller of the caller's limit and
`ResponseLimits.MaxPartBytes`; a response with more than one part is refused.

### Opt-in bounded file staging

`StagedBulkDataResolver` implements the same `object.BulkDataURIResolver`
interface without retaining the HTTP value in a `bytes.Buffer`. The existing
`Client.ResolveBulkDataURI` remains buffered and compatible. Select staging
explicitly:

```go
staged := dicomweb.StagedBulkDataResolver{
    Client: client,
    Staging: dicomweb.FileBulkDataStaging{
        Directory: privateTempParent,
        MaxBytes: 64 << 20,
    },
    ChunkBytes: 32 << 10,
}
source, err := staged.ResolveBulkDataURI(ctx, bulkURI, 64 << 20)
if err != nil { return err }
defer source.Reader.Close() // required even after a read/write error
_, err = io.Copy(destination, source.Reader)
```

`BulkDataStaging.BeginBulkData(ctx, maxBytes)` creates an unpublished stage.
`BulkDataStage.Write` accepts bounded chunks; `Publish` transfers one
exact-length reader; `Close` discards unpublished data and is harmless after
successful transfer. Custom implementations must enforce the supplied finite
limit, serialize Write/Publish/Close, retain no whole in-memory payload and
remove unpublished data after failure. They must not publish resources on an
error. The resolver verifies the published size against the bytes received.
This seam is temporary ownership, not shared caching, eviction or a worker pool.

The built-in backend creates one exclusive file in a unique temporary directory
(0600/0700 creation modes on POSIX). Use a caller-secured temporary parent;
on Windows, directory ACL policy belongs to that parent. Files are closed and
removed on caller Close. Reader Read and Close serialize; repeated/concurrent
Close is idempotent. If removal fails, the error is preserved and a later Close
can retry; successful cleanup is not reported while files remain. Filesystem
calls are synchronous: context cancellation does not hard-interrupt a blocked
filesystem syscall or an uncooperative custom writer. Borrowed client and
caller resources are never closed by the stage.

Default chunk size is32KiB; valid explicit values are1byte through1MiB. A file
backend's positive MaxBytes narrows the smaller of the caller's positive
maxBytes and the client's finite part limit; zero uses that limit, negative is
invalid. Response body/media/part/header limits, configured redirect policy,
Basic/Bearer authentication and same-origin URI validation use the existing
streaming client. The full HTTP response is consumed under its body limit,
including the terminal MIME boundary and any epilogue, and its Body.Close must
succeed before Publish. A non-EOF read error returned alongside final response
bytes is retained below MIME buffering, even when those bytes contain a complete
closing boundary and a later read returns EOF. No reader is returned on cancellation, a truncated
HTTP/MIME response, a second representation, exceeded limits or disk failure.
One zero-length representation is valid. Request-context cancellation after a
successful return does not replace the caller's Reader.Close obligation.

Staging limits disk bytes per resolution; concurrent resolutions each own their
independent limit. There is no aggregate cache/session quota. At most one chunk
plus the existing bounded HTTP/MIME buffers is retained by the resolver, while
the file may grow to the accepted payload size. Operating-system page cache,
whole-process RSS, downstream resident `object.ResolveBulkData` values and
physical filesystem allocation are separate quantities: staging cannot make a
consumer that explicitly materializes an entire value memory bounded. To keep
the full write path streamed, use the returned source directly or register this
resolver with `object.NewBulkDataWriteResolver` and the object's writer.

The offline causal tests cover an8MiB+17 synthetic generated stream with4KiB
writes, complete SHA comparison, private-file modes/size and concurrent Close;
transport and MIME truncation (including after a valid MIME closing boundary),
two representations, caller/client/backend limits, cancellation joined to the
HTTP handler, real file write failure, configured redirects/auth and writer
ownership. These new tests and the benchmark are prepared but not yet executed.
Historical buffered/origin/auth tests must also pass before qualification.

A future allocation campaign uses streamed synthetic1/8/32MiB payloads, no
resident full-payload server fixture,32KiB staging chunks, and the same limits.
Run each size/mode in a fresh process to avoid heap-arena carryover. For example:

```sh
go test ./net/dicomweb -run '^$' \
  -bench '^BenchmarkBulkDataResolutionStaging/file/8MiB$' \
  -benchmem -benchtime=1x -count=1
```

Record ns/op, bytes/op and allocs/op for every case plus whole-process peak RSS
(`/usr/bin/time -l` on macOS, or equivalent OS measurement). Allocation bytes/op
include HTTP/test-server/file overhead and may accumulate with repeated chunks;
they are not the simultaneous live-memory peak. RSS includes setup/runtime and
server overhead. Compare increasing payload sizes and document actual values,
not a universal speedup or PACS claim. No benchmark or memory figures are claimed
until the separately authorized finite campaign has run.

## Batched STOW-RS and Explicit VR Little Endian payloads

`StoreInstancesBatched` stores many instances with one STOW-RS request per
batch, in order. `StoreBatchLimits` bounds a request by instance count
(default 100) and by the sum of `StoreInstance.Size` (default 64 MiB); an
instance larger than the byte limit travels alone, and an unknown size counts
as zero. `Options.Timeout` applies to each request, so a large upload is not
bounded by one deadline. A request the server refuses with HTTP 413 is split
once into two halves by instance count, each sent once; later batches keep the
instance count and bytes of the larger half, and a half refused again is
reported, not split further. Payloads given as `Reader` cannot be read twice and are never
repeated. The callback receives every finished request with its instance
range, result and error, and can stop the upload. `PlanStoreBatches` returns
the request boundaries without sending.

`StoreInstance.Size` is the length of the payload sent: for an `Open` that
re-encodes a file it is the re-encoded length, which
`ExplicitLittleEndianStorePayloadSize` returns, not the file length. An
instance whose payload cannot be produced (`Open` or its reader fails, or it
is empty) cuts its request without the closing boundary and returns a
`*StorePayloadError` naming the instance. `StoreInstancesBatched` then reports
that instance alone, in its own batch with that error, and sends the other
instances of the request again, provided they can be read again; a server
that stored parts of the cut request receives them twice.

PS3.18 excludes Implicit VR Little Endian and Explicit VR Big Endian from the
web services. `StoreRequiresExplicitLittleEndian` reports such a source, and
`ExplicitLittleEndianStorePayload` turns a seekable Part 10 file in a native
syntax (including Deflated) into an Explicit VR Little Endian stream for
`StoreInstance.Open`. Every data set value is kept; File Meta Information
records the new syntax. Native Pixel Data streams from the file, except for
a Deflated source, whose values are held in memory. A Big Endian source
holds its other values in memory too, and the writer reverses its values into
Little Endian order, streamed Pixel Data included. A malformed fixed-width
value of a Big Endian source that is not a whole number of units, such as a
private FD of 4 bytes, does not refuse the upload: its whole units are
reversed and the trailing partial unit is sent unchanged
(`object.WriteFileOptions.KeepPartialByteOrderUnits`), as a Little Endian
source sends such a value as it is. The parse
is bounded by the file length, or for a Deflated source by 1024 times it, and
so is every element: an object whose Pixel Data exceeds 64 MiB is re-encoded
like any other. Encapsulated sources return `ErrStoreReencodeUnsupported`.

## STOW-RS with metadata

`StoreMetadata` and `StoreMetadataToStudy` send PS3.18 10.5 metadata
requests: `multipart/related; type="application/dicom+json"` with all
instances in one JSON array (the default), or `type="application/dicom+xml"`
with one part per instance, followed by one bulk data part per
`StoreBulkData`, whose `URI` is sent as `Content-Location`. Instances carry
`core.BulkDataValue` references for the values sent as bulk data. Before any
request the client checks that references and parts match one to one, that
URIs are unique, and that the metadata holds no File Meta Information. Bulk
data parts stream from `Open`, `Reader` or `Data` like `StoreInstance`
payloads, and the response is decoded as for `StoreInstances`.

## QIDO-RS pagination

`StudyPager`, `SeriesPager` and `InstancePager` walk a search with the
`offset` and `limit` parameters. Each `Next` call requests
`SearchPagerOptions.PageSize` results (default 100) from the current offset.
The offset advances by the number of rows received. A valid Study, Series
or SOP Instance UID at the corresponding search level is delivered once,
in first-seen order, even when the server repeats it across pages. Rows
without a usable UID are delivered as supplied. A page shorter than requested ends the
search unless the response carries a PS3.18 `Warning: 299` saying that
additional results can be requested, as servers send when they cap `limit`;
`SearchPage.MoreResults` and `SearchPage.Warnings` expose those notices. A
full page is followed by another request, so a result count that is an exact
multiple of the page size costs one final empty request.

```go
pager := client.StudyPager(params, dicomweb.SearchPagerOptions{PageSize: 50})
for {
    page, ok, err := pager.Next(ctx)
    if err != nil {
        return err
    }
    if !ok {
        break
    }
    consume(page.Datasets)
}
```

Series and instance searches that are not limited to one study use the
PS3.18 resources `/series`, `/instances` and `/studies/{study}/instances`:
`SearchAllSeries`, `SearchAllInstances` and `SearchStudyInstances`, their
`...Stream` forms, and `AllSeriesPager`, `AllInstancePager` and
`StudyInstancePager`. They take the same keys, `includefield`,
`fuzzymatching`, `limit` and `offset` as the scoped searches; a Study or
Series Instance UID that is known becomes an ordinary matching key. A base or
QIDO path ending in `/studies` names the root above it for `/series` and
`/instances`.

`All` collects the remaining pages. `MaxResults` stops after that many
delivered results. `MaxPages` (default 1000) fails with `ErrSearchPageLimit`.
The pager ends with `ErrSearchPagination` for an oversized page, an
unrepresentable next offset, an empty page that announces more results, or
a page with no new results. For pages containing a row without a usable
UID, it detects adjacent identical decoded pages independent of JSON
formatting.
`StudySearchCriteria.Offset` sets the offset of a single search.

## Retry policy

`Options.Retry` (nil by default) retries bodiless requests — QIDO-RS, WADO-RS
metadata, objects, frames, rendered and bulk data — after a transport
failure, a per-attempt timeout, or a status in `RetryStatusCodes` (default
429, 502, 503 and 504). The delay before attempt n+1 is
`min(MaxBackoff, InitialBackoff * Multiplier^(n-1))` minus a random fraction
of at most `Jitter`; `DefaultRetryPolicy` uses three attempts, 250 ms
doubling to 5 s and 20% jitter. A `Retry-After` header lengthens the delay,
and one asking for more than `MaxBackoff` returns the response at once.
Waits end when the context is done, with an error that matches
`context.Canceled` or `context.DeadlineExceeded`. `Options.Timeout` bounds
each attempt. STOW-RS bodies are streamed once and are never retried.
`OnRetry` observes each retry without request data.

## Response cache

`Options.Cache` takes a `NewResponseCache(maxBytes, ttl)`. It stores
successful buffered GET responses — QIDO-RS searches, WADO-RS metadata and
buffered object retrieval — keyed by URL and `Accept` header, evicts the
least recently used entries beyond `maxBytes`, expires entries after `ttl`
when it is positive, and skips responses marked `Cache-Control: no-store`.
Streaming retrievals, `Verify` and STOW-RS always reach the server. Callers
receive copies of cached responses. Because credentials are not part of the
key, share a cache only between clients authorized for the same data;
`Stats` and `Purge` support monitoring and invalidation.

## UPS-RS workitems

`Endpoint.UPSPath` optionally places the Worklist Service below `BaseURL`.
`CreateWorkitem`, `RetrieveWorkitem`, `UpdateWorkitem` (with the
`transaction` parameter), `ChangeWorkitemState`,
`RequestWorkitemCancellation`, `SearchWorkitems`, `SubscribeWorkitem`
(`DeletionLock` and filtered-global `Filter`), `UnsubscribeWorkitem` and
`SuspendWorkitemSubscription` send DICOM JSON and return a
`WorkitemResponse` with the `Location` header (the created workitem for
Create) and the text of any `Warning` headers; failures are `*dicomweb.Error` values with the HTTP status. AE
Titles in paths are escaped (spaces are allowed). The matching keys of a
filtered global subscription travel in one `filter` parameter holding a
comma-separated list of `attribute=value` pairs (PS3.18 §11.10), each pair
percent-encoded and the commas literal; a key or value containing a comma is
refused before any request.

`OpenWorkitemEventChannel(ctx, aeTitle)` opens `GET /subscribers/{aetitle}`
as an RFC 6455 WebSocket with the client's credentials (one retry after a
`401` from a dynamic bearer source) and the TLS configuration and dialer of
`Options.HTTPClient`'s `*http.Transport`; HTTP proxies are not used and
another `RoundTripper` is refused. `Next` returns `WorkitemEvent` values
(Affected SOP Instance UID, Event Type ID and the event information) in
arrival order and `ErrEventChannelClosed` after the server or `Close` ends the
channel. Open the channel before subscribing: events committed while no
channel is open stay in the server's outbox and are retried with backoff.

PS3.18 defines no `Location` for Subscribe, and a `201` `Location` normally
names the created subscription. Some origin servers nonetheless serve the
channel elsewhere and announce it there; dcm4chee-arc-light 5.35.2 answers
with `ws://{host}/dcm4chee-arc/aets/{AE}/ws/subscribers/{aetitle}`.
`Endpoint.SubscribedEventChannelURLs(aeTitle, response)` lists the standard
channel first and adds the announced one only as a fallback, and only when
it is clearly an event channel: a `ws`/`wss` URL, or an `http(s)` URL whose
path ends in `subscribers/{aetitle}` outside any `workitems` resource. The
announced URL is resolved against the request URL, mapped from `ws`/`wss` to
`http`/`https`, and dropped when it is malformed, carries credentials or
leaves the endpoint origin, because the channel receives the configured
credentials. `OpenSubscribedWorkitemEventChannel(ctx, aeTitle, response)`
opens the first of them that completes the handshake and otherwise returns
the error of the last one tried; `OpenWorkitemEventChannelURL(ctx, url)`
opens one URL.

## OpenID Connect

The core package only injects bearer tokens: `TokenManager` caches the access
token and serializes refresh through a `TokenRefreshFunc`. The optional
package `net/dicomweb/oidc` supplies that function for an endpoint protected
by an OpenID Provider. It uses only the Go standard library, `net/dicomweb`
does not import it, and programs that do not need it do not link it.

```go
client, err := oidc.NewClient(oidc.Config{
    Issuer:        "https://login.example.org/realms/pacs",
    ClientID:      "viewer",
    Scopes:        []string{"openid", "offline_access"},
    Audience:      "dicomweb",              // optional "audience" parameter
    Flow:          oidc.FlowAuthorizationCode, // or oidc.FlowDeviceCode
    FetchUserInfo: true,
})
session, err := client.BeginLogin(ctx) // open session.Prompt.AuthorizationURL
token, err := session.Wait(ctx)
store := oidc.NewMemoryRefreshTokenStore(token) // or an OS keyring store
manager := dicomweb.NewTokenManager(token.AccessToken, client.TokenRefreshFunc(store))
```

| Piece | Behavior |
| --- | --- |
| Discovery | `/.well-known/openid-configuration` over HTTPS; the issuer must match literally; every endpoint must be HTTPS without user information or fragment; S256 PKCE is required for the authorization code flow; metadata is cached per `Client`. |
| Authorization code | RFC 8252 loopback redirect on `127.0.0.1:0`, RFC 7636 S256 challenge, random `state` (compared in constant time, wrong `Host` refused) and `nonce`; `prompt=consent` when `offline_access` is requested. `Close` releases the listener. |
| Device code | RFC 8628 polling honoring `authorization_pending`, `slow_down` (+5 s), transport back-off (at most 30 s between polls) and the provider expiry; the verification URIs must be HTTPS. |
| Token response | `token_type` Bearer, a non-empty access token and a signed ID token (signature, `iss`, `aud`/`azp`, `exp`, `nbf`, `iat`, non-empty `sub`, login `nonce`, `at_hash`). A refresh may omit the ID token only when the stored subject is known, and a refreshed ID token or UserInfo response for another subject is refused. A missing refresh-token rotation keeps the previous token. |
| Refresh and revocation | `Refresh`, `TokenRefreshFunc(RefreshTokenStore)` (persists rotated refresh tokens) and RFC 7009 `Revoke`. |
| JWT/JWKS | `Verifier` with `RemoteKeySet` or `StaticKeySet`; RS256/384/512, PS256/384/512, ES256/384/512 and EdDSA (Ed25519). `none`, HMAC algorithms, critical headers, RSA keys under 2048 bits, private or encryption keys and points off their curve are refused; embedded `jwk`/`jku`/`x5u` headers are ignored. An unknown `kid` refetches the key set (rotation) at most every five seconds, and the cached set is refreshed after an hour. Default clock skew is one minute. |
| Server | `Verifier.Authorizer(policy)` validates `Authorization: Bearer` JWT access tokens for `ServerOptions.Authorize` (see [`DICOMWEB_SERVER.md`](DICOMWEB_SERVER.md#security-and-ownership)). |

All provider requests refuse redirects to another origin and stop after five
same-origin redirects, so codes and tokens are never forwarded to another
host. Bodies are bounded to 1 MiB. Errors are `*oidc.Error` values with a
kind, operation, HTTP status and the OAuth `error` code only when it is a
short safe token; provider descriptions, URLs and tokens are never kept.
`Token`, `LoginPrompt`, `LoginSession` and `IDToken` redact secrets and
claims from `String`, `GoString` and JSON. Persisting the refresh token (for
example in an OS keyring) is the caller's `RefreshTokenStore`. Tests run
against an in-process TLS provider with keys generated per run
(`net/dicomweb/oidc/client_test.go`, `jwt_test.go`, `authorizer_test.go`).

## Storage Commitment

`Client.CommitStorage` posts the request to
`/commitment-requests/{TransactionUID}`. The DICOM JSON profile uses a flat
Referenced SOP Sequence with up to 16,384 SOP Class and Instance UID pairs.
Pass the exact instances accepted by the destination. The client generates
a Transaction UID when the caller leaves it empty.

HTTP 200 completes the operation only after every requested class and instance
pair occurs exactly once across the success and failure sequences. A refusal
carries its numeric Failure Reason. The client checks the original resource
after HTTP 202. An optional Location must identify that same resource, and
redirects are refused. Polling respects Retry-After and the caller's context
within the operation timeout. `CheckStorageCommitment` can resume a single check
with the original transaction UID and references.

The Storage Commitment response is bounded to 8 MiB. The direct response is
DICOM JSON. XML, multipart and grouped study requests are outside this client
profile. An in-process HTTP peer exercises immediate and asynchronous results,
partial refusal, pending timeout and cancellation, mismatched references and
route binding. [PS3.18 Commit](https://dicom.nema.org/medical/DICOM/current/output/chtml/part18/sect_13.4.html)
· [PS3.18 Check Commit Result](https://dicom.nema.org/medical/DICOM/current/output/chtml/part18/sect_13.5.html).


## Tuning

```go
client := dicomweb.Client{
    Endpoint: endpoint,
    Options: dicomweb.Options{
        MaxBodyBytes: 2 << 30,
        ResponseLimits: dicomweb.ResponseLimits{
            MaxParts:           150_000,
            MaxPartBytes:       1 << 30,
            MaxPartHeaderBytes: 64 << 10,
            MaxMultipartDepth:  1,
        },
    },
}
```

Raise limits only from trusted deployment knowledge. Keep a total body limit
even for streaming calls; the total limit bounds a large number of individually
valid parts.

Buffered methods return no parts after a structural decode failure. Streaming
callbacks can observe valid earlier parts before a malformed closing boundary
is discovered. Consumers that require atomic behavior should stage callback
output and publish it only after the method returns `nil`.

Malformed media types, missing boundaries, incompatible representations,
truncation, and multipart structural-limit failures are returned as
`*dicomweb.Error` with `Kind == dicomweb.ErrorKindDecodeResponse`. Use
`errors.As` to inspect the nested `*dicomweb.ResponseDecodeError`; diagnostics
do not include response payloads, credentials, or raw endpoint queries. The
pre-existing whole-body limit retains `ErrorKindRequestFailure` for backward
compatibility. A streamed body that ends before its declared Content-Length
or final chunk is `ResponseDecodeTruncatedBody`, which a part reader also
returns; a complete body without its closing boundary is
`ResponseDecodeMalformedMultipart`. A connection that fails below HTTP (for
example, reset by the peer) while the client reads part headers or drains a
part is `ErrorKindRequestFailure` (`ErrorKindTimeout` for a deadline) with
the network error in its chain. A part reader returns that network error to
the callback unchanged.

The allocation benchmark uses an immutable 32-frame, 32 MiB multipart fixture:

```sh
go test ./net/dicomweb -run '^$' -bench '^BenchmarkRetrieveFramesBufferedVsStreaming$' \
  -benchmem -benchtime=1x -count=5
```

Run buffered and streaming sub-benchmarks in separate processes when measuring
peak RSS so retained Go heap arenas from the first case do not bias the second.

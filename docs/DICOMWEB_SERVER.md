# Embedded DICOMweb server profile

`net/dicomweb.Server` is an embeddable, backend-neutral HTTP handler for a
bounded first server profile of PS3.18 QIDO-RS, WADO-RS, and STOW-RS. It does
not require a database, filesystem layout, or PACS schema. This document is a
capability statement, not a claim of complete Studies Service conformance.

## Security and ownership

The handler denies every request unless `ServerOptions.Authorize` is present or
the application deliberately sets `AllowUnauthenticated`. CORS is disabled by
default. The optional middleware hook is the deployment boundary for TLS-aware
authentication, CORS, tracing, and rate policy. Never expose an unauthenticated
plaintext handler outside a trusted test environment.

This package does not provide TLS certificate management, an identity store,
CORS policy, rate limiting, a WAF, or durable audit-log storage. A production
deployment must supply those controls. Bearer JWT validation is available
from the optional `net/dicomweb/oidc` package: `oidc.Verifier.Authorizer`
returns an `Authorizer` that requires `Authorization: Bearer`, verifies the
token signature against the provider's JWK Set (with key rotation), the
issuer, the resource audience and the expiry, answers `401` otherwise, and
then calls an application policy that may answer `403` (see
[`DICOMWEB_CLIENT.md`](DICOMWEB_CLIENT.md#openid-connect)). The supported
deployment shape is:

1. an internet- or enterprise-facing reverse proxy terminates TLS, rejects
   oversized requests early, applies rate policy, and normalizes only trusted
   forwarded headers;
2. deployment middleware validates credentials and applies an explicit CORS
   allowlist when browser access is required;
3. `ServerOptions.Authorize` independently authorizes the routed DICOMweb
   operation before backend work; leaving it nil remains deny-by-default;
4. the handler and backend run on a private/trusted network segment with finite
   timeouts and least-privilege access to storage.

Proxy body/header/time limits should be no larger than the policy intended for
the service and must not accidentally bypass the lower `ServerLimits` bounds.
Set proxy timeouts slightly above the handler's `MaxDuration` so the handler can
cancel backend work and clean spool files before the proxy disconnects. CORS is
not authentication: never combine a permissive origin policy with cookies or
bearer credentials.

`AuditEvent` contains only operation, request ID, status, item count, duration,
and a closed error code. It never contains paths, URLs, query values, UIDs,
DICOM attributes, backend errors, or remote text. Request IDs are restricted to
a small header-safe alphabet.

Do not log raw request URLs, query strings, headers, multipart filenames,
DICOM JSON, UIDs, backend errors, or response bodies: each can contain PHI or
secrets. Access logs at a reverse proxy must apply the same redaction rule.

Retrieve and bulk readers are borrowed from the backend for one synchronous
callback and are closed exactly once by the handler. STOW parts are staged in
bounded mode 0600 temporary files before the first backend call. Metadata-based
STOW resolves `BulkDataURI` only against an exact `Content-Location` in the
current multipart request, streams the bytes into validated Explicit VR Little
Endian Part 10 staging files, and performs no URL or filesystem dereference.
This makes a
truncated or malformed multipart fail before any commit and provides the
backend with a size and SHA-256 digest. A staging file the server cannot
create, write, flush, seek, stat or close, such as on a full disk, is answered
`503` with `Retry-After` before any backend call, never as a fault of the
request; a staging file that is no longer the regular file of the size
written is an integrity failure, answered `500`.
`StoreBackend.Store` must consume the
reader synchronously and atomically decide stored, identical duplicate,
warning, or conflict; a separate existence check is not a safe commit gate.

QIDO and metadata results are also spooled to 0600 temporary files so the
handler can validate the complete dataset representations and set pagination warnings
without retaining a second response-sized copy in memory. `SpoolDirectory`
therefore may contain PHI from both requests and responses. Configure it on
access-controlled storage (and encrypted storage when required by deployment
policy); the handler closes and removes spool entries on success and error, and
reports a redacted backend failure if synchronous cleanup cannot complete. At
startup, the server scavenges recognized inactive request/response files and
bounded flat retrieval job directories. Unrelated entries, symlinks and active
bounded jobs are preserved; see the
[retrieval profile](DICOMWEB_RETRIEVE_TRANSFORM.md) for recovery/lifetime limits.
`SpoolRetentionAge` controls stale-file age and
`SpoolAggregateQuotaBytes` removes the oldest inactive owned entries until the
aggregate quota is met; zero values use finite defaults.

Backend-provided object and frame locations are not accepted. The handler
synthesizes relative same-origin locations from validated identifiers.
Outbound QIDO/WADO DICOM JSON `BulkDataURI` values are accepted only under the configured
same-origin `{ServiceRoot}/bulkdata/...`
tokens and are reauthorized through the bulk route on every request. Absolute,
cross-origin, query-bearing, or fragment-bearing values fail closed.

### Revocable Studies Service reads

`ServerOptions.AuthorizeReadLease` can issue a short-lived grant after the
normal request authorization step. It covers QIDO-RS and Studies Service
retrieval, including metadata, Part 10, frames, rendered, thumbnails, BulkData
and the optional WADO-URI route. The application supplies the expiry and can
recheck session state, the active policy version and required audit availability
in `ReadLease.Validate`. Expiry cancels the backend context at the deadline.
Once a check denies the grant, it stays denied for that request.

The scope of the grant and access to individual studies belong to the
application. A backend applies its authorized scope before computing a
QIDO page or its remaining count, and calls `dicomweb.CheckReadLease(ctx)`
before opening an object, advancing a cursor, or starting protected work.
The core rechecks its own boundaries, including after waiting for a WADO
preparation slot and before reading or sending response bytes. A check in the
backend uses the same grant as the server.

Denial before the response starts returns HTTP 403 without the staged
data. Denial during streaming stops further reads and aborts the response so
the client can detect truncation. The audit event identifies the failure as
`read_lease_denied`. Bytes already accepted by the transport and work already
admitted by an earlier check may finish. This includes an in-flight codec
call on the private staging copy. Design the application's validators to
perform fast live checks and honor cancellation.
STOW-RS and UPS-RS use the existing request authorizer.

## Endpoints and media types

The implemented HTTP contract is summarized below. `{study}`, `{series}`, and
`{instance}` are validated DICOM UIDs; `{frames}` is a comma-separated list of
one-based frame numbers; `{token...}` is a server-issued same-origin bulk token.

| Service | Method and route | Request / `Accept` | Success response | Normal success status |
|---|---|---|---|---:|
| QIDO | `GET /studies`, `GET /series`, `GET /instances` | DICOM JSON or Native XML metadata | DICOM JSON, or `multipart/related; type="application/dicom+xml"` | `200` |
| QIDO | `GET /studies/{study}/series`, `GET /studies/{study}/instances` | DICOM JSON or Native XML metadata | DICOM JSON, or `multipart/related; type="application/dicom+xml"` | `200` |
| QIDO | `GET /studies/{study}/series/{series}/instances` | DICOM JSON or Native XML metadata | DICOM JSON, or `multipart/related; type="application/dicom+xml"` | `200` |
| WADO metadata | `GET /studies/{study}/metadata`, `GET /studies/{study}/series/{series}/metadata`, `GET /studies/{study}/series/{series}/instances/{instance}/metadata` | DICOM JSON or Native XML metadata | DICOM JSON, or `multipart/related; type="application/dicom+xml"` | `200` |
| WADO objects | `GET /studies/{study}`, `GET /studies/{study}/series/{series}`, `GET /studies/{study}/series/{series}/instances/{instance}` | negotiated DICOM media | `multipart/related` (direct instance representation when explicitly negotiated) | `200` |
| WADO frames | `GET /studies/{study}/series/{series}/instances/{instance}/frames/{frames}` | negotiated frame media | `multipart/related` | `200` |
| WADO rendered | append `/rendered` to a study, series, instance, or selected-frames route | negotiated rendered media; optional `viewport`, `window`, `quality`, `annotation`, and `iccprofile` | direct representation for one result or `multipart/related` for collections/multiple frames | `200` |
| WADO thumbnail | append `/thumbnail` to a study, series, instance, or selected-frames route | direct rendered image media; optional two-value `viewport` | one direct rendered image | `200` |
| WADO bulk | `GET /bulkdata/{token...}` | negotiated bulk media | `multipart/related` or explicitly negotiated direct representation | `200` |
| STOW | `POST /studies`, `POST /studies/{study}` | `multipart/related` with root type `application/dicom`, `application/dicom+json`, or `application/dicom+xml`; `Accept: application/dicom+json` | `application/dicom+json` | `200`, `202`, or `409` |
| UPS-RS | `POST /workitems`, `GET /workitems`, `GET /workitems/{workitem}`, `POST /workitems/{workitem}`, `PUT /workitems/{workitem}/state`, `POST /workitems/{workitem}/cancelrequest` | `application/dicom+json` dataset | `application/dicom+json` or no body | `200`, `201`, or `202` |
| UPS-RS subscriptions | `POST /workitems/{workitem}/subscribers/{aetitle}`, `DELETE /workitems/{workitem}/subscribers/{aetitle}`, `POST /workitems/1.2.840.10008.5.1.4.34.5/subscribers/{aetitle}/suspend`, `GET /subscribers/{aetitle}` (WebSocket) | query parameters only | no body; WebSocket text messages | `200`, `201`, or `101` |

Common failures are `400` malformed request, `401` unauthenticated, `403`
forbidden, `404` unknown resource, `405` wrong method (with `Allow`), `406`
unacceptable response media, `413` resource limit, `415` unsupported request
media, `499` canceled client request, `504` deadline, and `500` redacted backend
failure. Error bodies are `text/plain; charset=utf-8` and deliberately contain
no backend or DICOM detail.

DICOM JSON remains the default representation. Native XML is selected through
`Accept: multipart/related; type="application/dicom+xml"`; each QIDO result or
WADO metadata instance occupies one bounded part. WADO XML parts include a
same-origin `Content-Location`, and the client verifies that its study, series,
and instance identifiers match the decoded dataset before returning it.

QIDO-RS implements all six native search resources below. When the service is
mounted below the origin root, prefix each route with `ServerOptions.ServiceRoot`
and mount the handler with `http.StripPrefix` using the same value.

- `GET /studies`
- `GET /series`
- `GET /instances`
- `GET /studies/{study}/series`
- `GET /studies/{study}/instances`
- `GET /studies/{study}/series/{series}/instances`

The handler validates case-sensitive standard-keyword filters, `includefield`, `limit`,
`offset`, `fuzzymatching`, `emptyvaluematching`, and
`multiplevaluematching`. Results use `application/dicom+json`; an empty result
is `200` with `[]`. Backends report the exact remaining count and which optional
matching modes were applied so the handler can emit `Warning: 299` without guessing.
`dicomweb.NewSearchMatcher` applies a request's filters and path UIDs to
candidate data sets with `qrmatch` rules; given
`SearchMatcherOptions.FuzzyPersonNames` (a `personname/fuzzy` encoder) it
applies phonetic person-name matching to `fuzzymatching=true` requests and its
`FuzzyMatchingApplied` result suppresses the fuzzymatching warning.
Private-tag and File Meta match keys are rejected in this initial profile rather
than accepting a private key without its required Private Creator.

WADO-RS implements study, series, and instance retrieval; study, series, and
instance metadata; selected one-based frames; and opaque same-origin bulk data.
Object and frame payloads stream as `multipart/related`; instance and bulk data
also support explicitly negotiated direct representations. Every transaction
that may return a payload requires an `Accept` header; absence returns `406`.
An explicit `*/*` selects the standard multipart default. The backend receives ordered media type and Transfer
Syntax preferences. It must return bytes in the syntax it declares. The default
handler returns `406` for an unacceptable representation. Opt-in
`RetrieveTranscode` stages and validates actual identities, then uses
`pixeldata.TranscodePath` for configured lossless conversions. `EnableWADOURI`
adds a partial `GET /wado` profile; `EnableBulkDataRanges` adds one byte range on
direct bulk data with explicit logical ReaderAt. See their media/publication
contracts in [prepared retrieval, URI service and ranges](DICOMWEB_RETRIEVE_TRANSFORM.md).

A backend that keeps Part 10 originals can implement metadata and bulk data
with `Part10Metadata` and `Part10BulkData`, given the same
`Part10MetadataOptions`. `Part10Metadata` parses a seekable object without
reading its large values and returns its data set, without File Meta
Information and group lengths, with root Pixel Data, Float Pixel Data and
Double Float Pixel Data, and binary or numeric values longer than
`BulkDataThreshold` (default 1,024 bytes) at any depth, replaced by the
`BulkDataURI` its callback returns. `FormatBulkDataPath` and
`ParseBulkDataPath` give a canonical token form of the attribute path
(`00540016.0/00281201`). `Part10BulkData` resolves that path to an
`application/octet-stream` part in Little Endian byte order, converting the
numeric, OW, OF, OD, OL and OV values of an Explicit VR Big Endian object by
the [byte order rules](PIXEL_TRANSCODING.md#byte-order) of `TranscodeDataSet`;
values of Little Endian objects also expose a `ReaderAt` for ranges. The
`InlineBinary` of OW, OF, OD, OL and OV values kept inline is in the same
Little Endian byte order, so one attribute does not change byte order with its
length. Every
non-deflated syntax is described, Implicit VR Little Endian and Explicit VR
Big Endian included. A deflated data set and the octet-stream form of
encapsulated Pixel Data, which is the decompressed pixels, return
`ErrUnsupported`; a path kept inline returns `ErrNotFound`.

The implemented response matrix is explicit:

- `application/dicom` accepts recognized DICOM Transfer Syntaxes except
  Implicit VR Little Endian and retired Explicit VR Big Endian, which PS3.18
  forbids for Web Services;
- uncompressed frame `application/octet-stream` accepts Explicit VR Little
  Endian and Encapsulated Uncompressed Explicit VR Little Endian;
- `image/jpeg`, `image/jls`, `image/jp2`, `image/jpx`, `image/jphc`,
  `image/jxl`, `image/dicom-rle`, and `application/x-deflate` accept only the
  corresponding standard Transfer Syntax family;
- `video/mpeg`, `video/mp4`, and `video/H265` accept only their corresponding
  MPEG-2, AVC/H.264, and HEVC/H.265 Transfer Syntax families.

The selected media type and Transfer Syntax are validated independently for
each multipart response part, as required by PS3.18.

Rendered and thumbnail routes are enabled when `ServerOptions.Renderer`
implements the backend-neutral `dicomweb.Renderer` interface. Without it these
routes return `501`. The renderer owns authorized resource lookup,
transfer-syntax decode, SOP-class support, presentation selection, and ordered
frame production. The handler owns query validation, media negotiation,
multipart framing, same-origin content locations, `private, no-store` cache
policy, and request-wide pixel/output budgets. It stages every rendered part
before publishing `200`, revalidates reported dimensions and cumulative bytes,
and returns a DICOMweb error when the renderer rejects an unsupported SOP class
or transfer syntax.

The `render.StandardEncodedRenderer` is the default reusable still-image
mechanism for an application renderer. It consumes an already-decoded
`render.Frame`, applies the shared modality/VOI grayscale or native color path,
crops before cancel-aware bilinear scaling, preserves aspect ratio, and emits
bounded `image/png` or 8-bit Baseline `image/jpeg`. A storage adapter interprets
the decimal DICOMweb source region, including defaults, clipping, and negative
width/height flips, then passes an integer `SourceRect` plus flip flags to the
encoder. Resource resolution remains outside `render` so neither library
package depends on Twin or an application database.

STOW-RS implements `POST /studies` and `POST /studies/{study}` for
`multipart/related; type="application/dicom"`, one Part 10 instance per part,
and metadata-based payloads rooted at `application/dicom+json` or
`application/dicom+xml`. JSON uses one non-empty array part; XML uses one strict
Native DICOM Model per metadata part. Metadata must precede bulk, must not
contain group 0002. Native `BulkDataURI` values match one used
`application/octet-stream` part by exact `Content-Location`. Compressed top-level
Pixel Data uses the still-image profile below. References are opaque
request-local keys: absolute-looking values are never fetched, and values from
an earlier request are never retained. The server reconstructs, bounds, hashes,
and reinspects every reconstructed Part 10 file before the first backend call.
The selected compressed transfer syntax is written in File Meta; all other
metadata values use Explicit VR Little Endian encoding.
The collection route may contain multiple studies; the scoped route UID is
checked against every instance before any backend call. Responses use DICOM JSON Retrieve URL, Referenced
SOP Sequence, Failed SOP Sequence, and Other Failures Sequence. HTTP status is
`200` for complete success, `202` for mixed success/warning/failure, `409` when
no instance is stored, `400` for malformed input, `413` for limits, and `415`
for unsupported request media.


### Compressed still-image metadata STOW

The bounded subset accepts `image/jpeg` with JPEG Baseline Process1(.50),
Extended Process2/4(.51), Lossless Process14(.57), or Lossless SV1(.70), and
`image/jls` with JPEG-LS Lossless(.80) or Near-Lossless(.81). These suffixes name
`1.2.840.10008.1.2.4.*` transfer syntax UIDs. Optional `transfer-syntax` must
match the registered media type and verified codestream coding parameters.
Without the parameter, the syntax is selected from those parameters; no
transcoding or second lossy encoding takes place. Process14 accepts predictor1
as well as other permitted predictors; SV1 requires predictor1. JPEG-LS .80
requires NEAR0, while .81 requires nonzero NEAR in this profile.

Single compressed frames use the exact metadata `BulkDataURI` as
Content-Location. Multiple compressed frames are separate parts at
`{BulkDataURI}/frames/{one-based number}`; every frame1..NumberOfFrames must occur
exactly once. `StoreBulkData.FrameNumber` writes this location from the base URI
without changing metadata. Arrival order does not determine frame order.
A direct part mixed with numbered parts, duplicates, missing/out-of-range
frames, differing coding/geometry between frames, and unreferenced parts are
refused. These are opaque request-local keys; no remote URI is fetched.

Rows, Columns, SamplesPerPixel, BitsAllocated, BitsStored and HighBit are
filled from inspected frame headers only when absent; explicit contradictions
are refused. Unsigned sample representation can be derived for the supported
lossy JPEG processes. Lossless JPEG and JPEG-LS do not encode DICOM signedness,
so PixelRepresentation must be explicit. Missing monochrome interpretation may
be MONOCHROME2 under PS3.18 10.5.2; color interpretation must be explicit because
component count alone does not prove RGB versus YBR. Color PlanarConfiguration
must be0. Signedness/interpretation and decoded geometry are checked through a
fresh built-in codec registry; structural inspection alone is not treated as
entropy validation.

All raw parts remain in the existing private bounded spools. Coding validation
holds one encoded frame and its bounded decoded samples at a time, bounded by
MaxPartBytes and MaxDecodedPartBytes. Original compressed frame spools are
rechecked for identity/size/SHA and copied by `object.WriteFileFrames`, which
writes offset tables, fragments and even padding. No all-frame resident payload
or new storage backend is introduced. Full multipart completion and sticky
non-EOF body errors are checked before any backend call. Existing request,
part/header, metadata/element and part-count limits, cancellation cleanup and
per-instance backend success/failure responses remain in force.

This profile excludes JPEG progressive/arithmetic/hierarchical processes,
JPEG2000/HTJ2K/JPEGXL, RLE/deflated-frame bulk and PNG/GIF transformations.
Whole-video and Encapsulated Document metadata use the separate profile below;
arbitrary-file import is excluded. It is a declared subset, not
complete STOW or IOD conformance. Offline JSON/XML single/multiframe and causal
failure tests are provided; execution and external-peer qualification are
separate evidence, not implied by this implementation description.

### Whole-video and Encapsulated Document metadata STOW

`Client.StoreMetadata` and `Server` support the following transaction-local
metadata/bulk matrix with both DICOM JSON and Native DICOM XML. Each instance
has exactly one whole payload, referenced by the exact `BulkDataURI` and
`Content-Location`. `StoreBulkData.FrameNumber` must be zero: video is one
complete representation even when Number of Frames exceeds one, not one part
per frame. No payload is transcoded or re-encoded.

| Bulk media type | SOP Class profile | Coding / payload |
| --- | --- | --- |
| `video/mpeg` | Video Endoscopic, Video Microscopic, Video Photographic | MPEG2 Main Profile/Main Level (.100) or High Level (.101), Program Stream |
| `video/mp4` | Same three Video VL SOP Classes | AVC/H.264 High Profile Level 4.1 (.102), non-fragmented MP4 |
| `application/pdf` | Encapsulated PDF (.104.1) | PDF 1.0–1.7 or 2.0 header and terminal EOF |
| `text/xml` | Encapsulated CDA (.104.2) | UTF-8 HL7 ClinicalDocument; declared DICOM MIME is `text/XML` |
| `model/stl` | Encapsulated STL (.104.3) | Finite binary triangles or bounded ASCII triangle records |
| `model/obj` | Encapsulated OBJ (.104.4) | UTF-8 vertex/normal/texture, face/line/point and material-reference records |
| `model/mtl` | Encapsulated MTL (.104.5) | UTF-8 named materials, finite scalar/color and texture-reference records |

SOP Class suffixes above use `1.2.840.10008.5.1.4.1.1`; transfer syntax suffixes
use `1.2.840.10008.1.2.4`. Video SOP Class suffixes are .77.1.1.1, .77.1.2.1
and .77.1.4.1 respectively, with explicit ES, GM and XC Modality. Documents
require DOC or M3D Modality and their matching MIME attribute (0042,0012).
A declared `transfer-syntax` must match inspected coding; documents use
Explicit VR Little Endian (.1.2.1). Optional `charset=utf-8` is accepted only
for the CDA bulk header. Other parameters are refused.

Video geometry, 8-bit unsigned 4:2:0 sample layout, frame count, frame timing
and compression method are inspected through `video.InspectSource`. Missing
verified pixel/timing facts may be derived; explicit contradictions refuse the
instance. Series Number and video Instance Number must be present but may be
empty (Type 2). Anatomic Region is required when Specimen Description is absent;
its code is supplied by the caller. No clinical identification is invented.
The profile preserves container bytes in one fragment with an empty Basic
Offset Table and only required even-length DICOM padding.

Documents require nonempty Instance Number, explicit YES/NO Burned In Annotation,
and the Type 2 Content Date, Content Time, Acquisition DateTime, Document Title
and Concept Name Code Sequence (the latter permits zero items). STL/OBJ require
Frame of Reference UID and Position Reference Indicator; M3D documents require
Measurement Units Code Sequence. Length is derived from the received bytes;
CDA HL7 Instance Identifier is derived only from its verified direct `id`
root/extension. Supplied length or identifier contradictions refuse the instance.
Original document bytes are streamed from the identity/hash-checked spool.

`encapdoc.InspectPayload` exposes the same bounded structural inspection for
reuse outside STOW, without file/network access or changing constructor
compatibility. Zero limits select finite defaults (256 MiB, depth 64, 100,000
records); hard admission ceilings are 1 GiB, depth 256 and 1,000,000 records.
STOW further applies its configured request/part/decoded-byte/element/frame
limits. Structural inspection holds one whole encoded payload in memory,
not decoded video pictures; MPEG parsing may additionally hold its bounded
elementary stream. XML token reads observe cancellation. OBJ indices must refer
to preceding records (positive or relative negative indices). Material/texture
reference strings remain opaque and are never fetched.

MIME/content/SOP/coding and missing mandatory-media-context errors are reported
through the existing per-instance Failed SOP Sequence: mixed results return
202, all failures 409. Malformed multipart, missing/duplicate/unreferenced bulk,
reused whole bulk across instances/attributes, resource overflow and cancellation
abort staging before any backend call. A URI from an earlier request cannot
resolve a payload in a later request. Backend persistence policy remains owned
by `StoreBackend`.


Primary authority: [PS3.18 Store](https://dicom.nema.org/medical/dicom/current/output/chtml/part18/sect_10.5.html),
[Store behavior](https://dicom.nema.org/medical/dicom/current/output/chtml/part18/sect_10.5.2.html),
[video media types](https://dicom.nema.org/medical/dicom/current/output/chtml/part18/sect_8.7.3.3.2.html),
[Encapsulated Document](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.24.2.html)
and [VL Image](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.8.12.html).

## UPS-RS workitems and event channel

`ServerOptions.Workitems` (or a `Backend` implementing `WorkitemBackend`)
serves the Worklist Service of PS3.18 section 11; `WorkitemSubscriptionBackend`
adds the subscription resources and `ServerOptions.WorkitemEvents` (a
`WorkitemEventHub`) the WebSocket event channel. `ups.NewDICOMwebBackend`
implements both interfaces over the `ups` state machine, so the REST and DIMSE
transports share validation, the Transaction UID lock, subscriptions and the
durable event outbox (see [`UNIFIED_PROCEDURE_STEP.md`](UNIFIED_PROCEDURE_STEP.md#dicomweb-ups-rs)).

| Transaction | Route | Request | Success |
|---|---|---|---|
| Create | `POST /workitems{?workitem}` | one dataset; the UID comes from `workitem` (also `AffectedSOPInstanceUID` or a bare UID query), else from SOP Instance UID in the dataset, else the backend assigns one | `201` with `Location` and `Content-Location` |
| Retrieve | `GET /workitems/{workitem}` | none | `200`, one dataset without Transaction UID |
| Update | `POST /workitems/{workitem}{?transaction}` | one dataset of modifications | `200` |
| Change State | `PUT /workitems/{workitem}/state` | Procedure Step State and Transaction UID only | `200` |
| Request Cancellation | `POST /workitems/{workitem}/cancelrequest` | optional dataset (reason, discontinuation code, contact) | `202` |
| Search | `GET /workitems{?query}` | QIDO-style matching keys, `includefield`, `limit`, `offset` | `200` with DICOM JSON |
| Subscribe | `POST /workitems/{workitem}/subscribers/{aetitle}{?deletionlock,filter}` | `filter=attribute=value{,attribute=value}` (PS3.18 §11.10) only on the filtered global UID `1.2.840.10008.5.1.4.34.5.1`; only literal commas separate pairs, so the earlier form of one percent-encoded `filter` parameter per pair is also accepted and a `%2C` in it stays in the value | `201` without `Location` (PS3.18 defines none; the event channel is `/subscribers/{aetitle}`) |
| Unsubscribe | `DELETE /workitems/{workitem}/subscribers/{aetitle}` | none | `200` |
| Suspend Global Subscription | `POST /workitems/{global}/subscribers/{aetitle}/suspend` | none | `200` |
| Open Event Channel | `GET /subscribers/{aetitle}` with an RFC 6455 upgrade | none | `101` |

Request bodies are `application/dicom+json` (optionally `charset=utf-8`)
holding a JSON array with exactly one dataset (a bare object is also
accepted), bounded by `MaxMetadataBytes` and the JSON value/depth limits; bulk
data references and group 0002 are refused (`400`), other media `415`.
Unknown query parameters are `400`. Backend sentinels map to `400`, `403`,
`404` (unknown workitem), `409` (wrong Transaction UID, a workitem already
claimed or in a final state, a duplicate UID) and `503` (resource limit);
other backend errors are redacted to `500`. Successful transactions with
backend warnings, such as an already completed workitem or a refused deletion
lock, add `Warning: 299 <service>: <text>` headers. Only DICOM JSON is
offered for Retrieve and Search (`406` otherwise). Authorization sees one
`Operation` per transaction, including `open_event_channel`, before any
backend work or protocol upgrade.

Each event channel message is a DICOM JSON object: the PS3.4 CC.2.4.3 event
information plus Affected SOP Class UID (UPS Event,
`1.2.840.10008.5.1.4.34.6.4`), Message ID, Affected SOP Instance UID and
Event Type ID. The hub writes every event to all open channels of the
Receiving AE Title (at most `MaxConnectionsPerAETitle`, default 8, and
`MaxConnections`, default 256, else `503` before upgrading), discards client
messages beyond 4 KiB, pings idle channels every 30 s and closes a channel
whose write exceeds `WriteTimeout`. Browser requests must be same-origin
unless `CheckOrigin` allows them (`403`), which blocks cross-site WebSocket
hijacking. Event channels are hijacked connections that the HTTP server does
not track; `Server.Shutdown` closes them with status 1001. The WebSocket
framing is a clean-room RFC 6455 implementation (`internal/websocket`) with
masking, fragmentation, UTF-8, close-code and size checks; extensions and
subprotocols are not negotiated.

The contract is covered by
[`server_workitems_test.go`](../net/dicomweb/server_workitems_test.go) and,
end to end over `ups`, by [`../ups/dicomweb_test.go`](../ups/dicomweb_test.go).

## Limits and cancellation

`DefaultServerLimits` is finite for request, metadata, encoded and decoded part bytes,
response, rendered pixels/output, URI, headers, query values/pairs, result count, offset, frame count,
multipart count, concurrency, and duration. Zero option fields receive these defaults; negative values are
invalid. The optional owned HTTP server also sets finite read-header, read,
write, idle, and header limits and supports graceful `Shutdown`.

`MaxQueuedRequests` and `MaxQueueWait` are the exception: zero disables the
queue, so a request above `MaxConcurrentRequests` receives `503` at once.
With both set, up to `MaxQueuedRequests` requests wait for a slot, each for
at most `MaxQueueWait` (clipped to `MaxDuration`), before routing,
authorization or reading the body; a full queue or an ended wait is `503`
with `Retry-After: 1`, and a canceled client leaves the queue with `499`.
Every `503` the handler writes carries `Retry-After`. The connection's read
and write deadlines (`ReadTimeout`, `WriteTimeout`) start with the request,
before its wait, so a request leaving the queue, with a slot or with its
`503`, has them moved to `MaxDuration` from that moment through
`http.ResponseController`: it keeps the same time to read its body and write
its answer as its handler context, instead of losing the connection without
a status. A middleware that wraps the `ResponseWriter` must provide `Unwrap`
for this to reach the connection.

`ServiceRoot` is a path, not an externally trusted origin override. When a TLS
terminator or reverse proxy changes scheme or authority, the application must
apply its usual trusted-forwarded-header policy before the handler; the core
does not trust arbitrary forwarded headers.

Request cancellation is checked before authorization, during query/result
work, while staging STOW parts, between backend calls, and during every streamed
copy. A backend reader that can block independently of its context remains
responsible for making its own `Read` context-aware.

## Minimal fail-closed localhost example

The example below intentionally binds only to loopback and requires an
authorizer. Use a synthetic backend in local tests; never put patient data in
examples, fixtures, URLs, or logs.

```go
limits := dicomweb.DefaultServerLimits()
limits.MaxRequestBytes = 512 << 20
limits.MaxPartBytes = 256 << 20
limits.MaxMetadataBytes = 32 << 20
limits.MaxDuration = 2 * time.Minute

handler, err := dicomweb.NewServer(dicomweb.ServerOptions{
    Backend:        syntheticBackend,
    ServiceRoot:    "/dicomweb",
    Authorize:      authorizer, // return ErrUnauthorized/ErrForbidden on denial
    Limits:         limits,
    SpoolDirectory: protectedTemporaryDirectory,
    Audit:          phiFreeAuditSink,
})
if err != nil {
    log.Fatal(err)
}

mux := http.NewServeMux()
mux.Handle("/dicomweb/", http.StripPrefix("/dicomweb", handler))
server := &http.Server{
    Addr:              "127.0.0.1:8042",
    Handler:           mux,
    ReadHeaderTimeout: 5 * time.Second,
    ReadTimeout:       3 * time.Minute,
    WriteTimeout:      3 * time.Minute,
    IdleTimeout:       30 * time.Second,
    MaxHeaderBytes:    1 << 20,
}
log.Fatal(server.ListenAndServe())
```

For production, replace loopback HTTP with the reverse-proxy architecture
above or configure an application-owned TLS server. Do not set
`AllowUnauthenticated`; that switch exists for isolated tests and explicitly
trusted deployments only.

## Deliberate initial limitations

This profile preserves its documented JPEG/JPEG-LS compressed STOW subset
without transcoding; it does not transform other compressed media, and does not
implement DELETE of instances or Native XML workitems. Optional retrieval
preparation uses RetrieveBackend Part 10 inputs and explicit codecs; it does
not create lossy derived identities or promise arbitrary codecs. WADO-URI
implements a documented subset, and bulk Range requires a known direct
random-access value. Presentation State rendering,
annotations, and ICC transforms depend on the injected renderer. These omissions mean the handler does not claim
the complete mandatory media-type surface of a current PS3.18 Studies Service.
Unsupported response media returns `406`; unsupported request media returns
`415`. Applications may add capabilities behind the backend interfaces without
weakening the default policy.

The package tests use the existing `dicomweb.Client` as an internal black-box
peer for all implemented transactions. An opt-in independent curl smoke gate is
available through `DICOMGO_DICOMWEB_CURL=1`; broader OHIF, dcm4che, and
dicomweb-client validation remains a deployment interoperability gate.

The primary contract tests are
[`server_test.go`](../net/dicomweb/server_test.go),
[`server_hardening_test.go`](../net/dicomweb/server_hardening_test.go), and
[`server_characterization_test.go`](../net/dicomweb/server_characterization_test.go),
plus [`server_rendered_830_test.go`](../net/dicomweb/server_rendered_830_test.go)
and [`server_stow_metadata_test.go`](../net/dicomweb/server_stow_metadata_test.go)
with its independent security suite,
and [`../render/encoded_test.go`](../render/encoded_test.go).

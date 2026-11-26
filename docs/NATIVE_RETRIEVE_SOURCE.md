# Original-first retrieval sources

`net/storetranscode.NewNativeSource` and `NewNativePathSource` implement the
optional `dimse.StoreTransferSyntaxSource` contract. `StoreSession` selects an
accepted presentation context before calling `OpenForTransferSyntax`. The
original transfer syntax is offered first; `Inspect` performs no pixel decode
or spool preparation. Native Explicit/Implicit VR Little Endian fallback is
offered only when the caller's registry can produce it. Legacy `StoreSource.Open`
and the eager [transcode source](CSTORE_TRANSCODE_SOURCE.md) remain supported.

If the peer accepts the original, its raw dataset bytes are sent unchanged from
a private, validated spool. Native fallback decodes once per preparation using
the existing transcoder, closes dataset/codec resources and writes an immutable
spool before any C-STORE command. No lossy re-encoding is enabled. Path sources
capture SOP identity, transfer syntax, size and SHA-256; changes before the
snapshot is completed are refused rather than resolving a newer archive object.

## Budgets and ownership

`StoreDescriptor.TransferSyntaxSizes` contains positive finite bounds for every
offered representation. The planner's minimum-bound admission is provisional:
after negotiation, per-item, remaining-total and in-flight budgets use the
selected bound before opening or sending. A small original does not reserve the
unchosen native fallback's upper bound. A fallback may still be conservatively
refused even if its eventual bytes would fit. Unknown-size new sources use a
finite input bound; legacy sources retain their previous size semantics.

Caller-owned `NativeResources` shares preparation slots and aggregate spool
reservations across sessions. Preparation slots end before network handles are
returned. Spool admission does not wait while a previous network handle holds
its reservation, avoiding a one-slot pipeline deadlock. Reservations remain
until successful cleanup. `Close` reports filesystem cleanup failures and can
retry them; those failures do not imply recovered disk space. Limits cover
logical bytes and transcoder inputs, with additional codec/buffer overhead;
they are not a process RSS guarantee. Codecs without a context-aware interface
cannot be forcibly interrupted.

`CGetSubOperation.LoadStoreSource` is an optional source loader; the core owns
its returned handle and closes it once, including partial error handles. It
validates identity, roles and selected syntax before the command. Existing
dataset/file loaders retain their contracts. Cancellation keeps the terminal
retrieve counters consistent, but cannot undo an already emitted C-STORE.

Association `Started`/`Finished`, `OnStorePrepared` and `OnStoreResponse` are
separate from legacy progress callbacks. The response hook reports only a real,
correlated remote status. The synchronous prepared hook can read bounded
metadata from the selected open handle; its reader expires after the callback,
and an absent observer performs no scan. Observers must return promptly.
Panic containment does not make a blocking observer asynchronous. A legacy
path handle avoids a later pathname lookup but does not promise an atomic
snapshot against concurrent in-place mutation; the native source uses its
private validated snapshot.

## Authorization before reads and responses

`NativeOptions.BeforeSourceRead` can enforce a caller's current authorization
after waiting for a preparation slot and before opening the source. For a
generic `NewNativeSource` it also covers the first inspection when the base
source may read data. A described `NewNativePathSource` inspects only its
detached descriptor. Enforce query and listing access before supplying those
descriptors to DIMSE.

`NativeOptions.BeforeStore` travels with the prepared handle. The serial and
pipelined `StoreSession` paths call it after `OnStorePrepared`. The C-GET SCP
calls it after validating the returned handle. A refusal closes the handle,
stops later transfers and returns `dimse.ErrStoreAccessDenied`. Issued
transfers keep their own outcomes. An unsent instance has no C-STORE response
and its UID does not appear in the retrieve failure Identifier. C-MOVE batch
handlers can return the same error to end a partial retrieve with the counts
of transfers already reported. C-FIND handlers can attach a
`dimse.CFindMatch.BeforeSend` callback to revalidate access before each Pending
response. A refusal sends a final failure without the protected Identifier.

These synchronous callbacks can check live policy and require a durable audit
append before granting the operation. They receive operation contexts and must
return promptly. Refusal details stay out of DIMSE responses. A check applies
to the next effect and cannot withdraw bytes already sent.


The receiver shares this policy between C-GET and C-MOVE: one preparation,
256 MiB input, 512 MiB output, 128 million pixels and 1536 MiB aggregate spool.
Interactive sends retain their existing source policy. The decoder registry is
owned by the desktop adapter. JPEG 2000 Part 1 is opt-in through the documented
Go or OpenJPEG build profiles; this does not register Part 2, HTJ2K, media or
JPIP as still-image native fallback.

Local wire tests cover seven synthetic JPEG Baseline/Lossless/SV1/Process 4,
RLE, JPEG-LS and JPEG 2000 fixtures against original, explicit and implicit
representations through C-GET/C-MOVE (42 cases per Go/OpenJPEG profile).
Original bytes and fixture samples/metadata are compared, with the fixture's
documented tolerance for lossy DCT inputs. Focused race tests cover budgets,
cancellation, immutable selected metadata, response ordering and cleanup.
These are local loopback and bounded fixture proofs, not a target-PACS,
large-study memory or clinical qualification. The small 548-byte benchmark
demonstrates zero decodes for the accepted original and one for native fallback;
it does not establish a network speedup.

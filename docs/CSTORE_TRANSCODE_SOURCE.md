# Explicit C-STORE representation preparation

`net/storetranscode.NewSource` composes an existing `dimse.StoreSource` with the
existing `pixeldata.TranscodeDataSetPath` frame pipeline for qualified profiles
and the established `pixeldata.TranscodeDataSet` pipeline for resident profiles. It does not add codecs to DIMSE,
change `StoreSession` negotiation defaults, or implement a second pixel planner.
A zero target delegates unchanged to the base source. Selecting a target is an
explicit request to prepare that representation, not permission to guess a
transfer syntax or to apply loss on codec failure.

```go
source, err := storetranscode.NewSource(dimse.NewPathStoreSource(path), storetranscode.Options{
    Target: transfer.RLELossless,
    Transcode: pixeldata.TranscodeOptions{
        DecoderRegistry: decoders, // caller-owned, needed for compressed input
        EncoderRegistry: encoders, // caller-owned, e.g. rle.RegisterEncoder
    },
    MaxSpoolBytes: 512 << 20,
})
if err != nil { return err }
result, err := session.Store(ctx, source)
source.RecordResult(result)
report := source.Report()
```

No package-global encoder registry is used. `Options.Dictionary` optionally
resolves implicit VR and caller-owned private catalogs during source preparation.

## Preparation, offers and ownership

Construction is lazy. The first `Inspect` prepares the representation completely
before the batch planner can advertise it. It opens the existing source, writes
its original dataset bytes to a bounded private temporary file, checks source
descriptors, hashes the bytes, and reads the dataset through the ordinary parser.
SOP Class/Instance identity and pixel representation are checked through the
existing object-backed StoreSource descriptor logic. A requested transformation
then runs through the existing transcoder and serializer, including metadata,
codec/profile validation and actual encoder execution. Thus a registration alone
does not produce a false offer when its runtime or input profile fails.

For ordinary `NewSource`, Inspect removes all temporary files before returning and caches only detached
descriptors, a SHA-256 digest and any derived identity. It keeps no open input
handle or batch of pixel buffers. Subsequent inspection returns that plan.
`Open` reopens the original, checks its descriptors and complete byte digest, and
prepares the representation again before the C-STORE command. A changed source,
runtime, producible syntax set, output identity or encoded size fails the open.
Create a new adapter to plan a deliberately changed source. A conservative size
check can reject a nondeterministic encoder whose output length changes between
preflight and open; such output is not silently substituted after negotiation.

One target is selected per source; heterogeneous batches can select different
targets. `FallbackOriginal` additionally permits the original syntax and exact
original dataset bytes, after the requested target in offer order. A target that
cannot be produced is excluded with a reason; original fallback requires this
explicit option. Resource exhaustion and cancellation abort preparation.
Equivalent-syntax pass-through requires no pixel codec and preserves the encoded
bytes. Native conversion, decode, encode and decode/encode are distinct report
operations. No codec is invoked while the dataset is being sent on the network;
the writer only copies a previously verified temporary representation.

Every opened handle owns an independent private temporary directory and closes
it once, including send failure or cancellation. The adapter closes handles it
opens from the base source, according to the base source's existing ownership
contract. Borrowed input objects are not closed or modified. Standalone callers
of `Open` must call the returned `Close`; StoreSession already does so. Source
methods permit concurrent independent opens when the base source and injected
registries support concurrent use. Reports are detached, synchronized snapshots.

## Limits and cancellation

`MaxSpoolBytes` bounds aggregate input plus output temporary dataset bytes for
one preparation/handle (default 2 GiB). Files use `0600` within a newly created
private directory. Input and output limits also apply separately. The adapter
uses `pixeldata.ResolveTranscodeLimits` to reuse the transcoder's finite defaults
and validation; negative limits are rejected. Its duration covers preparation,
including source copy, hash, parse and transcode. Cancellation is checked during
file reads/writes and copies; codecs retain their existing cooperative context
contract. No detached worker queue is introduced.

Qualified lossless little-endian transformations now read top-level Pixel Data
frame by frame using the existing incremental transcoder. Ordered frame spools
and staged output share the remaining aggregate temporary-file budget, and
pipeline bounds separately limit frames in flight. The complete verified Part10
output is retained and its dataset offset used during send; no output-sized
recopy or codec execution happens in the network writer. Original snapshots
remain counted in the adapter spool quota. Resident paths include lossy identity
rebinding, big-endian/deflated syntaxes, nested icons, rewrite/recovery and layouts
which cannot be safely indexed. Their existing whole-object limits remain.
See [incremental raw preparation](TRANSCODE_DATASET_PATH.md) for precise scope.
No process-RSS reduction has been measured by this source change alone.
Set `Transcode.Limits` for the application's memory envelope.
Opaque pass-through can defer Pixel Data. `StoreSession.MaxInFlightBytes` reserves
the largest offered representation size for each admitted operation; preparation
may additionally hold one source's working buffers. An explicitly lossy target
is never substituted in response to lossless codec failure.

## Lossy identity and outcomes

Lossy encoding requires `Transcode.AllowLossy`. The existing transcoder creates
a new SOP Instance UID and updates lossy history/derived-image metadata. The
adapter retains the first verified new identity across inspection, open and any
explicit retries so the command and transmitted dataset agree. It does not reuse
the original instance identity. `AllowLossy` and `FallbackOriginal` are mutually
exclusive because these alternatives can have different SOP Instance identities.

The original file/object remains unchanged. A new adapter for the same original
can create a new derived instance; keep the same adapter when intentional retry
must retain its planned identity. Delivery/retry remains governed by StoreSession.
No valid terminal response after an invoked C-STORE means an unknown outcome;
a valid refused/failure response means confirmed failure. The pipeline now applies
the same C-STORE status check as the serial client, aborts an unusable association,
and respects `ContinueOnError` before considering remaining sources. It does not
classify remote failure statuses as success or treat transport loss as confirmed
failure. These semantics follow the confirmed service in
[PS3.7 2026c Section 9.1.1](https://dicom.nema.org/medical/dicom/2026c/output/chtml/part07/chapter_9.html).

## Diagnostics and CLI

`Report` records original syntax, candidate operation, producibility, exclusion
reason, unavailable runtime/registry and lossy transformation status. The selected
syntax is recorded when the writer is called; it is not proof of delivery.
`RecordResult` optionally attaches the existing StoreSession outcome and
negotiated syntax, including rejection, canceled, unknown and not-sent results.
Reasons are stable categories such as `encoder-unavailable`, `decoder-unavailable`,
`runtime-unavailable`, `profile-unsupported`, `lossy-disallowed`, `resource-limit`
and `source-changed`. Generic backend failures remain `preparation-failed`;
typed causes remain accessible with `errors.Is`. Default error/report text omits
instance UIDs, origins, paths, patient data and backend-controlled text.

The CLI builds caller-owned builtin decoder registries and registers the existing
pure-Go RLE/JPEG-LS encoders only when `-transcode-to` is selected. JPEG Baseline
encoding additionally requires `-allow-lossy`; quality defaults explicitly to 90
and can be set with `-jpeg-quality`. Other requested syntaxes are offered only
if the selected implementation can actually produce them.

```sh
go run ./cmd/storescu -transcode-to 1.2.840.10008.1.2.5 -called HOROS 192.168.15.3:4007 synthetic.dcm
go run ./cmd/storescu -transcode-to 1.2.840.10008.1.2.4.80 -transcode-fallback-original -max-invoked 4 127.0.0.1:104 synthetic.dcm
go run ./cmd/storescu -transcode-to 1.2.840.10008.1.2.4.50 -allow-lossy -jpeg-quality 95 127.0.0.1:104 synthetic.dcm
```

`-transcode-spool-dir` chooses the temporary parent;
`-transcode-max-spool-bytes` sets its per-handle aggregate limit. Default CLI output
is unchanged without transcode selection. When selected, reports correlate only
by source ordinal and standard syntax UID. These are trusted-network tools;
transport/authentication/deployment protections remain caller responsibilities.

## Local qualification

Always-on tests cover exact lossless metadata/pixel roundtrip, original byte
preservation, missing codecs, unsupported profiles, runtime/source changes,
explicit original fallback, bounded spool writes (including a source ignoring a
write error), cancellation, close errors, independent concurrent handles, and
stable authorized lossy identity. Real local async tests hold two requests
outstanding and test success, refusal, cancel and transport abort without retrying
uncertain operations. The existing serial/planner/pipeline tests also run.


The optional pinned pynetdicom/pydicom/Pillow gate receives JPEG Baseline,
checks complete metadata and command/derived identity, and independently decodes
65,536 samples with libjpeg. The quality-95 synthetic ramp has explicit maximum
error, PSNR and local mean-error bounds; this does not qualify arbitrary lossy
clinical use. The profile is isolated from normal dependencies and never installed
by `make check`:

```sh
python -m pip install -r scripts/requirements-store-transcode-interop.txt
DICOMGO_PYNETDICOM_INTEGRATION=1 DICOMGO_PYTHON=/path/to/python go test -race ./net/storetranscode -count=1
```

## Explicit session-owned prepared representations

`NewSource` retains its existing default behavior: inspection releases temporary
bytes, and every open prepares again. `NewPreparedSession` is a separate opt-in
owner that retains one verified representation per `PreparedSource`:

```go
binding, err := storetranscode.NewBuiltinPreparedBinding(
    storetranscode.BuiltinPreparedBindingOptions{JPEGQuality: 95})
if err != nil { return err }
owner, err := storetranscode.NewPreparedSession(ctx,
    storetranscode.PreparedSessionOptions{MaxRetainedBytes: 512 << 20})
if err != nil { return err }
// Close the DIMSE session/attempts first, then close owner and binding with a
// bounded cleanup context. Preserve close errors.
source, err := owner.NewSource(dimse.NewPathStoreSource(path),
    storetranscode.Options{Target: transfer.RLELossless}, binding)
if err != nil { return err }
result, err := session.Store(ctx, source)
source.RecordResult(result)
```

This owner is limited to privately constructed immutable built-in pure-Go codec
bindings. It captures real codec constructors, parameters, ordered syntax set
and encoder profiles; it accepts no caller fingerprint, mutable registry or
runtime path. Optional/native runtimes, custom registries, custom dictionaries,
rewrite callbacks and codestream adapters use ordinary `NewSource`. A closed
binding or changed source/policy/syntax identity fails before C-STORE. Both
per-source spool limits and the aggregate session bound apply.

Each open reopens the original through its source contract, checks descriptors
and Origin, and hashes the complete dataset again. It checks every retained
representation's complete digest/size and opens independent reader handles,
retaining those checked inodes through the actual attempt. No encoder executes
on a reused open or network write. The same verified newly lossy bytes and
new derived SOP Instance UID survive an explicitly authorized retry. Original
fallback remains incompatible with newly lossy output.

Preparation reserves conservative capacity before writing, including the
existing incremental frame/staging budget. On publication, capacity becomes the
actual retained regular-file logical lengths in the private directory. Multiple readers do
not duplicate this charge. `Stats` distinguishes reserved capacity from retained
bytes and records their reserved-plus-retained peak; these values are not RSS
or physical APFS allocation. OS file removal is synchronous and a blocked
filesystem call itself is not hard-interrupted by the cleanup context. Resource exhaustion
fails explicitly rather than creating an unbounded queue. No global cache,
implicit eviction or cross-session reuse exists.

`Close(ctx)` rejects new operations, requests cancellation, revokes idle handles
and waits for active preparation/read/write exit before removing private files.
A blocked destination or uncooperative operation can exceed the cleanup context;
close then returns its deadline with truthful retained/reserved accounting.
Another close can wait for completion. Removal errors are reported and their
quota stays charged; a subsequent close retries only failed removals after
active operations and handles have drained. Concurrent callers share each
cleanup attempt and earlier error results remain stable after a retry succeeds.
Borrowed original sources, datasets and caller resources
are not destroyed. Close individual opened handles even on transfer failure;
then close the prepared owner on success, association rejection, unknown outcome
or cancellation.

Retention does not authorize transmission. `StoreSession.RetryUncertain` remains
false by default, and its existing attempt/reconnect limits remain authoritative.
An association rejection sends no data; absent a valid terminal response after
an invoked C-STORE, the result remains unknown. Explicit retry revalidates the
original and binding again; it never re-encodes to change a lossy identity.

`storescu -transcode-prepared-session` explicitly enables this lifetime for one
invocation and requires `-transcode-to`. `-transcode-prepared-max-bytes` controls
the aggregate owner quota (zero selects 2 GiB); it does not replace the existing
per-preparation `-transcode-max-spool-bytes` or DIMSE in-flight bound. There is no
automatic Twin benefit.

The informational `BenchmarkPreparedRepresentation` exposes
actual encoder call counts/time separately from original revalidation, spool
integrity checks, retained bytes and storage/destination copy time. Worker sums
can exceed wall time; copy includes destination blocking. Reuse adds retained
storage and repeated integrity/source reads, so no universal speedup is claimed.


## Prepared-session measured costs

This separate fixed campaign ran twelve fresh workers once on darwin/arm64,
Go1.27.1, CGO enabled, GOMAXPROCS=1 and GOMEMLIMIT=1GiB. Each worker used two
2×2 unsigned8-bit MONOCHROME2 frames (all8samples0..7), one initial Inspect and
32 Open→WriteDataSet→Close cycles. Ordinary and prepared arms shared the same
immutable pure-Go bindings: RLE Lossless or JPEG baseline quality95 with explicit
lossy authorization and maximum error4. Per codec, the worker order was
ordinary,prepared,prepared,ordinary,ordinary,prepared: three observations each.
The full G5 product source and external instrument were SHA-sealed; no retries,
filtering, threshold changes, unavailable-codec passes or skips occurred.

Actual encoder calls were66 for ordinary (2 initial plus64 repeated) and2 for
prepared (2 initial,0 repeated) in all twelve workers. Every output independently
passed full-sample/metadata decode using the selected target syntax, exact
selected descriptor size and dataset/descriptor SOP Instance UID checks. RLE and JPEG sample error was0 in this tiny corpus. Newly
lossy derived UID differed from original and stayed stable within each source;
wire bytes were identical across its32 writes. Fresh sources use distinct
derived UIDs, so cross-worker byte hashes are not compared. Final prepared
Entries/ReservedBytes/RetainedBytes were0; all temporary paths emptied. Outer
processes exited0 and their owned PGs were reaped/drained; source/binary SHA and
mode inventories matched before/after each worker. Five separate focused gates
passed19 new selectors,38 regressions, race19, minimum Go1.22.12 combined57 and
minimum-Go vet in both affected packages, without skips. Full integration
baseline qualification is a separate delivery check.


All12 values below are sums for the32 cycles (initial Inspect separately). Times in ms; encoder reuse is Final−Initial. RSS in MiB covers the whole worker.

| # | codec | arm/run | Initial | Open32 | Copy32 | Close32 | encoder initial | encoder32 | validation32 | integrity32 | RSS |
|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|1|rle|ordinary/1|14.756291|291.650166|1.527711|6.379080|0.000625|0.026622|unmeasured|unmeasured|25.359|
|2|rle|prepared/1|7.536458|3.027083|0.774834|0.021752|0.001083|0.000000|1.403166|0.547541|23.016|
|3|rle|prepared/2|11.290458|3.286666|0.854457|0.025165|0.001417|0.000000|1.545875|0.603001|22.969|
|4|rle|ordinary/2|11.459333|272.852962|1.140955|4.762167|0.001417|0.020585|unmeasured|unmeasured|24.438|
|5|rle|ordinary/3|8.216458|279.354752|1.099251|4.917415|0.036083|0.023004|unmeasured|unmeasured|24.812|
|6|rle|prepared/3|10.319917|3.660876|0.884249|0.035168|0.001375|0.000000|1.670296|0.657707|22.938|
|7|jpeg|ordinary/1|0.981625|11.053540|0.905834|4.620584|0.006583|0.096126|unmeasured|unmeasured|24.328|
|8|jpeg|prepared/1|0.592000|3.742707|0.748832|0.024540|0.018542|0.000000|1.710833|0.825830|23.531|
|9|jpeg|prepared/2|0.617209|4.142871|0.657667|0.028829|0.006750|0.000000|1.589207|1.098127|23.047|
|10|jpeg|ordinary/2|0.672916|10.367621|0.972834|5.639248|0.008082|0.136124|unmeasured|unmeasured|25.000|
|11|jpeg|ordinary/3|0.816750|12.504626|0.753832|6.124585|0.007250|0.117794|unmeasured|unmeasured|24.844|
|12|jpeg|prepared/3|0.595416|3.814121|0.588748|0.030917|0.007374|0.000000|1.511881|1.106668|22.891|

Medians [min–max] over three workers; times ms and RSS MiB.

| codec/arm | Initial | Open32 | Copy32 | Close32 | encoder32 | validation32 | integrity32 | RSS |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
|rle/ordinary|11.459333 [8.216458–14.756291]|279.354752 [272.852962–291.650166]|1.140955 [1.099251–1.527711]|4.917415 [4.762167–6.379080]|0.023004 [0.020585–0.026622]|unmeasured|unmeasured|24.812500 [24.437500–25.359375]|
|rle/prepared|10.319917 [7.536458–11.290458]|3.286666 [3.027083–3.660876]|0.854457 [0.774834–0.884249]|0.025165 [0.021752–0.035168]|0.000000 [0.000000–0.000000]|1.545875 [1.403166–1.670296]|0.603001 [0.547541–0.657707]|22.968750 [22.937500–23.015625]|
|jpeg/ordinary|0.816750 [0.672916–0.981625]|11.053540 [10.367621–12.504626]|0.905834 [0.753832–0.972834]|5.639248 [4.620584–6.124585]|0.117794 [0.096126–0.136124]|unmeasured|unmeasured|24.843750 [24.328125–25.000000]|
|jpeg/prepared|0.595416 [0.592000–0.617209]|3.814121 [3.742707–4.142871]|0.657667 [0.588748–0.748832]|0.028829 [0.024540–0.030917]|0.000000 [0.000000–0.000000]|1.589207 [1.511881–1.710833]|1.098127 [0.825830–1.106668]|23.046875 [22.890625–23.531250]|

Prepared retained logical spool was1376bytes for RLE and2130bytes for JPEG; ordinary had the same observed sizes while an Open lease was live, then removed them at lease Close. Prepared kept the verified representation across cycles until session Close. Session peak reserved+retained capacity was1MiB, a conservative logical admission reservation rather than physical disk use or measured heap growth. Quiescent spool observations do not bound every transient preparation peak.

Prepared median original validation over32 cycles was1.546ms RLE and1.589ms JPEG; spool integrity0.603ms RLE and1.098ms JPEG. Original snapshot/validation stream and prepared copy timers are also preserved in aggregate.json. Encoder timing is only the instrumented EncodeFrame invocation wrapper duration, excluding pipeline setup, source preparation and other codec coordination; it does not establish total codec CPU time; source preparation and filesystem work dominate these tiny fixtures. Timers overlap and must not be added as disjoint phases. Ordinary internal validation/integrity costs were not separately instrumented.

RSS includes worker startup, test runtime, fixture setup,32 output buffers and independent decode oracles. It does not measure incremental cache memory or physical APFS allocation. Three tiny synthetic runs per arm on one macOS host cannot establish universal speedup, larger-volume costs, target PACS interoperability or optional codec/native Windows performance. The full raw metrics and oracles—not only medians—are preserved.

# C-STORE batch sessions

`net/dimse.StoreSession` plans and sends heterogeneous C-STORE batches without
materializing every file or retaining one file descriptor per input.

## Sources and ownership

- `NewPathStoreSource` inspects Part 10 metadata with `index`, closes the
  inspection handle, and reopens one file only when its turn is sent. The data
  set byte range is copied verbatim in the original Transfer Syntax, including
  compressed, video, Big Endian, and deflated representations. A native
  Explicit/Implicit Little Endian fallback is re-encoded only when negotiated.
  Deflated Explicit VR Little Endian sources are native data sets: they are
  proposed as Deflated first and then as Explicit/Implicit Little Endian, which
  requires inflation but no pixel codec, bounded by 1024x the file size. A
  Deflated data set sent verbatim whose bitstream has an odd length gets the
  PS3.5 A.5 NULL pad, so no P-DATA fragment is odd (DCMTK aborts on one). Native
  sources are never deflated implicitly, and encapsulated sources are offered
  only in their own syntax.
  The final path component may not be a symbolic link, and each attempt opens
  the physical path with no-follow semantics before revalidating metadata.
- `NewDescribedPathStoreSource` accepts metadata already persisted by an archive
  but still reopens and revalidates the file before the C-STORE command.
- `NewRootedPathStoreSource` opens the absolute scanner root and every relative
  component with no-follow semantics. The CLI pins the physical root resolved
  during the scan, preserving its symlink policy across the scan-to-send
  interval instead of trusting a later path lookup.
- `NewFileStoreSource` and `NewDataSetStoreSource` borrow their inputs. The
  caller must keep them immutable and keep deferred providers open until the
  operation returns. The session never closes borrowed inputs.
- `NewLazyStoreSource` owns each file returned by its opener and closes it once
  on every path, including cancellation and remote failure.

Origins can contain sensitive filesystem data. They are returned only as an
explicit `StoreDescriptor.Origin`; errors and progress events use `SourceIndex`
and never include the origin, Patient Name, Patient ID, SOP Instance UID, or a
remote Error Comment.

## Planning and Transfer Syntax safety

`PlanStoreBatch` runs before the first network dial. It preserves input order,
deduplicates identical `(SOP Class, ordered writable Transfer Syntax list)`
contexts, assigns odd IDs `1..255`, and starts a new association before a 129th
unique context. Deduplication is per association: a pair repeated after a split
is proposed again on the association that carries it. `net/ul` independently
rejects any direct attempt to propose more than 128 contexts or duplicate IDs.
Each item is sent on its own planned context, so the wire syntax is the one the
peer accepted for that context (the acceptor's order, or the proposer's order
when it sets `PreferProposedTransferSyntaxOrder`), never map iteration order.

`TestStoreSessionHeterogeneousBatchAgainstSyntaxProfiles` sends CT, RLE MR,
32-bit RT Dose (Implicit), RT Plan, Deflated RTSTRUCT, nested SR and 12-lead ECG
waveform files with an unreadable file in the middle to peers that keep every
original syntax, accept only native or accept only Deflated; each persisted file
must equal its source attribute by attribute, including items, pixels,
fragments and waveform bytes, and unproducible items are refused without a
payload. `TestStoreSessionContextBoundariesOpenAssociationsOnlyWhenNeeded`
executes 127, 128 and 129 unique contexts against a peer. The opt-in
`TestStoreSessionHeterogeneousBatchIndependentPeers` repeats the batch against
DCMTK `storescp +xa` with dcmdump and pydicom 3.0.2 comparisons
(`DICOMGO_DCMTK_INTEGRATION=1`, optional `DICOMGO_PYTHON`).

Path and in-memory sources advertise the original Transfer Syntax plus only the
native Explicit/Implicit Little Endian fallbacks that `object.WriteDataSet` can
really produce. The caller can order those native choices; the Twin node
preference is preserved. Compressed inputs never gain a native fallback
implicitly. A value-free Pixel Data header captured from both indexed and
in-memory sources is checked against the declared Transfer Syntax before
dialing, and reopened writable capabilities must match the plan.

There is no implicit pixel transcode. The optional
[`net/storetranscode` source](CSTORE_TRANSCODE_SOURCE.md) explicitly prepares a
requested representation through the existing transcoder before advertising it.
It preserves original-byte pass-through, validates runtime/profile/identity,
revalidates the reopened source and owns bounded temporary files until close.
Lossy output requires explicit authorization and a stable derived identity.

## Lifecycle, retry, and results

Each association carries one active DIMSE operation at a time by default.
`StoreSessionOptions.MaxInvokedOperations` greater than one opts into
pipelining through `AsyncSession` and `StartCStoreEncoded`, limited to the
effective negotiated invoked window and `MaxInFlightBytes`. Peers that omit
sub-item `0x53` or accept window 1 keep the serial `StoreClient` loop; the
session never switches from pipeline to serial mid-association. Path payloads
are still written without materializing `*object.Object`. Successful and
warning responses keep the association reusable; after the final confirmed
response the session performs A-RELEASE. Cancellation of an in-flight item
uses A-ABORT: in-flight work is `StoreOutcomeUnknown`, and items that were
never invoked are `StoreOutcomeCanceled`. A transport/protocol error after a
command may have reached the peer yields `StoreOutcomeUnknown`, uses A-ABORT,
and does not repeat that item by default. With `ContinueOnError`, later items
can continue on a fresh association.

A release that fails or goes unanswered after every item of its association has
its outcome leaves those outcomes as they are and is counted in
`StoreBatchResult.ReleaseFailures`. With `ContinueOnError` a batch that spans
several associations goes on to the next one, and `StoreBatch` returns a
`StoreError` of stage `release` once the batch ends; without it the batch stops
at the first failed release and its later items stay not-sent.

Dial retry is bounded by `MaxAssociationAttempts`. Ambiguous item retry is
disabled unless `RetryUncertain` is set; enabling it gives at-least-once
semantics and may create a duplicate stored instance. All item results remain in
input order and distinguish success, warning, remote/local failure, unknown,
canceled, and not-sent states.

Planning limits are finite by default. Item and batch byte limits are also
enforced against bytes actually written, including sources whose size was only
a hint. `MaxInFlightBytes` further caps payload bytes admitted at once. The serial
baseline admits one payload; the opt-in pipeline admits up to the effective
invoked window without exceeding this budget. It bounds the sum of outstanding
objects, not the size of one: an object above it waits until nothing else is
outstanding and is then sent alone, with nothing admitted beside it. The
per-object bound is `PlanOptions.Limits.MaxItemBytes`. Progress callbacks run
synchronously after the payload handle is closed; callback panic/error is
redacted and stops new work.

## CLI

`cmd/storescu` accepts multiple files and directories, walks directories in
deterministic order without following symlinks, applies finite file, directory,
depth, and path-length limits, and delegates all UL/DIMSE work to
`StoreSession`:

```sh
go run ./cmd/storescu -- -called ORTHANC 127.0.0.1:4242 image.dcm export-directory/
go run ./cmd/storescu -- -max-invoked 4 -called ORTHANC 127.0.0.1:4242 image.dcm
```

`-max-invoked` defaults to 1 (serial). Values greater than one propose an
Asynchronous Operations Window and pipeline only when the peer accepts a
window greater than one.

`-max-send-pdu` caps outgoing P-DATA PDUs below the peer's Maximum Length;
the default 0 sends up to 128 KiB (`ul.DefaultMaxSendPDU`), as DCMTK does.

Output uses only one-based source ordinals, outcome, and status.

## Independent interop

Set `DICOM_STORE_INTEROP_ADDRESS` to an independently implemented Storage SCP
(for example pynetdicom, DCMTK, or Orthanc) to enable the opt-in test. Optional
`DICOM_STORE_INTEROP_CALLED_AE` and `DICOM_STORE_INTEROP_CALLING_AE` override AE
titles. `DICOM_STORE_INTEROP_FILES` accepts an OS path-list of non-PHI Part 10
fixtures, allowing one run to exercise mixed native and compressed syntaxes.

Planner allocation and real loopback association-reuse comparisons at 1, 10,
100, and 1,000 instances are available with:

```sh
go test ./net/dimse -run '^$' -bench 'Benchmark(PlanStoreBatch|StoreSessionAssociationReuse)' -benchmem
```

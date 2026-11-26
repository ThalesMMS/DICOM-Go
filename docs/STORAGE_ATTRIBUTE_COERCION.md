# Storage attribute coercion

Storage consumers can reuse `object.AttributeCoercion`, `MergeAttributes`,
`NullifyAttributes` and `RemapUIDs` at the DIMSE boundary. These hooks change
metadata on an independent copy. They do not publish an instance themselves.
Application persistence, workflow and policy remain with the consumer.

## Receiving

Set `StorageSCPOptions.AttributeCoercion` together with `StoreHandler`. The SCP
first parses the received dataset and checks its SOP Class/Instance against the
C-STORE command and accepted presentation context. It creates a fresh copy,
invokes the hook, validates and reparses the result under bounded limits, checks
the protected pixel attributes and identity again, then calls the handler.
Primitive buffers, sequences and pixel fragments supplied to the hook share no
mutable storage with the received dataset. The final handler dataset is also
detached from the object supplied to the hook.

The affected SOP Class and Instance cannot change on reception: the request
command already fixes both. A changed identity is refused with A900. A callback
error or panic is refused with C000; a typed `CStoreSCPError` may select an A7xx,
A9xx or Cxxx failure status. Bounds map to A700. Coercion failures always use
the fixed Error Comment `attribute coercion failed`; application errors, values
and UIDs are not echoed. `StoreCoercionError` retains its cause for `errors.Is`
and `errors.As`; consumers must not log the unwrapped application cause blindly.

When the hook returns a nonempty modified-attribute list and the handler accepts
with success and no error, the SCP responds B000. A no-op list retains success.
The list follows the existing `AttributeCoercion` contract; custom callbacks must
report changes accurately. Handler failures, warnings and explicit Error Comments
retain their existing behavior. A rejected hook never reaches the handler.

`StreamSink` and `AttributeCoercion` are incompatible and configuration fails
explicitly before command processing. Coercion requires the parsed path and a
finite sequence-depth limit; `MaxSequenceDepth < 0` is rejected with a hook.

## Sending

Wrap a replayable source with `NewCoercedStoreSource(base, StoreCoercionOptions)`.
Its first successful `Inspect` prepares every advertised representation and
invokes the hook once per representation before publishing a descriptor. It
checks the input payload identity against the base descriptor, prepares fresh
values and coherent File Meta Information, enforces output bounds and requires
identical resulting SOP Class/Instance UIDs across all representations.
Changing SOP UIDs and source-reference metadata is allowed on this path.

`Open` and `OpenForTransferSyntax` read the frozen bytes without invoking the
hook again or reopening the original input. Subsequent mutation of the original
source or a retained callback object cannot change that payload. Its descriptor,
per-syntax byte bounds and command UIDs describe the frozen result. Optional
`ReadMetadata` observes the selected frozen representation. A failed preparation
publishes no descriptor and sends no C-STORE for that source; normal batch
continue-on-error behavior may still send other successfully prepared sources.
Retries of a failed `Inspect` may invoke the hook again. Call `Close` on the
source and every opened handle to release retained bytes.

## Protected attributes

Both paths compare a private fingerprint of the encoded protected subset before
and after coercion. The subset includes, recursively and with sequence item
positions preserved:

- Every attribute in group 0028: dimensions, sample/bit layout, photometric
  interpretation, frame count, pixel padding, palettes and segmented LUTs,
  ICC profiles and lossy-compression history.
- Every attribute in group 7FE0: native Pixel Data, Float Pixel Data, Double
  Float Pixel Data, Extended Offset Tables and their lengths. Encapsulated
  Pixel Data includes all fragments and its Basic Offset Table.
- Repeating groups 60xx: overlay data and overlay layout, including attributes
  that interpret embedded overlays.

Changing, adding or deleting protected attributes is refused even when the
result remains encodable. This preserves pixel verification performed by a
transcode source. It is a conservative metadata-only profile, not complete IOD
validation, geometric validation, codec decoding or a check of every bulk-data
module. Existing parser and container/syntax checks still apply; these hooks do
not repair invalid input images or prove clinical correctness.

## Bounds, memory and cancellation

`CloneOptions` defaults to `object.DefaultCloneFileOptions`: 2 GiB for total,
element and pixel bytes, 100000 elements/items/fragments and depth 64. Limits
apply to preparation and the post-hook structural check/reparse. The internal
Part 10 clone includes generated File Meta Information, so a very tight total
or element-count limit can refuse a dataset whose raw bytes alone fit.
Encoded-byte/depth/element limits apply while reading; the item limit is checked
on the parsed structure before application code and again after it. A custom
`Dictionary` preserves creator-aware implicit private VRs through preparation.

`MaxTotalBytes` also bounds the sum of frozen dataset bytes for every advertised
representation of one source. Share a `StoreCoercionBudget` across sources to
limit aggregate retained payload bytes. A positive finite maximum is required.
Each preparation reserves up to its per-source maximum from available budget
before materializing or serializing payloads and uses the reservation as its
effective limit. It rolls back on failure and commits only actual encoded bytes
on success. `Inspect` and `Open` do not charge a prepared payload twice. A closed
source releases its charge when its last opened handle closes. Concurrent
preparations may refuse admission while another preparation holds a conservative
reservation; admission does not wait for that reservation to shrink.

The budget counts encoded payload lengths, not process RSS or peak allocation.
Preparation also holds the parsed working dataset and the independent clone;
a custom dictionary adds a reparse. Cached representations from earlier sources,
buffer capacity, object/sequence overhead and garbage awaiting collection add to
the peak. Borrowed source storage, source-specific transcode/spool allocations
and memory allocated by application callbacks are outside this budget. Multiple
concurrent preparations add their transient copy costs. This adapter creates no
temporary files; it materializes native/deferred values and encapsulated fragments
in memory. Streaming without materialization remains the separate uncoerced path.

Context cancellation is checked between stages, during streamed reads and frozen
writes, and before publishing the descriptor or invoking the handler. The existing
`AttributeCoercion` interface accepts no context: a running callback cannot be
interrupted by this adapter. Callback code must return before cancellation takes
effect at the next boundary. `Close` may wait for an ongoing preparation; callers
must not reenter the same source from its hook or mutate the hook object concurrently.

## storescu

Use repeated `-s keyword-or-tag=value` flags, for example:

```sh
storescu -s 'PatientName=Synthetic^Example' -s 'PatientID=TEST-001' \
  -coerce-max-bytes 67108864 -coerce-max-batch-bytes 268435456 \
  127.0.0.1:104 synthetic.dcm
```

Keywords and standard numeric tags resolve through the standard dictionary.
Known, unambiguous primitive VRs and their VM are validated before connecting.
An empty value produces a zero-length attribute; repeated assignments use the
last value for that tag. Unknown/private tags, command/File Meta attributes,
sequences, binary values and the protected groups above are rejected. Values may
contain `=`; a backslash denotes VM for VRs that use a value delimiter. Diagnostic
messages omit the assignment and its values. The flags are intended for values
valid in the selected text VR and dataset character repertoire; they do not offer
automatic character-set conversion or nested attribute-path editing.

Both coercion limits default to 2 GiB when zero. The per-source limit covers
copies and all its representations; the batch limit shares a finite retained-byte
budget across all sources. The batch retains frozen payloads until completion,
then closes every coercion source. These are logical-byte limits with the peak
memory qualifications above, not a 2 GiB RSS guarantee.

Coercion runs after explicit `-transcode-to` preparation. The CLI retains the
transcode source separately to report the actually negotiated representation and
its decoded lossy-verification metric. Protected bytes/layout/LUTs cannot change
after that verification. Original fallback remains forbidden with `-allow-lossy`.
UID remapping by `-s SOPInstanceUID=...` updates the outgoing command identity;
applications remain responsible for choosing an appropriate identity policy.

## Qualification

Synthetic loopback tests cover accepted/coerced/no-op/rejected Storage SCP paths,
warning and handler-failure semantics, unchanged sources, coherent command and
Part 10 identities, frozen native representations, private VRs, protected nested
pixels/LUTs/offsets/overlays, bounds, cancellation and shared-budget lifetime.
CLI loopback tests cover last-wins `-s`, invalid assignments without value echoes,
preparation refusal before connection, RLE and JPEG lossy report composition.
These fixtures establish local behavior; no target PACS acceptance is claimed.

The status contract follows [DICOM PS3.4 B.2.3](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_B.2.3.html).
Implementation uses the library's coercion primitives and synthetic fixtures;
no external implementation code was copied. Reproduce focused checks with:

```sh
go test ./net/dimse ./cmd/storescu -run 'Test(StoreCoercion|StorageCoercion|ParseArgs|RunAttribute)' -count=1
go test -race ./net/dimse ./cmd/storescu -run 'Test(StoreCoercion|StorageCoercion|RunAttribute)' -count=1
go vet ./net/dimse ./cmd/storescu
```


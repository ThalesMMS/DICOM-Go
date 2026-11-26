# Writable qrscp archive

`qrscp` keeps its existing read-only positional-root mode. `-write-root DIR`
instead enables an exclusively owned local archive; positional roots cannot be
combined with it. The parent of DIR must exist. New leaf directories are created
through pinned directory handles. Listener flags, aliases, TLS, calling-AE
admission and association limits apply to both modes.

```sh
qrscp -write-root ./synthetic-archive -listen 127.0.0.1:11112 -ae QRSCP \
  -move-destination DEST=127.0.0.1:11113
```

The writable backend composes `index`, `internal/netstore`, `internal/nofollow`
and the existing DIMSE SCPs. It does not introduce new DICOM matching, encoding
or normalized-service rules. Each canonical SOP Instance UID has one immutable
`UID.dcm` file. Duplicate UIDs return A900, without changing the existing file.
Patient identity within a Study UID and Study UID within a Series UID must agree.

## Persistence and bounded storage

C-STORE validates command/context/dataset identity and required study/series
identities, checks pixel encoding, and measures the complete encoded Part10
including the same File Meta AE provenance that will be written. It then uses
netstore's complete-file publication, verifies the file, persists its detached
metadata and SHA-256 in `.qrscp-index`, and admits it before returning success.
Source/Receiving File Meta AE titles identify the local AE; Sending identifies
the calling peer. Incoming bulk payload is bounded and released after the
operation; the catalog retains detached metadata only.

| Flag | Default | What is counted |
| --- | ---: | --- |
| `-max-instances` | 100000 | Persisted instances |
| `-max-instance-bytes` | 1 GiB | Complete Part10 file and bounded receive/read |
| `-max-storage-bytes` | 1 GiB | Sum of actual stored Part10 bytes |
| `-max-study-bytes` | 256 MiB | Actual Part10 bytes in each study |
| `-max-study-instances` | 10000 | Instances in each study |
| `-max-index-bytes` | 256 MiB | Serialized persistent metadata bytes |

Instance/study count and Part10 byte quota refusals return A700 before data
publication. Metadata rows have an additional 256 KiB per-record ceiling; an
index byte quota refusal can happen after image publication and then preserves
the image and makes the archive unavailable until reconciliation, as below.
These are logical disk/record budgets,
not a process RSS limit; temporary writes may coexist with published records.
No automatic deletion, eviction or retention expiration releases image storage.
Queries capture immutable catalog/tree generations. The backend rebuilds that
snapshot only when a query observes newly admitted instances, avoiding a full
tree rebuild for every object in a storage batch.

`index.OpenPersistentStore` is reusable metadata persistence, independent of
archive policy and QR matching. It validates versioned records, identity,
digest shape, bounded JSON, selected attributes and filenames on replay, and
returns detached records. Data-file verification remains the application's job.
Authored records are checked against their exact serialized byte quota before
allocating that JSON record, with at most 256 diagnostics/missing entries and
64 values per geometry slice. Ambiguous JSON duplicate/case-alias keys are refused.
The index stores explicitly selected patient/descriptive fields; use appropriate
profiles and a protected archive directory when storing sensitive metadata.

## Failure and restart contracts

The index and commitment stores hold exclusive kernel locks for their lifetimes.
They use pinned nofollow handles, regular-file checks, finite enumeration/read
budgets, private temporaries, file Sync and atomic relative publication. Unix
also flushes affected directories before success. Windows has file Sync and
atomic NT rename; there is no Unix directory-flush durability claim. Locking uses
supported Unix and Windows primitives; other platforms refuse. Unix behavior is
qualified by local tests. Windows qualification here is cross-compilation only;
native Windows execution remains a separate acceptance step.
Cooperating processes must share this ownership protocol. External edits,
deletions or renames during ownership and network-filesystem crash guarantees
are outside the supported profile.

A failure after image publication but before durable index admission preserves
the complete image, returns failure, and makes the archive unavailable. It does
not remove bytes that might have become durable or pretend a rollback succeeded.
The same rule applies to uncertain index publication. Restart validates every
indexed file by full bounded parsing, metadata and SHA-256. Missing files,
changed bytes, malformed rows and unexpected directory entries refuse startup.

An unindexed canonical Part10 file requires explicit `-repair-index`. Repair
fully validates it, applies all quotas, flushes it, and writes its metadata before
admission. It also removes bounded, regular unpublished netstore temporaries
whose reserved names have the exact generated form. Ordinary startup does not
silently recover or delete these files. A repair is refused if the platform
cannot complete a required flush. A durable index followed by a lost/failed ACK
replays normally; retrying that UID gets A900 instead of overwriting data.

## Storage Commitment

Enable `-storage-commitment` with a requestor callback allowlist:

```sh
qrscp -write-root ./synthetic-archive -storage-commitment \
  -commitment-callback MODALITY=127.0.0.1:11114
```

The existing Storage Commitment workflow owns N-ACTION/N-EVENT, deduplication,
leases and retry state. The reusable file TransactionStore persists its state
under `.qrscp-commitment` before successful Create/CAS. Callback N-ACTION from an
AE without a configured route is refused. Processing fully revalidates each
file's persisted identity/metadata/digest and returns the exact success/failure
partition. Missing, unsupported, conflicting or changed instances are not
reported as committed. The worker resumes callback transactions after restart,
and is canceled/joined before closing its store. Completed transactions are
retained for 24 hours; `-max-commitment-transactions` defaults to and is capped
at 10000. Further record/page/reference limits are described in
[the reusable store contract](STORAGE_COMMITMENT_FILE_STORE.md).

`-stgcmt-same-assoc` uses a dedicated STGCMT-only association with exactly one
accepted presentation context and the workflow's
single-reader ServeAction → Process → DeliverOnAssociation sequence. An accepted
association mixing STGCMT with Verification, Storage or QR is explicitly closed
before it is served. Multiple accepted STGCMT contexts are also refused before
reading a command. No dispatcher and event-report client read concurrently.
This profile processes one transaction per association. Interrupted delivery
remains durable; the same request can be resubmitted on a fresh dedicated
association. It is not converted into an unconfigured callback after restart.

Commitment describes verified local filesystem storage under these ownership
and durability conditions. It supplies no replicated archive, deletion policy,
clinical retention guarantee, or validation against a production PACS.

## Interoperability fault profiles

`-cfind-error 0xC123` returns that final failure without matches; success/pending
status values are refused. `-delay-cstore DURATION` delays writable C-STORE using
a cancelable timer, from zero through 30 seconds. Private IO test seams exercise
file/index publication and flush failures; they are not public flags and do not
claim to reproduce a particular kernel, filesystem or remote PACS failure.

Synthetic tests cover store → find → move → get with pixel/identity checks,
restart replay, duplicate and quota refusals, concurrent queries/stores, digest
mutation, explicit orphan reconciliation, durable callback restart, dedicated
same-association commitment and injected failures. Windows build qualification
is separate from native execution. No CI pipeline is required.

# Durable Storage Commitment records

`dimse.NewFileStorageCommitmentStore` provides a reusable implementation of
`StorageCommitmentTransactionStore` for the existing workflow. It persists
transactions, request/result digests, lease/retry state and versions. It does
not decide whether an instance is committed: the application's processor must
verify its own storage promise before returning a result.

```go
store, err := dimse.NewFileStorageCommitmentStore(existingDedicatedDirectory,
    dimse.FileStorageCommitmentStoreOptions{})
if err != nil {
    return err
}
defer store.Close()
workflow, err := dimse.NewStorageCommitmentWorkflow(dimse.StorageCommitmentWorkflowOptions{
    Store: store,
    Processor: processor, // verifies the application's declared storage promise
    Resolver: callbackAllowlist,
})
```

The directory must already exist and have one exclusive cooperating owner for
the lifetime of the store. Its lifetime OS lock excludes another store owner;
close or process exit releases it. External changes to files/directories are
unsupported and detected inconsistencies fail closed. Use a dedicated trusted
local directory, not a shared network filesystem or an application catalog.

Create persists version 1 before returning success. An existing transaction
returns `false`; the existing workflow compares its request digest for duplicate
or conflicting requests. CAS serializes the stored version check and durable
replacement, increments the version, and refuses overflow, changed request
identity/creation time, or a changed already persisted result. Every returned
transaction owns its slices and result. The workflow remains responsible for
protocol transitions, acceptance, replay leases, retry policy and result delivery.

Persistence uses a private temporary file, file Sync, atomic create-only
publication/replacement, and directory Sync on Unix. A publication followed by
an error returns failure and makes the generic store unavailable until restart;
there is no claimed rollback or successful acknowledgment. Windows provides
file Sync and atomic publication, without the Unix directory-flush guarantee.
The qualification described here does not include Windows native execution or
proof of power-loss behavior, filesystem hardware semantics, replication,
retention policy or remote PACS interoperability.

The version 1 JSON envelope has exact `schema_version` and `transaction` fields.
Transaction/result/reference fields use their exported Go names. Keys are
lowercase SHA-256 of the Transaction UID plus `.json`, and replay checks this
identity. Digests are recomputed; the result must be the exact disjoint partition
of the requested SOPs. References are canonical and sorted as produced by the
existing workflow. Unknown/case-variant/duplicate fields, unknown schema/state
values, invalid UTF-8, trailing documents, excessive JSON structure and invalid
UIDs, peers, counters, digests or records refuse construction. Store errors use
fixed messages without UID, AE or JSON values.

| Option | Default | Hard ceiling |
| --- | ---: | ---: |
| MaxTransactions | 10000 | 10000 |
| MaxRecordBytes | 4 MiB | 16 MiB |
| MaxTotalBytes | 64 MiB | 1 GiB |
| MaxReferences | 10000 | 10000 |
| MaxListBatch | 128 | 1024 |

Zero selects the default; negative values or values above a ceiling are refused.
MaxTotalBytes must be at least MaxRecordBytes.
Bytes count encoded JSON and logical records. Authored records undergo bounded
scalar/reference validation and exact JSON-byte admission before cloning the
transaction for persistence and serializing the whole record. Validation reuses
the existing bounded reference normalization. Admission encodes a bounded scalar
envelope and one bounded reference at a time. These quotas do not claim an RSS
bound, a distributed quota or tracking of external additions/deletions. Purge
reclaims successfully deleted records within this exclusive store.

Opening validates at most MaxTransactions records within the byte quotas. List
and purge scan that bounded set and retain at most MaxListBatch selected records
or keys; there is no complete in-memory transaction catalog. List orders by
UpdatedAt then UID, optionally filtering states and due leases/backoff. Purge
uses terminal states and an UpdatedAt cutoff, in UID order, preserving active
and retryable records. Cancellation does not erase transactions; a partial
purge error reports the number already durably removed.

Focused fixtures verify close/reopen, result cloning, concurrent/stale CAS,
bounded malformed replay, quotas, list/purge and the existing workflow's lease
recovery/result dedup/retry after reopen. Network composition and acknowledgment
behavior require separate workflow/application integration qualification.

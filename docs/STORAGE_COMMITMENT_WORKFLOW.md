# Storage Commitment workflow

`net/dimse` provides a high-level, persistence-neutral implementation of the
DICOM Storage Commitment Push Model. It builds on the existing normalized
DIMSE and UL stack; it is not a second networking implementation.

The legacy command, dataset, one-shot SCP, and in-memory tracker APIs remain
available. New applications should use `StorageCommitmentWorkflow` when they
need durable correlation, deferred processing, callback delivery, retries, or
restart recovery.

## Normative behavior

- N-ACTION uses Action Type ID 1. A success response means only that the SCP
  durably received the request.
- A successful result for every requested SOP Instance uses N-EVENT-REPORT
  Event Type ID 1.
- A result containing any failure uses Event Type ID 2. Its Referenced SOP and
  Failed SOP sequences form a complete, disjoint partition of the initiating
  request.
- Failure Reason accepts the PS3.3 C.14.1.1 values `0110`, `0112`, `0119`,
  `0122`, `0131`, and `0213`, named `StorageCommitmentFailureReason*`. They are
  not DIMSE statuses: `A700` is never a Failure Reason.
- A committer that cannot take a request because a configured bound (the
  reference count or a full transaction store) is reached answers N-ACTION
  with Resource limitation `0213H` (PS3.7 10.1.4.1.10); other refusals use
  Processing failure `0110H`.
- A callback on a separate association uses the same committing AE Title as the
  original association and negotiates the Storage Commitment SCP role.

## Persistence contract

Applications inject a `StorageCommitmentTransactionStore`:

```go
type StorageCommitmentTransactionStore interface {
    Create(context.Context, StorageCommitmentTransaction) (bool, error)
    Get(context.Context, string) (StorageCommitmentTransaction, error)
    CompareAndSwap(context.Context, string, uint64, StorageCommitmentTransaction) (StorageCommitmentTransaction, error)
    List(context.Context, StorageCommitmentTransactionQuery) ([]StorageCommitmentTransaction, error)
    PurgeCompleted(context.Context, time.Time, int) (int, error)
}
```

`Create` must be durable before returning. `CompareAndSwap` must compare the
stored `Version` and increment it atomically. `List` must honor its finite page
limit. Every implementation must defensively clone reference and result slices.

`StorageCommitmentTransaction.Clone`, `IsDue` and
`StorageCommitmentState.Terminal` give persistent stores the same copy, due and
purge semantics as the memory store; `examples/referencearchive` has a
file-backed store built on them.

`MemoryStorageCommitmentStore` is bounded and race-safe, but intentionally does
not survive process restart. A database-backed application implements the same
interface. Reusing that persistent store with a new workflow instance resumes
accepted processing work, prepared requests whose response publication was
interrupted, and ready callback delivery.

Processing and delivery are at-least-once. Claims use a random persisted lease;
an expired lease can be reclaimed after a crash. Application processors and
result consumers must be idempotent by Transaction UID plus the canonical
request/result digest. An identical duplicate is accepted without a second
consumer completion; a duplicate with different content fails closed.

## Requestor

The caller owns the association and store:

```go
workflow, err := dimse.NewStorageCommitmentWorkflow(
    dimse.StorageCommitmentWorkflowOptions{Store: store},
)
if err != nil { /* handle */ }

transaction, err := workflow.Request(ctx, assoc, references,
    dimse.StorageCommitmentRequestOptions{
        DeliveryMode: dimse.StorageCommitmentDeliveryCallback,
    },
)
```

The workflow persists `request_pending` before sending N-ACTION and moves the
record to `accepted` only after a successful response. An uncertain send remains
pending; `RetryRequest` is explicit because silently repeating a Transaction
UID after an uncertain exchange can create duplicate work at a peer.

To accept a report on the original association, call
`AwaitResultOnAssociation(ctx, assoc, transactionUID, wait)` after `Request`.
It is the association's sole reader while active: it serves N-EVENT-REPORT
with the workflow handlers, polls idle reads in 100 ms steps without consuming
a partial PDU, and returns when the stored transaction is terminal (the result
may also arrive on a separate callback association), when `wait` elapses, or
when the committer releases (`PeerReleased`). After a timeout the caller can
still release the association normally.

For a separate callback listener, use `ServeListener`, or combine
`workflow.NormalizedOptions(assoc)` with an application-owned
`ServeAssociation` loop. The receiver validates the peer Calling/Called AE,
role selection, Transaction UID, exact result partition, and failure reasons;
it stores the result before returning N-EVENT-REPORT success. `ServeListener`
negotiates each connection on its own goroutine, within
`ul.DefaultServerNegotiationTimeout` unless the accept options set another
timeout. A connection that fails negotiation, such as one that sends bytes
that are not an A-ASSOCIATE-RQ, is closed and reported to `OnError`; an
accept failure that leaves the listener usable, such as running out of
descriptors, is reported and retried after a pause of 5 ms doubling to 1 s.
Only a canceled context or a failed listener, such as a closed one, ends it.

`Wait` polls the injected store and never deletes a transaction when its context
is canceled. Database-backed stores may additionally expose their own native
watch/notification API.

## Committer

For a dedicated, long-lived association loop, use the handlers returned by
`NormalizedOptions`. `Create` completes before the N-ACTION success response is
formed. The durable record remains `acceptance_prepared` until the response
write boundary, so a concurrent worker cannot emit N-EVENT-REPORT first.
The response write renews a persisted acceptance lease. `ProcessDue` reconciles
a prepared record only after two lease intervals without a renewal if a crash
or transient store failure prevents the post-response promotion. Lease duration
has a one-second minimum. This deliberately favors durable at-least-once
recovery at the unavoidable store/network commit boundary; processors must
remain idempotent.

The application decides when commitment exists. It can:

- configure a `StorageCommitmentProcessor` and call `Process` or bounded
  `ProcessDue`;
- call `SetResult` from an external/manual job;
- use `StorageCommitmentDeliveryManual` and let another system consume the
  durable result.

`SetResult` persists and validates the complete result before any network
attempt. It never infers clinical commitment from C-STORE receipt.

## Delivery modes

### Separate callback

`StorageCommitmentCallbackResolver` maps the stored requestor AE to an
allowlisted address and `ul.DialOptions`. The workflow never derives a host from
untrusted AE text. It clones TLS, identity, and negotiation inputs, forces the
Storage Commitment presentation context and SCP role, verifies AE consistency,
and owns the association returned by the dialer.

`DeliverCallback` performs one persisted attempt. `DeliverDue` processes a
finite due page, applying bounded exponential backoff and resuming expired
leases after restart. Successful N-EVENT-REPORT acknowledgement marks the
transaction delivered before association cleanup. TLS errors never fall back to
plaintext.

### Same association

Call `ServeAction`, then process and `SetResult`, then call
`DeliverOnAssociation`. `ServeAction` writes N-ACTION-RSP before it returns.
`DeliverOnAssociation` borrows the association and requires accepted
Storage Commitment SCU/SCP roles. The original association's default roles are
sufficient; a separate callback association explicitly negotiates the
association requestor as the Storage Commitment SCP.

The caller must make this workflow the sole association reader for the whole
sequence. Do not run `ServeAssociation`, `Dispatcher`, or another
`NormalizedClient` concurrently on that association. The method never releases
or closes the borrowed association.

### External/manual

With `StorageCommitmentDeliveryManual`, `SetResult` leaves the durable record in
`result_ready`. The application reads it through `Get`/`List`, owns any external
delivery protocol, and calls the idempotent `CompleteManualDelivery` only after
that external handoff is durable. The record then becomes terminal and eligible
for retention cleanup.

## Limits, retention, and error safety

Defaults are finite: 10,000 SOP references, a 4 MiB normalized dataset, pages
of 128 transactions, five delivery attempts, and 30-second leases. Callers may
lower the reference cap and configure the other bounds. The reference cap may
not exceed 10,000 because the shared action/event builders enforce that hard
ceiling.

`PurgeCompleted` removes only terminal records older than the supplied cutoff
and applies the configured page limit. Active, leased, or retryable work is not
purged.

Persisted delivery failures use a closed class plus DIMSE status, retry count,
and timestamps. Peer Error Comment, address, AE, Transaction UID, SOP Instance
UID, Patient Name, and Patient ID are never copied into retry state or implicit
error text. Underlying errors remain available through `errors.Is`/`errors.As`
for deliberate local handling.

## Ownership summary

- passed associations, stores, processors, consumers, resolvers, listeners,
  and custom dialers are borrowed;
- callback associations returned by the dialer are owned by the workflow and
  are released after acknowledgement or aborted/closed after failure;
- `ServeListener` owns each accepted association, but not the listener;
- callback functions run outside store locks and receive detached records.

`cmd/stgcmtscu` is a requestor built on this workflow: `Request`, then
`AwaitResultOnAssociation` and, with `-listen`, `ServeListener` and `Wait`
for a callback association; see
[CLI_TOOLS.md](./CLI_TOOLS.md#stgcmtscu-storage-commitment).

## Interoperability

The default suite covers both event types, partial failure, same-association
delivery, a genuinely separate callback association, role selection, duplicate
result suppression, restart-compatible store reuse, retry/backoff, cancellation,
and malformed datasets. An opt-in pynetdicom test exercises N-ACTION followed
by a separate-association partial-failure N-EVENT-REPORT; the external-peer
procedure remains documented in `INTEROP_ORTHANC.md`. Deployments must also record their commitment policy,
callback routing, supported Storage SOP Classes, timeout behavior, and deletion
policy in their DICOM Conformance Statement.

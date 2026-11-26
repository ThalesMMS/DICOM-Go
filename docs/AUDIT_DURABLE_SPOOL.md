# Durable syslog audit forwarding


## Acceptance and delivery boundaries

`Log`, `LogRaw`, `LogSource` and `LogClinical` return success only after the
immutable RFC5424 record and the versioned/checksummed checkpoint have been
written, file-synced, renamed and directory-synced. The queue directory has one
process writer, private permissions (0700 directory, 0600 files), and must stay
private on a filesystem that actually supplies these guarantees. It is not
encrypted at rest. Audit payloads can contain identifiers even in the structural
profile; retention of a durable queue is a deliberate configuration decision.

This provides **at-least-once send attempts for accepted, unexpired records**,
subject to configured bounds and eventual transport availability. A successful
TCP/TLS write is a transport boundary; it does **not** acknowledge that a remote
repository persisted, validated or even received the complete audit record.
UDP has no reception acknowledgement. A process crash after socket write and
before checkpoint sync can repeat the identical record. There is no exactly-once
or remote durable-delivery guarantee. Disk/metadata corruption fails closed;
there is no automatic deletion of corrupt committed records to manufacture a
successful status.

| Interrupted phase | Recovery behavior |
| --- | --- |
| Before record publication | No accepted record; temporary files are removed on bounded recovery. |
| Record published, checkpoint not published | A complete checksummed orphan is recovered once; monotonic source replay is idempotent. The interrupted call may have reported no success. |
| Local acceptance complete, repository unavailable | Record remains pending after a process restart. |
| Socket write complete, repository checkpoint not synced | Attempt can repeat; bytes remain unchanged. |
| Repository checkpoint synced, file GC interrupted | Completed file is bounded garbage; it is never resurrected. |
| One repository completed, another unavailable | Only the unavailable repository remains pending, including after restart. |
| Full disk or uncertain sync/rename | Sanitized persistence error; the live logger stops admitting/sending until reopened and checked. No PHI-bearing underlying error is returned. |
| Missing/truncated/bad-checksum committed state or payload | Sanitized integrity error; forwarding stops and local application audit recording continues. |

## Destinations, limits and lifecycle

Each destination has a stable local ID, transport/address, cloned TLS policy and
independent checkpoint, connection, cancellation and exponential backoff. A
failed destination's enqueue wake cannot bypass its backoff. TCP and TLS use
RFC5425/RFC6587 octet-counting; UDP uses one complete message per datagram and
refuses records larger than 65,507 bytes. Short stream writes are completed;
short datagram writes fail. `Flush(ctx)` attempts active destinations concurrently.
`Close` cancels public Flush and Run, prevents late dial publication, closes
sockets, drains active operations, and releases the disk lock. It never performs
a final network flush or deletes pending records. Custom Dial callbacks must
honor context cancellation. Native filesystem operations, including `fsync`,
cannot be interrupted by Go context; cancellation is checked between recovery IO
chunks and between transport/queue operations.

Finite defaults are 1,000 pending records, 16 MiB of **record bytes**, 4 MiB per
record, 30 days queue age, 5 seconds initial retry, 1 minute maximum retry, and
10 seconds per send attempt. Configuration caps are 100,000 records, 1 TiB record
bytes, 64 MiB per record, 3,650 days, 16 active destinations, 256 retained
checkpoint identities, and 64 suppression rules. Zero selects finite defaults;
negative/invalid values are refused. `MaxBytes` bounds record bytes, not total
filesystem metadata: each record adds at most an 8 KiB header plus 8 bytes,
state is capped at `MaxMessages*2048 + 1 MiB`, and one bounded temporary write
may coexist. Directory traversal is bounded to `2*MaxMessages+32` entries and
checks exact names, private regular files, bounded headers, declared lengths and
checksums before payload allocation. Recovery hashes with a 64 KiB buffer.

Admission to a full queue refuses the **new** record; it does not evict an
accepted one. Age expiry is a terminal, counted policy action. Per-destination
pending counts and total pending bytes, sent attempts, failures, rejections,
expiry, suppression, source retirements, and live persistence/integrity failures
are available through `DurableStats`. Constructor integrity errors are reported
as sanitized errors because no live logger exists. Stats are local state,
not repository confirmations.

Suppression matches nonempty EventID/action/outcome vocabulary fields; empty
fields are wildcards. Empty catch-all rules are refused. Suppression retires a
tracked source ID durably and does not modify the Twin local trail. Rules affect
future enqueues; already accepted immutable records retain their original
payload and target list. Removing a destination pauses its pending records;
adding a destination affects future enqueues only. Reusing an ID with a changed
transport/security fingerprint while its old records are pending is refused.
Readding the same ID/policy resumes the backlog. Lowering limits below a stored
backlog refuses opening rather than dropping it. Changed retention can expire
pending records on reopen; that policy never deletes the local audit trail.

TLS trust pools and the Go1.22 certificate chains/OCSP/SCT/signature algorithm,
ALPN/cipher/curve slices and certificate name map are copied; a Leaf is reparsed
from its DER. Private keys, certificate objects already held by trust pools,
readers, caches, callback closures and their returned objects remain caller-owned
and must stay immutable/thread-safe. The automatic fingerprint covers configured
transport/address and supported scalar/slice/certificate settings, not arbitrary
callback state or the operating system's evolving trust store. Custom trust
pools/callbacks require a caller SHA256 `Fingerprint`, changed whenever that
policy changes: `CertPool.Subjects()` cannot identify different roots with the
same subject. The library cannot verify that the supplied policy digest is
truthful. Twin adds SHA256 over its configured CA/certificate/key file contents.

The ownership/cancellation/IO qualification is macOS. Windows lock code uses
`LockFileEx` and is cross-compiled only. A filesystem/platform that cannot supply
private permissions or directory sync is explicitly refused; native Windows
runtime/durability is **not qualified** by this work. RAM defaults remain usable.

## Twin local trail and restart recovery

The existing append-only `access_audit_events` trail remains best-effort and
independent: failure to record or forward never fails an audited viewer/network
operation. The archive AUTOINCREMENT ID is the feed cursor; timestamps and row
count are not cursor identities. New bounded `AccessAuditEventsAfter` reads at
most 256 rows in increasing ID order (the worker uses batches of 64). Pruning
rows does not reset the AUTOINCREMENT highwater.

For the structural profile, Twin commits the local event first and signals a
background replay worker. Its durable source cursor closes the crash gap between
that commit and queue admission. An enqueue failure/full queue keeps the source
pending for retry; malformed or individually oversized records and events older
than the configured spool retention are explicitly retired with a sanitized
diagnostic/counter. The cursor survives delivery GC, expiry and suppression, so
retired source events are never repeatedly recreated. Recovery can only recover
rows still retained in the local trail; local retention remains an independent
policy. A catalog rollback below the stored cursor is refused. Source identity
is bound to the archive path; copying/replacing a catalog while reusing an old
queue requires deliberate separate queue handling, not an inferred equivalence.

Initial enable starts at the current highwater, without historical backfill.
Disable pauses accepted backlog and persists a `SourcePaused` epoch. Events
recorded while disabled remain local. Resume advances that epoch to the current
catalog highwater before starting workers, including a crash between settings
save and application. Accepted pre-disable records keep their target list and
resume independently. Settings changes and session close cancel transport work;
there is no durable-mode final network flush on the frontend/settings path.
Paused/closed state is reported as the last sanitized backend stats snapshot.
These settings use config/backend APIs; no new UI controls are included.
Saving and applying an audit configuration share one serialization with audit
shutdown. A newer saved disable/revoked consent cannot be overtaken by an older
forwarder. Updates waiting for that serialization recheck session closure;
settings already saved when shutdown begins remain persisted for the next open,
but are not applied or started after the closed mark.

Example backend configuration (retain existing TLS fields when TLS is selected):

```json
{
  "accessAudit": {
    "retentionMonths": 12,
    "atna": {
      "enabled": true,
      "durableSpool": true,
      "spoolMaxMessages": 1000,
      "spoolMaxBytes": 16777216,
      "spoolMaxRecordBytes": 4194304,
      "spoolRetentionHours": 720,
      "destinations": [
        {"id": "primary", "transport": "tls", "address": "collector.example:6514"},
        {"id": "secondary", "transport": "tls", "address": "backup.example:6514"}
      ],
      "suppression": [{"eventID": "110100", "outcome": "0"}]
    }
  }
}
```

The private queue is `<archive>/.audit-spool`. Legacy `address/transport/TLS*`
fields still select one destination with ID `primary` when `destinations` is
empty. Config validation is side-effect-free; it does not open/leak a queue lock.

`ClinicalIdentities` and `QueryDataSets` authorize **transient** capture under
existing policy. They do not authorize disk storage. Selecting them together
with durable mode additionally requires `PersistClinicalPayloads=true`.
Clinical messages/association aggregates become durable only when their emit or
Finish successfully enqueues them. The minimal trail does not retain names,
query bytes or aggregate snapshots, so a crash **before clinical enqueue cannot
reconstruct that clinical payload**. When `ClinicalIdentities` is enabled, replay
retires those structural source IDs rather than inventing a clinical
reconstruction or silently persisting more local fields. With only
`QueryDataSets` enabled, view/export/send still use the structural profile and
their source IDs are replayed normally; independent query captures are enqueued
by association callbacks and do not replace or retire the structural feed.
Partial batches/unfinished aggregates remain subject to the existing best-effort
clinical contract. Reading the trail through Session can independently emit an
AuditLogUsed message; it is a new event, not source replay.

## Receiver and CLI

`NewReceiver` accepts a caller-owned persistence handler and transfers ownership
of each record's bytes. `Serve` consumes a supplied TCP/TLS listener;
`ServeUDP` consumes a packet socket. VERSION1 RFC5424 header, structured data,
UTF8 BOM and octet lengths are validated before delivery to the handler. Length
prefixes are checked before allocation/read of a declared payload. Defaults are
4 MiB per message, 32 stream connections and 30 seconds idle read timeout; caps
are 64 MiB and 1,024 connections. At least 32 KiB is required, as in PS3.15 A.6.
Handler errors and malformed/oversized records increment sanitized counters.
No application persistence ACK is added to syslog.

The receiver enforces decimal PRI without leading zeros (except `0`) and the
RFC5424 timestamp subset: uppercase `T`/`Z`, two-digit time/offset fields, no
leap seconds, and optional `.` followed by one through six fractional digits.
SD-IDs are unique and case-sensitive within each record; parameter names may
repeat. PARAM-VALUE accepts valid shortest-form UTF-8 including NUL, LF and DEL;
unknown escapes such as `\q` remain byte-for-byte intact as required by RFC5424
§6.3.3. `Raw` and `StructuredData` preserve the received values without control
character substitution or unescaping. Invalid UTF-8 is refused. The transient
SD-ID index grows in proportion to the bounded record; the wire-byte cap is not
an exact cap on all parsing/connection memory.

`cmd/syslogrcv` is an offline collector with a required private output directory,
immutable atomic record files, count/byte limits, bounded restart/temp cleanup,
and sanitized aggregate diagnostics. Use a directory owned by one collector
process. It does not index, query, interpret or certify clinical audit content.
TLS is the default (`-cert`/`-key` required); `-client-ca` enables mutual client
certificate verification. Plain TCP/UDP are explicit options for controlled
networks/fixtures. Stored output needs separate operator retention/removal;
a full collector refuses new records instead of evicting old ones.

```sh
go run ./cmd/syslogrcv -transport tls -listen 127.0.0.1:6514 \
  -directory /private/audit-records -cert collector.pem -key collector.key \
  -client-ca clients.pem -max-messages 10000 -max-bytes 268435456
```

## Normative scope and evidence

Primary sources consulted: [PS3.15 A.5](https://dicom.nema.org/medical/dicom/current/output/chtml/part15/sect_A.5.html),
[PS3.15 A.6](https://dicom.nema.org/medical/dicom/current/output/chtml/part15/sect_A.6.html),
[PS3.15 A.7](https://dicom.nema.org/medical/dicom/current/output/chtml/part15/sect_A.7.html),
[RFC5424](https://www.rfc-editor.org/rfc/rfc5424.html),
[RFC5425 §4.3/§6.3](https://www.rfc-editor.org/rfc/rfc5425.html), and
[RFC6587](https://www.rfc-editor.org/rfc/rfc6587.html). PS3.15/current was 2026d
when inspected. dcm4che AuditLogger spoolDirectory/multiple-repository behavior
was a clean-room reference only; no external implementation was copied.

Synthetic tests use real local TCP/TLS/UDP sockets, actual subprocess exit and
restart, independent destination checkpoints, pre-state orphan recovery,
send-before-checkpoint duplicate attempts, local-commit/enqueue and config-save
crash gaps, admission/expiry/suppression, corrupt/truncated files, disk failure
injection, bounded temp cleanup, cancellation/late dial, settings persistence
and TLS ownership. No external PACS/repository, clinical deployment, native
Windows execution, full ATNA conformance or performance benchmark is claimed.

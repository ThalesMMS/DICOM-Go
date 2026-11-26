# Instance Availability Notification

Package `ian` implements the Instance Availability Notification SOP Class
(`1.2.840.10008.5.1.4.33`) of PS3.4 Annex R (2026d) on top of the normalized
DIMSE layer. It reuses `NormalizedClient`, `NormalizedSCPOptions` and the
association runtime; it adds no dispatcher. The SCP is never enabled
implicitly: a server offers it only when the application accepts the SOP Class
and passes `Service.NormalizedOptions` to `ServeAssociation`.

## Attribute List

`ian.Build` writes, and `ian.Validate` checks, the N-CREATE Attribute List of
PS3.4 Table R.3-1 (Instance Availability Notification Module, PS3.3 C.4.23):

| Attribute | Rule applied |
| --- | --- |
| Specific Character Set | Sent only when set. |
| Referenced Performed Procedure Step Sequence | Present (Type 2), possibly empty; each item has Referenced SOP Class and Instance UIDs. Performed Workitem Code Sequence is sent empty by `Build` and accepted when present. |
| Study Instance UID | Present with a valid UID. |
| Referenced Series Sequence | At least one item, each with a Series Instance UID and at least one Referenced SOP Sequence item. |
| Referenced SOP Class / Instance UID | Present with valid UIDs. |
| Instance Availability | ONLINE, NEARLINE, OFFLINE or UNAVAILABLE. |
| Retrieve AE Title | Present with a value of at most 16 characters, unless the instance is UNAVAILABLE or the item carries a Retrieve Location UID, Retrieve URL or Retrieve URI. |
| Retrieve Location UID, Storage Media File-Set UID | Valid UIDs when present. |

Other attributes are passed through unvalidated. A failed check maps to the
N-CREATE statuses of PS3.7 10.1.5: 0120H Missing Attribute, 0121H Missing
Attribute Value or 0106H Invalid Attribute Value, with the offending tags (never
values) in the Attribute Identifier List. `ian.Group` turns located instances
into one notification per study, keeping series and instance order.

## SCU

`ian.Client` borrows an association that accepted `ian.PresentationContext()`
and sends one N-CREATE per notification (`Notify` for a prepared Attribute
List, `NotifyNotification` to build it). An empty notification UID lets the SCP
assign one. The client validates before sending and never retries: a transport
error leaves the outcome unknown.

## SCP

`ian.NewService(handler, options)` validates each N-CREATE and hands the parsed
notification, the Attribute List and the association's AE titles to the
application's `Handler`, which decides what follows (an index update, a
retrieve on its own queue). Statuses:

| Situation | Status |
| --- | --- |
| Valid notification, Handler returns nil | 0000H; the UID is echoed or assigned |
| Attribute List outside the table above | 0120H, 0121H or 0106H |
| Notification UID that is not a valid UID | 0117H Invalid Object Instance |
| UID already accepted (within `RememberedNotifications`, default 4096) or being handled | 0111H Duplicate SOP Instance |
| Same UID and byte-identical Attribute List with `AcceptIdenticalReplay` | 0000H, Handler not called again |
| More series or instances than `Limits` (default 1000 series, 100000 instances, 4 MiB) | 0213H Resource Limitation |
| Handler returns a `*ian.StatusError` | Its status |
| Handler returns another error | 0110H Processing Failure |

A Handler failure does not consume the UID, so the SCU may resend. Other
normalized operations are refused by the runtime with 0211H.

## Interoperability

`DICOMGO_PYNETDICOM_INTEGRATION=1 DICOMGO_PYTHON=/path/to/python go test ./ian
-run Pynetdicom -count=1 -v` runs pynetdicom 3.0.4 as SCU against the `ian` SCP
(success, 0111H, 0106H and 0120H) and as SCP for the `ian` client. DCMTK has no
IAN tool, so there is no DCMTK oracle.

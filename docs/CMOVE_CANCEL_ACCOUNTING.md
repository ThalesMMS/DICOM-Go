# C-MOVE cancellation and response counts


## Counts

The C-MOVE SCP deliberately omits Number of Remaining Sub-operations
`(0000,1020)` in every FE00 Cancel response. Pending responses still include
their existing Remaining count; normal terminal statuses retain their existing
field-presence rules. A caller constructing its own `CMoveResponse` may still
set the optional Remaining pointer explicitly.

In Cancel, Remaining represents requests not initiated because of cancellation.
The `CMoveHandler` and `CMoveBatchHandler` interfaces provide terminal results,
not exact wire-initiation events. Subtracting terminal results from a batch's
size also includes interrupted requests already sent. Omitting the optional
field avoids claiming that this number is an uninitiated count.
[PS3.4 C.4.2.3.1](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_C.4.2.3.html),
[PS3.4 C.4.2.1.6](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_C.4.2.html).

Completed, Failed and Warning are included in library-generated Cancel responses.
A batch handler must yield a terminal result already obtained exactly once,
even when cancellation arrives before that result is published. The SCP
records that result and returns cancellation before a next sub-operation or
another Pending response. A cancellation/deadline error without a terminal
exchange result contributes no fabricated remote success, warning or failure.
Handler implementations remain responsible for reporting truthful results and
observing the supplied context.

The serial `StoreSession` reports a correlated remote terminal response through
`OnProgress` once even when cancellation arrives between response handling and
progress publication. The callback's context may already be canceled. Its
existing item status/outcome remain authoritative; cancellation is still
returned when `OnProgress` is nil and the confirmed item is the last one. The
same ordering applies when a source's `Close` cancels the context after the
confirmed response and returns nil: terminal progress precedes association
Finished. The next source is not opened. An exchange without a response, or a
response with invalid correlation, remains uncertain and is not promoted to a
remote result.
This adjustment is specific to the serial path; asynchronous pipelining retains
its existing cancellation and outcome accounting.

## Initiation, interruption and teardown

Sending C-CANCEL is asynchronous. A Pending response is not a barrier that
prevents the remote SCP from starting another sub-operation before receiving
that cancel. When the SCP has observed cancellation before its next-step
boundary, no next request may start. When a C-STORE request is already in
transit, interruption can abort its storage association; its absence of a
C-STORE response must not be turned into successful completion.
[PS3.7 9.1.1.2 and 9.1.4.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part07/chapter_9.html).

An association whose current exchange has completed may be released normally.
An uncertain interrupted exchange requires abort/cleanup under `StoreClient`'s
existing contract. Cancellation does not require a graceful release of an
outstanding C-STORE exchange.

## Verification and limits

Deterministic tests order cancellation before the real adapter resumes its next
step, after a second request is already on the wire but before its response,
and after a first terminal response has arrived but before progress publication.
They check FE00, raw Remaining absence, mandatory terminal counts, exactly-once
publication, unopened next sources, response correlation, unknown outcomes,
and release versus abort. A three-instance fixture ensures that a third request
is rejected; the interrupted second request receives no synthesized response.
The generic C-STORE destination fixture stays strict about its declared request
count. Only the cancellation-specific fixture admits the identified already
started second request.

The earlier integration failure recorded only a P-DATA PDU type, so its exact
historical message cannot be reconstructed. A deterministic scheduling probe
reproduced the same helper error with a complete second C-STORE command and
dataset sent before C-CANCEL was observed. This proves the test's ordering
assumption can fail, not the historical packet contents or a new request
initiated after an observed cancel. No live PACS, native Windows, benchmark,
performance improvement or clinical validation is claimed.

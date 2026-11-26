# HL7 v2 and MLLP

Package `hl7` parses, builds and acknowledges HL7 version 2 messages, and
package `net/mllp` carries them over the Minimal Lower Layer Protocol. The
goal is to receive demographic updates (ADT) so an application can keep its
catalog coherent, and optionally to ask a master patient index for
demographics (IHE PDQ) or cross-referenced identifiers (IHE PIX). The
library supplies messages, parsing and transport only; deciding what to
update or merge stays with the application.

Both packages are clean-room implementations from the HL7 v2.5.1
specification (chapter 2 for the encoding rules and acknowledgments,
chapter 3 for ADT and the MLLP framing rules) and the IHE IT Infrastructure
Technical Framework (ITI-9 PIX Query, ITI-21 Patient Demographics Query,
ITI-30). dcm4che's HL7 modules informed the scope only; no code was taken
from them or from other implementations.

## Parsing

`hl7.Parse` and `hl7.ParseWithOptions` read one message in the ER7
("pipe and hat") encoding:

- Delimiters come from MSH-1 and MSH-2, any five distinct printable
  non-alphanumeric characters (`|^~\&` by default, the v2.7 truncation
  character included). Segments end with CR; LF and CR LF are accepted.
- Every field is split into repetitions, components and subcomponents
  (`Field`, `Repetition`, `Component`), and leaves are unescaped: `\F\`,
  `\S\`, `\T\`, `\R\`, `\E\` give the delimiters back, `\Xhh..\` the bytes it
  names in the message character set, `\.br\` a line break; `\H\` and `\N\`
  are dropped and any other sequence stays verbatim. The HL7 null `""` is
  kept as a value (`Field.IsNull`): a request to delete, unlike an empty
  field.
- MSH-18 (first repetition) selects the character set, and the whole
  message is decoded to UTF-8 before it is split, so multi-byte sets whose
  second bytes equal a delimiter (Big5 `許` is `B3 5C`, a backslash) parse
  correctly. A character before MSH-18 that would move MSH-18 once decoded
  is refused rather than guessed.

| MSH-18 | Decoding |
| --- | --- |
| empty, `ASCII` | ASCII; bytes above 7FH are refused unless `ParseOptions.DefaultCharacterSet` names another set (for example `UNICODE UTF-8` for senders that leave MSH-18 empty) |
| `8859/1` to `8859/9`, `8859/15` | ISO 8859 |
| `UNICODE UTF-8` | UTF-8, validated |
| `ISO IR87`, `GB 18030-2000`, `KS X 1001`, `BIG-5` | ISO 2022 JP, GB 18030, EUC-KR, Big5 |
| `UNICODE UTF-16`, `UNICODE UTF-32`, others | refused (`ErrCharset`) |

`ParseOptions` bounds the bytes (4 MiB), segments (10000), fields per
segment (1000) and repetitions per field (1000). Errors are `*ParseError`
values that name the segment position, segment ID and field number, never
a value.

`Message.String` encodes back to ER7 text, escaping delimiters, line breaks
and control characters and omitting trailing empty fields and components;
`Message.Marshal` encodes it in the MSH-18 character set and refuses text
that set cannot represent.

### Incremental ER7 input

`hl7.ParseStream(ctx, reader, options, consume)` consumes concatenated
unframed ER7 messages. Each message begins at an MSH segment and ends at
the next MSH or EOF. The reader buffers one encoded message and a small
lookahead, then uses the single-message parser for delimiters, escapes, and
MSH-18. A byte limit applies independently to each message. It counts CR,
LF, CR LF, and extra blank separators following that message as encoded
bytes. This API is for unframed input. MLLP sockets use `net/mllp`.
[HL7 v2.5.1 Chapter 2, §2.5.4](https://www.hl7.eu/HL7v2x/v251/std251/ch02.html)

The callback receives detached messages synchronously in input order.
Earlier callbacks remain consumed after a later parse or I/O failure.
`*hl7.StreamError` identifies the failed message by ordinal and its
starting byte offset. A nested `*hl7.ParseError` identifies the segment
and field when known. Errors from the input reader are reported as
`hl7.ErrStreamRead` without the reader's diagnostic text. Callback
errors return directly. The caller owns the reader and supplies deadlines
or close-on-cancel when a blocked read needs to be interrupted.

### Bounded batch files

`hl7.ParseBatch(ctx, reader, options)` reads one or more BHS/BTS pairs with
an optional enclosing FHS/FTS pair. The returned `BatchFile` contains the
parsed envelope segments and messages in wire order. When present, BTS-1
must equal the batch's message count and FTS-1 must equal the number of
batches. Missing or misplaced envelope segments and invalid counts fail
with `*hl7.BatchError`, which carries a byte offset and one-based batch or
message positions when applicable. The function returns a file only after
it validates the complete input. [HL7 v2.5.1 Chapter 2, §2.10.3](https://www.hl7.eu/HL7v2x/v251/std251/ch02.html)

`BatchOptions` defaults to 32 MiB of encoded file bytes, 1,000 batches,
and 10,000 messages across all batches. Each message also has its own
`MessageOptions` limits and declared character set. The envelope uses
`MessageOptions.DefaultCharacterSet`. The caller owns the reader and the
decision to apply any accepted messages.

## Acknowledgments

`hl7.NewACK(original, code, options)` builds an original-mode
acknowledgment: MSH with sender and receiver swapped and the original
version, processing ID and character set, `ACK^<trigger>^ACK`, MSA with
`AA`, `AE` or `AR` and the original MSH-10; `AE` and `AR` add ERR in the
v2.5 layout (ERR-2 location, ERR-3 code of table 0357, ERR-4 `E`).
`hl7.ACKFor(original, err, options)` chooses the code from a processing
error: `AA` for nil, `AR` for an `*hl7.Error` with `Reject` (unsupported
message type or event), `AE` otherwise, with the error's code and
location. MSA-3 is the code's generic text unless the caller sets one;
error messages never reach it. `hl7.ParseAcknowledgment` reads MSA and ERR,
including the v2.3 ERR-1 layout.

`hl7.ReadAckPolicy(message)` validates MSH-15 for acceptance and MSH-16 for
application processing. `AckPolicy.Wants(code)` applies the requested
condition to the corresponding outcome. `AL` requests all outcomes, `NE`
suppresses them, `ER` requests error or reject outcomes, and `SU` requests
success only. When both fields are absent or null, the policy calls for
an original-mode application response. When one field is provided, an
absent or null counterpart suppresses that phase. Invalid values return
a numbered MSH location without echoing field content.

For an enhanced-mode general accept acknowledgment, decide the outcome
after applying the receiving application's acceptance and storage rules.
Then use `NewACK` with `AckCommitAccept` (CA), `AckCommitError` (CE), or
`AckCommitReject` (CR) if the policy requests it. CE and CR carry ERR
and CA carries MSA without ERR. A later application acknowledgment is a
separate exchange. See [HL7 v2.5.1 acknowledgment rules][hl7-ack].
These helpers leave storage, duplicate detection, and delivery of each
message with the application. `mllp.Serve` and `Client.Send` still use
one application response per request, independently of MLLP2 transport
confirmations.

[hl7-ack]: https://www.hl7.eu/HL7v2x/v251/std251/ch02.html#Heading53

## ADT patient and encounter events

`hl7.ParseADT` supports A01/A04 admission and registration, A28/A31 person
creation and update, and A47 identifier correction, alongside the existing:

- A08 (update patient information): the PID's identifiers, name, birth
  date and sex.
- A40 (merge patient, patient identifier list): each PID with the MRG-1
  identifiers it absorbs, in order.

See [HL7 ADT mapping](HL7_ADT.md) for event shapes, sparse patient/visit
attributes, null handling and conversion limits. A47 is distinct from A40 and
does not populate a demographic update or merge.

Every populated PID-3 and MRG-1 repetition becomes a `PatientIdentifier`, an
`object.IDWithIssuer` (CX-1 as the Patient ID, CX-4 as the issuer: namespace,
universal ID and its type, as DICOM's Issuer of Patient ID and its
qualifiers hold them) with the CX-5 identifier type code. `Matches` applies
the DICOM rule that an issuer present on both sides must agree. The name is
returned in DICOM PN order, `Family^Given^Middle^Prefix^Suffix`, the birth
date as YYYYMMDD, and `DICOMSex` maps PID-8 to M, F or O. Other message
types and events are rejected (`AR`, codes 200 and 201); a missing PID or
MRG, or one without an identifier, is an error (`AE`, codes 100 and 101)
with its location.

Applying an update or a merge is the application's decision: the library
never merges patients, and identity reconciliation should be explicit
(previewed and confirmed) and keyed by the Patient ID with its issuer.

## PDQ and PIX queries

The optional IHE queries are message builders and parsers, sent with an
`mllp.Client`:

- `hl7.NewPDQQuery(parameters, options)` builds QBP^Q22 (ITI-21) with
  `QPD|IHE PDQ Query|<tag>|@PID.5.1.1^VALUE~...`, `RCP|I|<limit>^RD` and,
  for a continuation, DSC.
- `hl7.NewPIXQuery(identifier, domains, options)` builds QBP^Q23 (ITI-9)
  with the identifier to cross-reference and, optionally, the assigning
  authorities to return.
- `hl7.ParseQueryResponse` reads RSP^K22 and RSP^K23: the acknowledgment,
  QAK tag and status (OK, NF, AE, AR), each PID as a `Patient`, and the PDQ
  continuation pointer.

## MLLP transport

A frame is `<VT> payload <FS><CR>` (0BH, 1CH, 0DH). `mllp.Reader` is strict:
bytes before the start block, a start block inside the payload, an end
block without CR, an empty frame and a payload over the limit (1 MiB by
default) are errors, and the server closes the connection on any of them.
`mllp.WriteFrame` refuses a payload that contains a frame byte.

`mllp.Serve(ctx, listener, options)` reads one frame at a time per
connection, parses it, calls the `Handler` and writes its response (nil
sends `AA`; a handler panic answers `AR` with code 207):

- A message whose body does not parse but whose MSH does is answered `AR`
  (code 102, or 207 for a limit); without a readable MSH no
  acknowledgment can be addressed and the connection closes.
- `IdleTimeout` (5 minutes) bounds the wait for a frame, `ReadTimeout`
  (30 seconds) its reception once started, `WriteTimeout` (30 seconds) the
  response; `MaxConnections` (64) bounds concurrent connections, closing
  the excess at once.
- `OnError` receives framing, timeout and parse errors, which name
  conditions and sizes, never content.

`mllp.Client` sends one message at a time and returns the parsed response,
`ErrResponseMismatch` when its MSA-2 acknowledges another control ID; after
a transport or framing error the client is closed.

### Encoded original ACKs

`ServerOptions.OriginalACKHandler` is an opt-in MLLP1 alternative to the
regular `Handler`. It receives the parsed request and a read-only view of
the exact ER7 payload for the duration of the callback. The server checks
that original-mode semantics apply and that the message has a single
MSH-10. The callback can return an encoded AA, AE or AR already committed
by the consuming application. The server bounds and validates the
returned ACK, requires MSA-2 to match MSH-10 and sends its bytes directly.
A callback error, panic or invalid response yields a generic AE when a
response can be framed. Diagnostics omit the callback's error text.

The application defines durable acceptance and replay policy. See
[`hl7rcv` durable inbox](HL7_CLI.md#durable-inbox-and-ack-replay) for a
profile that atomically stores both ER7 and AA before acknowledging
receipt.

## Explicit MLLP2 transport confirmations

`ClientOptions.Release` and `ServerOptions.Release` select `mllp.MLLP1`
(the zero-value default) or `mllp.MLLP2`. Match the release to the endpoint;
there is no automatic negotiation, downgrade, reconnect or retransmission.
An invalid release refuses before dial/listen serving. `NewClient` retains its
signature: an invalid release creates an unusable client that the caller must
still close. Endpoint catalogs and application replay/idempotence policies
belong to the application.

The explicit MLLP2 profile interoperates with the pinned
[dcm4che MLLPConnection](https://github.com/dcm4che/dcm4che/blob/acbbb85eee1a2fcc441eb2d8e65023fece2b40ec/dcm4che-hl7/src/main/java/org/dcm4che3/hl7/MLLPConnection.java#L93-L127).
Each direction confirms a complete frame with exactly one byte in its own
MLLP frame: `<VT><0x06><FS><CR>` for transport ACK and
`<VT><0x15><FS><CR>` for transport NAK. A raw unframed ACK byte, a multi-byte
confirmation, or any other value is refused. The confirmation parser shares
the connection's buffered reader with application frames and admits at most
one payload byte. The standard's name or an old package comment does not
establish a broader Release 2 conformance claim.

A client writes the request, receives its transport ACK, receives the
application response frame and transport-ACKs that complete frame, then parses
and correlates its HL7 response. A server transport-ACKs a bounded complete
request frame before parsing/calling the handler; its application response
still follows the existing AA/AE/AR processing result. It waits for that
response's transport ACK before accepting the next message. A malformed or
over-limit incoming request may receive transport NAK and is then closed;
there is no stream resynchronization. EOF or cancellation does not invent a
NAK. MLLP2 bounds both outgoing and incoming application frames;
`MaxFrameBytes` retains its received-frame meaning in MLLP1.

For MLLP2, authored client messages and handler responses use the reusable
`Message.MarshalWithLimit(MaxFrameBytes)` encoder. It emits canonical ER7
through the shared escape/charset rules incrementally, stops before growing
encoded output above that limit, and returns no partial output on `hl7.ErrLimit`.
It also checks stateful charset residue at close. Output buffer capacity is
capped; transformer buffers are fixed, and buffer growth/header validation may
temporarily allocate additional memory proportional to the limit. Caller-owned
MSH-18 codes are admitted under a closed 32-byte ceiling before case
normalization; all supported codes and aliases fit this ceiling. Caller-owned
message storage and traversal of its empty slices are outside this output
allocation budget. Do not mutate a message concurrently with encoding.
MLLP2 refuses oversized output before writing it; legacy `Marshal` and the
MLLP1 outgoing profile keep their existing behavior.

Transport ACK means the receiver admitted the bounded frame into this
transport operation. It does not assert HL7 AA, file publication, durable
inbox/commit or processing by a HIS. An application response may still be
AE/AR, fail to parse/correlate, or never arrive after transport ACK. MLLP2
is an explicit endpoint profile, not a universal HL7 integration requirement.

`TransportACKTimeout` defaults to 30 seconds. Each client confirmation phase
uses the earlier of that timeout, the existing whole-network-exchange
`ResponseTimeout` and the context deadline. It never resets the whole exchange
budget. The server bounds confirmation writes by both `WriteTimeout` and
`TransportACKTimeout`, and bounds waiting for the response's confirmation by
`TransportACKTimeout`, always respecting context. Its normal frame-start read
timeout does not replace the confirmation deadline. Existing idle, read,
connection and TLS bounds remain. Positive short writes continue until the
complete frame is sent; zero progress refuses with `io.ErrShortWrite`.
Invalid writer byte counts also refuse with `io.ErrShortWrite`, but leave the
request result unknown because such a writer cannot establish what reached
the peer.

For MLLP2, `ExchangeError` exposes `Phase` and `RequestState` with an unwrap-able
cause. `ErrTransportNAK`, `ErrTransportConfirmation`, EOF, timeout and context
cancellation remain distinguishable via `errors.Is`/`errors.As`. Request state
is not-sent before any known accepted wire bytes, unknown after an uncertain write
or missing/malformed confirmation, rejected after a valid NAK, and acknowledged
after a valid ACK even if the application response fails. Application
parse/correlation errors have their own phases; existing
`ErrResponseMismatch` and its parsed response remain available. AA/AE/AR are
HL7 application values, never transport confirmations.

Transport/framing failure (including NAK) closes and poisons the client; later
calls return that failure without another write. Local pre-wire size/input
refusal does not poison an aligned connection. A complete acknowledged frame
with a bad application response leaves transport aligned, but its application
outcome still requires caller policy; there is no implicit retry. Queued
`Send` calls can be canceled without writing. Active cancellation closes I/O
and joins its cancellation callback before releasing the serialization gate.
Server shutdown closes network I/O and joins existing handlers; handlers and
filesystem work remain cooperative with context, without a new hard-wall
promise for arbitrary callbacks.

```go
options := mllp.ClientOptions{
    Release: mllp.MLLP2,
    TransportACKTimeout: 5 * time.Second,
    ResponseTimeout: 30 * time.Second,
    MaxFrameBytes: 1 << 20,
}
client, err := mllp.Dial(ctx, endpointAddress, options)
```

## Security

MLLP has no authentication. Like the DIMSE stack, the server and client are
trusted-network components: expose them only on trusted networks, or set
`ServerOptions.TLSConfig` (with `ClientAuth: tls.RequireAndVerifyClientCert`)
and `ClientOptions.TLSConfig`. HL7 messages carry patient data: neither
package logs, and their errors and the ERR and MSA-3 texts they generate
never quote message content.

## Tests

`go test ./hl7 ./net/mllp` covers parsing of synthetic ADT, ACK, PDQ and
PIX messages (custom delimiters, escapes, null values, Latin-1 and Big5),
exact encodings of the generated ACK and queries, refusals, framing errors,
the server and client end to end (AA, AE, AR, handler panic, response
mismatch, timeouts, connection limit, mutual TLS) and that no error quotes a
patient name. `FuzzParse` and `FuzzReadFrame` exercise arbitrary input.

`TestMLLPInteropPythonPeer` (opt-in) exchanges frames with an independent
peer written with the Python standard library from the framing rules alone:
two messages in one write split at odd offsets to the Go server, and a Go
client message to a Python server:

```sh
DICOMGO_HL7_INTEGRATION=1 DICOMGO_PYTHON=/path/to/python3 \
  go test ./net/mllp -run TestMLLPInteropPythonPeer -count=1 -v
```

The default synthetic TCP peer independently encodes/reads framing and tests
both releases, transport ACK versus AA/AE/AR, NAK, malformed/truncated/missing
confirmations, closure, limits, cancellation, partial writes and no replay.
Mutual TLS and server shutdown with a partial request or pending response
confirmation are also tested.

`TestMLLPInteropDCM4chePeer` is an additional opt-in peer using actual qualified
external dcm4che MLLPConnection/InputStream/OutputStream/Release classes. Its
synthetic Java harness tests public Connection exchanges in both directions
for MLLP1/2 and uses real stream classes to inject NAK, malformed confirmations,
closure and deadlines. The classpath and Java runtime are explicit; selecting
an unavailable or failing runtime fails the gate rather than skipping it:

```sh
DICOMGO_DCM4CHE_HL7_CP=/path/to/dcm4che-classes:/path/to/slf4j-api.jar \
DICOMGO_JAVA=/path/to/java DICOMGO_JAVAC=/path/to/javac \
  go test ./net/mllp -run '^TestMLLPInteropDCM4chePeer$' -count=1 -v
```

Qualification uses synthetic loopback messages and pinned transport classes;
it is not full dcm4chee deployment, HIS acceptance, durable commit or clinical
qualification. Java is optional and is not imported or required by the default
pure-Go library build.

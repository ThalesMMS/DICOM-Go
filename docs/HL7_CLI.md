# HL7 command tools

These tools wrap the reusable `hl7` and `net/mllp` packages. They handle HL7 v2 ER7 messages and original-mode acknowledgments; they do not implement an ADT business workflow, interface engine, HL7 v2 XML ITS, enhanced acknowledgment workflow, or clinical validation.

```sh
go run ./cmd/hl7snd -h
go run ./cmd/hl7rcv -h
go run ./cmd/hl72xml -h
go run ./cmd/xml2hl7 -h
```

## Send one message

```sh
hl7snd -address 127.0.0.1:2575 -timeout 10s message.hl7
hl7snd -address 127.0.0.1:2575 -mllp-release 2 -transport-ack-timeout 5s message.hl7
hl7snd -address '[::1]:2575' -max-bytes 1048576 - < message.hl7
```

Input is parsed before connecting and MSH-10 must be present. The sender applies a finite timeout to dialing, transmission, and the response. It requires exactly one valid MSA, correlation of MSA-2 with the input MSH-10, and AA. Success writes `AA` and exits 0. A failed or short summary write exits 1. AE, AR, missing or mismatched MSA, CA/CE/CR, malformed frames, and timeouts exit 1. Message values and ACK free text are omitted from diagnostics. One command sends one message; concatenated messages with another MSH are refused by the existing parser.

Both network commands default to `-mllp-release 1`. Set
`-mllp-release 2` only for an endpoint configured for framed MLLP2 transport
confirmations; the receiver must select the same release.
`-transport-ack-timeout` defaults to 30 seconds and must be positive, at most
five minutes. It is ignored by transport in release 1. The sender's existing
`-timeout` and context cap the whole dial/network exchange, including transport
confirmations. The receiver additionally bounds transport confirmation writes
by its existing write timeout and waits for confirmation of each response
under the transport timeout. There is no automatic protocol downgrade or retry.

Transport ACK admits a bounded frame before application handling and does not
assert AA, persistence or durable HIS commit. The sender still requires its
correlated application AA; transport ACK followed by AE/AR exits 1. A transport
NAK is diagnosed separately from application rejection. Missing/malformed
transport confirmation after a write leaves the request's result unknown;
transport ACK with an unavailable application response identifies the transport
receipt separately. Every such error exits 1 without replay. Configure endpoint
profiles and any idempotent recovery in the consuming application. See
[transport phases and limits](HL7_MLLP.md#explicit-mllp2-transport-confirmations).

## Receive bounded messages

```sh
mkdir received
hl7rcv -listen 127.0.0.1:2575 -duration 5m -timeout 10s \
  -max-connections 16 -max-messages 1000 -max-bytes 1048576 \
  -output-dir received -max-output-bytes 67108864
```

The default listener is `127.0.0.1:2575`, not a wildcard address. `-listen 127.0.0.1:0` chooses a local available port, which is printed in the initial `listening` summary. These CLI wrappers currently use plain MLLP; the reusable transport supports TLS, but these commands expose no TLS configuration. Use them on a trusted network. No receiver option changes firewall settings.

`-duration` defaults to five minutes and cannot exceed 24 hours. Frame size defaults to 4 MiB and cannot exceed 64 MiB. Idle, frame-read and ACK-write timeouts are finite, defaulting to 30 seconds, at most five minutes each. Connections default to 16 and cannot exceed 128. Handler admissions default to 1000 and cannot exceed 100000. On the first attempt beyond the admission quota, the receiver cancels and joins its handlers; that final attempt may receive AR or a closed connection. The last permitted admission alone does not stop the listener. Invalid frames may be refused before reaching the handler and are still bounded by frame/connection/time limits. Expiration or Ctrl-C closes the listener and active connections and waits for handlers. Filesystem operations finish before their handler exits; cancellation does not promise an instantaneous interruption of file sync.

In release 2, the earlier transport ACK does not wait for file publication and
cannot stand in for the application acknowledgment. Without a persistence
option, AA means the message was parsed and admitted. With `-output-dir`, the
receiver canonicalizes ER7, checks the frame-size limit again, reserves a per-run cumulative byte quota, and writes a mode-0600 temporary file in that existing directory. It syncs the file and atomically creates a random `message-<hex>.hl7` destination with a hard link. Existing paths are never replaced. Temporary files are removed after success or failure. AA follows successful publication; serialization, quota, or persistence failure produces AE. Missing MSH-10 produces AR. Hard-link support is required; there is no non-atomic replacement fallback. The quota accounts for bytes created by this process, not preexisting directory content; file sync does not imply directory-sync durability or a replay/outbox protocol.

The final summary reports admitted/persisted/failure counts. Any observed transport or message refusal gives exit 1. Normal lifetime expiration or cancellation, without observed refusals, gives exit 0. Receiver summaries contain no patient data or control IDs. A failed or short summary write gives exit 1; failure of the initial summary closes the listener before network serving starts.

### Durable inbox and ACK replay

```sh
hl7rcv -listen 127.0.0.1:2575 -replay-dir hl7-inbox -max-output-bytes 67108864
```

`-replay-dir` selects the MLLP1 inbox profile and replaces `-output-dir`.
It accepts single original-mode messages with one nonempty MSH-10.
Use a private local directory dedicated to this endpoint. The receiver
can create the leaf directory and acquires exclusive ownership for its
lifetime. The parent directory must exist.

AA confirms that one private record contains the exact ER7 received and
its encoded ACK. Each record uses the SHA-256 of the ER7 as its filename
and stores the original ER7 and ACK in base64 JSON fields. A resend of the
same bytes returns the original ACK byte for byte, including after the
receiver restarts or the first response is lost. Different ER7 bytes form
a separate receipt even when MSH-10 matches. The inbox does not dispatch
ADT changes or any external application side effect.

The receiver checks record integrity before replay. Invalid receipts and
exhausted storage produce AE, without overwriting committed receipts.
`-max-output-bytes` counts the records already on disk and the receiver also
caps the inbox at 100,000 records and 4 MiB per inbound or outbound frame.
Stored receipts remain available for replay when the disk or record quota
is full. Retransmissions count toward `-max-messages`. The summary separates
newly persisted records from replayed ACKs.

The store syncs each file and publishes it atomically. Unix also syncs the
directory. Windows uses file sync and atomic rename, with recovery across
process restarts; power-loss durability of the directory is filesystem
dependent. The inbox contains patient data. Keep it within the receiving
application's access and retention controls.

## Convert the decoded structure

```sh
hl72xml -output message.xml message.hl7
xml2hl7 -output normalized.hl7 message.xml
hl72xml -output - - < message.hl7 > message.xml
```

Both converters require one input and `-output`. `-` selects stdin/stdout. File output uses the same private staging, file sync, atomic no-overwrite creation, and cleanup as the receiver. A complete conversion is prepared before any output; stdout can still fail partway through a write. Input and output are each bounded by `-max-bytes` (default 8 MiB, maximum 64 MiB); XML tokens are bounded by `-max-nodes` (default 100000, maximum 1000000). Reusable `XMLOptions` also bound segments, fields per segment, and repetitions per field; zero values select defaults and negative values are explicitly refused. Flags with malformed values receive generic diagnostics rather than echoing arguments.

The XML is the library's **private structural format, version 1**, not an HL7 XML interchange standard. The root is `hl7-message`, with exactly `version="1"`, `field`, and `encoding` attributes. Ordered `segment` nodes have a `name` attribute; ordered `field`, `repetition`, `component`, and `subcomponent` nodes have no attributes. A subcomponent contains decoded UTF-8 text with XML escaping. Empty structural nodes are retained. MSH-1 and MSH-2 appear as ordinary structural fields and must agree with the root delimiters. Custom legal ER7 delimiters, repeated fields, components, subcomponents, null markers, and the decoded character-set semantics are preserved. ER7 output uses the existing encoder, including its canonical escapes and removal of trailing empty fields/components; the original byte spelling is not a round-trip guarantee.

Unknown elements or attributes, namespaces, DTD/directives, comments, mixed structural text, additional documents, inconsistent delimiters, and nesting outside this schema are refused. An initial XML declaration is permitted. Character values that XML 1.0 cannot represent are explicitly refused rather than replaced. Failures report categories without XML or ER7 values. Converter success exits 0, malformed CLI usage exits 2, and conversion, limit, cancellation, or output refusal exits 1.

## Qualification limits

Synthetic loopback tests cover both releases, framed transport ACK/NAK separated
from application acceptance, invalid release/timeout flags, AA/AE/AR, enhanced-ACK refusal, correlation mismatch, missing MSA, response timeout, receiver persistence/quota/cancellation with a partial frame, atomic no-overwrite behavior, custom-delimiter XML round trips and malformed/over-limit XML. They establish this implementation's bounded behavior, not external HL7-system interoperability, native Windows execution, clinical acceptance, or complete XML ITS conformance.

Inbox tests exercise distinct ER7 with one control ID, concurrent duplicate
delivery, restart after commit, an abandoned temporary file, exact quota and
corrupt record refusal.

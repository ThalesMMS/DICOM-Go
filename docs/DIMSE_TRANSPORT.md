# Explicit DIMSE connection transport

`ul.DialOptions.Transport` supplies one requestor transport policy. Its zero
value opens TCP directly and retains the existing negotiation, TLS, ARTIM and
PDU limits. It does not read HTTP_PROXY, HTTPS_PROXY or NO_PROXY.

```go
options.Transport = ul.TransportOptions{
    LocalAddress: &net.TCPAddr{IP: net.ParseIP("192.0.2.10")},
    HTTPConnect: &ul.HTTPConnectOptions{Address: "proxy.example:8080"},
}
association, err := ul.DialContext(ctx, "archive.example:11112", options)
```

The local address binds the physical outgoing socket (to the proxy when one
is selected). Port zero selects an ephemeral source port. Binding to a local
address that is unavailable fails; it never silently switches interfaces.

When the source port is left to the system, the built-in TCP path connects
again, up to three more times within the dial context, if a connect fails with
an error that a fresh ephemeral port can avoid: address in use or no buffer
space left, which usually mean ephemeral port pressure. Under such pressure
Windows can choose a port whose TIME_WAIT connection went to the same peer, and
the connect then fails with WSAEADDRINUSE (Tcpip event 4227) before reaching
the peer; another attempt from a fresh port succeeds. An address-not-available
error is retried only when the system also chose the source IP and the remote
address is valid: with a configured source IP that is not on the host, or on
Windows with an invalid remote address such as port 0, it fails at once. A
configured source port, a refusal by the peer and every other error fail at
once. `ul.LocalEndpointUnavailable` reports whether an error names the local
side of the connection (address in use, address not available, no buffer
space).

The other outgoing TCP clients of this module share the same policy and
bounds: the default HTTP client of `net/dicomweb`, its UPS-RS event
channel dial, the default OIDC client, the `dsig` timestamper and `jpip`
defaults, `net/fhir`, `net/hl7v3`, `net/mllp`, the TCP and TLS syslog of
`net/audit` and the LDAP directory client of `net/netconf/ldapconfig`. A
caller-supplied HTTP client or dialer keeps its own dial.
`ul.SourcePortRetryDialContext(d)` returns that dial for any other client, for
example `transport.DialContext = ul.SourcePortRetryDialContext(nil)` on a
clone of `http.DefaultTransport`; only `tcp`, `tcp4` and `tcp6` are retried,
and never from a configured source port. `ultest.WithSourcePortCollision`
(package `net/ul/ultest`, for tests only) makes the first connect attempt
of such a dial come from a given endpoint, such as one still in TIME_WAIT to
the same server, and reports whether a dial met the collision.

A `DialContextFunc` may instead supply an owned stream. UL calls it once per
explicit Dial attempt, with `tcp`, the physical destination and the caller's
context (or its bounded CONNECT child). Without a proxy the destination is
the original DICOM address; with a proxy it is the explicit proxy address.
The function must implement cancellation and be concurrency safe. UL closes
returned streams on connection, CONNECT, TLS or association failures, including
an erroneous connection returned together with an error. A nil successful
connection is refused. A custom dialer gets no implicit retry or direct
fallback.

Custom dialers own pre-connect local binding and kernel socket-buffer policy.
Combining a custom dialer with UL LocalAddress or nonzero SocketBuffers refuses
before dialing, rather than ignoring an option. The built-in TCP path sets
SocketBuffers before connect, including for CONNECT. Its existing 64 MiB size
limits remain. EffectiveSocketBuffers accepts raw TCP only; tunnel and TLS
wrappers explicitly report unsupported. It does not claim requested sizes are
the sizes granted by the operating system. Custom streams must support the
net.Conn deadline contract used by negotiation and DIMSE.

## HTTP CONNECT and TLS

This explicit adapter implements plain HTTP/1.1 CONNECT using authority-form
`host:port`, including bracketed IPv6. Proxy and target authorities are limited
to 1024 bytes and cannot contain userinfo, URL paths or whitespace. Username
and password together are bounded to 8192 bytes. Optional Basic authentication
is sent only to the proxy; this hop is plaintext, so credentials require a
trusted protected network. This adapter does not implement HTTPS-to-proxy,
SOCKS, SSH, VPN management, authentication negotiation or environment discovery.

Dial plus CONNECT has a 30-second default timeout; a positive explicit timeout
or earlier caller deadline bounds it. Cancellation interrupts CONNECT reads
and writes through the existing joined connection-deadline mechanism. Response
status and headers together have a 32 KiB default limit; explicit limits range
from 256 bytes to 1 MiB. Rejected response bodies are never drained or retained.
Malformed, oversized, early-closed and non-2xx responses close the stream.
Errors expose typed status codes and finite library messages, never proxy
reason phrases, headers, bodies or authentication values. Caller-defined dialer
errors are caller-owned and must likewise avoid secrets.

As [RFC 9110 section 9.3.6](https://www.rfc-editor.org/rfc/rfc9110.html#section-9.3.6)
specifies, a 2xx response switches to a tunnel immediately after the headers.
Content-Length does not consume tunnel bytes. Bytes prefetched by the bounded
reader remain available to subsequent TLS or UL reads. Informational responses
are refused in this finite adapter; no redirect or authentication retry occurs.

Destination TLS starts over that tunnel and uses the original DICOM address
for the existing default SNI/certificate name policy. The adapter does not
change certificate verification, TLS minimum versions, Called AE or logical
requestor telemetry. RemoteAddr still reports the physical socket peer; explicit
UL endpoint telemetry identifies the logical DICOM destination through a proxy.
The default disclosure policy continues to omit endpoint values.

VerifyCEcho and DialRetrieveSCU expose the same TransportOptions. Store sessions
and Storage Commitment delivery clone its address/profile values when freezing
DialOptions; custom function closures remain caller-owned. C-MOVE return C-STORE
connections are new associations initiated by the remote SCP: the requestor's
outgoing tunnel does not configure them. C-GET sub-operations use the existing
association. Any secondary requestor association must receive the policy again;
UL has no process-global transport or peer-policy inference.

## Validation scope

Synthetic loopback tests cover custom context/address/count, invalid mixed
policies, source binding, socket-buffer behavior, authentication, accepted and
rejected responses, malformed/oversized/incomplete headers, prefetched bytes,
CONNECT/TLS cancellation and deadlines, destination certificate acceptance and
refusal, unchanged Called AE and logical telemetry. DIMSE convenience callers
and retained store policies have causal forwarding/ownership tests. These are
local protocol fixtures, not qualification against a hospital proxy or PACS.

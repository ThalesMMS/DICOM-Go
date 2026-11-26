# CDA submission over HTTP

`net/hl7v3` sends a UTF-8 HL7 v3 CDA document in one HTTP POST to a
configured endpoint. The caller selects the exact URL and verifies that the
peer supports this XML POST profile. The client uses the existing
`encapdoc.InspectPayload` CDA structure and document ID checks, then sends
the supplied bytes unchanged as `application/xml; charset=utf-8`.

```go
client, err := hl7v3.NewClient("https://documents.example.test/receive",
    hl7v3.Options{Timeout: 10 * time.Second})
if err != nil {
    return err
}
receipt, err := client.Submit(ctx, cdaBytes)
if err != nil {
    return err
}
// Interpret receipt.StatusCode and receipt.XML under the endpoint protocol.
```

The endpoint comes from trusted application configuration. HTTPS verifies
the hostname and certificate chain with the system roots or an explicit
`Options.RootCAs` pool. The constructor clones a supplied pool. Cleartext
HTTP requires explicit `AllowLoopbackHTTP` and a numeric loopback address,
such as `127.0.0.1` or `[::1]`. User information and URI fragments are
refused. Each submission makes a direct HTTP/1.1 exchange with no
environment proxy, connection reuse, redirect handling or automatic retry.

## Delivery states

| Result | Meaning |
| --- | --- |
| Successful `Response` | A complete HTTP 2xx response, with optional bounded XML bytes. Interpret the status and XML according to the endpoint profile. HTTP 202 can mean processing is queued. |
| `DeliveryError{State: NotSent}` | Local validation or cancellation stopped the request before sending, or the transport failed before obtaining an HTTP connection. This includes connection refusal and certificate verification failure. |
| `DeliveryError{State: Uncertain}` | The connection was obtained and a complete usable response did not arrive. The peer may have received the document. Reconcile with the peer before resending. |
| `DeliveryError{State: Rejected}` | An HTTP non-2xx status arrived. The status is available for diagnosis. The error body and redirect destination are ignored. |

Use `errors.As` for `*hl7v3.DeliveryError` and `errors.Is` for causes
including `context.Canceled`, `context.DeadlineExceeded`,
`hl7v3.ErrRedirect`, and `hl7v3.ErrResponseTooLarge`. The errors
report the state and known HTTP status without exposing the URL,
document, redirect location or peer response body. Even when a status
arrives, the endpoint-specific outcome remains the application's
responsibility. [HTTP POST, idempotence and retry](https://www.rfc-editor.org/rfc/rfc9110.html#section-9.2.2) ·
[HTTP 202 Accepted](https://www.rfc-editor.org/rfc/rfc9110.html#section-15.3.3)

## Limits and verification

The default submission timeout is 30 seconds. An earlier caller deadline
applies across validation, networking and response reading. Request and
response bodies default to 4 MiB each and permit explicit limits up to
64 MiB. Response headers are limited to 64 KiB. Structural XML checks
admit a single root and bound depth and tokens to 64 and 100,000. A
nonempty response uses a UTF-8 XML media type with identity encoding.
DTD declarations and response processing instructions other than the XML
declaration are rejected. Structural checks run without loading external
resources. [XML media types](https://www.rfc-editor.org/rfc/rfc7303.html)

The profile exposes the submitted bytes and receipt as documents.
SOAP envelopes, WS-Security, clinical template conformance, and durable
application receipt tracking are outside this transport profile.

```sh
go test ./net/hl7v3 ./encapdoc -count=1
go test -race ./net/hl7v3 -count=3
```

[Loopback regressions](../net/hl7v3/client_test.go) exercise both trusted
and untrusted TLS, byte-exact POST, redirect refusal, HTTP failure,
truncation, timeout after the full POST arrives, and XML and byte limits.

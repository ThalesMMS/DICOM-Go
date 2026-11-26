# Offline CRL policy for DIMSE TLS

`ul.ClientTLSConfig` and `ul.ServerTLSConfig` accept an optional
`TLSProfileOptions.Revocation`. Nil, or `TLSCRLNone` without lists, retains the
previous TLS behavior. No CRL distribution point, AIA, OCSP or other network
lookup is performed. This is a restricted offline profile, not a complete
implementation of RFC 5280 revocation processing or a certification claim.

```go
config, err := ul.ClientTLSConfig(ul.TLSProfileOptions{
    TrustedCAs: roots,
    ServerName: "pacs.example.test",
    Revocation: &ul.TLSRevocationOptions{
        Mode: ul.TLSCRLChain,
        CRLs: lists, // parsed x509.RevocationList values with signed Raw DER
    },
})
```

The caller must provide ordinary trust first. CRLs do not establish trust or
override certificate validity, hostname, EKU or TLS-profile checks. Revocation
cannot be enabled with `TLSIgnorePeerCertificate`. A server using
`TLSVerifyPeerCertificateIfGiven` still permits an absent client certificate;
when one is present, its ordinary trust and the configured CRL policy apply.
Use `TLSRequirePeerCertificate` for required mTLS.

## Certificate and CRL scope

`TLSCRLLeaf` checks the peer leaf. `TLSCRLChain` checks the leaf and every
intermediate in a verified path, excluding the trust anchor. A directly trusted
peer leaf is always checked, even if it is also the anchor. A self-issued leaf
can authenticate its own CRL. A directly trusted non-self-issued leaf with no
verified issuer cannot satisfy this profile by omission. Alternative verified
paths are accepted only if at least one complete path satisfies the policy.

Accepted material consists of complete direct X.509 v2 CRLs signed by the
certificate's issuing CA. Applicability requires issuer DER equality, matching
AKI/SKI when both are present, and `x509.RevocationList.CheckSignatureFrom`,
including its CA/key-usage checks. A similar issuer name alone is insufficient.
`ThisUpdate` and `NextUpdate` must exist, be ordered, and contain the sampled
time in `[ThisUpdate, NextUpdate)`. Missing, expired, future, inapplicable or
unauthenticated evidence fails closed for every certificate that must be checked.

Delta CRLs, issuing-distribution-point/scoped CRLs, indirect certificate-issuer
entries, `removeFromCRL`, duplicate extensions and unprocessed critical
extensions are refused explicitly. AKI supports the usual `keyIdentifier`;
issuer/serial-qualified AKIs are refused rather than ignoring GeneralName
qualifiers. The identifier is decoded from signed DER independently of Go's
version-specific parsed `AuthorityKeyId` representation. Noncritical unknown extensions do not
establish narrower scope or additional authority. This profile does not merge
delta or certificate-hold state.

Every authenticated current full CRL for an issuer contributes to the decision:
**any current authenticated list containing the serial revokes it**, regardless
of input order or CRL number. A future `revocationDate` does not authorize the
connection; the serial remains revoked under this conservative policy. Stale or
badly signed lists cannot establish a good state. A current clean list may
supply good evidence when other candidates are stale or unauthenticated. Replace
the bundle for new configurations rather than accumulate overlapping epochs.

## Bounds, snapshots and connection lifecycle

Construction limits are 64 lists, 16 MiB aggregate signed DER and 100,000
aggregate entries. `PreflightTLSCRLDER` walks bounded ASN.1 structure and counts
the complete aggregate before any full x509 parser can materialize current and
legacy entry slices. It is an allocation admission gate, not CRL validation or
authentication; full x509 parsing and cryptographic checks remain authoritative.
These are input limits, not a 16 MiB RSS promise. Caller CRLs are copied and reparsed
from signed `Raw`; changing their parsed fields or DER after construction does
not alter the active policy. Trust pools are cloned when policy is enabled.
Local private keys are governed by the ordinary `crypto/tls` ownership contract
and are not arbitrarily cloned.

`ValidateTLSCRLs` performs bounded material/temporal preflight. It cannot prove
issuer applicability or signature authority before the peer's verified path is
available. Each `VerifyConnection` samples one time and uses it for its ordinary
path verification and CRL check. The server retains native client authentication
and additionally reverifies a presented client path in the callback. This
callback runs on full and resumed connections; it is not the resumption-skipped
`VerifyPeerCertificate` callback. No reader, timer loop or background goroutine
is added to TLS transport.

Configurations are immutable snapshots for concurrent handshakes after
construction. A changed CRL file/pool requires a newly built configuration.
Connections already established do not receive retroactive revocation or
automatic reconnect/retry behavior.

## Diagnostics

`errors.As(err, *ul.TLSRevocationError)` exposes `Kind` and bounded chain,
certificate and list positions (`-1` when unavailable). `Error()` contains no
subject, CN, serial, PEM or path. Use `errors.Is` with
`ErrTLSCertificateRevoked`, `ErrTLSRevocationUnknown`, or `ErrTLSCRLInvalid`.
Kinds distinguish revoked, missing, expired, not-yet-valid, issuer-mismatch,
bad-signature, unsupported, invalid-material and limit. Ordinary x509 trust
errors remain wrapped separately and accessible through `errors.As`; they are
not relabeled as a CRL result.

## Twin backend integration

DIMSE nodes persist `TLSCRLPolicy` (`""`/`none`, `leaf`, `chain`) and `TLSCRLFile`.
The receiver persists `ReceiverTLSCRLPolicy` and `ReceiverTLSCRLFile`. Enabled
policy requires TLS and ordinary peer verification; receiver policy also
requires its client CA file. DICOMweb/OIDC do not inherit this DIMSE policy.
Empty/none defaults retain existing configurations and legacy helper behavior.

The shared backend loads one regular PEM/DER bundle through a bounded descriptor
before Dial or receiver Start. Its 16 MiB cap includes PEM/file bytes, separately
from the library DER cap. PEM blocks must all be `X509 CRL`; headers, leading or
trailing non-whitespace, malformed first blocks and excess lists are refused.
File/material errors expose no path or contents. Policy syntax/scope is validated
on node/config save, while material is loaded for a dependent operation. The
legacy helper used by worklist, MPPS, UPS and print delegates to the DIMSE profile
only when a CRL policy is active. HTTPS entry points reject an incompatible
DIMSE policy before cached transport reuse or a custom HTTP client override,
without reading the CRL file. Receiver changes require restart to take effect.
Both shells expose the shared policy/file controls: MacStyle Nodes has a CRL
dialog and Listener has TLS Settings; WindowsStyle exposes them in its Nodes
and Listener forms. Form validation checks syntax/scope without reading the
file. Headless form tests cover persistence, rejection and dialog behavior;
they do not establish native Windows GUI or live-PKI qualification.

## Qualification

Default tests use only generated synthetic certificates, CRLs and localhost.
They cover leaf/chain distinctions, trust anchors, malformed/unsupported
material, temporal boundaries, signature/issuer failures, conservative sets,
caller mutation, bounded file loading, configuration round trips and previous
TLS defaults. Actual TLS 1.2 and 1.3 session resumption is established with
`ConnectionState.DidResume` before a replacement policy rejects revoked or
unknown certificates on **both** requestor and acceptor callbacks. New client
trust also rejects a cached server certificate; server CA changes may invalidate
tickets/client-certificate selection before the callback and are not presented
as the same resumed-path experiment.

Optional DCMTK checks run against the pinned local 3.7.0/OpenSSL 3.6.4 tools:

```sh
DICOMGO_TLS_CRL_DCMTK_DIR=/path/to/pinned/bin \
DICOMGO_TLS_CRL_DCMTK_DICTIONARY=/path/to/pinned/dicom.dic \
go test ./net/ul -run '^TestTLSCRLDCMTKLocalInterop$' -count=1 -v
```

The test verifies tool/dictionary hashes and versions before use. It exercises
`echoscu → Go SCP` and `Go SCU → storescp`, `+crv`/`+cra`, valid/leaf-revoked/
intermediate-revoked cases, correlated C-ECHO and release. OpenSSL's full-chain
policy can include the anchor, so the test explicitly provides its self-issued
CRL in addition to the intermediate CRL. This qualification does not imply
identical handling of every RFC feature, a real PACS session, a native Windows
run or clinical validation. Build-cache location and adjacent source HEAD do
not establish a tool binary's build provenance.

Design authority: [RFC 5280 §5/§6.3](https://www.rfc-editor.org/rfc/rfc5280.html),
[Go TLS Config](https://pkg.go.dev/crypto/tls#Config),
[RFC 8446 C.2](https://www.rfc-editor.org/rfc/rfc8446.html), and
[DICOM PS3.15 B.12](https://dicom.nema.org/medical/dicom/current/output/chtml/part15/sect_B.12.html).
Behavioral comparison: DCMTK public `+crl`/`+crv`/`+cra` contracts and
[OpenSSL verification flags](https://docs.openssl.org/3.0/man3/X509_VERIFY_PARAM_set_flags/).
Product implementation and fixtures are clean-room; no reference-project code
was copied.

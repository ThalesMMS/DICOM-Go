# Declarative network configuration

`net/netconf` describes one Device, its Connections and Application Entities,
and per-AE Transfer Capabilities. Version 1 is an opt-in JSON subset of the
network model in [PS3.15 Annex H](https://dicom.nema.org/medical/dicom/current/output/chtml/part15/chapter_H.html).
The optional [LDAP reader](#ldap-directory-reader) maps directory declarations
into these models. The model itself implements neither automatic discovery,
a remote-node credential store nor a DIMSE service simply because its UID is declared.

The synthetic example is [two-aes.json](../net/netconf/testdata/two-aes.json).
One listener accepts `CT_SCP` with Verification/CT Storage in Explicit VR Little
Endian and `MR_SCP` with Verification/MR Storage in Implicit VR Little Endian.
The same file is used by the storescp and Twin backend qualification tests.

```go
device, err := netconf.ReadFile(ctx, path, netconf.Options{})
if err != nil { return err }
connection, err := netconf.CompileConnection(ctx, device, "loopback", netconf.Options{})
if err != nil { return err }
// Validate application service/TLS constraints before binding.
accept := ul.AcceptOptions{CalledAERegistry: connection.Registry()}
```

The consumer owns the listener, TLS credentials, service handlers, persistence,
admission and shutdown. `Address`, `TLSRequired`, `AETitles` and `Profiles` provide
the selected connection's policy. An empty connection ID auto-selects only when
the document declares one Connection. Port zero explicitly requests a local
ephemeral listener; no discovery protocol publishes its assigned port.

## Model and compatibility

`schemaVersion` is 1. Connections have unique IDs, a hostname, a TCP port and an
explicit `tlsRequired` boolean. AEs have a title, Connection ID references,
explicit `associationInitiator`/`associationAcceptor` flags and capabilities.
Installed flags are optional: Device omission means true, and Connection/AE
omission inherits Device. Inactive declarations remain serializable but are
excluded from listener compilation. A selected disabled Connection, Device or
connection without an installed acceptor fails. An empty capability catalog
cannot compile an installed acceptor.

Titles are bounded to 16 decoded bytes, use UL normalization for padding and
remain case-sensitive. For example,
`CT_SCP` and `ct_scp` are different AEs. JSON parsing and serialization canonicalize
title padding. Names/descriptions may contain Unicode; titles, UIDs and hostnames
use their own bounded domains. Unknown fields/versions, duplicate JSON keys,
duplicate normalized AE titles, duplicate Connection IDs or SOP/local-role
pairs, dangling references, invalid UTF-8/control characters, invalid UIDs,
empty/duplicate TS lists and unsupported roles are refused. Input slice order,
including transfer syntax preference order, is retained.

Each capability has one SOP Class UID, local role `SCU` or `SCP` and an ordered
nonempty `transferSyntaxUIDs` list. The compiler inverts local roles into the UL
requestor-role policy. An omitted wire role selection therefore means requestor
SCU/local SCP, as in [PS3.7 D.3.3.4](https://dicom.nema.org/medical/dicom/current/output/chtml/part07/sect_D.3.3.4.html).
Dual local roles for one SOP must have identical ordered TS lists: UL currently
has one list per SOP, so differing lists fail rather than being merged.


## Listener selection

`ul.CalledAERegistry` owns a frozen catalog. Its constructor admits the complete
logical backing before creating maps, nested slices and string copies. Detached
inspection cannot modify active policy. The registry selects the validated
Called AE before admission, identity handling or presentation-context negotiation.
The socket's request is read once; negotiation never loads a file or invokes a
catalog-selection callback.

An unknown/inactive AE receives permanent service-user rejection reason 7
([PS3.8 9.3.4](https://dicom.nema.org/medical/dicom/current/output/chtml/part08/sect_9.3.4.html)).
Malformed AE/application-context requests keep existing UL behavior. A selected
AE has an exclusive SOP/TS/role catalog. Global catalogs/rules, promiscuous mode,
AlwaysAcceptDefaultRole, matching-one-AE policy and common-general SOP fallback
conflict with the registry and fail construction. Shared extended negotiation
handlers remain filtered by accepted SOPs. Context, TLS, admission, identity,
PDU/time limits, observability and association slots retain their existing
connection-wide lifecycle. A nil registry preserves the single-AE path.

## Consumers

`storescp -net-config file.json [-net-connection id]` uses this loader/compiler.
It excludes explicit `-address`, `-aetitle`, `-config-file`/`-profile`, transfer
syntax preference/list, `-accept-all` and `-promiscuous` flags, including explicit
false values. TLS credentials, finite resource/timeout flags and read/persistence
policy remain independent. All active AEs must declare only known Storage or
Verification local-SCP services and readable TS envelopes. An unsupported service,
local-SCU declaration or TLS mismatch refuses startup before storage directory
creation and listener binding.

Twin uses the same JSON in its session and headless receivers. Application
service toggles, TLS, archive/import policy and caller/host/identity allowlists
remain constraints; they cannot augment the JSON catalog. Selected local AE
identity is retained for received Source/Receiving File Meta, FIND Retrieve AE
and MOVE's outgoing Calling AE. Sending AE and Move Originator retain their
separate meanings. Legacy aliases keep their existing primary local identity.
Configuration changes require stop/start; there is no reload or fallback catalog.
See Twin's `docs/NETWORK_CONFIGURATION.md` for persisted settings and constraints.

## Bounds, ownership and cancellation

Zero limit fields choose finite defaults; negative values and impossible arithmetic
settings fail. No unlimited sentinel is accepted.

| Bound | Default |
| --- | --- |
| Raw JSON bytes / depth / raw scalar bytes | 1 MiB / 16 / 8192 |
| Name / description / hostname decoded bytes | 128 / 4096 / 253 |
| Connections / AEs | 16 / 64 |
| Capabilities total / per AE | 4096 / 1024 |
| TS entries total / per capability | 16384 / 256 |
| Logical model/compiled backing per admission | 8 MiB |

The raw pass checks schema shape, duplicate keys, depth, raw scalar spans,
decoded UTF-8 sizes (including escaped surrogate pairs), and cumulative
counts/backing before exact model arrays or copied model strings. Decoded models undergo a
second count/backing preflight before validation maps. Compilation also admits
temporary rule/map headers and the frozen catalog together before either copy. Registry
limits are passed explicitly, so a larger valid netconf limit does not silently
reset to a hidden default. Profile conversion also preflights the expanded
dual-role representation before copies. Serialization counts the exact escaped
output and its schema/scalar admission before creating a canonical header view
or serialization buffer. Canonical JSON disables HTML escaping; Unicode line
separators retain encoding/json's escaping. Empty inactive catalogs serialize
as arrays, and title padding is removed without modifying the caller's model.

These are logical limits, not RSS ceilings. The bounded raw buffer, caller/decoded
model, validation maps, temporary profile headers and detached registry may
coexist. Profile conversion additionally has a bounded detached inspection and
owned result. Reader growth, encoder scratch/capacity, runtime/GC, transport and
caller-owned original backing are not exact heap accounting. A parsed model does
not retain the raw JSON or open handle; a compiled registry retains no mutable
model slices. Getters return detached lists; strings are immutable Go values.

`Parse`/`Write` borrow their streams; `ReadFile` owns and closes its handle.
Failures return no partial Device/compiled catalog. Errors use fixed diagnostics
without values, UIDs or paths; cancellation and I/O causes preserve `errors.Is`.
Context is checked before reads, between schema/model/compile stages and before
publication. An arbitrary blocking Reader/Writer cannot be forcibly interrupted.

Qualification uses synthetic loopback fixtures and negative matrices for AE,
SOP, TS and roles, mutation after compilation, cancellation and cumulative
admission. It does not establish target-PACS interoperability, LDAP conformance,
clinical suitability, full IOD validation or native Windows behavior.

## LDAP directory reader

`net/netconf/ldapconfig` is an optional read-only adapter using
[DICOM PS3.15 2025e Annex H](https://dicom.nema.org/medical/dicom/2025e/output/chtml/part15/chapter_H.html)
and the public [go-ldap v3 API](https://pkg.go.dev/github.com/go-ldap/ldap/v3).
It requires an explicit server URL and Devices base DN. It performs no DNS SRV
discovery, automatic server contact, directory administration or LDAP writes.
Consumers that do not call it need no directory server or native LDAP runtime.

```go
snapshot, err := ldapconfig.Read(ctx, ldapconfig.Config{
    URL: "ldaps://directory.example.test:636",
    BaseDN: "cn=Devices,cn=DICOMConfiguration,dc=example,dc=test",
}, ldapconfig.Options{})
// Authentication, if required, is supplied as an ephemeral BindDN/Password
// by the application vault, never embedded in the URL or persisted here.
```

Device/Connection/AE/Transfer Capability declarations map to the existing
schemaVersion 1 netconf model; every selected device is validated before any
snapshot is returned. Device installation is required by the LDAP schema;
optional AE/Connection installation inherits it. Device children contain AEs
and connections; transfer capabilities belong directly to an AE. Connection DN
references resolve only inside the selected device, never through extra queries
or foreign referrals. SOP class, local role and transfer syntax order are
preserved. Unsupported/contradictory mappings and dangling references fail
without a partial snapshot. Unknown auxiliary attributes/classes are ignored
within the same admission budgets; vendor data is not requested.

An omitted LDAP `dicomPort` remains zero with an initiator-only diagnostic; it
does not invent port 104. Such a discovered connection is descriptive and must
not be directly compiled as a local ephemeral listener without consumer policy.
TLS cipher-suite declarations imply `TLSRequired`, without supplying TLS keys.
`CT_A` and `ct_a` remain distinct case-sensitive AE identities. Device IDs hash
canonical parsed DN attribute names while preserving value case; connection IDs
use the same identity policy. DeviceName selectors use escaped exact LDAP
equality filters; AETitle selection occurs after complete reference validation.

`ldaps://` verifies TLS immediately; `ldap://` negotiates verified StartTLS
before binding or searching. TLS defaults to system roots and URL hostname and
requires at least TLS 1.2. Supplied roots/server identity remain caller policy;
insecure verification and custom verification callbacks are refused. There is
no password downgrade. An explicit `AllowAnonymousCleartext` option is limited
to anonymous `ldap://` consumers; Twin does not expose it. URL userinfo, filters,
queries, fragments and foreign path scopes are refused. Bind credentials are
excluded from JSON and diagnostic formatting. Errors preserve fixed typed
authentication, transport, schema, protocol, referral and resource sentinels;
server diagnostics, configured values and credentials are not exposed.

Defaults bound a complete read to 20 seconds, 64 entries per requested page,
128 pages total, 32 devices, 4,096 entries total, relative DN depth 8, 64 KiB per
entry packet/accounting, 1 MiB per LDAP packet, 16 MiB cumulative decrypted
bind/search frames, 8 MiB model accounting, BER depth 16 and 8,192 BER nodes per
packet. Options have finite hard ceilings. Page count/entry/model budgets apply
across the device query and every child query, not separately per device.
Paging cookies are bounded to 4,096 bytes. A server may return the same cookie
for every page; a page with a cookie and no entry ends the read as a protocol
failure. Every page of one search repeats the same request, including its size
limit, because a server that keeps the cursor, such as ApacheDS, ends a paged
search whose request changes. Server result limits fail closed. An absent paging
control permits a complete bounded single-page result, not an
incomplete-result fallback.

A read of a whole directory leaves out a device whose declarations are
incomplete or contradictory, and a device that declares no Application Entity,
such as a web application of a dcm4che configuration. `Snapshot.Skipped` names
each with `device-invalid` or `application-entities-absent`; the other devices
stay readable. A device selected with `DeviceName` must be valid: the read
fails with the schema sentinel otherwise.

Declared packet lengths are admitted before allocation; an iterative walk
admits BER depth/node limits before the dependency decoder. Response shapes,
embedded paging BER, count/cookie types and optional control criticality are
checked before dependency typed assertions. Duplicate/foreign controls and
referrals are refused. StartTLS's pre-handshake LDAP response has a separate
16 KiB budget. TLS handshake transport has Go TLS limits and the same deadline;
encrypted transport and caller/decoder/runtime allocations are not exact RSS
accounting. Queries are sequential on one owned connection. Context/deadline
closes that connection; no alias dereference, foreign referral following or
retry to another endpoint occurs.

Tests use an original synthetic loopback LDAP wire fixture with self-generated
certificates, actual verified LDAPS/StartTLS and paged device/AE searches. They
exercise escaped literals, case-sensitive models, missing ports, reference
failures, schema contradictions, packet/depth/control/aggregate limits,
authentication, refusal of referrals and cancellation. They do not establish
institutional schema interoperability, clinical approval or directory server
conformance. Twin's directory workflow
owns protected profiles, credentials, cache and explicit local import.

```sh
go test -race ./net/netconf/ldapconfig -run '^TestLDAPDirectory.*$' -count=1
```

## LDAP user identity and groups

`ldapconfig.Authenticate` uses an explicitly configured user base DN and group
base DN. It resolves one literal `uid`, optionally using an application-supplied
search bind, then binds with the end user's own credentials. Both steps use
verified TLS with LDAPS or StartTLS. The group lookup runs as the authenticated
user and requires readable `objectClass` and `member` on matching
`groupOfNames` entries. It checks direct membership and returns group DNs.
The consumer maps these DNs to roles and owns policy, expiry, and audit.

A result requires successful completion of both LDAP searches. A non-success
result, referral, conflicting identity, malformed entry, unexpected response
control or local budget failure discards the whole identity. The group limit
defaults to 128 and accepts explicit limits from 1 to 256. The optional limits
also bound the connection deadline, directory depth, total LDAP entries, BER
frames and materialization. `IdentityDNKey` normalizes attribute names and
escaping for explicit local mappings while preserving DN value case. This
profile covers direct `groupOfNames` membership with `uid` login. The server
must expose these attributes to the authenticated user.

The wire tests use a synthetic TLS peer. The opt-in test takes a fixture JSON
from a locally managed OpenLDAP loopback server. It accepts the fixture
generated by Isis-DICOM-Viewer's `Tools/Scripts/ldap_loopback_fixture.py`.

```sh
go test -race ./net/netconf/ldapconfig -run '^TestLDAPIdentity' -count=1
DICOMGO_OPENLDAP_IDENTITY_FIXTURE=/path/to/fixture.json go test ./net/netconf/ldapconfig -run '^TestLDAPIdentityOpenLDAP$' -count=1
```

The transport, bind and completion rules are based on the LDAP protocol and
authentication specifications. [LDAP protocol][ldap-protocol] ·
[LDAP authentication][ldap-authentication] · [LDAP group schema][ldap-schema]

[ldap-protocol]: https://www.rfc-editor.org/rfc/rfc4511.html
[ldap-authentication]: https://www.rfc-editor.org/rfc/rfc4513.html
[ldap-schema]: https://www.rfc-editor.org/rfc/rfc4519.html#section-3.5

# DCMTK association configuration profiles

`net/ul.ParseAssociationConfig` reads the documented DCMTK two-level `.cfg`
format. `ReadAssociationConfigFile` opens/closes its own handle. A parsed config
is immutable and retains no reader or open file; every `Profile` call returns
detached slices, role pointers and extended bytes. The source must be stable
during reading. File operations and a caller's blocking `io.Reader.Read` cannot
be interrupted by context; cancellation is checked around bounded reads and
semantic entries. The caller owns cancellation of its underlying stream.

```go
config, err := ul.ReadAssociationConfigFile(ctx, "association.cfg", ul.AssociationConfigOptions{})
if err != nil { return err }
profile, err := config.Profile("Default", ul.AssociationConfigModeRequestor)
if err != nil { return err }
session, err := dimse.NewStoreSession(address, dimse.StoreSessionOptions{
    PlanOptions: dimse.StorePlanOptions{PresentationContexts: profile.Contexts},
    DialOptions: ul.DialOptions{
        RoleSelections: profile.RoleSelections,
        ExtendedNegotiation: profile.ExtendedNegotiation,
    },
})
```

Profile selection does not enable a codec, transcode, service handler, TLS,
identity, admission policy, AE title, asynchronous window, or common extended
negotiation. Applications must supply their actual capabilities and network
security settings independently.

## Format and limits

Required supersections are `[[TransferSyntaxes]]`, `[[PresentationContexts]]`
and `[[Profiles]]`. Optional supersections are `[[SCPSCURoleSelection]]` and
`[[ExtendedNegotiation]]`. Named sections use `[Name]`. Numbered keys start at
one with no gaps: `TransferSyntaxN`, `PresentationContextN`, `RoleN` and
`ExtendedNegotiationN`. Context values are `SOPClass\TransferSyntaxList`; roles
are `SOPClass\SCU`, `\SCP` or `\BOTH`; extended values are
`SOPClass\contiguous-even-length-hex`. Optional role/extended lists may be empty;
individual extended entries require data. A profile requires
`PresentationContexts = List` and may reference `SCPSCURoleSelection` and
`ExtendedNegotiation` lists.

Labels/references/keys normalize ASCII case and whitespace; symbolic UID names
preserve case. Numeric UIDs use the existing canonical UI validator, including
the 64-byte limit and OID component rules. An optional `=` precedes symbolic
names. Standard keywords resolve through the existing PS3.6 registry; explicit
compatibility aliases cover the active names in DCMTK 3.7.0+DEV official example
files, including historical/draft/retired names. Resolving a UID does not declare
runtime envelope or pixel codec support. Other private UIDs can be numeric;
unrecognized symbolic names fail explicitly. `LocalEndianExplicit` and
`OppositeEndianExplicit` use host order, or the explicitly supplied
`LocalByteOrder` (little/big) for deterministic compilation.

LF, CRLF and CR are accepted. `#` begins a comment only in the first column;
indented continuations belong to the preceding value. There is no include,
environment interpolation or inline-comment substitution. This reader
deliberately rejects controls/non-ASCII syntax, duplicate normalized sections
or keys, unknown keys/supersections, extra separators, gaps/leading-zero
indices and unresolved references. DCMTK's generic reader can replace duplicate
keys or ignore entries after the first gap; this reader diagnoses them rather
than silently changing policy. These stricter malformed-input behaviors are
not a claim of byte-for-byte parser equivalence.

Zero limit fields select finite defaults; negative/overflow-invalid limits
fail. Checks precede copying/hex decoding/profile expansion.

| Resource | Default |
| --- | ---: |
| Input bytes | 1 MiB |
| Physical line / logical value | 8 KiB each |
| Normalized name | 128 bytes |
| Named sections / total entries / profiles | 1024 / 16384 / 256 |
| Context catalog rules / syntax offers per list | 1024 / 256 |
| Role items / extended items per list | 1024 / 1024 |
| Extended data per item / parsed total | 1024 bytes / 64 KiB |
| Compiled profile logical allocation admission | 8 MiB |

Compiled admission is a documented logical charge, checked in full before
output or auxiliary map allocation: 128 bytes plus SOP UID length per context;
16 bytes plus UID length per syntax entry; 64 bytes plus SOP UID length per
role; 64 bytes plus SOP UID length plus exact payload length per extended item.
Acceptor selection additionally charges 64 bytes plus 64 per entry for its
unique-SOP map, and the same for the role-index map when roles exist. Requestor
selection needs neither map. Output context/rule/role/item/syntax slices and
extended bytes use exact `make(len)` capacities, avoiding append growth or
payload capacity rounding beyond their admitted lengths.

These charges conservatively represent application structures but are not a
hard allocator-byte or RSS bound: Go map buckets, allocator size classes,
stack/GC/runtime overhead, retained parsed config and source-reader buffers are
excluded from that interpretation. Finite input/count limits separately bound
parsing metadata. Creating an `ExtendedNegotiationHandler` retains an additional
frozen response map/payload copy; each response owns another exact-capacity
copy, outside the already completed `Profile` admission. Applications control
handler lifetime and concurrent requests. Profiles reference shared parsed
definitions until selection, preventing eager expansion of every profile's
catalog. Caller-provided larger limits remain finite, subject to protocol widths.

## Negotiation and C-STORE planning

A requestor profile has at most 128 contexts with IDs `1,3,...,255`; repeated
SOPs remain separate contexts in original order. An acceptor profile is a
bounded rule catalog, may exceed 128 rules, and rejects duplicate SOPs. Only
one transfer syntax is accepted per proposed context, with the acceptor's
configured preference. These follow [PS3.7 D.3.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part07/sect_D.3.2.html)
and [PS3.8 9.3.2.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part08/sect_9.3.2.2.html).

`StorePlanOptions.PresentationContexts == nil` retains automatic planning.
A non-nil fixed template must be nonempty and already use canonical sequential
odd IDs; UL assigns those IDs, so accepting arbitrary odd IDs would silently
change the requested template. Malformed/noncanonical templates fail before
clone/source inspection/dial. An admitted template preserves exact context
order, IDs, unused offers, repeated SOPs and transfer-syntax order on the wire.
Sources absent from the configured SOP/producible-syntax intersection fail
planning before the session calls `Open` or sends a command. Planning still
calls `StoreSource.Inspect`: existing implementations can perform bounded
metadata/preparation I/O there, including explicit coercion. A profile does
not eliminate that existing cost or reconstruct unsupported representations.
Source descriptors and independent per-representation limits remain intact;
an accepted alternate context can be used if an earlier context is refused.
Serial, async/pipeline, reconnect and cancellation use the existing session.

Role keywords describe the **requestor** in both modes. Missing declarations
retain requestor SCU/acceptor SCP; no role inversion or
`AlwaysAcceptDefaultRole` exception is inferred. Role/extended SOPs must occur
in the selected profile. The acceptor cannot grant unoffered roles. See
[PS3.7 D.3.3.4](https://dicom.nema.org/medical/dicom/current/output/chtml/part07/sect_D.3.3.4.html).

For the seven nonempty intersections of SCU/SCP/BOTH proposals and policies,
real-socket qualification matches DCMTK's role bits, accepted contexts and
requested static extended replies. In the two disjoint cases (SCU versus SCP
and SCP versus SCU), the existing Go UL rejects the unusable context with
user-rejection and omits its role/extended reply. DCMTK 3.7.0+DEV instead accepts
the context with both role bits zero (NONE), returning its configured extended
reply. PS3.7 D.3.3.4 permits turning each proposed role down to zero;
[PS3.8 9.3.3.2](https://dicom.nema.org/medical/dicom/current/output/chtml/part08/sect_9.3.3.2.html)
also permits user-rejection of a presentation context. The Go policy remains
conservative; it is a documented difference, not negotiation equivalence for
those two cases. This loader does not change existing UL negotiation behavior.

Extended bytes are opaque service-specific application information.
`AssociationProfile.ExtendedNegotiationHandler` freezes a static response map
and returns detached configured data only for requested configured SOPs;
UL additionally filters accepted contexts. It does not prove the service's
capability claims. The application must substantiate their meaning under
PS3.4/private service rules. A response is omitted when not requested, as
[PS3.7 D.3.3.5](https://dicom.nema.org/medical/dicom/current/output/chtml/part07/sect_D.3.3.5.html)
requires. DCMTK's configuration acceptor may construct an unsolicited configured
extended reply; this adapter deliberately follows the standard's omission rule.
The socket control with no extended request confirms that existing Go
`DialContext` rejects DCMTK's unsolicited response as `ErrInvalidUserItem`; a
Go acceptor using this handler omits it and completes the association.

## Standalone CLIs

Both `storescp` and `storescu` accept `-config-file file -profile name` together,
with no inferred default name. They load/validate once before binding/dialing
or scanning source paths; the run uses that frozen policy. With neither flag,
existing defaults remain unchanged. Resource limits, TLS/identity/AE settings,
timeouts, cancellation and publication behavior remain independently configured.

`storescp` uses the selected profile exclusively, adding no hidden Verification
or default syntax contexts. Profile flags conflict with explicitly supplied
`-accept-all`, `-promiscuous`, `-transfer-syntaxes` or any `-prefer-*` flag
(including explicit boolean false). Each selected syntax must have a readable
dataset envelope in the existing runtime registry; no pixel decoder is required
to store a compressed envelope. Only known Storage and Verification classes,
and requestor SCU-only roles, are supported. Private/unimplemented services,
SCP/BOTH requestor roles and nonempty configured extended replies fail explicitly
before listener/storage directory publication. Config bytes cannot substantiate
Storage Level, signature or coercion claims this CLI does not negotiate.
`-bit-preserving` remains an independent persistence choice.

`storescu` supports known Storage classes and requestor SCU-only roles. A
configured Storage extended request must be exactly `030000000200`: six bytes
claiming SCU-only operation, unspecified signature level and N/A coercion,
under [PS3.4 B.3](https://dicom.nema.org/medical/dicom/current/output/chtml/part04/sect_B.3.html).
Other claims/services/receiving roles fail explicitly. `-s` metadata coercion,
`-transcode-to` and its existing explicit lossy/verification/fallback options
can coexist. They determine producible representations; the profile only
restricts offers and admission. No automatic codec selection/transcode is
inferred from the profile. An excluded source or unavailable representation is
reported through the existing bounded session rather than widening the policy.

The complete official DCMTK examples load in generic UL: the requestor's
Default has 128 contexts; the acceptor's Default/AllDICOM catalogs have
215/183 rules. The generic catalog is service-neutral. The narrower CLI can
reject a complete official profile containing unreadable envelopes or services
it does not implement; that is not full standalone CLI support for every entry.

## Qualification

Synthetic fixtures cover parsing/admission/ownership/cancellation, catalogs
larger than 128, canonical wire templates and real serial/pipeline C-STORE.
Opt-in external qualification uses unchanged official configs, an independently
authored temporary harness around public DCMTK configuration APIs, and real
loopback sockets with installed `storescu`/`storescp`. Qualification compares
every SOP and ordered syntax in the three official profiles, all 215 acceptor
rules in three bounded associations with refusal controls, and the complete
128-context official SCU proposal in one association. It also exercises RLE
C-STORE in both directions: DCMTK to Go with parsed/bit-preserving persistence,
and Go to DCMTK with explicit native-to-RLE preparation, RLE-only offers and
bit-exact decoded samples. The role matrix qualifies seven equivalent cases,
two documented differences, and unsolicited extended-response refusal. These
checks used DCMTK 3.7.0+DEV (source revision `8d68392d`) offline.

Set `DCMDICTPATH` to the installed `dicom.dic` if that DCMTK build requires it.
The temporary dump harness is an opt-in external executable and is not shipped
with the library:


```sh
DICOMGO_DCMTK_INTEGRATION=1 \
DICOMGO_DCMTK_BIN=/path/to/dcmtk/bin \
DICOMGO_DCMTK_CONFIG_DIR=/path/to/dcmtk/dcmnet/etc \
DICOMGO_DCMTK_ASSOCIATION_ORACLE=/path/to/temporary-public-api-dump-harness \
go test ./net/ul ./cmd/storescp ./cmd/storescu -run AssociationProfile\|AssociationConfigDCMTK -count=1
```

Missing opt-in tools/fixtures/harness are explicit skips, not acceptance.
No upstream cfg or implementation is copied into the product. These checks
exercise configured negotiation and synthetic offline C-STORE behavior; they
do not establish live-PACS interoperability, clinical validation, arbitrary
codec availability, every private service's extended semantics or native
Windows execution.

# C-MOVE and C-GET retrieve proxy

`dimse.RetrieveProxy` is a Query/Retrieve SCP for C-MOVE and C-GET whose
instances come from a pluggable upstream instead of local files: another
archive over DIMSE, WADO-RS, a cache. The proxy locates the instances
first, so every response carries exact sub-operation counts, then fetches
and delivers them one by one. It is a library component: wiring, the
destination table and authentication stay with the application.

The design is a clean-room implementation from PS3.4 Annex C (C-MOVE and
C-GET behavior) and PS3.7 (command fields and statuses). dcm4che's
retrieve task and instance locator informed the shape only; no code was
taken from it.

## Wiring

```go
source, _ := dimse.NewDIMSERetrieveSource(dimse.DIMSERetrieveSourceOptions{
	Address: "archive:104", CalledAETitle: "ARCHIVE", CallingAETitle: "PROXY",
})
proxy, _ := dimse.NewRetrieveProxy(dimse.RetrieveProxyOptions{
	Source:  source,
	AETitle: "PROXY",
	Destinations: dimse.MoveDestinationPolicy{
		Destinations: map[string]dimse.MoveDestinationAddress{
			"VIEWER": {Address: "10.0.0.5:11112"},
		},
		CallerOnly: true,
	},
})
// For each accepted association (ul.AssociationServer handler):
handler := proxy.Handler(assoc)
err := dimse.ServeAssociation(ctx, assoc, dimse.AssociationSCPOptions{
	CMoveHandler: handler, CGetHandler: handler,
})
```

The acceptor must support the Patient or Study Root MOVE and GET SOP
Classes and, for C-GET, the Storage SOP Classes with the SCP role offered
to the requester (`ul.RoleSelectionItem{SCURole: true, SCPRole: true}`).

## Retrieve sources

`RetrieveSource.Locate` receives the downstream request (model, level,
Identifier, priority, calling AE Title, Move Destination) and returns a
`RetrieveSet`: the located `RetrieveInstance`s, a `Fetch` for each, and
`Close`. The proxy calls `Fetch` once per instance, in order, then `Close`
exactly once when the operation ends, canceled or not.

- `NewDIMSERetrieveSource` walks the upstream hierarchy with C-FIND down to
  the IMAGE level, one query per study and per series as hierarchical
  matching requires, then fetches each instance with an IMAGE-level C-GET on
  one association kept for the whole retrieve (reopened after a transport
  failure). It uses the downstream request's model, Patient or Study Root.
- `RetrieveSourceFuncs` adapts a locate and a fetch function, for example a
  QIDO-RS search and a WADO-RS instance retrieve.

SOP Class UID is optional in a located instance. DCMTK's `dcmqrscp`, for
one, does not return (0008,0016) in C-FIND; the class is then read from the
fetched instance, and both the upstream C-GET association and the Move
Destination association also propose `DefaultCGetStorageSOPClassUIDs`. A
fetched instance whose SOP Instance UID, or known SOP Class UID, differs
from the located one is a failed sub-operation and never forwarded.

## Sub-operation statuses and counts

Each sub-operation's status is the C-STORE status the receiving peer
returned: the Move Destination for C-MOVE, the requester itself for C-GET.

| Outcome | Counted as |
| --- | --- |
| `0000H` | completed |
| C-STORE warnings `0001H`, `0107H`, `0116H`, `Bxxx` | warning |
| Any other C-STORE status | failed |
| The source cannot provide the instance, or provides another | failed |
| No accepted context for the SOP Class or the fetched transfer syntax | failed |
| Transport failure toward the destination | failed, and every remaining sub-operation with it |

Pending responses follow each sub-operation with the remaining, completed,
failed and warning counts. The final status is `0000H` when every
sub-operation completed and `B000H` otherwise, with the C-GET Failed SOP
Instance UID List. C-CANCEL interrupts the fetch or store in progress and
answers `FE00H` with the counts so far; the set is closed either way. The C-MOVE SCP counts C-STORE warnings `0001H`, `0107H`
and `0116H` as warnings for every handler, as the C-GET SCP already did.

C-MOVE sub-operations carry the requester's AE Title as Move Originator
Application Entity Title and the C-MOVE Message ID as Move Originator
Message ID, on one association from `RetrieveProxyOptions.AETitle` to the
Move Destination.

## Refusals

| Condition | Status | Error Comment |
| --- | --- | --- |
| Move Destination unknown, or refused by the policy | `A801H` | `Move Destination unknown` |
| The source cannot locate (upstream down, C-FIND failure) | `A701H` | `Unable to calculate number of matches` |
| More than `MaxInstances` located (at most 65535) | `A702H` | `Too many sub-operations` |
| Move Destination cannot be reached | `A702H` | `Unable to perform sub-operations` |

The destination is resolved before the upstream is consulted, so a refused
C-MOVE causes no upstream traffic. An unknown and a refused destination get
the same answer, so a caller cannot probe the policy; the application can
tell them apart through `ErrMoveDestinationUnknown` and
`ErrMoveDestinationRefused`.

## Destination policy

`MoveDestinationPolicy` maps Move Destination AE Titles to addresses
(`MoveDestinationAddress`, with an optional per-destination TLS
configuration). `Callers` restricts, per destination, the calling AE Titles
that may use it, and `CallerOnly` restricts every caller to its own AE
Title. Any `MoveDestinationResolver` can replace it, for example one backed
by the application's node catalog.

## Transfer syntaxes

The proxy never transcodes compressed pixel data. A fetched instance is
sent in its own transfer syntax or, for a native one, in another accepted
native little endian syntax (`StoreTransferSyntaxUIDs`). Toward a Move
Destination the proxy proposes, for every SOP Class, one presentation
context per transfer syntax: the located instances' syntax hints first, then
`DestinationTransferSyntaxUIDs` (Explicit then Implicit VR Little Endian by
default), falling back to one context per SOP Class beyond 128 contexts.
`NewDIMSERetrieveSource` proposes `TransferSyntaxUIDs` for the upstream's
C-STORE sub-operations, uncompressed by default, which asks the upstream to
decompress; add the compressed syntaxes the downstream peers accept to pass
them through unchanged. C-GET sub-operations use the requester's accepted
contexts through `CGetSubOperation.LoadFile`.

## Limits and security

- Instances are held in memory one at a time, as received from the upstream
  C-GET (whose data set limits apply).
- Error Comments never carry upstream error text, which may name patients;
  the Go errors keep the cause for the application's own diagnostics.
- Like the rest of the network stack, the proxy is a trusted-network
  component: it adds no authentication, and the destination policy is only
  as strong as the AE Titles peers present. Restrict listeners to trusted
  networks, or configure TLS (`ul.ServerTLSConfig`, `ul.ClientTLSConfig`) and
  user identity negotiation on both sides.

## Tests

`go test ./net/dimse -run 'RetrieveProxy|MoveDestinationPolicy'` runs
in-process peers: an upstream C-FIND and C-GET SCP, the proxy, a Storage SCP
destination and C-MOVE and C-GET requesters. They check pending and final
counts against destination statuses (warnings `B007H` and `0107H`, failure
`A700H`, an offline upstream instance), Move Originator forwarding, upstream
association reuse, the Patient Root hierarchy walk, policy refusals without
upstream traffic, `A701H` and `A702H`, transfer syntax selection, a
mismatched fetch, C-CANCEL during a fetch, and an upstream that does not
report SOP Class UID.

`TestRetrieveProxyDCMTKPeers` (opt-in) puts DCMTK on both sides: `dcmqrscp`
as the upstream, `movescu` as requester and its own Move Destination,
`getscu` as C-GET requester, and a refused `movescu` from another AE Title:

```sh
DICOMGO_DCMTK_INTEGRATION=1 DICOMGO_DCMTK_BIN=/path/to/dcmtk/bin \
  go test ./net/dimse -run TestRetrieveProxyDCMTKPeers -count=1 -v
```

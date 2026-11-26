# DIMSE connection flags

The eight requestors (`echoscu`, `findscu`, `storescu`,
`dicom-go-retrieve`, `ianscu`, `mppsscu`, `stgcmtscu`, `upsscu`) and three
listeners (`echoscp`, `storescp`, `qrscp`) use
[`internal/dimsecli.ConnectionFlags`](../internal/dimsecli/connection.go).
Each accepts `-host`, `-port`, `-called`, `-calling`, `-timeout` and
`-c AET@host:port`. Existing TLS profiles, certificate flags and requestor
User Identity options retain their respective roles.

| Flag | Requestor | Listener |
| --- | --- | --- |
| `-host` / `-port` | Peer host and TCP port | Bind host and TCP port |
| `-called` | Peer AE title | Local SCP AE title |
| `-calling` | Local AE title | Optional exact Calling AE filter |
| `-c` | Sets peer AE, host and port | Sets local AE, bind host and port |
| `-timeout` | The tool's existing timeout scope below | Association negotiation timeout |

The listener Calling AE filter is case-sensitive, permits padded equivalent
AE titles, and returns Calling-AE-title-not-recognized for a different title.
It composes with existing association admission policies. Its empty default
permits any Calling AE; an explicitly empty AE flag is invalid. This option
neither enables User Identity requests on an SCP nor authenticates the caller.
The existing called-AE matching/profile options remain effective.

## Defaults and timeout scope

| Tool | Endpoint | Called / local AE | Calling AE | Timeout |
| --- | --- | --- | --- | --- |
| `echoscu` | `localhost:11112` | `ANY-SCP` | `ECHOSCU` | 10s, dial/negotiation |
| `findscu` | `localhost:11112` | `ANY-SCP` | `FINDSCU` | 10s, existing dial/find context |
| `storescu` | Explicit endpoint required | `ANY-SCP` | `STORESCU` | 10s, session negotiation and read/write progress |
| `dicom-go-retrieve` | `127.0.0.1:4242` | `ORTHANC` | `DICOMGO` | 30s, retrieve context and release |
| `ianscu` | `localhost:11112` | `ANY-SCP` | `IANSCU` | 30s, association/operations |
| `mppsscu` | `localhost:11112` | `ANY-SCP` | `MPPSSCU` | 30s, association/operations |
| `stgcmtscu` | `localhost:11112` | `ANY-SCP` | `STGCMTSCU` | 30s + existing callback wait context |
| `upsscu` | `localhost:11112` | `ANY-SCP` | `UPSSCU` | 30s, association/operations |
| `echoscp` | `127.0.0.1:11112` | `ECHOSCP` | Any | 10s, negotiation |
| `storescp` | `127.0.0.1:11112` | `STORESCP` | Any | 15s, negotiation |
| `qrscp` | `127.0.0.1:11112` | `QRSCP` | Any | 10s, negotiation |

`storescu -host` without `-port` uses 11112; `-port` without `-host` uses
localhost. The positional legacy address still supplies both values.
Every explicit `-timeout` must be positive; `-timeout 0s` is rejected.
EchoSCP negotiates on its accept loop, so its 10s default bounds a peer that
connects and sends nothing; it no longer negotiates without a limit. A
listener's timeout does not set the process lifetime or replace idle,
progress, shutdown or release limits. EchoSCU's new flag controls the existing dial deadline; it does not
introduce a deadline for receiving C-ECHO. No timeout is silently broadened.

## Existing spellings and conflict policy

`-called-ae` and `-called-aet` alias `-called`; `-calling-ae` and
`-calling-aet` alias `-calling`. They are accepted by all eleven tools and
marked deprecated in help. Requestors also accept the legacy
`-remote host:port` endpoint spelling.

| Tool | Additional retained spelling |
| --- | --- |
| `echoscp` | `-ae-title` → `-called` |
| `storescp` | `-address` → host/port; `-aetitle` → `-called`; `-negotiation-timeout` → `-timeout` |
| `qrscp` | `-listen` → host/port; `-ae` → `-called` |
| `storescu` | Positional `host:port` before files/directories |

Defaults do not count as explicit choices. Repeated explicit values must agree,
regardless of spelling or order. `-c SCP@localhost:104 -port 104` is accepted;
`-port 105 -c SCP@localhost:104` and the reverse order are rejected. AE padding,
decimal port spelling and duration spelling are normalized before comparison;
`1000ms` equals `1s`. Host spellings are compared literally without DNS lookup:
`localhost` does not equal `127.0.0.1`. A failed composite value leaves every
connection field unchanged.

With an explicit endpoint, StoreSCU accepts an additional matching positional
endpoint. A different valid endpoint is rejected. A colon in a filename,
including a Windows drive path, does not make it an endpoint: the complete
strict host/decimal-port parser must accept it first. A relative filename that
is itself a valid `host:port` is ambiguous; prefix it with `./` to select a file.
Flags precede positional files. MPPS/UPS retain their leading action argument.
Storage Commitment's `-listen` remains the separate callback listener setting.

StoreSCP `-net-config`/`-net-connection` continue to select the network catalog.
Explicit endpoint and AE flags, including every alias and `-c`, conflict with
that mode in either argument order. Existing profile/syntax exclusions remain.
Negotiation timeout flags are compatible with that mode; conflicting values
between `-timeout` and `-negotiation-timeout` are still rejected.

## Address and AE validation

Ports are decimal 1–65535 for requestors; listeners also allow 0 for an
ephemeral port. Services such as `http`, signs and values above 65535 are
rejected. Composite endpoints require `host:port`; IPv6 uses brackets:
`SCP@[::1]:104` or `SCP@[fe80::1%en0]:104`. The `-host` value itself uses an
unbracketed IP. The final `@` separates the AE from the endpoint, allowing a
legal AE such as `SCP@GROUP`. An empty listener host binds all interfaces;
requestor hosts must be nonempty. Host parsing performs no DNS or interface
lookup. Host/composite address flags are bounded to 1024 bytes.

AE values use the library's existing
[`ul.ValidateAETitle`](../net/ul/aetitle.go): 1–16 normalized ASCII characters,
case-sensitive, no backslash or control bytes. Surrounding padding is trimmed;
raw AE flag input is bounded to 1024 bytes before trimming.

## Examples

```sh
echoscu -c STORESCP@127.0.0.1:11112 -calling ECHOSCU -timeout 10s
storescu -c ARCHIVE@[::1]:104 -calling MODALITY image.dcm
storescu -called ARCHIVE -calling MODALITY '[::1]:104' image.dcm
dicom-go-retrieve -c ORTHANC@127.0.0.1:4242 -calling DICOMGO -method get -study-uid 1.2.3
storescp -c STORESCP@127.0.0.1:11112 -calling MODALITY -timeout 15s
mppsscu create -c MPPS@localhost:11112 instance.dcm
upsscu find -c UPS@localhost:11112 -k ProcedureStepLabel=VERIFY
```

Coverage includes every CLI's help, defaults, canonical and historical
spellings; strict parsing and order-independent conflicts; listener policy
composition; and synthetic C-ECHO loopbacks. These checks do not constitute
PACS interoperability or clinical acceptance. Native Windows execution and
performance measurements are outside this change.

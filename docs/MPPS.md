# Modality Performed Procedure Step

The DICOMweb client exposes `CreateMPPS`, `UpdateMPPS` and `RetrieveMPPS`
for the UTF-8 DICOM JSON profile of PS3.18 §15. These use the same
attribute lists produced by the builders below. Create and Update send one
JSON object using the provided UID, and require 201 or 200 respectively
with an empty success body. Retrieve is uncached and verifies one represented
dataset, its state and any supplied SOP Class/Instance identity.
The methods bound responses to 8 MiB or a smaller caller limit, refuse HTTP
redirects and validate any Location or Content-Location against the exact
resource. An unconfirmed write is left to caller reconciliation.

The local HTTP checks are `TestMPPSWebTransactions`,
`TestMPPSWebRejectsUnexpectedResponses`,
`TestMPPSWebDoesNotRedirectOrSendInvalidIdentity` and
`TestMPPSWebCancelsReadsAndBoundsBody` in `net/dicomweb`.
See [PS3.18 §15](https://dicom.nema.org/medical/dicom/current/output/chtml/part18/chapter_15.html).

Package `mpps` implements the Modality Performed Procedure Step SOP Class
(`1.2.840.10008.3.1.2.3.3`) of PS3.4 F.7 (2026d) on top of the normalized DIMSE
layer. It reuses `NormalizedClient`, `NormalizedSCPOptions`, the association
runtime, command codecs and Message ID correlation; it adds no dispatcher.
MPPS is never enabled implicitly: a server offers it only when the application
accepts the SOP Class and passes `Service.NormalizedOptions` to
`ServeAssociation`.

## Supported subset

- N-CREATE and N-SET of the MPPS SOP Class. The retired MPPS Retrieve and
  Notification SOP Classes, N-GET and N-DELETE are not provided and are refused
  by the runtime.
- Attributes of Table F.7.2-1 for Performed Procedure Step Relationship
  (Scheduled Step Attributes Sequence with study, accession, requested and
  scheduled step identifiers, patient name/ID/birth date/sex, Referenced
  Patient Sequence), Performed Procedure Step Information and Image
  Acquisition Results (Modality, Study ID, performed protocol codes, Performed
  Series Sequence with image and non-image references). Other attributes
  allowed on N-CREATE (for example Billing and Material Management, Gender
  Identity) are stored as received but not validated; N-SET refuses them.
- Code Sequence items need exactly one of Code Value, Long Code Value and URN
  Code Value; Coding Scheme Designator is required with Code Value or Long
  Code Value and optional with URN Code Value (Type 1C, PS3.3 Table 8.8-1a).
- Text follows the Specific Character Set of the N-CREATE. An N-SET that
  declares a different character set is refused with 0106H: the SCP does not
  convert text (PS3.4 F.7.2.2.3 allows this).

## Lifecycle and statuses

| Situation | Result |
|---|---|
| N-CREATE with a Type 1 attribute or a Type 2 attribute absent | 0120H Missing Attribute, Attribute Identifier List |
| N-CREATE with a Type 1 attribute empty | 0121H Missing Attribute Value |
| N-CREATE with status other than IN PROGRESS, or a bad UID, date, time or code | 0106H Invalid Attribute Value (PS3.4 F.7.2.1.3) |
| N-CREATE with an invalid Affected SOP Instance UID | 0117H Invalid Object Instance |
| N-CREATE without Affected SOP Instance UID | the SCP assigns a UID and returns it |
| N-CREATE of an existing UID | 0111H Duplicate SOP Instance, unless `AcceptIdenticalCreateReplay` and the step is unchanged and byte-identical |
| N-SET of an attribute Table F.7.2-1 marks "Not allowed" | 0106H with the attribute |
| N-SET of an unknown step | 0112H No Such SOP Instance |
| N-SET of a COMPLETED or DISCONTINUED step | 0110H, Error Comment "Performed Procedure Step Object may no longer be updated", Error ID A710H (Table F.7.2-2) |
| N-SET to COMPLETED/DISCONTINUED without End Date, End Time or a Performed Series item | 0110H, Error Comment "Final State attributes missing or invalid", Attribute Identifier List |
| Series or reference count over the limits | 0213H Resource Limitation |

Sequences in an N-SET replace the stored sequence whole (PS3.4 F.7.2.2.2).
A failed operation publishes nothing. N-SETs racing on one step are serialized
by the repository's compare-and-swap with bounded, jittered retries; an update
that still loses is refused with 0110H, never merged silently, and once a step
is final every later update is refused. Diagnostics carry tags, never values.

## Persistence and application boundary

`Service` stores through the `Repository` interface (`Create`, `Get`,
`CompareAndSwap`). `MemoryRepository` is a bounded reference that does not
survive a restart. Scheduling, RIS or worklist coupling, authorization, audit
and UI stay in the application. To relate a step to a worklist item, copy the
Scheduled Procedure Step identifiers of the Modality Worklist response into
`ScheduledStep`; for an unscheduled procedure leave the Type 2 identifiers
empty. The library never derives clinical data the modality did not send.

## Client

`Client.Create` and `Client.Set` validate before sending and return the status
fields of a failure together with a `*dimse.NormalizedStatusError`. A
transport error leaves the outcome unknown and is not retried automatically.
Enable `AcceptIdenticalCreateReplay` on an SCP to let a modality repeat an
N-CREATE whose response it did not receive.

`cmd/mppsscu` drives the client from the command line, building the
N-CREATE and the final N-SET from instance files; see
[CLI_TOOLS.md](./CLI_TOOLS.md#mppsscu-modality-performed-procedure-step).

## Example and evidence

`go run ./examples/mpps` runs an SCP and SCU on loopback with synthetic data:
N-CREATE, COMPLETED with the acquired series, and a refused late update.

- `TestValidateCreateFollowsTableF721`, `TestValidateCreateCodeItemIdentifiers`,
  `TestServiceLifecycleCompleted`,
  `TestServiceLifecycleDiscontinuedAndRefusals`,
  `TestServiceIdenticalCreateReplayIsOptIn`, `TestServiceConcurrentUpdates`
  (run with `-race`) and `TestClientAndServiceOverAssociation`.
- Opt-in independent peer (`DICOMGO_PYNETDICOM_INTEGRATION=1`,
  `DICOMGO_PYTHON`): `TestPynetdicomSCUAgainstServiceSCP` has pynetdicom
  3.0.4 create, update and complete a step against the `mpps` SCP and check
  0110H/A710H, 0106H and 0112H refusals; `TestClientAgainstPynetdicomSCP` has
  a pynetdicom SCP whose handlers enforce the lifecycle record the commands,
  SOP Class/Instance UIDs, statuses, series and image references the `mpps`
  client sent.

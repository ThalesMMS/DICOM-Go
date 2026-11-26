# Clinical result association

`clinicalresult` links a submitted diagnostic revision to an order for an
already authorized patient. The application verifies the sender, resolves the
patient against its trusted identity crosswalk, and supplies the eligible
orders. The same order numbers may exist for several patients.

Each identifier consists of a system and a value. Both are compared exactly,
with the role of an order identifier kept separate. Every supplied order
reference must resolve to a single eligible order within the authorized
patient. An unrecognized patient, conflicting known demographics, split
references, and ambiguous candidates prevent publication. A partial birth
date contributes only the precision present. Errors identify the failure
without including patient identifiers, report text, or order values.

## Wire projections

- `OrderFromFHIR` and `ResultFromFHIR` read a bounded FHIR R4 JSON
  `ServiceRequest` or `DiagnosticReport`. The binding supplies the verified
  base URL including its tenant, the local Patient logical ID, and the resolved
  patient identifier. A report uses one eligible `ServiceRequest`
  `basedOn` reference. Absolute references must match the configured base.
  A reference with only a business identifier must specify the ServiceRequest
  type and have a trusted classification as a placer, filler or accession
  alias. The resource's `meta.versionId` is an opaque revision label. The
  application supplies the producer's explicit revision sequence.
  See [FHIR R4 DiagnosticReport](https://hl7.org/fhir/R4/diagnosticreport-definitions.html),
  [ServiceRequest](https://hl7.org/fhir/R4/servicerequest-definitions.html) and
  [FHIR references](https://hl7.org/fhir/R4/references.html).
- `OrderFromHL7` and `ResultFromHL7` read bounded v2.5.1 ER7
  `ORM^O01` new orders and `ORU^R01` results with one patient and one
  OBR. The caller supplies the trusted authority crosswalk and stable order or
  result identity. Results use the modeled OBR-25 states F, C and X
  for final, corrected and no result available, respectively. Locally
  defined OBR fields require a separately verified application mapping.
  The result sequence comes from the verified producer. MSH-10 identifies
  the delivery. See [HL7 ORC order identifiers](https://www.hl7.eu/HL7v2x/v251/std251/ch04.html#Heading37),
  [HL7 OBR-25 status](https://www.hl7.eu/HL7v2x/v251/std251/ch04.html#Heading99)
  and [HL7 ORU message structure](https://www.hl7.eu/HL7v2x/v251/std251/ch07.html#Heading43).

Both projections bind the exact admitted result bytes with a SHA-256 digest.
Supply the complete effective report revision and retain its original payload
according to the application's storage and access policy.

## Revision handling

A ledger keeps a limited index of revision metadata. Its lineage key combines
the authorized patient, the trusted producer, and the logical result identifier.
A later declared correction or withdrawal updates that entry while keeping
the same associated order. A lower sequence is stale. Replaying the same
sequence, revision label, status and digest returns the existing revision.
A conflicting replay or an attempt to move a result to another order fails.
Withdrawals retain a tombstone for ordering and cannot be implicitly reopened.

`Ledger.Apply` checks the patient and current eligible orders before each
application, including retries. The application owns payload storage, durable
publication and the identity grant lifecycle. The ledger limits each order to
16 identifiers, each request to 10,000 candidate orders and each in-memory
index to a caller-selected maximum of 100,000 result lineages.

## Regression coverage

```sh
go test -race ./clinicalresult -count=3
```

The cross-protocol regression links HL7 results to FHIR orders for two patients
with the same placer and filler numbers. A correction stays in the authorized
lineage, a delayed older correction keeps the current revision, and the second
patient's result is unchanged. Tests also exercise wrong namespaces, conflicting
demographics, origin and tenant checks, withdrawal and concurrent corrections.

# Explicit clinical audit messages

`net/audit` separates PHI-free DIMSE telemetry/legacy builders from an explicit
`ClinicalPolicy`. Its zero value authorizes neither patient/study identities
nor query datasets. `IncludeIdentities` and `IncludeQuery` must be enabled by
the caller. Rejected values, paths, remote error text and encoded datasets do
not appear in `ClinicalError`; it carries only a stable code and field path.

Typed builders cover Instances Transferred, Begin Transferring, Instances
Accessed, Study Deleted, Order Record, Patient Record, Procedure Record,
Import, Export, Audit Log Used and Query. `ValidateClinical` checks their
documented PS3.15 A.5.3 specialization, bounded inputs and XML profile. A
builder does not imply the host application implements the corresponding
order, procedure or media workflow. Source, destination and an optional known
third-party requestor remain distinct actors; unknown identities are refused,
never invented. This narrow profile requires a real Patient Name, and uses
Study Description or an explicitly present Study ID as Study Name; it does not
guess either from a UID. Query data is re-encoded in the actual negotiated
transfer syntax and base64-encoded only with explicit query authorization.
It represents the decoded logical dataset, not necessarily identical P-DATA
bytes.

## Aggregation and delivery

Association-scoped aggregation groups patient, study, action, outcome and the
explicit encrypted/anonymized properties, deduplicating SOP instances/classes.
`Finish` is explicit, retryable and idempotent after acceptance. A successful
emit means the caller accepted the event into its queue, not that an audit
repository received or durably retained it. The strict clinical syslog path
refuses a full spool without evicting older records; the legacy telemetry path
retains its documented drop-oldest behavior. TLS/repository trust, retention,
durable recovery and monitoring remain deployment policy.

Zero limits select finite defaults: 64 groups, 10,000 instances, 64 classes,
1024 objects, 1 MiB identity data, 1 MiB query data and 4 MiB XML per message.
Negative limits are invalid. Detached copies prevent a mutable caller or sink
from changing a retained event during retry. These are bounded association and
message contracts, not a process-wide storage policy.


## Validation boundary

The primary reference is [DICOM PS3.15 2026d A.5](https://dicom.nema.org/medical/dicom/2026d/output/chtml/part15/sect_A.5.html).
Pinned RNC SHA-256:
`25bcd0b0a6c6a95e642b64fbe3ef0237ad764dee83b36bc94cd73a1c10c791a0`.
An opt-in grammar test takes `DICOMGO_AUDIT_RNG` and verifies derived RNG hash
`bc69b0605907f81412c771f708313eb16c38ea13746d01f0cec6c1b8c65afa91`.
Task-local rnc2rng 2.7.0 required normalization of 71 comment markers only;
quoted strings and non-comment constraints were checked unchanged. This is
documented conversion evidence, not complete converter equivalence.

Eleven builder messages pass the derived grammar; negative grammar controls
and runtime specialization tests run separately. macOS xmllint/libxml2 2.9.13
accepted isolated invalid base64 in both Query and Detail despite the schema's
base64 datatype. Runtime validation rejects these inputs independently. Grammar
acceptance therefore does not prove base64 validity, A.5.3 semantics, IHE ATNA
certification or a production audit-repository connection. Local DIMSE tests
verify 100-store aggregation, actual query syntax/charset, correlated warning
outcomes and C-GET/C-MOVE participants; no hospital PACS is qualified by them.

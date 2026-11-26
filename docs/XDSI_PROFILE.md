# Optional XDS-I.b transaction clients

The `net/xdsi` package implements a synchronous, explicitly
configured subset of RAD-68 and RAD-69. It has no application-global registry,
listener or initialization effect. Importing another library package does not
enable these transactions.

## Declared profile

The RAD-68 client publishes one stable DICOM Part 10 KOS manifest for one study.
The manifest contains whole-image references from registered, non-retired image
SOP classes. The library reuses the SR KOS writer and sets the DCM Manifest
title, the DCMR TID 2010 identifier, and each referenced series' Retrieve AE Title
and Retrieve Location UID. References and the manifest have distinct SOP and
series identifiers. The imaging source UID is explicit.

The document repository receives a SOAP 1.2 MTOM/XOP
ProvideAndRegisterDocumentSetRequest. Its ebRIM submission contains one
DocumentEntry, one SubmissionSet and their Original HasMember association.
The document bytes, SHA-1 size/hash metadata, patient identifiers, required
coded metadata, clinical creation time and submission time are present.
An optional registered prior entry UUID generates an RPLC association.
A successful HTTP status is followed by examination of the registry response.
RAD-68 has success and failure results.

The RAD-69 consumer sends a SOAP 1.2 MTOM/XOP
RetrieveImagingDocumentSetRequest grouped by study and series, with the manifest's
source UID and a caller-supplied transfer-syntax list. It returns per-instance
successes and errors in manifest order. Every exposed instance has matching
Part 10/file-meta SOP identity, study, series, local patient/ISO issuer and an
advertised transfer syntax. The SHA-256 result describes the exact received
bytes. Transport success with a mismatched instance produces an instance error
and no bytes for that instance. Registry success, partial success and failure
remain separately visible.

The profile uses one imaging-source endpoint and explicit ISO OID patient
domains. It supports whole image instances. Report publication, cross-community
gateways, registry discovery, manifest download, asynchronous exchange, JPIP,
transcoding and server-side repository/source actors require separate
integrations. The supplied manifest remains usable with other DICOM APIs.

## Configuration and ownership

Construct a client with `NewClient(ClientConfig{...})`. Both repository and
imaging-source URLs must be explicit. HTTPS is required unless the caller sets
`AllowHTTP` for an explicitly accepted transport. Redirects are refused.
Supply an `http.Client` configured with the deployment's certificate trust
and credential-bearing transport. The package adds the transaction envelope
and content headers. Its default request timeout is 30 seconds when the
provided client has no timeout.

`Mapping` maps the local patient identity from the KOS into the affinity
domain. `PublicationMetadata.SourcePatient` must equal the KOS identity and
`PublicationMetadata.Patient` must equal the returned mapping. A bare
PatientID never supplies an assigning authority. `PublicationMetadata`
also supplies source/submission OIDs, codes, language and clinical timestamps.
Applications obtain those values from their integration agreement and source
clinical information. A displayed manifest creation time does not determine
the clinical creation time used by registry queries.

`Authorize` checks the declared transaction and copied SOP references
before I/O and before results are returned. A rejected check returns
`ErrAuthorization`. It can consult a revocable application policy
without importing an application session into the DICOM library.

RAD-68 additionally requires `HoldInstances`. The application verifies
that all referenced objects are retrievable, retains a stable source snapshot
and returns its release function. The client releases it on all outcomes.
The application is responsible for availability throughout the registered
manifest's lifetime. When withdrawing a subset, publish a new manifest with
the removed references omitted and `ReplacesEntryUUID` set to the previous
registered entry. All-instance removal and its registry operation require a
separate integration.

Neither transaction is retried automatically. A lost response, cancellation,
or authorization failure after transmission can leave a remote publication
outcome unknown. Reconcile with the repository before resubmitting a unique ID.

The manifest and retrieval result own their in-memory bytes. `Bytes` and
`References` return copies. No manifest URL is followed, no attachment is
written to disk and no log includes patient identifiers or peer response
bodies.

## Resource bounds

| Resource | Bound |
|---|---:|
| Image references in a manifest | 512 |
| Manifest Part 10 | 4 MiB |
| Inherited source metadata | 4 MiB, 4096 elements, 12 levels |
| SOAP XML | 2 MiB, 20,000 elements, 24 levels |
| XML attributes per element | 24 |
| Retrieved document, default | 64 MiB |
| Total reply wire bytes, default | 128 MiB |
| Parsed metadata per retrieved document, default | 4 MiB |
| Configured document or aggregate ceiling | 1 GiB |

`Limits` allows a deployment to lower or raise the last transport
budgets within the finite ceiling. Multipart part count is bounded by the
requested document count plus one SOAP part. Duplicate Content-IDs, unused
attachments, missing XOP targets, unsolicited results, unsupported required
SOAP headers and inconsistent registry results are rejected. XML directives,
external entities and excessive structure are refused. The byte budgets bound
resident attachment data. This profile does not stream attachments to a
consumer while their identity is still unverified.

## Local controlled-peer verification

From `dicom-go` on Windows amd64:

```sh
go test -p 2 ./net/xdsi -count=1
go test -p 2 -race ./net/xdsi -count=2
make check
```

`TestControlledPeerPublicationThenRetrieve` starts a TLS loopback peer
with a separate XML data model and literal SOAP/MTOM serializer. Its two small
CT objects use synthetic patient IDs and fixed study/series/SOP identifiers.
The peer validates the publication's registry patient mapping, local patient,
KOS evidence and routing, format code, attachment linkage, byte count and
SHA-1. It stores the manifest, then validates the RAD-69 hierarchy and syntax
list. The test reopens the stored manifest, retrieves the two instances and
compares their original bytes and SHA-256 digests.

Additional tests exercise partial success, all-instance failure, repository
rejection, wrong SOP/patient/source identities, missing mapping, preflight
authorization, post-receive revocation, lease release, malformed and
over-budget XML/MIME, missing/extra attachments, redirection, cancellation and
replacement associations.

The receiver rejects duplicate dataset attributes before constructing the
last-wins DICOM object map. A compressed metadata expansion is subject to the
same byte ceiling as native metadata. The tests exercise both refusals.

The loopback fixture is a controlled protocol regression. Qualification with
an independently implemented registry/repository and imaging source remains
a deployment step. Record its product/version, actor options, affinity-domain
codes and identities, certificate/authentication configuration, fixture hashes,
wire validation, registry queries, replacement visibility and per-instance
outcomes. Run the same positive/partial/rejection/mismatch cases against that
peer. These client transactions alone do not establish IHE actor conformance
or Connectathon qualification.

## Normative references

- [IHE Radiology TF revision 23.0, RAD-68 and RAD-69](https://www.ihe.net/uploadedFiles/Documents/Radiology/IHE_RAD_TF_Vol2.pdf)
- [ITI-41 publication message and response](https://profiles.ihe.net/ITI/TF/Volume2/ITI-41.html)
- [ITI-43 retrieval results](https://profiles.ihe.net/ITI/TF/Volume2/ITI-43.html)
- [Document-sharing metadata representation](https://profiles.ihe.net/ITI/TF/Volume3/ch-4.2.html)
- [Sender metadata requirements](https://profiles.ihe.net/ITI/TF/Volume3/ch-4.3.html)

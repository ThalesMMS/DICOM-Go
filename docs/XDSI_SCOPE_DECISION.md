# XDS-I.b optional package scope decision


The library already provides reusable DICOM object and network primitives.
XDS-I.b would introduce another protocol stack and a publication lifetime
contract. A standalone KOS file or a successful DICOMweb retrieve does not
demonstrate those behaviors. There is no XDS-I.b implementation or configured
XDS test-peer recipe in the current library. No affinity-domain mapping,
repository identity or external validation peer was supplied for this campaign.
Delivering an unqualified endpoint would make the capability statement less
useful than an explicit boundary.

## Verified existing building blocks

| Current source | Reusable behavior | Missing XDS-I.b behavior |
|---|---|---|
| [`sr/kos.go`](../sr/kos.go), [`sr/sr.go`](../sr/sr.go) | Generic KOS content and study/series/SOP evidence hierarchy | XDS manifest-specific validation and retrieval identities; the current evidence writer emits study, series and SOP references without Retrieve AE Title/Location UID |
| [`sr/kos_rejection.go`](../sr/kos_rejection.go), [rejection profile](KOS_IOCM.md) | Bounded single-study IOCM rejection notes | A publication manifest; rejection titles and archive rejection semantics cannot substitute for it |
| [`object/`](../object/), [`parser/`](../parser/) | Part 10 encoding/parsing, owned datasets and explicit resource budgets | Manifest metadata mapping to document sharing metadata |
| [`net/dicomweb/client.go`](../net/dicomweb/client.go), [`net/dimse/`](../net/dimse/) | DICOM instance retrieval and transport primitives | RAD-69 SOAP messages and XDS registry/repository transactions |

The inspection searched the protocol/SR/UPS Go sources for `SOAP`, `MTOM`,
`ebRIM`, `WADO-WS`, `XDS-I`, `RAD-68` and `RAD-69`. No implemented transaction
was found. Dictionary names and UPS identifiers are not protocol support.
This conclusion concerns the inspected source, not an interoperability test.

## Transaction scope

[RAD TF-2 revision 23.0, sections 4.68 and 4.69](https://www.ihe.net/uploadedFiles/Documents/Radiology/IHE_RAD_TF_Vol2.pdf)
defines two distinct paths. RAD-68 sends publication documents and metadata
from an Imaging Document Source to a Document Repository, which forwards
metadata to the registry. For a set of DICOM instances, the submitted document
is a KOS Part 10 manifest (`application/dicom`); the referenced instances stay
retrievable at the imaging source. Its evidence hierarchy needs retrieval
identities in addition to Study/Series/SOP UIDs. Published references becoming
unavailable require manifest lifecycle handling.

RAD-69 retrieves the referenced DICOM instances from an Imaging Document Source.
It extends ITI-43 with study/series identities and supported transfer syntaxes,
alongside repository/document identities. It is a SOAP 1.2 MTOM/XOP transaction,
not a renamed WADO-RS request. Gateway/community routing belongs to a separately
scoped XCA-I integration.

The underlying [ITI-41 transaction](https://profiles.ihe.net/ITI/TF/Volume2/ITI-41.html)
requires SOAP 1.2, MTOM/XOP and the IHE web-services conventions for requests
and responses. Its repository submission includes structured document-sharing
metadata, not just a binary KOS upload. A repository response incorporates
registry results; an HTTP success alone cannot establish publication success.

[ITI TF-3 section 4.2](https://profiles.ihe.net/ITI/TF/Volume3/ch-4.2.html)
defines the constrained ebRIM representation of DocumentEntry, SubmissionSet,
classifications and associations. The document and submission patient identities
must agree. The XDS patient identifier belongs to the affinity domain, whereas
sourcePatientId represents the local domain. Product policy must supply that
mapping and the domain's coded metadata; the library must not guess either
from a PatientID string or an AE title.

The inherited [ITI-43 response](https://profiles.ihe.net/ITI/TF/Volume2/ITI-43.html)
can express success, partial success and failure. A future retrieval adapter
must preserve per-document outcomes and validate each returned object's
identity and transfer syntax before import.

Implementing these two transactions alone would still require evaluation of
all required actor transactions, options and groupings before claiming the
full profile. [RAD TF-1 section 18](https://www.ihe.net/uploadedFiles/Documents/Radiology/IHE_RAD_TF_Vol1.pdf)
defines that actor-level scope and the dependence on XDS.b. This decision makes
no claim of IHE integration-profile conformance or Connectathon qualification.

## Architecture if implementation is resumed


Proposed runtime policy for that implementation: finite XML depth/elements,
MIME parts, manifest references and total/per-document byte budgets; streaming
attachments with bounded ownership; explicit allowed peer endpoints and caller
transport/authentication; no automatic dereference of manifest URLs or XML
external entities. Record failures without patient data in diagnostic logs.
These are design requirements, not controls already implemented for XDS-I.b.

## Conditions for reopening implementation

1. Choose source/consumer roles and options, synchronous/asynchronous support,
   and whether discovery, report publication or community routing is included.
2. Supply a test affinity domain with patient mapping, coded metadata,
   repository/source identities, endpoint/authentication configuration and a
   synthetic instance lifecycle. Keep credential values outside versioned files.
3. Select an independently implemented test registry/repository and an imaging
   source/consumer peer for RAD-69. Record versions, options and local execution
   commands; no CI pipeline is required.
4. Qualify manifest evidence/retrieval identities, metadata consistency,
   MTOM/XOP correlation, transfer-syntax negotiation and multi-document results
   with positive, partial, rejection, cancellation and resource-limit cases.
5. Demonstrate actual publication, registry lookup and manifest download, then
   RAD-69 retrieval of the referenced instances. Verify stored object identities
   and bytes/decoded pixels as appropriate, and prove replacement behavior when
   an instance becomes unavailable. Local SOAP mocks would be regression tests,
   not the independent registry/repository acceptance evidence.

## Validation of this decision


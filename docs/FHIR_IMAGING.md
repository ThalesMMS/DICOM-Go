# FHIR R4 imaging search

`net/fhir` projects DICOM study metadata to FHIR R4 `ImagingStudy` and
queries one explicitly configured FHIR service. Applications own the
relationship between a local DICOM patient and a FHIR patient resource.

## DICOM metadata projection

`MapImagingStudy` accepts parsed datasets for one study and requires the
application to provide a `Patient/<id>` subject. An optional
`Endpoint/<id>` reference can identify a separately configured endpoint.
These references are carried as data and are never opened by the library.
The mapper checks Study, Series and SOP Instance UIDs, refuses duplicate
instances, and checks for conflicting patient identifiers or series
modalities. Up to 100,000 instances can be mapped. An accession number is
included when its identifier system is explicitly configured.

DICOM Study Instance UID is encoded with the `urn:dicom:uid` identifier
system and `urn:oid:` value prefix. Series and instances are sorted by
their number when available and then by UID. Study fields come from the
first dataset and series fields from the first dataset supplied for each
series. If a dataset declares a valid time zone offset and at least
hour-and-minute precision, its offset is carried into `started`. Seconds
are zero-filled at minute precision as allowed by FHIR R4. An absent or
invalid offset leaves the date alone. The response decoder retains the
exact sender's date/time string instead of converting it to local time.
[FHIR R4 ImagingStudy](https://hl7.org/fhir/R4/imagingstudy.html)
· [FHIR R4 dateTime](https://hl7.org/fhir/R4/datatypes.html#dateTime)

## Bounded search and authorization

`NewClient` accepts a trusted service base such as
`https://ehr.example.test/r4/tenant-a`. `SearchImagingStudies` submits a
GET to `ImagingStudy` or a URL-encoded form POST to `ImagingStudy/_search`.
The client follows `Bundle.link` with relation `next` as GET and preserves
server-provided query text. Continuations must resolve to the same
configured origin, exact tenant base and `ImagingStudy` search path.
A DNS hostname uses its ASCII form, including A-labels for internationalized
names, consistently across the service base, token audience and next links.
A server responding to a POST should link to that typed GET path.
Encoded, ambiguous, or wider paths and redirects are refused before
forwarding credentials. The search returns matching ImagingStudy projection
records and leaves included resources and outcomes outside that result.
All pages must pass before the result is returned.

The defaults are 30 seconds per search, 32 pages, 8 MiB per response and
32 MiB across pages. Page admission occurs before requesting credentials
or making the next HTTP request. Repeated GET URLs and conflicting JSON
keys or pagination links are refused. JSON property names use ASCII.
HTTPS uses peer and hostname
verification. A caller can explicitly enable HTTP for numeric loopback
fixtures. Diagnostics omit response text, URLs and credentials.

`Authorize`, when set, supplies a fresh SMART grant before every request.
The grant audience must match the configured FHIR service base and tenant.
`CheckSMARTScopes` checks the granted context and resource operation,
including SMART v1 `read` and `write` grants and SMART v2 `cruds`
permissions. A resource-type POST to `_search` uses the search permission
`s`. A POST to the resource type itself uses create permission `c`.
Grant filters, patient compartment enforcement, OAuth discovery, token
verification and refresh belong to the integrating application.
[FHIR R4 Search](https://hl7.org/fhir/R4/search.html)
· [SMART App Launch scopes](https://hl7.org/fhir/smart-app-launch/STU2.2/scopes-and-launch-context.html)

## Verification

```sh
go test -race ./net/fhir -count=3
go vet ./net/fhir
```

The tests use synthetic DICOM metadata and HTTP and TLS loopback peers.
They cover offsets, hierarchy identity, the exact page cap, transport
limits, tenant and origin refusal, SMART search and create scopes, and
rejection before a continuation can transmit credentials. The package
supports the R4 JSON search profile described here. It does not claim
general FHIR schema conformance or qualification against a deployed EHR.

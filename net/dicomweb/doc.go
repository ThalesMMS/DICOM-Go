// Package dicomweb provides neutral DICOMweb client and embeddable server
// primitives for QIDO-RS, WADO-RS, STOW-RS, and UPS-RS (workitems with a
// WebSocket event channel) endpoints.
//
// The package owns reusable request construction, response parsing, typed
// HTTP/DICOMweb errors, and bearer-token injection. TokenManager provides an
// in-memory access-token cache with serialized refresh and one safe retry after
// a challenged replayable request. The optional subpackage net/dicomweb/oidc
// adds OpenID Connect discovery, native-app logins, refresh-token exchange and
// JWT/JWKS verification; this package does not import it. Applications remain
// responsible for endpoint profiles, OS-protected credential persistence,
// authorization policy, jobs, archive import/export, and user-facing state.
//
// Server is deny-by-default: callers must provide an Authorizer or explicitly
// set AllowUnauthenticated. It supplies bounded routing, DICOM JSON/XML response
// validation, multipart streaming, injected rendered/thumbnail resources,
// complete-request Part 10 or metadata-plus-bulk STOW staging, graceful
// shutdown, and PHI-free audit events.
// Storage identity, duplicate policy, and rendered resource lookup are backend
// responsibilities. See docs/DICOMWEB_SERVER.md for the implemented PS3.18
// profile and intentional limitations.
package dicomweb

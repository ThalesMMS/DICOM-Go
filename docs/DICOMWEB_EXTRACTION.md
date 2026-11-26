# DICOMweb ownership boundary

The Twin adapter delegates reusable request/response mechanics to
`net/dicomweb`:

- QIDO-RS study search, WADO-RS metadata/retrieve, and STOW-RS store client
  calls live in `dicom-go/net/dicomweb`.
- QIDO-RS study criteria, return fields, and DICOM JSON study-match extraction
  live in `net/dicomweb`.
- Dynamic bearer-token injection, in-memory access-token lifecycle, serialized
  refresh, redaction, and one retry of a challenged replayable request live in
  `net/dicomweb`. Streaming STOW-RS bodies are never replayed.
- OpenID Connect discovery, the authorization-code (PKCE, loopback) and
  device logins, refresh, revocation, UserInfo subject binding and ID-token
  JWT/JWKS verification live in the optional `net/dicomweb/oidc` package.
- DICOM JSON value extraction lives in `dicom-go/dicomjson`.

Twin entry points that remain thin or application-owned are:


Reusable QIDO-RS, WADO-RS and STOW-RS request/response helpers and bearer-token
application belong in `dicom-go/net/dicomweb`; OpenID Connect protocol
helpers belong in `dicom-go/net/dicomweb/oidc`. Application endpoint
profiles, persistent credential storage,
authorization policy, jobs, archive import/export, receiver policy, operation
history, UI summaries and auto-query remain Twin/application responsibilities.

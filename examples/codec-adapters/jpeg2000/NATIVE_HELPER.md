# Persistent OpenJPEG / OpenJPH sessions

`OpenNativeSession` is an explicitly owned, optional decoder for the qualified
unsigned 8/16-bit, one/three-component fixture profile. A session keeps one native
worker and its stdin/stdout pipes alive across frames. It implements `Decoder`
and `ContextDecoder`; wrap it with `NewWithDecoder` to obtain a pixel-data codec.
The pure-Go default and existing CLI constructors keep their existing selection.
No runtime is found, downloaded, compiled or registered by opening this profile.

```go
session, err := jpeg2000.OpenNativeSession(ctx, jpeg2000.NativeSessionConfig{
    Executable: packagedHelperPath,
    Backend: jpeg2000.NativeOpenJPEG, // NativeOpenJPH for HTJ2K
    Timeout: 10 * time.Second,
})
if err != nil { return err }
defer session.Close() // the owner must outlive every registered consumer
codec := jpeg2000.NewWithDecoder(session)
if err := registry.RegisterCodec(transfer.JPEG2000LosslessOnly.UID, codec); err != nil {
    return err
}
```

Use an OpenJPEG session for `.90/.91` and an OpenJPH session for
`.201/.202/.203`. `RegisterClinicalWithNativeSessions` scopes the helper to
qualified full-resolution unsigned samples with matching coded and stored
precision and retains the CLI for other profiles. The owner closes the
sessions when the consumer is released. Twin can exercise this route with an
explicit development session.
Packaged `codecfull` continues to use the audited CLI adapters.

## Ownership, limits and cancellation

- One call runs at a time in each session; independent sessions may run in
  parallel. Queue waiting, admission, startup/handshake and pipe I/O obey the
  caller deadline or the configured timeout, whichever is tighter.
- Each input and output is limited to 64 MiB. Lower limits can be configured;
  higher limits are refused. Metadata and codestream headers are checked before
  sending. The helper independently checks dimensions and output-size factors
  before multiplication/allocation. Replies are size/ID/geometry checked before
  delivery.
- `MaxNativeBytes` optionally requires the helper to install its operating-system
  limit before handshaking. Windows caps process commit through a job object
  and enforces the maximum working set. Unix applies an address-space limit.
  A helper that cannot install the limit refuses the session.
- `Admit` may reserve encoded + decoded IPC bytes. Its release is called after
  any admitted result, including errors. Budget the resident process separately.
- Cancellation or timeout kills and collects the owned child before returning;
  the supervised pipe goroutine is also joined, including blocked writes. Unix
  workers have an owned process group. Windows helpers run hidden and do not
  spawn children. A canceled reply is discarded. The next call starts and
  verifies a fresh worker. A queued cancellation does not kill the active call.
- Process/protocol failures consume a bounded lifetime restart budget (two
  restarts by default; a negative `MaxRestarts` disables restarts). Native
  malformed/unsupported/size errors fail the call without consuming this
  budget. There is no automatic CLI fallback.
- The handshake verifies protocol 1, the requested engine and exact runtime
  version. Outputs are owned little-endian interleaved bytes; the borrowed
  codestream is not modified or retained. `Close` is idempotent. `Stats` reports
  the live PID, process starts, delivered frames and failure count for accounting.

## Build and package explicitly

The project-authored [helper](helperc/native_helper.cpp) only calls public SDK
APIs. Use OpenJPEG **2.5.4** and OpenJPH **0.31.0**, with OpenJPH source pinned to
`c68064d0e4cad8e96bab9a068f6cc4e7799744fc`. Keep upstream sources/build output
outside this repository. Follow the existing
[native dependency policy](../../../docs/CODEC_DEPENDENCY_POLICY.md) and ship
both upstream BSD notices with packaged native runtimes. No native binaries or
upstream codec implementation are added to this module.

On Windows with UCRT64 OpenJPEG 2.5.4 and a separately installed pinned OpenJPH
SDK (static library built with TIFF support off), run from the repository root:

```powershell
$env:PATH = 'C:\msys64\ucrt64\bin;' + $env:PATH
$sdk = 'C:\path\to\openjph\install'
g++ -O2 -std=c++17 -static-libgcc -static-libstdc++ `
  -IC:/msys64/ucrt64/include/openjpeg-2.5 -I "$sdk\include" `
  dicom-go/examples/codec-adapters/jpeg2000/helperc/native_helper.cpp `
  -L "$sdk\lib" -lopenjph -lopenjp2 -o C:\path\to\j2k-helper.exe
```

For Unix, the corresponding SDK include/library flags can come from
`pkg-config --cflags --libs libopenjp2 openjph`; use the same version/source pins
and package the runtime libraries with the helper. The helper supports binary
standard pipes on both paths, but the recorded native qualification here is
Windows amd64 only. A cross-build is not macOS runtime evidence.

From **this nested module**, default offline lifecycle tests need no SDK:

```sh
go test -race . -run '^TestNativeSession' -count=1
```

For native qualification, set `DICOM_GO_J2K_NATIVE_HELPER` to the packaged helper
path and run `go test . -run '^TestNativeHelperQualifiedFixtures$' -count=1`.
That gate checks all 12 existing OpenJPEG/OpenJPH fixture oracles three times,
including RPCL, tiled RGB, 12/16-bit and irreversible reconstructions.

For paired CLI/helper measurement, additionally set explicit
`DICOM_GO_OPENJPEG_DECOMPRESS`, `DICOM_GO_OPENJPH_EXPAND` and an output path in
`DICOM_GO_J2K_COST_REPORT`, then run:

```sh
go test -tags codecfull . -run '^TestNativeHelperFrameCost$' -count=1 -v
```


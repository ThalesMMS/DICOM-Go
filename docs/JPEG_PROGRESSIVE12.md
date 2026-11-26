# Legacy JPEG progressive 12-bit decoding

The default explicit `pixeldata/jpeg.Register` decoder registry reads a
restricted legacy SOF2 Huffman profile of retired Spectral Selection `.53`
(Process 8) and Full Progression `.55` (Process 12). No progressive encoder
is registered. Existing 8-bit decoding remains on Go `image/jpeg`, including
its receiver tolerance for successive approximation under `.53`.

## Qualified profile

12-bit input requires BitsAllocated 16, BitsStored 12, HighBit 11 and unsigned
samples. MONOCHROME1/2 has one component and absent PlanarConfiguration; RGB
has three components and explicit PlanarConfiguration 0. Every component uses
1x1 sampling (4:4:4). Output consists of owned interleaved little-endian samples;
MONOCHROME1 affects presentation only. The color codestream must identify raw
RGB through Adobe APP14 transform 0 or component IDs R/G/B without JFIF.
YCbCr and `JPEGColorAlways` are outside this 12-bit profile and return typed
unsupported-photometric errors. Guess, Never and Photometric retain RGB.

`.53` requires Ah=Al=0 in every scan. `.55` accepts spectral selection and
successive approximation, with separate DC and AC initial/refinement paths,
EOB runs and numbered restart intervals. Scans may select one or more DC
components, in frame order; every AC scan selects one component. Each
component's first DC scan precedes its AC scans. Bands may follow any order,
but first passes cannot overlap and refinements must follow the previous Al,
decreasing it by exactly one. Ah and Al are limited to 0..13. This profile
requires first-pass coverage of all 64 coefficients and final Al=0 for each
component at EOI. Incomplete approximations, even if useful as JPEG previews,
are explicitly outside the qualified archival profile.

Quantization tables used by a component cannot change during progression.
Huffman tables may be redefined between scans. The decoder validates table
selectors, canonical Huffman trees, coefficient categories/runs, sampling,
dimensions, IDs, scan order, entropy padding, EOB boundaries, restarts, EOI and
trailing bytes. A single DICOM Item pad byte is allowed. Sequential scan-field
recovery never bypasses progressive validation. Arithmetic, hierarchy, signed
samples, reduced stored precision and subsampling remain refused.

## Resources and cancellation

Admission accounts for retained native frames, compressed source, the largest
joined codestream, frame/plan headers, marker/table scratch, padded sample
planes and retained int32 coefficient blocks. The combined request is limited
to 512 MiB and the host integer range before allocations. Work is limited to 256 scans per frame and 8,388,608 block visits per
request, with one shared budget across every decoded frame; `jpeg.ErrProgressiveResourceLimit`
is wrapped by `jpeg.ErrInvalidFragment` at the public decoder boundary.
Cancellation is checked during markers, entropy traversal, allocations, MCU
loops, IDCT and output assembly. Failed or canceled calls publish no frames.

## Independent synthetic qualification

The [committed corpus and receipt](../pixeldata/jpeg/testdata/progressive12/manifest.json)
contains 16 cases, each with two distinct 41x37 frames: unsigned mono/RGB,
spectral/full progression, gradients, flat bands, high-frequency edges and
restart streams. Source rasters and Part10/PNM inputs are project-owned,
deterministic and PHI-free. Vendor implementation source and image assets were
not used. Project fixtures may be redistributed under the project license.

On Windows/amd64, Go 1.26.4, DCMTK 3.7.0 `dcmcjpeg +es/+ep +bt +sr +cr`
generated the ordinary streams. Own spectral scan scripts and libjpeg-turbo
3.2.0 `cjpeg -precision 12 -restart 3B` generated restart cases. Every stream
was independently decoded by DCMTK `dcmdjpeg`/IJG12, preserving 12-bit stored
metadata and raw RGB with `+cn +px -bs +te`. The receipt records tool/runtime
versions and hashes, source hashes, exact commands, fixture hashes, metadata,
per-component source PSNR and errors. Expected `.raw` files come exclusively
from that independent decoder. All 32 frames differed by at most one stored
level; flat bands were exact. Default tests enforce a two-level IDCT tolerance.

`TestProgressive12IndependentFixturesAndCallers` also checks decompression,
transcoding to native Explicit VR Little Endian and grayscale/RGB16 display.
Malformed sequences, metadata, limits, deterministic real cancellation at
every observed checkpoint and concurrent reuse of one codec have separate
regressions. The preserved 8-bit corpus is tested independently.

Run from the library module, with the native tools on PATH:

```sh
GOMAXPROCS=2 go test -p 1 ./pixeldata/jpeg -run '^TestProgressive' -count=1
DICOMGO_JPEG_PROGRESSIVE12_INTEROP=1 \
  DICOMGO_JPEG_PROGRESSIVE12_EVIDENCE_DIR=/absolute/isolated/output \
  GOMAXPROCS=2 go test -p 1 ./pixeldata/jpeg \
  -run '^TestProgressive12DCMTKQualification$' -count=1 -v
```

Explicit `DICOMGO_JPEG_PROGRESSIVE12_EXPORT_DIR` additionally writes regenerated
`.jpg`, independent `.raw` files and `manifest.json` into that selected directory.
Without it, the interop test only writes its isolated evidence directory.
Regeneration changes DCMTK-generated SOP UIDs/Part10 hashes; raster and JPEG
sample qualification remains deterministic. A failed interop run replaces its
evidence receipt with `Successful=false` and never publishes a successful manifest.

The clean-room derivation uses [ITU-T T.81 Annexes B, C, E and G](https://www.w3.org/Graphics/JPEG/itu-t81.pdf).
The retired process identities are listed in the [DICOM UID registry](https://dicom.nema.org/medical/dicom/current/output/chtml/part06/chapter_A.html).
The subset does not claim other progressive profiles or clinical interoperability.


The main checkout was validated on Windows 11/amd64 with Go 1.26.4 and the
UCRT64 GCC toolchain. Complete tests for `pixeldata/jpeg`, `pixeldata/codecprofile`,
`pixeldata` and `pixeldata/display` passed. Regenerating all 32 progressive
12-bit frames against DCMTK passed with a maximum sample difference of one;
the existing baseline, extended 12-bit and progressive 8-bit interoperability
regressions also passed. The cancellation/concurrent-use and shared-request
work-budget regressions passed under the race detector. Twin's supported-JPEG
study-loader regressions passed, and both native frontend binaries built.

`make check` was run in the library: formatting and vet passed, but the complete
test baseline remains unsuccessful because of existing Windows permission,
filename, file-lock, socket-buffer, unrelated mesh/registration/render assertions
and timing failures. The affected JPEG packages passed within that run.
The remaining `make build docs-check codec-policy-check codec-optional-check
codec-conformance-check codec-manifest-check` gates passed separately, as did
Twin's native build and release-compliance checks. An attempted Windows/386
test is blocked by existing integer-range compilation errors in
`validation/write_limits.go`; no 386 functional qualification is claimed.
This validation does not include a macOS run, native GUI interaction or an
external PACS.

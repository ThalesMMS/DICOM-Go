# JPEG-LS HP compatibility

`DecodeRecovery.JPEGLSHPColorTransforms` explicitly admits the nonstandard
APP8 `mrfx` HP1, HP2 and HP3 transforms. Its zero value refuses them. This is
compatibility with legacy files, not a new standard DICOM color interpretation
or a claim of universal JPEG-LS conformance. Standard encoding is unchanged.
[DICOM PS3.5 8.2.3](https://dicom.nema.org/medical/dicom/current/output/chtml/part05/sect_8.2.3.html)
does not define a JPEG-LS-specific color transformation Photometric
Interpretation. The [CharLS public API](https://github.com/team-charls/charls)
provides the HP extension identifiers; no vendor implementation is copied.

## Qualified profile

- JPEG-LS Lossless `.80`, NEAR=0; unsigned RGB, three components.
- BitsAllocated=BitsStored=8 or 16, HighBit=BitsStored-1,
  PlanarConfiguration=0, matching codestream precision and full-range MAXVAL.
- One complete line- or sample-interleaved scan (ILV=1 or 2), with the existing
  JPEG-LS framing, dimensions and resource limits.
- Exactly one valid APP8 `mrfx` declaration before the first scan. HP0 remains
  a no-transform declaration. Unknown, duplicate, misplaced or
  malformed HP declarations are refused.

Near-Lossless `.81`, NEAR>0, component-interleaved HP frames, intermediate
precisions, signed/YBR/palette data and custom reduced MAXVAL are unqualified
and refused. Restart markers and other extensions are outside this profile.
The pure-Go implementation checks cancellation during inverse transformation;
the existing CharLS limitation remains: one native frame cannot be interrupted
inside the native call.

Use a caller-owned registry and pass
`DecompressOptions{Recovery: pixeldata.DecodeRecovery{JPEGLSHPColorTransforms: true}}`
to decompression, or carry that recovery through
`pixeldata.DecodeContextWithRecovery` for direct codec calls. Both the pure-Go
decoder and the optional CharLS lossless adapter report
`jpeg-ls-hp1-color-transform`, `jpeg-ls-hp2-color-transform`, or
`jpeg-ls-hp3-color-transform`, together with `jpeg-ls-hp-backend-pure-go` or
`jpeg-ls-hp-backend-charls` to identify the implementation actually used.
An injected third-party decoder is not labeled as CharLS.
The optional direct backend receives the policy
through `DecoderInput.AllowHPColorTransforms`. InspectCodestream also honors
the explicit context policy and reports HPColorTransform; strict inspection
still refuses HP1/HP2/HP3. All output remains canonical sample-interleaved,
little-endian RGB. Codec routing continues to report its selected backend.

## Equations and independent qualification

Let `N=2^precision`, `H=N/2`, `Q=N/4`; reduce channel results modulo N.
The equations were re-derived from independent synthetic encoder/decoder
observations and checked on every fixture sample, without porting vendor code.

| Transform | Inverse from encoded channels `(x,y,z)` |
|---|---|
| HP1 | `G=y; R=x+G-H; B=z+G-H` |
| HP2 | `G=y; R=(x+G-H) mod N; B=z+floor((R+G)/2)-H` |
| HP3 | `G=(x-floor((y+z)/4)+Q) mod N; R=z+G-H; B=y+G-H` |

`pixeldata/codecfixture/testdata/codecs/jpegls-hp/generate.cpp` is a project-owned
generator using only the installed public GDCM CharLS API. It produces RGB
extremes, half-range boundaries and deterministic textured samples in 33x17
images. It independently encodes and decodes every stream, requiring exact
agreement with the original synthetic RGB arrays. The fixture directory
contains no PHI and no imported vendor images or implementation sources.


```powershell
go test ./pixeldata/jpegls -run '^TestHPColorTransforms' -count=1
$env:DICOMGO_JPEGLS_HP_GDCM = '1'
go test ./pixeldata/jpegls -run '^TestHPColorTransformsGDCM$' -count=1 -v
# From examples/codec-adapters/jpegls, with a qualified CharLS library selected:
go test -tags jpegls_charls ./... -run '^TestCharLSHPColorTransforms$' -count=1 -v
```

Regenerate only into a separate candidate directory, then compare each hash
against `generation.json`; generation never establishes qualification alone.
The generator can be compiled with MSYS2 g++ using the GDCM include directory
and `-lgdcmcharls`, without changing the dependency-free module.

# Pure-Go JPEG-LS Near-Lossless decoding

The builtin registry includes `.81` (`1.2.840.10008.1.2.4.81`) through
`jpegls.RegisterNearLossless`. Callers can also use `jpegls.NewNearLossless()`.
The existing `New`, zero `Codec`, `Register` and `.80` binding remain strictly
lossless and reject NEAR>0. Encoding is a separate explicit API documented in
[Near-Lossless encoder policy and evidence](JPEGLS_NEAR_LOSSLESS_ENCODER.md);
no encoder is enabled implicitly.
`codecfull` still deliberately selects its optional CharLS adapter; callers
may explicitly select that backend instead of the base pure-Go decoder.

Supported metadata: MONOCHROME1/MONOCHROME2 with one component and ILV=0,
unsigned or signed, or unsigned RGB with three components and ILV=0/1/2; 2-16 stored bits
in 8/16 allocated bits; HighBit=BitsStored-1; matching stream precision;
PlanarConfiguration=0 for RGB. Every scan in a frame uses the same NEAR/ILV.
MAXVAL must be the full precision range. NEAR is validated against
0..min(255, MAXVAL/2). LSE ID=1 defaults are resolved only after SOS supplies
NEAR; thresholds must be ordered and at least NEAR+1. Default and explicit
thresholds are accepted; the independently measured custom profiles use RESET=64.

Palette color under `.81`, YBR_FULL, restart markers, transforms, mapping
tables and custom MAXVAL are explicitly rejected.

Signed `.81` (Pixel Representation 1) decodes as signed `.80` does: the
codestream holds the BitsStored-wide unsigned codes (two's complement
patterns), the decoder returns them, and Pixel Representation interprets them
afterwards, as DCMTK/CharLS and pydicom do. NEAR bounds the error of the
unsigned codes, not of the signed values: at the code midpoint 2^(BitsStored-1)
(the most positive value next to the most negative one) a code error of NEAR
or less can change the sign and the value by nearly the full range. Away from
it the signed values keep the bound. gdcmData `D_CLUNIE_CT1_JLSN.dcm` (signed
16-bit CT, NEAR=4) decodes to GDCM's MD5 and to DCMTK `dcmdjpls` exactly, and
within 4 of the lossless `D_CLUNIE_CT1_RLE.dcm` of the same image. The
encoder still refuses signed `.81`. See [DICOM PS3.5 2026c, 8.2.3](https://dicom.nema.org/medical/dicom/2026c/output/chtml/part05/sect_8.2.3.html)
and [pydicom's signed pixel explanation](https://pydicom.github.io/pydicom/stable/guides/encoding/jpeg_ls.html#pixel-representation).

## Algorithm and evidence

The existing LOCO-I implementation is extended, following
[T.87 Annexes A-C](https://www.itu.int/rec/T-REC-T.87-199806-I/en): NEAR gradient
quantization, quantized RANGE, bias adaptation in reconstructed units,
lossless-only special mapping, dequantization and wrap/clamp reconstruction.
Scalar and interleaved decoding share the same state and run-interruption logic.
The scan, Item/frame assembly, output validation, 512 MiB request/working-set
limits and cancellation paths are reused. No native dependency enters the core.


Two distinct checks are required: every reconstructed sample must equal the
independent CharLS result **exactly**; separately, every reconstructed sample
must differ from the original unsigned sample by at most NEAR. NEAR is never
used as tolerance between decoders. `sourceSamples` records the original raw
path/hash and the normative bound independently of `reconstruction`. The
source-bound check makes no PSNR, spatial-quality or clinical-quality claim.


Provenance: the in-tree algorithm was extended without copied external code.
The [CharLS 2.4.2 tool](https://github.com/team-charls/charls/tree/36dd3307e070d8fbc765c3ba890b7e681046fa39)
is BSD-3-Clause. The generated data are project-authored, MIT and contain no PHI.
`jpegls-near/generation.json` records the runtime binary hash. Native binaries
and downloaded source remain outside the repository.

## Local reproduction

```sh
python scripts/generate-jpegls-interleave.py --near-lossless --charls /path/to/CharLS-2.4.2-library --output /tmp/near-candidates
go test ./pixeldata/... -count=1
go test -race ./pixeldata/jpegls ./pixeldata/codecfixture ./pixeldata/builtin -count=1
go test ./pixeldata/jpegls -run '^$' -fuzz '^FuzzDecodeNearLossless$' -fuzztime=15s -parallel=2
make check
```

The shared generator keeps its original lossless mode. Generation writes
candidates; hash review remains explicit. Default tests use only committed
fixtures. Negative coverage includes UID/NEAR mismatch, mixed scans, invalid
LSE/bounds, unsupported signed/photometric profiles, precision/geometry conflicts,
truncation, request limits, deadlines, in-frame cancellation, concurrent reuse
and no partial output after a later frame fails.

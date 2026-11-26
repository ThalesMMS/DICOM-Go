# Authored GDCM quirk regression corpus


## Scan contract

Run from this module:

```sh
go test -race ./internal/quirkcorpus -run '^TestGDCMQuirk' -count=1
go run ./cmd/quirk-scan -dir testdata/quirks > quirk-results.json
```

`quirk-scan` exits nonzero on an unexpected stage, metadata mismatch, changed
carrier or admission failure. An expected rejection is a passing regression,
not evidence of image support. Its JSON records strict and explicitly opted-in
recover modes separately, source SHA-256, independently checked source metadata,
decoded pixel SHA-256, rendered RGBA SHA-256, diagnostics and applied recoveries.
The scan registers the JPEG Baseline codec in a local registry; rejection tests
cannot pass just because a codec is absent.

Before parsing, the scanner reads at most 64 KiB plus one sentinel byte, verifies
the exact authored carrier and parses that owned snapshot. Parser budgets are
1 MiB total, 64 KiB per element/Pixel Data, 256 elements, 32 fragments and eight
sequence levels. Decompression permits four frames, 4,096 pixels, 64 KiB native
and input bytes, and expansion ratio 256. Each CLI case has a ten-second context.
Canceled scans fail before input admission. Files declaring hundreds of MiB or
thousands of frames are rejected before those payload allocations/codec work.

The four stages are independent:

- **Parse:** complete read, error-labelled partial prefix, or rejection.
- **Preserve:** retained dataset and File Meta semantics stay unchanged across
  decoding/recovery. Private blobs, original VRs, sequence items, overlay data,
  group lengths, BOT and fragment boundaries are compared. The original carrier
  also stays unchanged; rejected parses preserve only the source file.
- **Decode:** bounded decoded samples and decoded layout match the authored
  oracle. Successfully parsing a damaged image does not qualify decoding.
- **Render:** dimensions and all RGBA bytes agree with an independent DICOM
  LINEAR 128/256 window oracle. This is CPU **2D** rendering, not application 3D.

Metadata validation independently checks Rows/Columns, Number of Frames, samples,
photometric, allocated/stored bits, high bit, signedness presence/value, Pixel
Spacing, orientation, position, dataset/File Meta SOP classes and overlay layout.
Nine canaries change dimensions, geometry, photometric, bit depth, signedness or
File Meta while keeping exactly the same Pixel Data hash; all must fail.
Partial native prefixes cannot be written: the test requires `ErrPartialFile`
and zero destination bytes. No missing samples are filled.

## Expected results

Notation is `parse / preserve / decode / render`. `source` means only the
original carrier is retained; `-` means that a later stage was not run.

| Authored case | Strict | Recover | Explicit policy / limitation |
|---|---|---|---|
| PMS-IncompletePixelData | reject / source / - / - | partial / partial / - / - | Terminal native truncation; inspection prefix only |
| GEMS-IncompletePixelData | reject / source / - / - | reject / source / - / - | First fragment incomplete; no complete fragment to retain |
| SIEMENS-IncompletePixelData | reject / source / - / - | partial / partial / - / - | Odd terminal native prefix; inspection only |
| SIEMENS-JPEG-CorruptFrag | ok / ok / reject / - | same | Truncated JPEG codestream |
| GENESIS_SIGNA-JPEG-CorruptFrag | ok / ok / reject / - | same | Corrupt JPEG table length |
| ACUSON-8-YBR_FULL-JPEG-TrailingInfo | ok / ok / reject / - | same | Non-padding bytes after EOI are preserved but refused |
| GE_JPEG-Broken | ok / ok / reject / - | same | SOI/EOI without an image frame |
| GDCMFakeJPEG | ok / ok / reject / - | ok / ok / ok / ok | `NativePixelDataUnderEncapsulated`, exact native size |
| Bug_Philips_ItemTag_3F3F | reject / source / - / - | same | Bad item tag (3F3F,3F00)/length 63; no guessed magic repair |
| BogusItemStartItemEnd | reject / source / - / - | ok / ok / ok / ok | `SequenceRecovery.ReplaceWrongDelimiter` |
| ItemTerminatorVLnot0 | reject / source / - / - | ok / ok / ok / ok | `RecoverDelimiterAnomalies`; diagnosed ignored length |
| EmptyItemStarter | ok / ok / ok / ok | same | Empty item followed by a populated item; both retained |
| MismatchSOPClassUID | ok / ok / ok / ok | same | Conflicting classes retained and checked; no IOD claim |
| VRUNInMetaHeader | ok / ok / ok / ok | same | File Meta UN/raw UID retained; not silently rewritten |
| ImplicitDeclaredAsExplicit | reject / source / - / - | ok / ok / ok / ok | Dedicated transfer-syntax probe/recovery |
| PhilipsByteSwapping | ok / ok / mismatch / - | ok / ok / ok / ok | `NativeOWByteSwap`, producer-specific, never inferred |
| PhilipsWith15Overlays | ok / ok / ok / ok | same | Fifteen authored overlay groups, unchanged base pixels |
| OverlayPrivateDataInBetween | ok / ok / ok / ok | same | Private data interleaved with overlay attributes |
| YBRisGray | ok / ok / ok / reject | same | One-sample YBR declaration; no guessed photometric rewrite |
| ElemZeroNotGroupLength | ok / ok / ok / ok | same | Explicit LO at element zero retained; not schema validation |
| FLAIR-wrong-group-length | ok / ok / ok / ok | same | Wrong UL group length retained, never trusted as a boundary |
| RMI_Mattes_unevenLengthTags | reject / source / - / - | ok / ok / ok / ok | `OddLengthPolicy = AcceptOddLength` |
| CR-Preamble-NoMeta | reject / source / - / - | ok / ok / ok / ok | Dedicated missing-File-Meta syntax recovery |
| MissingPixelRepresentation | ok / ok / reject / - | same | Signedness absence checked; never assumed unsigned |
| TestVRSQUN1 | ok / ok / ok / ok | same | Defined UN opaque in strict; `RecoverUN` builds known SQ clone |
| TestVRSQUN2 | ok / ok / ok / ok | same | Undefined UN implicit item parsing, nested UID checked |
| SinglePrecisionSC | ok / ok / reject / - | same | Float Pixel Data retained; integer-frame API refused |
| US_512x512x2496_JPEG_BaseLine_Process_1 | ok / ok / reject / - | same | Small carrier declares large dimensions/frame count; decode budget |
| i32.XADC.7.215MegaBytes | reject / source / - / - | same | Small carrier declares 215 MiB Pixel Data; parser byte budget |

All ordinary image cases use authored 2×2 samples `[0,64,128,255]`, unsigned
8-bit MONOCHROME2, spacing `[1.5,2]`, orientation `[1,0,0,0,1,0]` and position
`[5,7,9]`. Overrides are explicit in the manifest: neutral JPEG RGB samples are
all 64, byte swapping has a 16-bit LE sample oracle, float values are
`[0,0.25,0.5,1]`, and the frame-budget case declares 512×512×2496. Synthetic
patient/UID fields identify this corpus; these deliberately malformed objects
are not full clinical IOD examples. A valid JPEG control, made by removing only
the authored trailer, must decode and render the independent gray oracle.

To regenerate in a fresh directory:

```sh
go run ./cmd/quirk-scan -generate -dir quirk-output
```

Generation refuses to replace an existing different file. The regression test
requires the committed manifest, carriers and authored expectations to agree.

## Independent Windows evidence


The separately installed MSYS2 GDCM **3.2.5** was exercised through the own
[`gdcm_oracle.cpp`](../testdata/quirks/gdcm_oracle.cpp) harness and
[`verify_gdcm.py`](../testdata/quirks/verify_gdcm.py) bounded process driver. The
[recorded results](../testdata/quirks/gdcm-3.2.5-windows.json) contain 20 decoded
cases, two decoder refusals, three image-reader refusals, two native process
failures and two workloads deliberately not run because of declared budgets.
Of the 20 decoded cases, **15** match authored samples, stored layout/semantics
and stored geometry together. This is evidence for these tiny carriers only.

The differences remain explicit. GDCM fills missing native tails for the two
partial-prefix fixtures, leaves the unknown byte order different, and assumes
or changes metadata for one-sample YBR and absent signedness. GDCM accepts the
JPEG trailer that this library currently refuses. GDCM's SC ImageReader uses
default image spacing/origin even when the stored tags match, so the harness
records **stored-tag geometry and image geometry separately**. Neither a pixel
hash nor stored geometry qualifies its default image geometry as equivalent.
The two process failures are not counted as successful reads or support.

Reproduction with the actual Windows installation:

```powershell
$env:PATH = 'C:\msys64\ucrt64\bin;' + $env:PATH
g++ -std=c++17 -O2 -IC:/msys64/ucrt64/include/gdcm-3.2 -LC:/msys64/ucrt64/lib testdata/quirks/gdcm_oracle.cpp -o "$env:TEMP/gdcm-quirk-oracle.exe" -lgdcmMSFF -lgdcmDSED -lgdcmCommon
python testdata/quirks/verify_gdcm.py --oracle "$env:TEMP/gdcm-quirk-oracle.exe" --output "$env:TEMP/gdcm-quirk-results.json"
```

`make -k check` was run on the library: formatting, vet, build, documentation,
codec policy, optional adapters, conformance and codec manifest passed. The
test baseline remains red: the first run repeated the **33** pre-existing
Windows failure identities; the final run reported **32** of those identities
(the existing interactive-VR timing failure did not recur). No new corpus
failure appeared. The final source checks also passed the focused race tests,
vet/build and documentation gates after adding authored retained-value checks.

## Provenance and remaining gaps

The [GDCM data repository](https://github.com/malaterre/gdcmdata) was inspected by
root filename inventory, without downloading its DICOM files. No root
redistribution license was identified. The project
[third-party policy](THIRD_PARTY_PROVENANCE.md#rules) prohibits importing those
files. The named conformance dataset's file-specific licenses and original
vendor payloads remain unavailable. No claim is made that the historical 223
missing reference entries were recovered. The own harness uses installed public
GDCM headers/APIs; production parsers and codec implementations were not copied.

Real ultrasound 512×512×2496 and 215 MiB XADC files are still absent. Their small
analogues qualify pre-allocation limits, not throughput, peak memory, large-file
completion, vendor-wide support or clinical safety. The manifest records those
gaps rather than adding them to a support count.

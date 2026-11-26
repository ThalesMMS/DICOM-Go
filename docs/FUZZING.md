# Fuzzing and adversarial checks

The `fuzz` package contains Go fuzz targets for untrusted decoding surfaces:

- `FuzzDICOMParse`: Part 10, raw dataset and parser decoding across supported transfer syntaxes.
- `FuzzDICOMJSON`: DICOM JSON unmarshal followed by compact and pretty marshal.
- `FuzzULPDU`: Upper Layer PDU decoding plus DIMSE command-set decoding.
  Unit tests beside `net/ul` additionally check that a 2 MiB A-ASSOCIATE-RQ is
  refused from its header and that a truncated body only allocates what
  arrived. Its seeds include the malformed A-ASSOCIATE-RQs of DCMTK's
  `tparseassoc` and `tassocleak` tests, regenerated in
  `internal/dicomtest.AssociationRQVectors`, which `net/ul` also runs as
  parser and acceptor regression tests.


The `net/dicomweb` package keeps client response fuzzing beside its private
parsing seams:

- `FuzzDICOMwebMetadataJSONInstanceRefs`: DICOM JSON metadata, instance
  reference extraction, response media types, transfer-syntax parameters and
  deterministic decoding.
- `FuzzDICOMwebMetadataReferences`: bounded metadata-reference extraction with
  complete, fallback, empty and truncated JSON seeds.
- `FuzzWADOResponseParsing`: single-part and multipart buffered objects,
  streamed objects and streamed frames through an offline HTTP transport,
  including boundaries, raw headers, truncation, media/transfer-syntax
  compatibility and interacting body/part/count limits.

These targets use synthetic valid and malformed seeds. The DICOMweb targets
cap input and header values before parsing and configure deliberately small
body, part-count, part-size, raw-header and multipart-depth limits. Those hard
limits are the allocation guardrails. The smoke command additionally uses two
fuzz workers, a per-target test timeout and Go's soft `GOMEMLIMIT`; it does not
use the much larger production response defaults.

The `pixeldata/display` package fuzzes 60xx overlay planes with
`FuzzOverlayFromObject`: arbitrary Overlay Rows, Columns, Origin, Number of
Frames in Overlay, Image Frame Origin and Overlay Data go through
`OverlaysFromObject`, frame selection and burn-in. Its seeds are the malformed
planes of the overlay regression tests (maximal planes carrying two bytes, SS
extreme origins, frame counts that overflow `int`); extracted overlays must pass
`Overlay.Validate` and a selected frame is never larger than its data.

The `pixeldata/rle` package fuzzes the RLE Lossless codec (PS3.5 Annex G):

- `FuzzDecodeRLEFrame`: one arbitrary RLE frame decoded strictly and with the
  opt-in `DecodeRecovery.RLESegmentOverrun`, as a whole and as a region, for
  every supported layout (up to 15 segments) and frames up to 64×64.
- `FuzzDecodeRLEObject`: fragments, Basic Offset Table, Number of Frames and
  image metadata through `Codec.DecodeContext`, so frame grouping, RLE header
  detection and metadata validation run with the segment decoder.
- `FuzzRLERoundTrip`: `Encoder` output decoded back bit-exactly, with a
  well-formed header, even segment offsets and no recovery needed.

The decode targets check that decoding never panics, fails only with the
package's typed errors, returns exactly the declared frame size, never
reports a recovery a strict decode did not need, recovers and reports
(`rle-segment-overrun`) every run-level failure only when asked, and that a
region is the crop of the full frame. Their seeds, shared with
`TestRLEMalformedSeeds` in `pixeldata/rle/fuzz_test.go`, are written from
Annex G and each names the class of DCMTK RLE decoder fix it models
(defective header, invalid or zero-length stripe, uninitialized output after
a short stripe); no DCMTK code or test file is used. They cover 0, 16 and
0xFFFFFFFF segments, offsets that are zero, inside the header, past or equal
to the fragment length, decreasing or equal, segment counts that do not match
samples × bytes, stripes one byte or one run short, literal and replicate
runs past the frame, a replicate run without its value, no-op-only stripes,
three 16-bit samples, odd-length fragments and runs after a complete stripe.
A PackBits segment of n bytes decodes to at most 64n bytes, so a strict
decode refuses segments too short to fill the declared frame before
allocating it, and segments are decoded in place into the one frame buffer
(`TestStrictDecodeRefusesUnfillableFrameBeforeAllocating`,
`TestDecodeAllocatesTheFrameOnce`). The opt-in recovery zero-fills, so it
allocates the declared frame once.

`FuzzPresentationFromObject` covers the whole presentation parse. Its input is
a data set (Explicit VR Little or Big Endian, or Implicit VR Little Endian,
chosen by a flags byte that also toggles `LUTOptions.Legacy`), read with a
256 KiB input cap and 64 KiB element, 1,024-element and depth-8 parser limits.
It goes through `ModalityLUTFromObject`, `VOIFromObject`, `ModalityLUTAt`,
`VOIAt`, `VOIsAt`, `PaletteFromObject` (including segmented palettes),
`PresentationFromObject`, `ShutterFromObject` and `OverlaysFromObject`, and
the results are applied to 4x4 and 16x16 frames (`RenderGray`,
`RenderGray16`, `RenderColor`/`RenderColor16` for a palette,
`Presentation.Apply`/`ApplyGray16`, `Shutter.ApplyTo`), together with the raw,
unvalidated overlay of group 6000. Every error must wrap one of the package's
typed errors (or `object.ErrFunctionalGroupConflict`); a LUT holds at most
the 65,536 entries a descriptor can declare or what its data holds, with every
entry inside its bits per entry; shutters and overlays pass their validation.
The seeds are synthetic data sets written from PS3.3 C.11, C.7.6.3.1.5,
C.7.9, C.9 and C.7.6.11, modelled on the defect classes of DCMTK `87cccaa5`
(8-bit LUT with an odd entry count), `99bae4af` (partial palette),
`601b227e`/TALOS-2024-1957 (US/SS descriptor, US/OW LUT Data) and GDCM
`21a79309`/TALOS-2024-1924 (LUT Data size against the descriptor): descriptor
entries 0 (65,536), first values mapped at the range ends, bits 0/7/8/16/17,
missing or empty data, segmented palettes with bad lengths, opcodes, linear
starts and indirect offsets, overlays with maximal or short planes, and
degenerate rectangles, circles and polygons. No code or test file of those
projects was used. Its first campaign found that `Presentation.ApplyGray16`
walked every declared bit of an overlay (4.3e9 for 65535x65535 carrying two
bytes), that indirect segments copying empty segments made segmented palette
expansion quadratic (now capped at one segment per word plus one per entry),
and that far circular shutter centers or radii overflowed the squared
distance; each has a regression test in `presentation_bounds_test.go`.

The `dictionary` package fuzzes the run-time DCMTK `.dic` loader with
`FuzzParseDIC` (strict and tolerant modes under small file, line, entry,
repeating-entry and skipped-line limits); see
[runtime dictionaries](PRIVATE_CREATORS.md#runtime-dictionaries-in-dcmtk-dic-format).
Its authored seeds live in `dictionary/testdata/fuzz/FuzzParseDIC`.

The `nifti` package fuzzes NIfTI-1/2 headers with `FuzzReadHeader`:
arbitrary bytes go through `ParseHeader` and `Read` under small voxel, payload
and header limits with each transform policy. Every failure must be a typed
`*nifti.ReadError`; every success must have finite affines, positive spacing
and readable voxels. Its committed seeds in
`nifti/testdata/fuzz/FuzzReadHeader` are synthetic headers (both versions and
byte orders, pairs, extensions, qform/sform combinations, sheared and tiny
sforms, NaN values and transforms, gzip, truncation); regenerate them with
`DICOM_GO_NIFTI_WRITE_FUZZ_SEEDS=1 go test ./nifti -run TestWriteReadFuzzSeeds`.

The `nrrd` and `metaimage` packages fuzz their readers with `FuzzReadNRRD`
and `FuzzReadMetaImage` under small voxel and payload limits: arbitrary bytes
must fail with an error or decode to a volume that writes and reads back with
the same dimensions.

The `validation` package additionally contains bounded synthetic fuzz targets
for VR strings, VM delimiters and nested sequence/report limits. The exact
commands are documented in [`VALIDATION.md`](VALIDATION.md#testing-and-performance-contract).

Normal `go test ./...` execution runs only the deterministic seed inputs, so it
stays fast enough for the standard baseline. Longer fuzz campaigns are opt-in:

```sh
make fuzz-smoke

go test ./fuzz -run='^$' -fuzz=FuzzDICOMParse -fuzztime=10m
go test ./fuzz -run='^$' -fuzz=FuzzDICOMJSON -fuzztime=10m
go test ./fuzz -run='^$' -fuzz=FuzzULPDU -fuzztime=10m
go test ./net/dicomweb -run='^$' -fuzz=FuzzDICOMwebMetadataJSONInstanceRefs -fuzztime=10m
go test ./net/dicomweb -run='^$' -fuzz=FuzzDICOMwebMetadataReferences -fuzztime=10m
go test ./net/dicomweb -run='^$' -fuzz=FuzzWADOResponseParsing -fuzztime=10m
go test ./pixeldata/display -run='^$' -fuzz=FuzzOverlayFromObject -fuzztime=10m
go test ./dictionary -run='^$' -fuzz=FuzzParseDIC -fuzztime=10m
go test ./pixeldata/display -run='^$' -fuzz=FuzzPresentationFromObject -fuzztime=10m
go test ./pixeldata/rle -run='^$' -fuzz=FuzzDecodeRLEFrame -fuzztime=10m
go test ./pixeldata/rle -run='^$' -fuzz=FuzzDecodeRLEObject -fuzztime=10m
go test ./pixeldata/rle -run='^$' -fuzz=FuzzRLERoundTrip -fuzztime=10m
go test ./nifti -run='^$' -fuzz=FuzzReadHeader -fuzztime=10m
go test ./nrrd -run='^$' -fuzz=FuzzReadNRRD -fuzztime=10m
go test ./metaimage -run='^$' -fuzz=FuzzReadMetaImage -fuzztime=10m
```

`make fuzz-smoke` runs each selected DICOMweb and RLE target
(`DICOMWEB_FUZZ_TARGETS`, `RLE_FUZZ_TARGETS`) independently for five
seconds with a two-minute outer timeout, at most two workers, and a 1 GiB soft
memory target. It uses and removes an isolated Go build/fuzz cache so an
accumulated machine cache cannot make the seed-baseline phase grow across
runs. Override `FUZZ_SMOKE_TIME`, `FUZZ_SMOKE_TIMEOUT`,
`FUZZ_SMOKE_PARALLEL`, or `FUZZ_SMOKE_MEMORY` for a deliberate longer campaign.
Set `FUZZ_SMOKE_GOCACHE` to an absolute disposable directory only when a CI job
needs to own cleanup itself.
Do not use `FUZZ_SMOKE_MEMORY` as a substitute for target-local byte, count and
depth caps: `GOMEMLIMIT` is a garbage-collector target, not a hard address-space
limit.

Go minimizes a failing input and writes it under
`net/dicomweb/testdata/fuzz/<Target>/<hash>` (or the corresponding package for
another target). Reproduce it with
`go test ./net/dicomweb -run '<Target>/<hash>'`. Keep only a minimal synthetic
regression input; never add PHI, credentials, real endpoint identifiers, or
site-specific metadata to a seed or crash corpus.

Parser targets in `parser/` (`FuzzContainerBoundaryRecovery`,
`FuzzSequenceRecovery`) carry deeply nested seeds inside a defined-length
container, within and beyond their depth limit. Sequence nesting is bounded
by default (`parser.DefaultMaxSequenceDepth` = 128 sequence and item frames;
`dicomjson.DefaultMaxSequenceDepth` and the DICOM XML default = 64 data sets),
and the parser's per-header container lookup is O(1), so even an unlimited
read stays linear in the depth. `TestNextDeepNestingScalesLinearly` and
`BenchmarkReaderNextDeepNesting` (1,000 to 100,000 levels, reported as
`ns/level`) guard that property.

Long fuzz and race runs should be separate. Allocation assertions based on an
exact `AllocsPerRun` count are intentionally avoided because parser allocations
vary with valid element count and Go version. The enforced contract is that
accepted work remains bounded by the target input cap and configured parser
byte/count/depth limits, and oversized inputs fail before unbounded parsing.

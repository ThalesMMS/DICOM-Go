# Lossy transcode error verification

`pixeldata.TranscodeOptions.MaxPixelError` enables a measured error limit for a
newly encoded lossy representation. `nil` preserves the existing policy. A
finite nonnegative `*float64` enables verification, including a limit of zero.
`AverageBlockSize` is the square block side; zero selects one. Negative sides,
a positive side without a limit, NaN/Inf/negative limits, and an explicit limit
combined with `SkipVerification` fail with `ErrTranscodeVerificationOptions`.
The limit is copied for the call; callers must not mutate borrowed inputs while
the call is reading them. `ResolveTranscodeVerificationOptions` shares this
validation/snapshot policy with adapters. `storetranscode.NewSource` validates
and copies the limit for its later preparation/open calls and requires an
explicit target for these options, including on a pass-through route.

Without a limit, the existing `TranscodePath` policies remain: transformed
lossless output is decoded and checked for exact pixel bytes, lossy output is
decoded, and `SkipVerification` skips that extra check. Structural readback,
codec validation, finite limits and atomic publication remain mandatory. The
in-memory adapters keep their existing default verification behavior; the new
lossy limit applies to `TranscodeDataSet`, `TranscodeCoreDataSet`, `TranscodeFile`
and both `TranscodePath` routes. It does not retroactively measure losses in a
pass-through/equivalent representation, native decoding, a lossless encoding,
or an exact `CodestreamTranscoder` conversion. Their lossy metric stays nil.
Existing lossy authorization, derived identity and compression history policy
remain separate requirements.

## Domain and comparison

The source is the native raster delivered to the encoder, after source decode
and any explicitly requested `Rewrite` or overlay extraction. The other raster
is the newly encoded frame decoded with the explicit target decoder registry.
Samples are interpreted by the existing `SampleFormat.Reader` in little-endian
order: High Bit locates Bits Stored, unused bits are masked, and signed samples
are sign-extended according to Pixel Representation. Differences are numeric
stored-sample units. Rescale Slope/Intercept, modality LUT, VOI/windowing,
presentation LUT and display transforms are not applied; an error of one is
not automatically one HU or a perceptual/color error.

Both decoded rasters must have equal rows, columns, component count, Bits
Stored, signedness and photometric interpretation. The comparator supports
MONOCHROME1/MONOCHROME2 and full-resolution RGB/YBR_FULL with 8-, 16- or 32-bit
integer containers. Container width and High Bit may differ when both describe
the same effective stored range. Planar/interleaved layouts are indexed to the
same component. Palette indices, subsampled YBR, incompatible color spaces,
different signedness/precision, floating-point/packed samples and changed
geometry fail explicitly with `ErrTranscodeComparisonUnsupported`; the metric
does not infer a conversion. A codec may explicitly report RGB after decoding
JPEG color output; it is comparable to an RGB encoder input when all the other
requirements match. An explicit palette/YBR rewrite can similarly establish a
common RGB input. Its rewrite error is outside this encoding-only measurement.

For side `B`, blocks partition each frame starting at `(row=0,column=0)`.
For each block `q`, component `c`, and its actual pixel count `Nq`:

```text
Eq,c = abs(sum over q of (source[p,c] - decoded[p,c])) / Nq
MaximumError = max over all frames, blocks and components of Eq,c
MaximumSampleError = max over all samples of abs(source - decoded)
```

The right and bottom partial blocks are included and divided by their actual
area. A B×B image has one block per component. Components are never averaged
together, and blocks never cross frames. Division produces a `float64` metric
without integer truncation; the limit is compared to that reported binary64
value. Equality passes, and a larger measured error fails. For `B=1`, the
metric is maximum absolute sample error. For `B>1`, opposite signed errors can
cancel within a block: zero then means equal block means, not equal samples.
`MaximumSampleError` exposes the unaveraged error even when cancellation occurs.
Signed differences use an `int64` accumulator; an area whose worst-case sum
cannot fit is rejected explicitly before scanning rather than overflowing.

## Reports, failure and resource behavior

Accepted measurements are returned in `TranscodeReport.LossyVerification`, with
maximum block/sample errors, limit, block side, frame count and component-block
count. `Verification` is `TranscodeVerificationPixelError` (`pixel-error`). A
failed limit returns a `*LossyVerificationError` with the one-based failed frame,
measured error, limit and side, and supports `errors.Is(ErrTranscodePixelError)`.
It exposes aggregate errors rather than sample values or backend text.

Streaming shares the decoder call already required by verification. Each frame
is measured inside its bounded frame task and only its aggregate result reaches
ordered spool consumption. The comparator creates no sample raster or block
array: it keeps readers, one signed sum and aggregate counters. The existing
verification buffer charge and frame window therefore still cover retained
package buffers; codec-private workspace and RSS are outside that charge. The
whole-object route remains bounded by whole input/output limits, and measures
one encoded frame at a time against its borrowed native frame. Its path adapter
uses that report instead of decoding the lossy output again. Verification adds
one target decode per frame to in-memory lossy adapters, plus a sample scan.

Comparison polls context during the scan; codec cancellation remains limited
by the existing codec interface. A failed frame cancels/joins pipeline work and
prevents destination replacement; the previous destination and source remain
intact and private temporaries are cleaned. `storetranscode.Source` verifies
before offering a representation and again before exposing its writer, records
accepted aggregate metrics or a detached failure report, and does not supply
a failed representation to C-STORE. The batch layer can redact preparation
errors; the source report retains the safe verification diagnostic.
At writer selection the report uses that handle's measured representation,
rather than a previous preflight's measurements, and clears any failed retry.

## CLI

```sh
dcmconv -ts jpeg-ls-near -near 1 -allow-lossy \
  -max-pixel-error 1 -average-block-size 1 -o out.dcm in.dcm

storescu -transcode-to 1.2.840.10008.1.2.4.50 -allow-lossy \
  -jpeg-quality 85 -max-pixel-error 12 -average-block-size 4 \
  localhost:104 in.dcm
```

Both tools default `-max-pixel-error` to `-1` (disabled) and block side to zero
(one). Other negative limits, NaN/Inf and invalid block policies are usage
errors. `dcmconv -verify=false` cannot accompany an explicit error limit.
`storescu` requires `-transcode-to` for the new policy and prints source-indexed
accepted metrics or a safe failure diagnostic. Neither flag grants lossy
authorization. Values in these examples demonstrate configuration, not a
recommended clinical tolerance.

## Evidence and clean-room reference

Controlled fixtures test unsigned boundaries, signed/shifted Bits Stored,
unused bits, representation equivalence/refusal, complete/partial blocks,
per-component means, cancellation, immutable limits, default behavior, decoder
reuse, resource and publication behavior. Builtin JPEG-LS NEAR=1 tests cover
8/12/16-bit grayscale/RGB and require a measured maximum of one, accept limit
one and reject zero. CLI tests exercise the builtin codec and preserve an
existing destination on refusal. Store adapter tests verify detached reports,
cleanup, refusal before Open and no association attempt for a rejected source.
These are controlled local tests; no target-PACS or clinical tolerance claim is
made, and no performance campaign is part of this feature.

The behavioral reference was inspected at dcm4che commit
`acbbb85eee1a2fcc441eb2d8e65023fece2b40ec`:
[Compressor.java](https://github.com/dcm4che/dcm4che/blob/acbbb85eee1a2fcc441eb2d8e65023fece2b40ec/dcm4che-imageio/src/main/java/org/dcm4che3/imageio/codec/Compressor.java).
The independent implementation keeps configurable decode/compare and
difference-of-block-means semantics. It includes all border blocks, correctly
interprets unsigned/signed stored samples, uses bounded wide sums and retains
fractional means rather than reproducing the reference's byte/short signed
reads, omitted last/border blocks or integer division. No reference code was
copied. Modality-domain processing is a separate stage in
[DICOM PS3.3 C.11](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_C.11.html).

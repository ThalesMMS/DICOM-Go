# Raster page inspection and exact Secondary Capture import


## Initial matrix

| Input feature | Accepted behavior |
| --- | --- |
| Classic TIFF 6.0 | `II` or `MM`, version 42, sorted unique IFD fields and a finite noncyclic next-IFD chain; source page order retained |
| Samples | Unsigned grayscale or RGB, uniform 8 or 16 bits per component; exact integer values |
| Photometric | Black-is-zero becomes MONOCHROME2; white-is-zero becomes MONOCHROME1 with unchanged stored values; RGB stays RGB |
| Compression | Uncompressed strips only; other compression codes and predictors refuse explicitly |
| Organization | Chunky or separate sample planes, one or multiple strips; exact strip counts and byte spans |
| Orientation | All eight TIFF orientations; samples rotate/reflect to visual top-left, dimensions exchange for orientations 5–8 |
| Alpha | TIFF ExtraSamples refuses, including opaque alpha; PNG must be opaque as checked by the existing importer |
| ICC | TIFF ICC and PNG iCCP profiles refuse because this path does not perform color management |
| Other native rasters | Existing PNG/BMP/PNM/PGX decoders; 8/16-bit allocated unsigned stored samples, including PGX precision below 16 bits |
| Channels | `all`, `red`, `green` or `blue`; component selection requires RGB and retains the selected samples exactly |

BigTIFF, tiled/sub-IFD images, masks/reduced-resolution subfiles, palettes,
YCbCr/CMYK/Lab, signed/floating samples, unequal bit depths, packed 1/2/4-bit
TIFF and alpha/profile semantics are outside this matrix. They return errors
rather than a guessed raster. Descriptive/private source metadata is ignored
and never copied. PNG gamma/chromaticity and source resolution do not establish
calibrated physical geometry or color; samples are imported as stored codes.
No TIFF resolution, scale, units, orientation or description is interpreted as
DICOM patient geometry. Burned-in annotations remain potentially present.

## Inspection, preview and construction

TIFF inspection validates metadata and strip spans without allocating decoded
pixel planes. Other native formats reuse the bounded import decoders during
inspection. Page numbers are one-based. `DecodeRasterPage` returns owned,
interleaved little-endian samples and never mutates source bytes. Channel
selection is explicit; it does not calculate luminance or reduce precision.

Preview limits its output to at most 1024 pixels per edge, with an explicit
8-bit display mapping. That presentation conversion never changes the stored
samples. MONOCHROME1 preview inverts presentation while retaining stored codes.

Each `BuildRasterPage` result is a new single-frame Secondary Capture object,
with `DERIVED\SECONDARY` image type and the existing common-module constructor
and IOD/value validation. This permits heterogeneous page dimensions and
RGB16. Multi-frame True Color SC requires 8-bit samples, so concatenating RGB16
pages into that class would violate its
[DICOM content constraints](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_A.8.5.4.html).
The generic single-frame SC IOD has
[no pixel-format constraint](https://dicom.nema.org/medical/dicom/current/output/chtml/part03/sect_A.8.html).
This is converted non-DICOM content, not an original clinical acquisition.

## Limits and cancellation

`ImportLimits` retains its existing defaults: input 256 MiB, admitted native
sample bytes 512 MiB, 16 Mi pixels per page and 1024 frames. Limits apply before
decoding or sample allocation. TIFF additionally caps 4096 IFDs, 256 fields per
IFD, 65536 fields across the request, 65536 values per structural field and
262144 structural values across the request. Those arrays occupy at most
1 MiB, independent of a malicious source's advertised counts. Strip spans must
fit the actual input; dimensions also fit DICOM US and the host integer range.

Context checkpoints cover input admission, every directory/field, every 1024
field values, every image row and every 4096 source columns, component
compaction, preview rows and the reused object builder. Errors and cancellation
return no page/model. The page APIs describe admitted allocations, not an RSS
or instant operating-system I/O cancellation guarantee.

## Current synthetic and independent qualification

Own finite fixtures verify all eight orientations, 8/16-bit stored values,
white-is-zero presentation, exact RGB/component selection, heterogeneous
multi-page inspection, typed refusals, bounded input/metadata/pixels, cyclic
IFDs, immutable concurrent use and deterministic cancellation at every observed
checkpoint. Generated SC objects pass IOD/value validation and Part 10 sample
readback. No vendor implementation or asset was copied. The implementation is
derived from the public [Adobe TIFF 6.0 specification](https://image-js.github.io/tiff/media/TIFF6.pdf).


```sh
# Run from dicom-go-dev; native libtiff tools must be on PATH.
DICOMGO_TIFF_INTEROP=1 DICOMGO_TIFF_EVIDENCE_DIR=/absolute/isolated/output \
  GOMAXPROCS=2 go test -p 1 ./encapimage \
  -run '^TestRasterTIFFLibTIFFQualification$' -count=1 -v
go test -p 1 -race ./encapimage \
  -run '^TestRasterCancellationConcurrentImmutableAndPNGPrecision$' -count=1
go test -p 1 ./encapimage -run '^$' -fuzz '^FuzzRasterTIFFAdmission$' \
  -fuzztime=5s -parallel=2
```

The focused race check passed; a five-second fuzz run exercised 274788 inputs
without a failure. The complete `encapimage` package passed on this host.
These checks do not establish external PACS acceptance or clinical suitability.

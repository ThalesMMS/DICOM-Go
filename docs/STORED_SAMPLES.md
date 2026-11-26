# Stored samples

`pixeldata` exposes stored samples as integers, separate from raw bytes,
modality values and presentation pixels. The three stages stay distinct:

| Stage | API | Applies |
|---|---|---|
| Storage | `pixeldata.StoredSamples`, `ReadStoredSamples`, `SampleFormat.Reader`, `ComputeStoredSampleStatistics` | byte order, the BitsStored field at HighBit, sign extension from BitsStored |
| Modality | `display.DecodeModality*`, `render.PixelValueAt` | Rescale Slope/Intercept or a Modality LUT, once, to stored values |
| Presentation | `display.RenderGray`, `frame.(*Frame).GetImage` | VOI window or LUT, MONOCHROME1 inversion, colour |

A stored sample is not a modality value: a CT stored sample is not in
Hounsfield units until the modality transform is applied. The storage API
never rescales, looks up LUTs, windows, inverts MONOCHROME1 or converts colour,
and the modality and presentation APIs keep their contracts. The executable
`ExampleStoredSamples` prints one row through all three stages.

## Format

`SampleFormat` holds BitsAllocated (8, 16 or 32), BitsStored, HighBit, the
signedness from Pixel Representation and the container byte order (nil is
little-endian); `Metadata.SampleFormat(order)` builds it. `Validate` refuses 1-
and 64-bit containers, BitsStored outside 1..BitsAllocated and a HighBit that
does not place BitsStored inside the container, with a `*SampleFormatError`
naming the attribute (`errors.Is(err, ErrInvalidSampleFormat)`). `Range`
returns the smallest and largest stored value. Bits above HighBit are ignored,
so a value already sign-extended in its container is read to the same number:
interpretation happens once.

Examples with BitsAllocated 16: BitsStored 13 and HighBit 12 read raw `0x1830`
(and `0xF830`) as -2000; BitsStored 12 and HighBit 11 read `0x0800` as -2048.

## Reading

- `ReadStoredSamples[T](ctx, dst, src, format)` writes every sample of `src`
  into the caller's `dst` in storage order (interleaved or planar exactly as
  stored) and allocates nothing.
- `StoredSamples[T](ctx, src, format, maxSamples)` returns a new caller-owned
  slice; `maxSamples` (default `DefaultMaxStoredSamples`, 2^28) bounds the
  allocation (`ErrSampleBudgetExceeded`).
- `T` is any Go integer type that holds the whole range of the format;
  otherwise `ErrSampleTypeTooNarrow` is returned before anything is written
  (signed samples into unsigned types, 16-bit unsigned into `int16`, ...). No
  `any` and no type assertions are involved.
- `SampleFormat.Reader()` gives a `StoredSampleReader` for random access, such
  as sampling one voxel or editing one pixel.
- A source that is not a whole number of containers is `ErrSampleDataLength`;
  a short destination is `ErrSampleBufferTooShort`.
- `ctx` is checked every 65,536 samples; after cancellation or an error the
  destination's contents are unspecified and `StoredSamples` returns nil.
- The source is only read and never retained. Interpretation always copies:
  zero-copy access is to the raw bytes (`ExtractNativeFramesView` aliases the
  object read-only; `ExtractNativeFrames` returns its own copy).

## Frames and effective metadata


## Statistics and Pixel Padding

`ComputeStoredSampleStatistics` interprets each sample once and returns the
count, minimum, maximum and exact mean (a 128-bit sum) of stored values, with
padding samples counted separately. `PixelPaddingFromObject` reads Pixel
Padding Value (0028,0120) and Pixel Padding Range Limit (0028,0121) as stored
values: US for unsigned and SS for signed samples (implicit VR data sets are
resolved by Pixel Representation when read), one value each, within the
format's range; anything else is `ErrInvalidPixelPadding`. With a range limit,
the closed range between the two values is padding, in either order.

## Shared implementation


## Evidence

- `internal/storedsample`: values, ranges, bits above HighBit, invalid formats.
- `pixeldata`: `TestStoredSamplesInterpretSignednessAndBitField` (8/16/32
  bits, signed/unsigned, extremes, little/big-endian),
  `TestStoredSamplesRefuseLossyDestinationTypes`, `TestStoredSamplesFailTyped`,
  `TestStoredSamplesOwnershipAndCancellation`,
  `TestStoredSampleStatisticsExcludePadding`,
  `TestPixelPaddingFromObjectFollowsPixelRepresentation`,
  `TestStoredSampleReaderRandomAccess`, `ExampleStoredSamples`.
- `pixeldata/frame`: `TestStoredSamplesAgreeAcrossNativeCodecStatisticsAndRendering`
  runs one signed 13-bit frame through native Explicit and Implicit VR Little
  Endian, Explicit VR Big Endian, RLE Lossless and JPEG-LS Lossless decoding
  and checks access, statistics with padding, a single modality rescale (a
  double rescale is reported), render sampling and rendered pixels.
- Independent oracle (opt-in): `TestPydicomStoredSamplesOracle` compares every
  sample of ten formats (signed/unsigned 8, 12, 13, 16, 24-in-32 and 32 bits,
  extremes) in little- and big-endian Part 10 files with pydicom 3.0.2/NumPy
  2.1.3 `pixel_array`. Run it with `DICOM_GO_PYDICOM_STORED_SAMPLES=1` and
  `DICOM_GO_PYTHON`; without them it skips and qualifies nothing.
- JPEG 2000 adapter module: `TestCoreOutputFeedsStoredSamples` checks the core
  backend's signed and unsigned 12/16-bit output and the signedness-mismatch
  policy against the corpus source samples.

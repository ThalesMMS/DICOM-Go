# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project uses Go module semantic version tags.

## [Unreleased]

### Fixed


### Added


- cmd: User Identity negotiation in `storescu`, `echoscu`, `findscu` and
  `dicom-go-retrieve`. Shared `-user-identity`, `-username`,
  `-user-identity-secret-file`, `-user-identity-secret-env` and
  `-user-identity-response` flags propose username, username/password,
  Kerberos, SAML or JWT identities; the password or token is read only from a
  file or a named environment variable and never printed.
  `dimse.RetrieveDialOptions.UserIdentity` and
  `dimse.CEchoVerificationOptions.UserIdentity` propose it for C-MOVE, C-GET
  and C-ECHO verification.
- net/dimse: `StorageCommitmentWorkflow.AwaitResultOnAssociation` lets a
  Storage Commitment requestor serve the committer's N-EVENT-REPORT on the
  association that carried its N-ACTION for a bounded wait, returning early
  when the stored transaction is terminal or the committer releases, and
  leaving the association releasable after the wait.
- net/dicomweb: QIDO-RS study searches request and return Issuer of Patient
  ID. `StudySearchParams` adds `includefield=IssuerOfPatientID` and
  `StudyMatch.IssuerOfPatientID` carries (0010,0021), so callers can keep a
  Patient ID within its issuer when matching remote studies to a patient.
- realworld: `Mapping.Quantity` reads the coded quantity of the Quantity
  Definition Sequence (the (246205007, SCT, "Quantity") row, or the retired
  (G-C1C6, SRT) of older writers); `Mapping.Name` (LUT Label, LUT
  Explanation, quantity, then "RWV") and `Mapping.UnitText` (the UCUM code,
  empty for "1", else the code meaning) give the text a viewer shows; and
  `ValuesFor` maps a stored value through every mapping whose range holds it,
  in sequence order, without preferring one. `Unit` is now an alias
  of the new `Code`.
- sr: `ViewerMeasurement.Derivation` writes the measurement's Derivation
  concept modifier, with `MeanDerivation`, `MinimumDerivation`,
  `MaximumDerivation` and `StandardDeviationDerivation` from CID 7464, and
  `MeasurementKindRealWorldValue` records a value in the units of a Real
  World Value Mapping, named by the caller's concept (default (246205007,
  SCT, "Quantity")); it requires units.


  Reports are typed and value-free. Opt-in comparisons with dciodvfy
  (`DICOMGO_DCIODVFY`) and dcmpschk agree on the objects dicom-go writes
  and on copies with an Attribute removed. The writers' tests now require
  no IOD error, and the fixes are:
  - `seg`: Image Type, Lossy Image Compression, Content Date/Time,
    Dimension Organization UIDs and identification.
  - `rtstruct`: Structure Set Date/Time, Series Number, Operators' Name,
    ROI Generation Algorithm and ROI Interpreter.
  - `vps`: Presentation Creation Date/Time, anatomic/view attributes,
    identification and an empty Compositor Sequence outside MPR.
  - `sr`: KOS series UID and study.
  - `multiframe`: classic conversion writes the true Type 2C Attributes.


- `cmd/dcm2json`, `cmd/json2dcm`, `cmd/dcm2xml` and `cmd/xml2dcm`: Part 10
  <-> DICOM JSON / Native DICOM Model XML with optional File Meta
  Information, BulkDataURI files confined to the document directory and a
  character set policy; cross-checked with DCMTK dcm2json, json2dcm and
  dcm2xml --native-format.
- `dicomxml` decodes documents declaring a non-UTF-8 encoding (such as the
  ISO-8859-1 written by DCMTK dcm2xml) through `dicomxml.CharsetReader`.

- DICOMDIR application profiles and icons: `WriteOptions.Profile`
  (STD-GEN-CD, STD-GEN-DVD/USB-JPEG/J2K, STD-XA1K-CD/DVD) validates SOP
  classes, transfer syntaxes, image constraints and Type 1 keys before
  writing and adds the profile's directory keys; `WriteOptions.Icons` renders
  64x64 or 128x128 MONOCHROME2 Icon Image Sequences from the representative
  frame (opt-in black icon on failure); `FileSet.ValidateProfile` preflights.
  Cross-checked with DCMTK dcmmkdir.

- `encapimage.FromJPEG` and `cmd/jpg2dcm`: Secondary Capture or VL
  Photographic objects around an existing JPEG without re-encoding, with the
  transfer syntax taken from the frame header, personal APPn/COM segments and
  trailing data removed by default (value-free report), ICC profile moved to
  (0028,2000) and generated UIDs. Cross-checked against DCMTK img2dcm,
  dcmdump and dcmdjpeg.
- The retired JPEG transfer syntaxes 1.2.840.10008.1.2.4.52 to .66 are read
  and written as encapsulated data sets (decoding still needs a codec).

- `Object.ConvertTextToCharacterSet`: re-encode text into a restricted
  character set with an explicit policy for unrepresentable characters
  (fail, transliterate or discard) and a value-free report; UIDs and
  non-text VRs are never touched. Cross-checked against DCMTK dcmconv.
- Write options: `SequenceLength` (undefined, preserved or defined lengths
  for sequences and items) and `GroupLength` (preserve, recalculate, create or
  remove, also inside items) on `WriteFileOptions` and the new
  `WriteDataSetWithOptions`; byte-identical to DCMTK `dcmconv -F` with
  `+g`/`+g=`/`-g` and `+e`/`-e`. Defaults are unchanged.
- Package `attrpath`: attribute paths (keywords, tags, private creators,
  `[n]`/`[*]` items) with dcmodify-style Find/Create/Insert/Modify/Remove,
  VR-checked values and ordered edit scripts; DCMTK tpath vectors as tests
  and a fuzzed parser. New `cmd/dcmodify` (identical to DCMTK dcmodify on
  the opt-in comparison), nested `findscu -k` paths, and
  `deid.Options.Edits` for post-de-identification edits.
- Curated private catalog: 225 more reviewed DCMTK private.dic definitions
  (Philips, Siemens, Fuji FDMS, SPI), chosen from attributes still unresolved
  in gdcmData and cross-checked against Explicit VR files; private UN
  attributes in the corpus fall from 2660 to 1856. Selection criteria and
  rejected candidates are recorded in `docs/CURATED_PRIVATE_CATALOG.md`.
- Package `siemens`: bounded, fuzzed parser for Siemens CSA headers (SV10,
  NOSV, byte-swapped, DATASET, Interfile, markup, zeroed), an SV10 writer, the
  ASCCONV protocol as an ordered key/value map with the slice array, and
  helpers for NumberOfImagesInMosaic and SliceNormalVector. It matches
  `gdcmdump --csa` on the gdcmData corpus.
- `multiframe.SplitMosaic` / `MosaicGeometryOf`: split Siemens mosaics into
  classic slices with corrected positions, inverted slice order and ASCCONV
  cross-checks; identical to GDCM SplitMosaicFilter on the gdcmData mosaic.
- JPEG Lossless: opt-in `DecodeRecovery.JPEGLosslessPredictor6Overflow` for
  16-bit predictor 6 streams from encoders that overflowed 16-bit integers
  (bit-exact with `dcmdjpeg +w6`, reported as
  `jpeg-lossless-predictor6-overflow`). The Cornell category-16 quirk now
  reads its 16 extra bits as an extension, as `dcmdjpeg +wc` does; synthetic
  vectors for both defects are checked against DCMTK.
- JPEG Lossless encoder: pure-Go Process 14 (`.57`, predictors 1-7) and SV1
  (`.70`) through `jpeglossless.RegisterEncoder` / `NewProcess14Encoder`,
  with optimal Huffman tables per frame, 2-16 stored bits, signed monochrome
  and unsigned RGB/YBR_FULL. `dcmdjpeg` decodes every predictor bit-exactly
  (opt-in oracle); throughput is close to `dcmcjpeg` and sizes are within
  0.1%. `storescu` registers it for `-transcode-to`.
- Lossy JPEG: 12-bit color (Process 4 with any sampling factors and libjpeg
  triangle upsampling), signed monochrome samples, and Bits Stored below the
  codestream precision. The color policy is explicit:
  `pixeldata.JPEGColorConversion` (guess, photometric, always, never) through
  `DecompressOptions.JPEGColorConversion`, and the decoded photometric is
  reported in `Frames.PhotometricInterpretation` and written by
  decompression. Results match `dcmdjpeg` within two steps and 12-bit
  `djpeg` within one (opt-in oracles). Opt-in
  `DecodeRecovery.JPEGSequentialScanQuirks` accepts sequential scans with
  non-standard Ss/Se/Ah/Al, so nine gdcmData Clunie JPLY files now decode
  within the lossy tolerance.
- RLE Lossless: YBR_FULL; Bits Allocated in multiples of 8 up to 15 segments
  (decode and encode); frames spanning several fragments; Planar
  Configuration ignored for one sample, and decoded color is always
  interleaved (the decompressed data set says 0). Opt-in
  `DecodeRecovery.RLESegmentOverrun` tolerates truncated or overrunning
  runs, as GDCM does. Four gdcmData files now match GDCM, and a fuzz target
  was added.
- Color rendering: YBR_PARTIAL_422 (native subsampled or decoded) and decoded
  YBR_PARTIAL_420; RGB and YBR in 16-bit containers (Bits Stored 9–16)
  scaled to 8 bits, or kept at 16 bits per channel by `display.RenderColor16`
  (PALETTE COLOR too). `display.ValidateColorPhotometric` checks photometric
  against the transfer syntax. The results are compared with DCMTK `dcm2img`.
- Automatic windows: `render.AutoWindowWith` with min/max, min/max ignoring
  extremes, histogram (fraction ignored per end) and region or mask
  restriction. Pixel Padding is excluded (`render.Frame.PixelPadding`), and
  the results match DCMTK `dcm2img +Wm/+Wn/+Wh/+Wr`. `render.PixelValueRange`
  returns the padding-free value range.
- Display calibration (PS3.14): `display.GSDFLuminance` and `GSDFJNDIndex`
  implement the GSDF. `display.ParseCharacteristicCurve` reads monitor and
  printer curves, interpolated by spline or polynomial fit.
  `DisplayCalibration.GSDFLUT` maps P-values to DDLs with ambient light,
  illumination and density limits, and matches DCMTK `dcm2img +Dm/+Dp`.
  `display.RenderGray16` and `Presentation.ApplyGray16` keep 16-bit P-values.
  Presentation LUT Shape LIN OD comes from `PrintDensity.LinODPresentationLUT`
  and `display.PrintDensityFromObject`. `printmgmt` keeps Illumination and
  Reflected Ambient Light, and `FilmBoxPresentation.PrintDensity` applies
  Min/Max Density.
- GSPS overlay activation: `gsps.State.OverlayActivations` and
  `State.Overlays` read and write Overlay Activation Layer (60xx,1001) and
  the state's own overlay planes; `gsps.ActiveOverlays` selects the overlays
  to display while a state is applied (activated groups only, the state's
  plane first). `display.EmbeddedOverlaysForFrame` extracts embedded
  overlays from one decoded frame.
- Overlays: `display.Overlay` exposes group, type, subtype, label,
  description, frames and Image Frame Origin (`Frame`, `ForImageFrame`,
  `Unpacked`) and ROI statistics; `display.EmbeddedOverlays` extracts
  overlays from unused sample bits; transcoding to encoded syntaxes refuses
  embedded overlays unless `TranscodeOptions.ExtractEmbeddedOverlays` moves
  them to Overlay Data.
- Package `realworld`: `MappingsAt` resolves Real World Value Mapping items
  per frame (per-frame, shared, top level), linear or LUT, with range and
  coded units; `PhilipsScaleAt` reads the Philips MR private rescale and
  Scale Slope on request, with the display and floating point values. Neither
  affects the default display.
- `Object.PixelSpacingAt` resolves the spacing that applies to a frame with
  its source and meaning (patient, detector, calibrated, RT image plane,
  scanned or unknown) from Pixel Spacing, Imager Pixel Spacing, Pixel
  Measures, Nominal Scanned and RT Image Plane Pixel Spacing and the
  calibration attributes; `EstimatedPatientSpacing` applies the
  magnification factor only on request.
- Per-frame modality and VOI for enhanced multi-frame objects:
  `object.FunctionalGroupItem` resolves a functional group macro per frame
  (per-frame, then shared, reporting `ErrFunctionalGroupConflict`);
  `display.ModalityLUTAt`, `display.VOIAt` and `display.VOIsAt` (all stored
  windows with explanations) and `frame.RescaleAt`/`frame.WindowAt` use it,
  and `frame.ExtractFrames` applies each frame's own transforms.
- `DecodeRecovery.ReconcileWithCodestream` replaces Image Pixel Module
  attributes that are absent or contradict the JPEG frame header, listing
  each in `DecompressReport.Reconciliations`; `DecodeRecovery.JPEGLosslessEncoderQuirks`
  retries JPEG Lossless frames with a nonzero Se, non-unit fill bits or the
  Cornell/MARCONI extra bits after category-16 differences, reporting each.
- `DecodeRecovery.DefaultImagePixelModule` assumes Samples per Pixel 1 and
  MONOCHROME2 for ACR-NEMA objects without them (when native Pixel Data fits
  one sample per pixel) and takes Number of Frames from Planes;
  `DecodeRecovery.Unpack12BitPacked` unpacks 12-bit packed Pixel Data to 16
  bits allocated. Each repair is a reported diagnostic and the decompressed
  data set records the values.
- `TransferSyntaxRecoveryOptions.AllowLegacySyntaxes` reads ACR-NEMA Implicit
  VR Big Endian (a new probe candidate) and the declared GE private syntax
  1.2.840.113619.5.2 (Implicit VR Little Endian with Big Endian Pixel Data),
  normalizing the data set to Implicit VR Little Endian and recording the
  encoding in `TransferSyntaxResolution.Legacy`; `transfer.LegacySyntax`
  describes both.
- `ReadFileOptions.AllowZlibDeflate` reads Deflated data sets wrapped in a zlib
  header (RFC 1950), verifying the Adler-32 trailer and reporting a
  `zlib-deflate-wrapper` boundary recovery; writing still emits raw deflate.
- `ReadFileOptions.AllowPartialRead` returns the complete top-level elements
  read before a truncated or malformed element with a `*PartialReadError`
  (`ErrPartialRead`); `File.PartialRead` describes where the read stopped.
  Partial Files are refused by `WriteFile` without
  `WriteFileOptions.AllowPartial` and by pixel data decompression
  (`ErrPartialFile`). `parser.Reader.ReadDataSetPartial` is the parser-level
  equivalent.
- `SequenceRecovery` options: undefined-length OB/OW as SQ when content starts
  with an item, VOI LUT Sequence written as OW, DCMTK-style replacement of the
  wrong delimiter, and defined-length private SQs kept as UN bytes when they do
  not parse (GDCM NOSHADOWSEQ), each reported as a boundary recovery.
- `VRRecovery` options: OX Pixel Data as OW, DCMTK-style dictionary VR and
  length field size preference, the Siemens private UL length-6 workaround and
  GDCM 1.x short-length UN, each reported with declared and used VR.
  `BoundaryRecovery` gains `DeclaredVR`/`UsedVR`; DCMTK tparser and telemlen
  vectors are tests.
- `RecoverDelimiterAnomalies` repairs byte-swapped Philips item tags (with
  Big Endian item content), delimiters with nonzero lengths, orphan
  delimiters and `(FFFE,0000)` group lengths in sequences, each reported as a
  boundary recovery and capped by `MaxDelimiterRecoveries`.
- `RecoverImplicitVRElements` reads elements with invalid VR bytes in an
  Explicit VR Little Endian data set as Implicit VR when their length fits the
  container and source, capped by `MaxImplicitVRElements` and reported per
  element as `implicit-vr-element`. DCMTK's tparser wrong-explicit-VR vectors
  are imported as tests.
- `ReadFileOptions.AllowImplicitVRFileMeta` reads File Meta Information
  written in Implicit VR Little Endian on every read path and reports it as an
  `implicit-vr-file-meta` recovery on `File.Meta`; rewriting emits conforming
  Explicit VR File Meta.
- Opt-in decode recoveries: `DecompressOptions.Recovery` with
  `IgnoreInconsistentBasicOffsetTable` rebuilds frame boundaries from the
  marker scan when the Basic Offset Table contradicts Number of Frames or the
  Item layout, and `DecompressOptions.Report` returns the applied diagnostics.
  JPEG and JPEG Lossless codecs now honour the decode context.
- `pixeldata.Frames.Diagnostics` reports tolerated, standard-permitted
  deviations as fixed codes. The JPEG codec decodes 8-bit Baseline (SOF0)
  frames under JPEG Extended (`.51`), a subset of Process 2 & 4, reporting
  `jpeg-sof0-under-extended`, and reports nonzero Item pad bytes; progressive,
  lossless and other processes stay refused under `.50`/`.51`.
- Third-party provenance: `THIRD_PARTY_PROVENANCE.json` records upstream
  commit, source lines, source and artifact SHA-256, license and distribution
  class for every derived artifact, and `internal/provenance` fails `make check`
  when an artifact, license file or notice is missing or changed. GDCM
  reference tables (pixel/repaired-file MD5, media storage, reader offsets,
  reader black list, overlay MD5) and DCMTK test data (characteristic curves,
  fictitious worklist dumps, CIELab colors, GB18030/GB2312 byte tables) are
  imported under `testdata/upstream/` with `LICENSE.GDCM`, `LICENSE.DCMTK` and
  `LICENSE.VTK`; `THIRD_PARTY_NOTICES.md` now separates embedded, optional
  module and test-only material.
- Interop evidence: `internal/interopcheck` verifies persisted and retrieved
  content (syntax, identity, every attribute and sequence item, every native
  frame and sample) with value-free mismatch reasons, and five negative
  controls fail the gate despite success statuses, locally and against a
  tampering pynetdicom peer. `make pydicom-interop` now also runs the stored
  samples, charset conversion and DICOMDIR oracles.
- `printmgmt`: Basic Grayscale and Color Print Management Meta SOP Class client
  and opt-in SCP (film session, STANDARD film boxes, image boxes, printer
  N-GET) with per-association state, deep-copied jobs to an injectable
  backend, limits, no automatic re-printing, and a loopback example.
- `mpps`: Modality Performed Procedure Step SCU client and opt-in SCP service
  over the normalized DIMSE layer, with Table F.7.2-1 validation, the
  IN PROGRESS/COMPLETED/DISCONTINUED lifecycle (0110H/A710H after a final
  state), an injectable compare-and-swap repository and a loopback example.
- `examples/referencearchive`: opt-in Storage Commitment Push Model committer
  with a durable file-backed transaction store, per-reference verification
  (identity, class, admitted file and SHA-256) and callback delivery.
- `net/dimse`: named Storage Commitment Failure Reason constants and
  `StorageCommitmentTransaction.Clone`/`IsDue`, `StorageCommitmentState.Terminal`
  for persistent store implementations.
- `parser`, `object`: `RecoverContainerOverruns` reads complete items and
  sequences whose declared length overruns their container only to the
  container end, drops item headers with no body and closes undelimited
  children, reporting each repair through `BoundaryRecoveries`.
- `object`: `TextOptions.CharsetFallback` authorizes the fallback character
  set separately for undeclared scopes and for declarations that reject the
  bytes; `Object.ConvertTextToUTF8` converts every scope to verified UTF-8,
  atomically or keeping unconvertible scopes with their declaration pinned,
  with value-free diagnostics.
- `object`: `Object.RecoverUN`, opt-in recovery of known VRs from UN
  elements (PS3.5 6.2.2) into a copy, with contextual VRs from normative
  context only, per-data-set private creators, bounded sequence payloads,
  `Strict` mode and value-free structural diagnostics.
- `pixeldata`: typed stored-sample access (`SampleFormat`, `StoredSamples`,
  `ReadStoredSamples`, `StoredSampleReader`), stored-value statistics with
  Pixel Padding (`ComputeStoredSampleStatistics`, `PixelPaddingFromObject`)
  and `frame.(*Frame).StoredSampleData`; `display`, `render` and `nifti` now
  share one stored-sample interpretation (see `docs/STORED_SAMPLES.md`).
- `examples/codec-adapters/jpeg2000`: `NewCore`/`RegisterCore`, an optional
  pure-Go JPEG 2000 Part 1 backend adapted from amrshadid/go-dicom v1.6.0 with
  signed/unsigned numeric output, PS3.5 8.2.4 reconciliation, caller budgets,
  cooperative cancellation and opt-in JP2 recovery (`.90/.91` only).
- `pixeldata/codecfixture`: fourteen synthetic OpenJPEG 2.5.4 JPEG 2000 Part 1
  corpus fixtures with source samples, independent reconstructions and
  per-fixture irreversible policies; `CorpusFixtures`, `CorpusFilePath` and
  `ReadSourceSamples` expose the manifest to adapter runners.
- `examples/codec-adapters/jpeg2000`: corpus qualification report, truncation
  and working-memory tests and `BenchmarkJPEG2000Backends` (verified-output
  comparison of the core, legacy pure-Go and OpenJPEG backends). The core
  backend stays opt-in (profile decision in `PRODUCTION_PROFILE.md`).
- `pixeldata/encapsulated`: `JPEG2000` frame format with structural codestream
  framing (SOC, segment lengths, `Psot`, EOC) and one-byte Item padding.
- `net/ul`: `AcceptOptions.PreferProposedTransferSyntaxOrder` accepts the
  proposer's first supported transfer syntax per presentation context.
- `dicomdir`: bounded IMAGE file-set scan, validation, query/statistics,
  deterministic two-pass DICOMDIR authoring, strict portable File IDs,
  source revalidation, and atomic root publication with pydicom interop.
- `net/telemetry` and `net/ul`: opaque stable association IDs, safe structured
  UL/DIMSE lifecycle and command events, explicit endpoint HMAC policy, bounded
  opt-in raw P-DATA capture, atomic metrics, progress-based timeouts and a
  pre-negotiation bounded association server with graceful deadline shutdown.
- `net/dimse`: phase-specific `SCPControls`, panic/cancel-safe service queues,
  cancel grace for cooperative handlers, and correlated operation durations and
  status categories for C- and N-services.
- `net/dimse`: generic typed N-EVENT-REPORT, N-GET, N-SET, N-ACTION,
  N-CREATE and N-DELETE request/response models, context-aware SCU operations,
  shared-dispatch SCP handlers, preserved status-detail fields and opt-in
  bidirectional MPPS interoperability tests against pynetdicom.
- `examples/ndimse`: a synthetic in-memory N-GET exchange demonstrating the
  generic normalized SCU and SCP APIs.
- `video`: bounded, context-aware inspection and streaming extraction for DICOM
  MPEG, H.264, and HEVC transfer syntaxes, including fragment/container
  validation and deterministic frame timing metadata for application-owned
  native media backends.
- `gsps`: image/frame-specific Displayed Area selection with
  SCALE TO FIT/TRUE SIZE/MAGNIFY calibration and complete rectangular,
  circular, polygonal, and bitmap Display Shutter read/write/apply support,
  including strict malformed/unknown-field errors and exact P-Value, CIELab,
  and overlay-data preservation.
- `pixeldata/display`: a staged grayscale display pipeline (`Pipeline` /
  `RenderGray`) implementing the DICOM modality and VOI transforms — stored
  pixel extraction, Rescale Slope/Intercept, Modality LUT Sequence, Window
  Center/Width with LINEAR/LINEAR_EXACT/SIGMOID functions, and VOI LUT Sequence
  — with `*FromObject` extractors and invalid-LUT-descriptor reporting.
- `pixeldata/display` presentation stage: a `Presentation` transform modeled
  after the modality/VOI transforms covering MONOCHROME1 / Presentation LUT
  Shape inversion, DICOM overlays (groups 0x6000-0x601E) burn-in, and
  rectangular/circular/polygonal/bitmap display shutters with intersection
  semantics, plus Presentation LUT Sequence application. GSPS interpretation
  returns an explicit `ErrUnsupportedGSPS` limitation.
- `pixeldata/display` color path: `RenderColor` converts PALETTE COLOR (via
  red/green/blue palette LUTs), YBR_FULL and YBR_FULL_422, and RGB (interleaved
  or planar) to RGBA, with `PaletteFromObject` and a metadata-only
  `ColorMetadataFromObject` (Color Space and raw ICC Profile, no color
  management). Segmented palette LUTs are expanded from discrete, linear, and
  indirect segments. Unsupported photometric/layout cases return typed errors
  instead of misleading images, preserving the lightweight default dependency
  policy.
- `pixeldata/display`: `VOILUT.WindowByte` (window a single pre-sampled modality
  value) and `DecodeModality` (extract stored pixels to modality values for
  callers that re-window the same frame, such as render caches).
- `sr`: typed DICOM Structured Reporting primitives for Key Object Selection
  (KOS) and Basic Text SR — document metadata, the content-item tree
  (CONTAINER/TEXT/CODE/IMAGE/NUM), coded entries, image references, and numeric
  measurements — with dataset encode (`Document.Dataset`) and decode
  (`ReadDocument`) for synthetic round-trips.
- DIMSE Query/Retrieve: Study Root C-MOVE SCU implementation with status handling
  and a small `dicom-go-retrieve` CLI for interoperability testing.
- DIMSE Storage Commitment Push Model primitives: N-ACTION and N-EVENT-REPORT,
  including status classification helpers and transaction UID correlation helper.
- Documentation: Query/Retrieve and Storage Commitment design notes, conformance
  scope updates, and Orthanc interop setup guide.

### Changed


- roi: intensity statistics without mandatory sorting. Count,
  extremes, sum, mean and variance now come from one streaming pass
  (Neumaier's compensated sum, Welford's update shifted by the first value),
  the median from a linear-time selection, and the values are sorted only
  when a percentile is first asked for. The new `IntensityMoments` family
  (`NewIntensityMoments`, `IntensityAccumulator`,
  `VolumeMask.IntensityMoments`, `Segmentation3D.IntensityMoments`,
  `LabelIntensityMoments`, `IntensityStatistics.Moments`) skips the order
  statistics and holds no values. Order statistics are those of the former
  sort, to the last bit of the interpolation; sums and moments agree to
  rounding (pinned against the former computation on odd, even, duplicate,
  constant, offset and non-finite sets). `BenchmarkIntensityStatisticsLargeVOI`
  (2.6 million voxels in a 256 × 256 × 64 mask): 1.53 s, 128 MB in 44
  allocations before; 0.30 s, 21 MB in 2 allocations with the median; 0.09 s
  and no allocation for the moments.

- imagefilter, registration, mesh, roi, render: a shared concurrency budget. Parallel kernels run on their calling goroutine plus helpers
  reserved without waiting from one process-wide budget in
  `internal/parallel` (`GOMAXPROCS-1` helpers by default), so concurrent
  filters, registrations, mesh extractions and renders together no longer
  each start a goroutine per core; nested calls run inline instead of
  deadlocking. Work is claimed dynamically in chunks, so results do not
  depend on the helpers granted. N4 bias field correction gains
  `N4Options.Workers` (it used `GOMAXPROCS` unconditionally) and fits its
  B-spline over fixed sample blocks summed in order, so its field is the
  same for any worker count; an N4 correction and VR renders run together
  stay within the budget and match their results alone.

- render: cubic (Catmull-Rom) reslices whose rows run along one voxel axis
  of an untilted volume — axial, coronal and sagittal planes at any zoom or
  fractional position, and slabs along a voxel axis — precompute each axis's
  clamped taps, weights and voxel offsets once per row (once per frame for
  the varying axis, kept per worker while rows share it) instead of once per
  sample, after vtkImageReslice's per-axis weight tables. Samples
  read the same voxels with the same weights in the same order, so images
  are identical bit for bit (pinned on packed 16-bit, float32 and payload
  readers, a position-table volume, past-the-edge planes and every slab
  reduction); oblique rows, tilted volumes and CPR keep the per-sample path.
  `BenchmarkMPRCubicSeparable` (128³ CT phantom, one core, best of 8
  interleaved runs): 384² planes 29.4–29.9 → 8.3–8.4 ms, a 256² slab of 9
  samples 122.6 → 43.6 ms; the oblique `BenchmarkMPRInterpolation/cubic` is
  unchanged (12.6 ms).
- parser: a value the reader skips or defers (`SkipPixelData`,
  `DeferPixelData`, `DeferWaveformData`, deferred large values) is seeked past
  instead of read and discarded when the source is seekable with a known end
  (a regular file or a sized in-memory reader) and the value ends before the
  source end, its container and `MaxTotalBytes`. A header-only read
  of a file with 64 MiB of native Pixel Data reads 48 bytes instead of
  64 MiB; other sources, and values that would fail those bounds, are still
  read, so truncation and limit errors are unchanged.

- transfer: `DeflatedImageFrameCompression` is now a supported encapsulated
  Explicit VR Little Endian syntax (`Encapsulated`, `CodecAvailable`) instead
  of a dataset-deflated one; files in this syntax are no longer inflated as a
  whole, and their Pixel Data is read as fragments.


- Authored UI values are validated as UIDs (PS3.5 9.1) on every output (Part
  10, raw data set, DIMSE data sets, DICOM JSON, Native XML): letters, empty or
  leading-zero components, a root other than 0/1/2, a second arc above 39 under
  0/1, or more than 64 bytes return `core.ErrValueNotRepresentable`. Values read
  from existing files stay raw and are not re-validated; File Meta derived from
  a read UID keeps its bytes, so legacy files can still be rewritten and cloned.
  DIMSE command UIDs are encoded as given so legacy objects can be forwarded.
- `parser`: deferred-value locations are appended without a deduplication hash
  during the initial ordered parse; the index is built lazily on explicit
  rewind and retained across later generations without changing location order,
  ambiguity, replay, or caller-ownership behavior.
- DICOM UID syntax validation is shared by the generic validator, clinical
  relationship resolver, DICOMweb server, DICOMDIR, UPS and DIMSE storage
  planning. Canonical UIDs now consistently enforce OID root and second-arc
  constraints while each API retains its existing error or status contract.
- `net/dicomweb`: `Client.InstanceMetadata` now returns the raw DICOM JSON
  metadata datasets for one instance; callers that need an `InstanceRef` must
  obtain it from the returned dataset rather than receiving a projected value.
- `net/dicomweb`: `TokenManager.Logout` is terminal. Subsequent `Token` calls
  return `ErrBearerTokenUnavailable` and cannot reacquire credentials; callers
  must construct a new manager to authenticate again.
- `pixeldata/frame` grayscale rendering now delegates to `pixeldata/display`,
  removing the duplicated windowing/modality math while preserving output.
- `render`: VR transfer-function colors are now linearized from sRGB before DVR
  compositing and converted back to sRGB for display output; STL iso-surface
  export documents the intentional watertight voxel-boundary mesh policy instead
  of smoothing with marching cubes.
- Conformance and README limitations updated to reflect newly added DIMSE
  capabilities and remaining non-goals.

### Fixed

- net/dimse: C-GET responses carried the Failed SOP Instance UID List
  (0008,0058) in the command set, and C-MOVE responses omitted it. Both SCPs
  now send it in the Identifier of Cancel, Warning and Failure responses
  (PS3.4 C.4.2.1.4.2, C.4.3.1.4.2); `CMoveResponse` gains
  `FailedSOPInstanceUIDListOrNil`, and both receivers read the list from the
  Identifier (C-GET still accepts it from the command set of older peers).
- net/dimse: the C-MOVE SCP counted the C-STORE warning statuses 0001H,
  0107H and 0116H of a sub-operation as failures; they are warnings, as the
  C-GET SCP already counted them.
- storescp: data sets are read through the shared P-DATA reader
  (`dimse.NewPDataReaderWithPrefix` and `dimse.ReadPDataDataSet`, now
  exported) instead of the tool's own `object.ReadDataSetWithOptions`, so the
  PS3.5 A.5 pad byte or an empty last PDV after a Deflated data set is
  consumed with it rather than read as the next command. storescp now
  also accepts Deflated Explicit VR Little Endian, after the native syntaxes,
  and stores such instances in it. A test sends, on one association, Deflated
  objects with odd (padded, pad alone, unpadded) and even (empty last PDV, no
  pad) bitstreams, with and without data in the command's P-DATA-TF and with
  `-bit-preserving`; an opt-in test receives Deflated instances from DCMTK
  `storescu -xd`.
- examples/codec-adapters/jpeg2000: the default path decodes JPEG 2000 Part 1
  (.90/.91) with the core backend. go-jpeg2000 returned a flat image
  (every sample 2048 or 32768) for the base module's pure-Go encoder output
  and for OpenJPEG streams; it now only decodes Part 2. Committed OpenJPEG
  fixtures (`testdata/openjpeg`) and every pure-Go encoder case decode bit
  for bit through `Register`. The core ignores an SGcod transform flag on a
  stream with fewer than three components, as OpenJPEG does, instead of
  refusing it.

- dictionary/std: the tag dictionary is generated from PS3.6-2026d (DCMTK
  `dicom.dic` at `8d68392d`) instead of 2026b, the edition of the Storage SOP
  Class catalog, so (0040,E012) Display URI (UR) and (3004,007F) Dose
  Calculation Model Name (LO) are named. `std.Edition` records the
  edition; `cmd/dicomdictgen` reads it from the `dicom.dic` header, refuses a
  header without one or with different PS3.6 and PS3.7 editions, and pins the
  source URL to the commit given by `-commit`. A test fails when the tag
  dictionary and `uid.StorageCatalogEdition` diverge. The vendored file now
  has a provenance entry and notice.
- printmgmt: `Client.CreateFilmBox` returns the Film Box UID and image boxes
  when the SCP answers with a warning status (for example 0107H, optional
  attributes ignored); it used to return only the warning, leaving a created
  Film Box unusable.
- pixeldata/jpeglossless: a JPEG Lossless object labelled YBR_FULL_422 whose
  SOF3 has three full resolution (1x1) components was refused with
  `ErrUnsupportedPhotometricInterpretation`, although GDCM and DCMTK decode it. As PS3.5 8.2.1 lets the codestream control decompression, the
  samples now decode unconverted and are reported as YBR_FULL
  (`Frames.PhotometricInterpretation`, the decompressed data set and frame
  readers), with the diagnostic
  `jpeg-lossless-ybr-full-422-full-resolution`. gdcmData
  `JPEGLosslessYBR_FULL_422.dcm` matches GDCM's pixel MD5 and DCMTK `dcmdjpeg
  +cn`. Subsampled SOF3 components remain unsupported and now name the
  sampling factors in the error.
- net/dimse: `SendCFindRequest` wrote the C-FIND Identifier uncompressed on a
  presentation context negotiated as Deflated Explicit VR Little Endian, so
  the peer failed to inflate it (DCMTK `dcmqrscp`: "ZLib Error: invalid
  stored block lengths") and aborted the association. The
  Identifier now goes through the data set path every other DIMSE data set
  uses (`SendDataSet`): deflated with the PS3.5 A.5 pad and sent in even
  PDVs. C-MOVE and C-GET identifiers and the C-FIND, C-MOVE and C-GET
  response identifiers already used that path and are now covered by the
  same test.
- object, net/dimse: a C-STORE in Deflated Explicit VR Little Endian sent an
  odd-length last P-DATA fragment whenever the deflate bitstream was odd, and
  DCMTK `storescp` aborted the association ("DIMSE: Odd Fragment Length"). Writers now add the single trailing NULL byte PS3.5 A.5 requires
  after an odd bitstream (`WriteFile`, `WriteDataSet`,
  `WriteDataSetWithValidation`), and DIMSE data set senders pad a Deflated
  bitstream copied verbatim from a source file that lacks it, so every PDV
  is even (PS3.8 E.1). Receivers read a Deflated data set with or without
  the pad and consume it through its last fragment, including a pad or an
  empty PDV sent after the end of the bitstream, which was previously left
  on the association and read as the next message.
- realworld: Double Float Real World Value First Value Mapped is (0040,9214)
  and Last Value Mapped (0040,9213); the two tags were swapped, so a
  mapping whose range was written only in double float came out reversed
  and was rejected as invalid.
- examples/codec-adapters/jpeg2000: the default path (`New`, `Register`)
  decodes HTJ2K with the core backend instead of go-jpeg2000, whose HT
  decoder only read its own non-standard output and misdecoded OpenJPH
  codestreams (560 of 561 samples of an 8-bit image). Committed
  OpenJPH fixtures (`testdata/openjph`: 8/12/16-bit, gray and RGB, RPCL,
  irreversible) decode bit for bit; HT tests now encode with the base
  module's standard encoder.

- roi: intensity and segmentation statistics skip ±Inf as well as NaN, as
  documented, and report the skipped count (`IntensityStatistics.NonFinite`,
  `VolumeStats.NonFiniteCount`); `Segmentation3D.Statistics` previously let
  NaN and ±Inf into its moments and histogram.
- net/dicomweb: Basic authentication no longer clones the caller's transport
  on every request, which opened a new connection and TLS handshake each
  time; the transport is validated and reused.
- sr: angle measurements were exported as (121207, DCM, "Angle"), but PS3.16
  defines DCM 121207 as Height; they are now (1483009, SCT, "Angle"). Code
  Meanings follow PS3.16: measurement concepts those of their Context Group
  (CID 7470, 7471; `codes.ContextGroup.Member`), and the units those of the
  Context Groups, (mm, UCUM, "mm") and (deg, UCUM, "Degree").
- dicomdir: committing a DICOMDIR with icons failed for file-sets of more
  than a few dozen images: the read-back allowed 16 elements per record,
  fewer than an image record with its Icon Image Sequence has. Records with
  NUL-padded text (for example Study Time or Series Number), which some
  writers produce, were refused by the strict record validation; the
  padding is now trimmed.
- render: `VRInteractiveLOD.Observe` ignored a frame measured as taking zero
  time, which Windows' coarse monotonic clock reports for small previews, so
  the interaction scale sometimes stayed at its floor after a fast frame. A
  zero time now counts as the fastest frame.
- net/ul, net/dimse: receiving a data set cost about a millisecond more per
  PDU as the transfer went on: the P-DATA reader added a context layer for
  every PDU, and each deadline and value lookup walked the growing chain,
  so 64 MiB in 4 KiB PDUs took 19 s. `WithActiveTransfer` is now idempotent
  and the per-read cancellation watcher uses `context.AfterFunc` instead of
  a goroutine; the same transfer takes 0.2 s.


- Overlay Data (60xx,3000) stored as OW in Big Endian data sets is read with
  its bit packing normalized.
- Pixel data decompression no longer refuses a File produced by a transfer
  syntax recovery reader because File Meta still names the recovered syntax.
- The transfer syntax probe no longer lets a few dictionary VR mismatches
  make a long explicit VR prefix ambiguous.
- Encapsulated JPEG, JPEG-LS and JPEG 2000 frames accept one trailing pad
  byte of any value when it makes the frame even (PS3.5 A.4 does not fix the
  value; Clunie and LEADTOOLS samples pad with `FF`/`20`), in the frame plan,
  EOT lengths and the streaming assembler alike; `View.Padded`/`PadByte`
  report it. Five gdcmData JPEG Lossless files now match GDCM's pixel MD5.
- JPEG Baseline/Extended colour decoding reconstructs 4:2:2 and 4:2:0 chroma
  with a triangular filter instead of nearest-neighbour replication and uses a
  fixed-point full-range BT.601 transform: the maximum difference to DCMTK on
  `US-GE-4AICL142.dcm` drops from 78 to 3 levels (PSNR 35 to 64 dB).
- `net/dimse`: an `AsyncSession` performing within a finite window no longer
  rejects a compliant peer's next request as a window violation when that
  request's admission races the release of a slot by a just-completed terminal
  response; slot attempt and registry scan now share one lock. Admission that
  fails because the session is closing is no longer counted as a violation.
- `sr`: tracking UIDs derived from the report UID are always valid; a report
  UID that is invalid or too long to extend yields a deterministic `2.25` UID.
- `net/dimse`: a Storage Commitment committer answers N-ACTION with Resource
  limitation 0213H instead of Processing failure 0110H when a configured bound
  or a full transaction store refuses the request.
- `parser`: items, sequences, values, fragments and element headers that
  would cross their enclosing defined-length container now fail with
  `ErrContainerOverrun` before any byte outside the container is read, instead
  of later after misreading the following elements.
- `parser`: Explicit VR writers now write undefined-length UN sequences as
  PS3.5 6.2.2 defines (Implicit VR Little Endian items) instead of refusing
  them, and values inside such sequences in a Big Endian data set are held in
  the data set's byte order, so reading, writing and Big/Little Endian
  transcoding keep them; round trips are byte-exact.
- `dicomjson`, `parser`: LT/ST/UT/UR are single-valued; several JSON values or
  authored components under them are refused instead of being joined into one
  value with a backslash, and a JSON value containing the backslash delimiter
  under a multi-valued VR or PN is refused. IS/DS diagnostics no longer echo
  the value.
- `net/dimse`: Deflated Explicit VR Little Endian path sources are also offered
  as native Little Endian (no pixel codec) and their re-encoding no longer
  fails on deferred replay or a file-size byte budget; the peer's choice
  decides the wire syntax.
- `object`, `net/dimse`: top-level Pixel Data that contradicts the Transfer
  Syntax (native bytes under an encapsulated syntax or fragments under a native
  one) is refused by the writers (`ErrPixelDataEncodingMismatch`) and answered
  with C000 by the Storage SCP and C-GET receiver instead of being persisted
  under a false syntax.
- `transfer`: RLE Lossless joins the preservable receive syntaxes; a receiver
  using `ReceiveTransferSyntaxUIDs` no longer rejects RLE associations.
- `examples/referencearchive`: compressed instances are stored and retrieved in
  their own syntax, with DCMTK and pydicom interop evidence.

- `object`: Implicit VR reads resolve "US or SS" attributes (for example
  Smallest Image Pixel Value, Pixel Padding Value, LUT Descriptor) from the
  Pixel Representation of the same data set or nearest enclosing one, so
  Implicit->Explicit conversion no longer declares signed values as US.
  `Object.ResolveContextualVRs` resolves empty VRs of authored contextual
  attributes and returns `ErrAmbiguousVR` instead of guessing.
- `parser`, `dicomjson`, `dicomxml`: authored text values are checked against
  their VR (`core.ErrValueNotRepresentable`): text under a binary VR such as US
  and IS/DS values violating syntax, length or the IS 32-bit range are
  rejected instead of written. `dicomxml` now round-trips empty IS/DS
  components.
- `net/dimse`: an association that ends before the last P-DATA fragment is
  reported as `io.ErrUnexpectedEOF`; a truncated data set is no longer
  returned as complete.
- `pixeldata`: Big Endian conversion reverses native OW Pixel Data with Bits
  Allocated 32/64 at the sample width (1249000 was stored as 250085395) and
  uses each item's own Bits Allocated.
- `object`/`parser`: `DeferPixelData` defers only top-level Pixel Data; a
  nested Icon Image Sequence value is materialized and no longer makes the
  top-level value ambiguous.
- `deid`: explicit burned-in pixel redaction now supports RGB native pixel data
  with `PlanarConfiguration=1`, masking the requested rectangle in each color
  plane instead of rejecting planar images.


Initial public release of `dicom-go`.

### Added

- M0: repository architecture, package boundaries, Makefile quality targets and
  contribution baseline.
- M1: dataset parser support for native transfer syntaxes, sequences, items,
  fragments and defensive read limits.
- M2: writer support for raw elements, datasets and Part 10 files.
- M3: generated standard tag dictionary and standard UID registry.
- M4: native uncompressed pixel data metadata and frame extraction.
- M5: DICOM Upper Layer PDU codec and association negotiation.
- M6: DICOM JSON marshal/unmarshal support.
- M7: minimal DIMSE C-ECHO and C-STORE support with SCU/SCP helpers.
- M8: user documentation, runnable examples, conformance scope and release
  notes.
- Part 10 file read/write through the `object` package.
- Native transfer syntax support for Implicit VR Little Endian, Explicit VR
  Little Endian and Explicit VR Big Endian.
- Encapsulated Pixel Data fragment parsing/preservation.
- Optional DICOM RLE Lossless codec in `pixeldata/rle`.
- CLI tools: `dcmdump`, `echoscu`, `echoscp`, `storescu`, `storescp`,
  `dicomuidgen`, and dictionary generation tools.
- Runnable examples under `examples/`.

### Known Limitations

- No claim of full DICOM conformance.
- No TLS or user identity negotiation.
- Does not include C-FIND, C-MOVE, C-GET, N-* services or Storage Commitment.
- No automatic transcoding.
- Not supported: JPEG, JPEG-LS, JPEG 2000, JPEG XL, MPEG or HEVC pixel decoding.
- Parser limits are opt-in and should be configured for untrusted input.

# dicom-go

[JPEG 2000 Part 2 encoding](docs/JPEG2000_PART2_ENCODER.md) supports explicit
bounded grayscale component collections, with independent composed-decoder
evidence and provisional interoperability status.

[Displacement inspection contracts](docs/DEFORMATION_INSPECTION.md) preserve original direct-field nodes and bounded regional presentation metadata.

[Surface volume probe materials](docs/SURFACE_PROBE.md) define scalar-source registration, normal bands and color ownership.

[Per-vertex geometry colors](docs/VR_GEOMETRY_COLORS.md) retain linear RGB attributes with explicit validation and a headless library oracle.

`dicom-go` is a Go library that supports resource-bounded DICOM file and dataset processing,
metadata interchange, pixel pipelines, DIMSE and DICOMweb networking, headless
rendering, and reusable clinical-object workflows.

The module favors explicit options, concrete value types, caller-owned
registries, context-aware operations, and typed errors. Application catalogs,
credentials, job orchestration, audit retention, and user interfaces remain the
responsibility of applications built on the library.

[Progressive preview source and geometry](docs/GPU_PROGRESSIVE_PREVIEW_PLAN.md)
provides metadata and retained original even slices for device preparation,
without constructing a CPU-derived preview.

This project does not claim complete DICOM conformance. Review the maintained
[capability and limitation scope](./docs/CAPABILITIES.md) before processing
untrusted data or connecting to production peers.

[Incremental derived image and SEG writing](docs/INCREMENTAL_DERIVED_EXPORTS.md)
provides explicit source ownership, replay/buffer limits and original measured
memory/time evidence alongside the compatible in-memory builders.

[Experimental 2D phase and wavelet analysis](docs/PHASE_WAVELET_2D.md) exposes
bounded physical coefficients and reconstruction. Its fixed synthetic ridge
evaluation found no advantage over existing Frangi and does not create a preset.

[Context-aware headless VR empty-space skipping](docs/VR_EMPTY_SPACE_CONTEXT.md)
combines gradient, label and registered-overlay bounds with exact-image fixtures
and measured synthetic performance.
[CPR lumen measurements](docs/CPR_LUMEN_QUANTIFICATION.md) provide physical area/diameters, selected-reference reductions, graphs and SR export.
[RGB and independent volume components](docs/VOLUME_COMPONENTS.md) define interleaved source ownership and component presentation contracts.
[Vessel-guided CPR](docs/CPR_VESSEL_GUIDANCE.md) derives bounded vesselness for cancellable path analysis.
[Crop region controls](docs/VR_CROP_REGIONS.md) support subvolume, fence, cross and their inverses.
[Colored maximum-intensity projection](docs/VR_COLORED_MIP.md) classifies the
raw modality maximum through RGB transfer functions, with opaque projection output.

[Bounded morphological contour proposals](docs/MORPHOLOGICAL_CONTOURS.md) provides opt-in component correspondence, physical bounds and reviewed segment edits.

## Install

The base module requires Go 1.22 or newer.

```sh
go get github.com/ThalesMMS/dicom-go
```

The default module is pure Go. Optional codec adapters and their runtime
requirements are documented in the
[codec capability matrix](./docs/CODEC_CAPABILITY_MATRIX.md) and
[dependency policy](./docs/CODEC_DEPENDENCY_POLICY.md).

## Quick start

### Read and write a Part 10 file

`object` is the primary high-level package for Part 10 files and datasets.
Opened files must be closed because deferred-value options may retain the input
file.

This minimal example assumes a locally trusted input. For untrusted input, use
`object.OpenFileWithOptions` and set non-zero `MaxTotalBytes`,
`MaxElementBytes`, `MaxPixelDataBytes`, `MaxDeformableVectorGridBytes`,
`MaxSequenceDepth`, `MaxElements`, and `MaxFragments` budgets appropriate to
the deployment; zero numeric limits are unlimited.

```go
package main

import (
	"fmt"
	"os"

	"github.com/ThalesMMS/dicom-go/object"
)

func main() {
	file, err := object.OpenFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer file.Close()

	metadata := file.Metadata()
	fmt.Printf("SOP class: %s\nTransfer syntax: %s\n",
		metadata.SOPClassUID, metadata.TransferSyntaxName)

	output, err := os.Create("copy.dcm")
	if err != nil {
		panic(err)
	}
	if err := object.WriteFile(output, file); err != nil {
		_ = output.Close()
		panic(err)
	}
	if err := output.Close(); err != nil {
		panic(err)
	}
}
```

Use `object.OpenFileWithOptions` for explicit parsing and resource policy. Raw
datasets require an explicit transfer syntax. Transfer-syntax recovery is a
separate opt-in API; strict readers never infer it automatically. See
[Transfer Syntax Recovery](./docs/TRANSFER_SYNTAX_RECOVERY.md).

### Encode DICOM JSON and Native DICOM Model XML

Both encoders operate on the same `*object.Object`. Native XML uses finite
defaults. For untrusted DICOM JSON, set every `dicomjson.Limits` field
explicitly because zero limits retain compatibility behavior.

```go
package main

import (
	"fmt"
	"os"

	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dicomxml"
	"github.com/ThalesMMS/dicom-go/object"
)

func main() {
	file, err := object.OpenFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer file.Close()

	jsonData, err := dicomjson.Marshal(file.Dataset, dicomjson.DefaultOptions())
	if err != nil {
		panic(err)
	}
	xmlData, err := dicomxml.Marshal(file.Dataset, dicomxml.DefaultOptions())
	if err != nil {
		panic(err)
	}
	fmt.Printf("DICOM JSON: %d bytes; Native XML: %d bytes\n", len(jsonData), len(xmlData))
}
```

Bulk-data references are preserved without implicit network or filesystem
access. Resolution must be configured explicitly and remains resource-bounded.

Runnable examples cover [file reading](./examples/readfile),
[file writing](./examples/writefile), [DICOM JSON](./examples/json),
[pixel data](./examples/pixeldata), [rendering and ROI](./examples/render-roi),
[DIMSE echo](./examples/echo), and [DIMSE storage](./examples/store).

## Choose the right package

### Dataset and file foundation

| Package | Use it for |
|---|---|
| `dicom` | Compatibility facade over common file/dataset read, write, and validation APIs; `object` owns the high-level implementation. |
| `core` | Tags, VRs, lengths, elements, datasets, sequences, fragments, and primitive values. |
| `object` | High-level datasets, Part 10 read/write, metadata access, cloning, walking, and deferred values. |
| `parser` | Token-level dataset parsing and writing with explicit syntax and limits. |
| `encoding` | Endian-aware primitive and DICOM character-set helpers. |
| `transfer` | Transfer-syntax definitions, lookup, negotiation helpers, and registries. |
| `dictionary`, `dictionary/std`, `dictionary/tags`, `dictionary/uid` | Dictionary contracts, generated standard data, common tags, and UID lookup, including the Storage SOP Class catalog (PS3.4 B.5-1, B.6-1, GG.3-1 and PS3.6) with category, Modality and a bounded SCU subset. |
| `dcmtime`, `personname` | Typed DICOM DA/TM/DT and PN parsing. |
| `validation` | Opt-in element/dataset validation and lifecycle hooks with structured findings. |
| `dicominspect`, `index` | Bounded, presentation-neutral inspection and detached metadata indexing. |

### Interchange, filesets, and derived objects

| Package | Use it for |
|---|---|
| `dicomjson`, `dicomxml` | PS3.18 DICOM JSON and PS3.19 Native DICOM Model XML (`cmd/dcm2json`, `json2dcm`, `dcm2xml`, `xml2dcm`). |
| `dicomdir` | Bounded DICOM file-set reading, validation, querying, and transactional writing. |
| `deid` | Conservative metadata de-identification and Basic Application Confidentiality Profile workflows. |
| `dsig` | PS3.15 digital signatures: verify every signature of a data set or its items, sign with RSA or ECDSA, check the RSA signature profiles and signer trust, and request and verify RFC 3161 certified timestamps. |
| `encapdoc` | Encapsulated PDF, CDA, STL, OBJ and MTL construction, payload extraction and [explicit model metadata reading](docs/MANUFACTURING_MODELS.md); [bounded inert CDA narrative reading](docs/CDA_NARRATIVE.md). |
| `encapimage` | Secondary Capture and VL Photographic objects around an existing JPEG, without re-encoding (`cmd/jpg2dcm`). |
| `multiframe` | Enhanced CT/MR/PET to classic single-frame conversion, classic series to Legacy Converted Enhanced, and Siemens mosaic split. |
| `siemens` | Siemens CSA headers (parse, encode, scrub), ASCCONV protocol, MedCom History, and mosaic slice counts and normals. |
| `mrdiffusion` | Per-frame MR diffusion b-value and gradient direction with their source (MR Diffusion Macro, classic standard attributes, Siemens CSA and MR header, GE, Philips, Toshiba/Canon Image Comments), and log-linear ADC fitting. [Offline DTI reconstruction](docs/DTI_TENSOR_RECONSTRUCTION.md) adds explicitly oriented LPS tensors, FA/MD/eigenvalues/direction/RGB and eight scalar research Parametric Maps with source-frame provenance. |
| `vendors` | Manufacturer detection and bounded decoders for GE Protocol Data Block, ELSCINT1 protocol, ELSCINT1 PMSCT_RLE1/PMSCT_RGB1 private pixel data and VEPRO private blobs (Siemens CSA and MedCom History are in `siemens`). |
| `papyrus` | Opt-in, reported conversion of Papyrus 3.0 files into one standard instance per image (`cmd/pap2dcm`). |
| `seriesderive` | Non-destructive derived-series copies. |
| `derivation` | Source Image, Derivation Code and Derivation Description records (top level or Derivation Image functional group), shared by lossy transcoding, `multiframe`, `seriesderive` and `seg`. |
| `nifti` | Geometry-validated NIfTI export, NIfTI-1/2 import to DICOM LPS geometry, and labelmap-to-SEG conversion. |
| `nrrd` | NRRD read/write (raw, gzip, bzip2, text, hex; detached headers) in DICOM LPS geometry, NIfTI-plan export, and 3D Slicer `.seg.nrrd` read (segments to SEG) and write (DICOM segments to Slicer segment keys). |
| `metaimage` | MetaImage (.mha/.mhd) read/write in DICOM LPS geometry, with zlib compression, and NIfTI-plan export. |

### Pixel data, rendering, and ROI

| Package | Use it for |
|---|---|
| `pixeldata` | Pixel metadata, native/encapsulated frame extraction, codec registries, decompression, and transcoding. |
| `pixeldata/builtin` | A fresh caller-owned registry of the built-in pure-Go decoders. |
| `pixeldata/frame`, `pixeldata/display` | Typed frame decoding and the DICOM grayscale/color display pipeline. |
| `pixeldata/compare` | Pixel comparison metrics (maximum and mean absolute error, RMSE, PSNR, SNR) as DCMTK dcmicmp reports them (`cmd/dcmdiff -pixels`). |
| `pixeldata/jpeg`, `pixeldata/jpeglossless`, `pixeldata/jpegls`, `pixeldata/rle`, `pixeldata/deflateframe` | Built-in JPEG, RLE and Deflated Image Frame Compression codec implementations for their documented subsets. |
| `pixeldata/jpeg2000` | Pure-Go JPEG 2000 Part 1 and HTJ2K encoder (lossless and lossy); decoding is in the optional JPEG 2000 adapter. |
| `pixeldata/hologic` | Explicit opt-in reader for the bounded [legacy Hologic private JPEG-LS layout](docs/HOLOGIC_LEGACY_JPEGLS.md). |
| `pixeldata/codecfixture`, `pixeldata/codecprofile` | Synthetic conformance fixtures and auditable codec-profile metadata. |
| `iconimage` | Embedded preview icons: the Icon Image Sequence and the GEIIS and GEMS_Ultrasound_ImageGroup_001 private icons, rendered without decoding the image. |
| `render` | Headless 2D rendering, stacks, MPR/MIP/slabs, CPR (fast-marching centrelines), VR (with render-only prefilters, including multiscale vessel enhancement for angiography), encoded images, box-filtered display pyramids, and bounded volume storage. Opt-in [topological fast marching](docs/FAST_MARCHING_TOPOLOGY.md) declares strict or no-handle cubical constraints, finite resources and incomplete-front diagnostics. |
| `render/qualification` | PHI-free renderer qualification inputs, traces, and comparison evidence, including neighbourhood-tolerant frame comparison against alternative baselines with difference images. |
| `pet` | Classic PET calibration metadata and body-weight SUV conversion from modality-rescaled values. Interpret the result for the input instance. Callers applying it across a series must establish that the calibration is applicable to all selected instances. Enhanced per-frame calibration is outside this scope. |
| `render/colorbar` | Calibrated colour legends (scalar bars): VOI window ranges, nice ticks, exact value-to-colour mapping for palettes after windowing, discrete band legends, and PET Units labels. |
| `mesh` | [Bounded STL, OBJ and MTL readers](docs/MANUFACTURING_MODELS.md) with explicit physical scale; iso-surfaces as indexed triangle meshes in patient LPS mm, from a `render.Volume`, a volume snapshot or any scalar field: Marching Cubes with a generated face-consistent case table (watertight, consistently oriented, closed at the grid boundary by default, gradient normals) or the voxel-exact faces; streamed slice by slice on parallel slabs, deterministic, with exact triangle and byte budgets checked before allocation and cancellation. Export as binary STL, PLY, OBJ and binary glTF 2.0 (`.glb`, coloured parts); per-vertex ambient occlusion; closest-point, signed-distance and ray queries, surface-to-surface distances (Hausdorff, mean, percentiles) and geodesic paths along surfaces. One-plane patient-space sections and bounded closed clipping preserve concave shells/cavities, distinguish generated cap faces and export derived geometry; see [plane clipping](docs/MESH_PLANE_CLIPPING.md). Bounded solid union, intersection and A minus B preserve source-face provenance and corner attributes with explicit ambiguity refusal; see [mesh Boolean operations](docs/MESH_BOOLEAN.md). |
| `mesh` point clouds | Bounded statistical candidates and opt-in distance reduction with protected points, reproducible selection and detached attribute/index maps; [processing and rejected reconstruction experiment](docs/POINT_CLOUD_PROCESSING.md). |
| `imagefilter` | Display-domain enhancement on post-VOI values: Zuiderveld CLAHE for 8–16-bit images (tiles, clip limit, bins, cancellation, parallel). Recursive (IIR) Gaussian smoothing and derivatives of float32 volumes and 2D images: σ in mm per axis, orders 0–2, gradient magnitude, Laplacian and Hessian, scale normalization, O(n) in σ, parallel, cancellable. Histogram matching by quantile landmarks (adapted from ITK's HistogramMatchingImageFilter): thresholded intensity histograms, reusable reference histograms, a monotonic piecewise-linear mapping for matching a follow-up MR to its prior so one window serves both; a display/analysis transform, never written to stored pixels. Multiscale Hessian vesselness after ITK: Frangi objectness (tubes, blobs, plates; automatic or fixed γ) and Sato line measure, bright or dark objects, best-scale map, closed-form 3×3 eigenvalues, z slabs with a 6σ margin under a working-memory bound, parallel, cancellable. Edge-preserving denoising and sharpening: Perona–Malik and curvature (MCDE) anisotropic diffusion (after ITK, time step in mm²), bilateral filter with σ in mm and a range σ in modality units, and a Gaussian unsharp mask with an optional threshold. Median filtering in a box with the radius in mm (Huang sliding histogram for integer data, quickselect otherwise), exact against brute force. Slab-wise execution of neighbourhood filters (median, bilateral, non-local means, in-plane unsharp) from a slice reader to a slice writer with a z halo under a working-memory bound, bit-identical to whole-volume filtering; anisotropic diffusion bounds its scratch with slab buffers. Blockwise non-local means (Coupé et al.) with patch and search radii in mm, block step, noise σ and β, 2-D or 3-D, slab-streamable. N4 bias field correction (a modified port of ITK's N4): the smooth multiplicative non-uniformity of surface coils estimated by histogram sharpening and a multilevel cubic B-spline on a block-averaged grid, then divided out; optional mask, cancellable. [Known-PSF Richardson–Lucy restoration](docs/PSF_DECONVOLUTION.md) supports bounded nonnegative 2D/3D research data with explicit forward model, borders and derived provenance; no default or clinical enhancement. |
| `imagefilter` Poisson research denoising | Opt-in count-domain quadratic MAP with explicit gain, nonnegative domain, bounded deterministic sweeps, cancellation and derived provenance; [model, bias, synthetic qualification and measured time/RSS](docs/POISSON_DENOISING.md). No Rician or clinical/viewer preset. |
| `imagefilter` tensor diffusion | Explicit physical CED/EED with a justified variational stability bound, finite resources and derived provenance; [numerical qualification, measured structures and overshoot limits](docs/TENSOR_DIFFUSION.md). |
| `roi` | Vector/raster ROI geometry, measurement, segmentation, interpolation, and statistics, including physical-space sphere/ellipsoid/box VOIs with partial-volume 3D statistics and the PERCIST peak (SUVpeak); 6/18/26-connected components and seeded region growing (connected threshold, confidence, neighbourhood and isolated, after ITK) with voxel budgets and cancellation; multi-rater consensus by label voting and binary or multi-label STAPLE (after ITK) with per-rater performance. Grayscale morphology of float32 volumes: reconstruction by dilation/erosion, regional and extended maxima/minima, h-maxima/h-minima, grayscale hole filling, ball erosion/dilation/opening/closing and white/black top-hats in mm; marker-controlled watershed (with or without lines, optionally masked), component labelling and splitting of touching objects by distance-map watershed; bounded and cancellable. 3D curve skeletons (Lee thinning after ITK), spur pruning and skeleton centrelines. Opt-in scalar connected SLIC partitions in physical space ([profile and qualification](./docs/SLIC_PARTITION.md)). Bounded scalar Chan–Vese regional-energy proposals with explicit initialization, physical units and scoped ITK comparison ([profile and measured limits](./docs/CHAN_VESE.md)). |
| `roi` binary anti-alias | Opt-in bounded projected physical-curvature surface estimate with owned source/field and RMS stopping; [model and synthetic surface/cost/RSS qualification](docs/BINARY_ANTIALIAS.md). No volume/topology/clinical guarantee. |
| `roi` medial thickness | Bounded sparse curve-skeleton thickness in mm with original-component distributions and undefined values outside the skeleton. [Contract, independent primitive comparisons and measured discretization bias](docs/MEDIAL_THICKNESS.md). |
| `roi` local texture | Bounded voxel-wise GLCM contrast, joint entropy, angular second moment and inverse difference moment with physical rectangular windows, masked pair endpoints, explicit quantization/aggregation and regional halos. [Contract and independent ITK comparison](docs/LOCAL_TEXTURE_MAPS.md). |
| `roi` probabilistic classification | Scalar Gaussian-mixture EM, explicit Bayes priors and physical six-neighbor Potts mean-field posteriors, with finite admission, cancellation and detached proposals. [Profile and qualification](./docs/PROBABILISTIC_CLASSIFICATION.md). |
| `roi/autothreshold` | Automatic histogram thresholds (Otsu, multi-Otsu, Li, Triangle, Yen, entropy and others), global or inside a mask. |
| `roi/calcium` | Coronary artery calcium scoring of non-contrast CT: Agatston, volume and mass scores per vessel (LM, LAD, LCX, RCA, other) from lesions assigned by the reader, with increment normalization, contrast refusal and a TID 1500 report. |
| `video` | Validation and streaming extraction of encapsulated video, fragmented MP4 remux of H.264/HEVC, and [explicit-backend AVC decode/transcode](docs/VIDEO_CODEC.md) in a bounded classic MP4 profile. |
| `jpip` | Policy-bounded retrieval and decode of JPIP-referenced pixel streams, a JPIP server with data-bin indexing (JPP/JPT-streams, sessions, bounded resources), and data-bin reassembly ([server](./docs/JPIP_SERVER.md)). |

For native frame delivery with bounded channel backpressure and consumer
cancellation, see [frame channel streaming](docs/FRAME_CHANNEL_STREAMING.md)
and the [runnable example](examples/stream-native-frames/main.go).

### Clinical objects and workflows

| Package | Use it for |
|---|---|
| `clinicalrelation` | Normalized references and relationships among clinical DICOM objects. |
| `dsa`, `dynamic` | Mask Module subtraction planning (AVG_SUB, TID, REV_TID, frame averaging, pixel shift) and temporal/spatial acquisition modeling. [Bounded temporal scalar maps](docs/TEMPORAL_MAPS.md) provide explicit statistics and causal/centered moving targets on corresponding grids. |
| `gsps`, `vps` | Grayscale Softcopy and volumetric presentation-state read/write/application models. |
| `hangingprotocol` | Bounded, viewer-neutral Hanging Protocol decoding. |
| `microscopy` | Tiled whole-slide microscopy pyramids, tiles, channels, annotations, and viewport composition. [Registered scalar acquisition mosaics](docs/REGISTERED_TILE_MOSAICS.md) add bounded neighbour translation estimation, global cycle qualification and regional derived composition. |
| `parametricmap`, `rtdose` | Parametric Map and RT Dose parsing, lazy sampling, and analysis. |
| `realworld` | Opt-in Real World Value Mapping (linear and LUT, with units, per frame) and the Philips MR private scale. |
| `rtstruct`, `seg` | RT Structure Set and Segmentation read/write plus ROI conversion. |
| `surfaceseg` | Surface Segmentation Storage read/write with validation: segments, surfaces (points, normals, triangles; strips, fans and facets on read), colours, opacity, generation algorithms and references, round-tripping `mesh.Mesh` and `mesh.Part`. |
| `spatialreg` | Affine and deformable spatial registration parsing and application; Spatial Registration Storage writing from validated matrices. Regional, cancellable Jacobian (volume change) of deformable grids, or a summary that does not allocate the grid. |
| `registration` | Parametric 3D transforms for image registration after ITK's layouts: Euler (ZXY/ZYX), versor rigid, similarity, affine and composite, with parameter and position Jacobians, centre of rotation as fixed parameters, inverses, and conversion to and from the DICOM Frame of Reference Transformation Matrix (which is the inverse of the fixed-to-moving transform). Landmark fits (rigid by Horn's quaternion, similarity, affine) with residuals and FRE, and iterative closest point between point clouds. Mean squares, normalized correlation and Mattes mutual information metrics between images, with masks, sampling and exact parameter gradients. [Local NCC and greedy SyN](docs/SYN_NEIGHBORHOOD_CORRELATION.md) add full local derivatives, bounded halfway fields, inverse/undefined-domain diagnostics and Windows synthetic ITK/ANTs qualification. [Masked FFT NCC translation proposals](docs/MASKED_FFT_NCC.md) use two independent masks, actual overlap, physical search bounds and explicit ambiguous/border diagnostics in 2D/3D. [Local Gaussian point-cloud objectives](docs/GAUSSIAN_POINTSET_REGISTRATION.md) support unpaired density matching with physical sigma, bounded top-K neighbors and exact piecewise gradients. [Regularized 3D landmark RBF warps](docs/LANDMARK_WARP.md) expose fit diagnostics, Jacobians and fold inspection without claiming an inverse or diffeomorphism. [Weighted image and paired-point objectives](docs/MULTI_METRIC_REGISTRATION.md) share a transform and expose per-component contributions, constant scales, finite limits and cancellation. Resampling of a moving image onto a fixed grid through a transform, displacement field or Spatial Registration mapping (trilinear, nearest or Gaussian label interpolation, explicit padding) and voxel-wise subtraction in modality units. Rigid motion correction of dynamic (4D) series: every phase registered to a reference phase and resampled onto its grid, streamed and cancellable. |
| `optimize` | Minimizers for registration: regular-step gradient descent with a windowed convergence monitor, Nelder–Mead, Powell and the (1+1) evolution strategy, with per-parameter scales (`registration.PhysicalShiftScales`). |
| `sr` | Structured Reporting documents, templates, references, measurement reports, and KOS content. |
| `sr` semantic presentation | Bounded continuous/separate narrative, verification and predecessor metadata, by-reference identity and preserved technical traversal; [shared presentation contract](docs/SR_SEMANTIC_PRESENTATION.md). |
| `codes` | PS3.16 terminology generated from its DocBook: named DCM, SCT, UCUM, UMLS and LOINC codes (`codes/dcm`, ...), lookup of every code, and the Context Groups with members, inclusions and extensibility (`sr.StandardContextGroups` feeds them to template validation). |
| `ultrasound` | Ultrasound region calibration, Doppler measurements, and biometry. |
| `ups` | Unified Procedure Step state, query, subscription, event, and delivery workflows. |
| `mpps` | Modality Performed Procedure Step client and opt-in SCP lifecycle over normalized DIMSE. |
| `ian` | Instance Availability Notification builder, validator, client and opt-in SCP over normalized DIMSE. |
| `printmgmt` | Basic Grayscale/Color Print Management client and opt-in SCP with an injectable print backend. |
| `waveform` | Calibrated waveform reading, sampling, annotations, display layout, and [bounded Basic Voice/General Audio PCM](docs/AUDIO_WAVEFORM.md). |
| `qrmatch` | PS3.4 Query/Retrieve matching independent of a database. |

### Networking

| Package | Use it for |
|---|---|
| `net/ul` | Upper Layer PDU codecs, association negotiation, TLS hooks, User Identity items, and bounded servers. |
| `net/dimse` | DIMSE commands, SCU/SCP helpers, Query/Retrieve, normalized services, async sessions, and Storage Commitment. |
| `net/dicomweb` | QIDO-RS, WADO-RS, STOW-RS, and UPS-RS (with WebSocket event channels) client/server primitives with typed errors and finite limits. |
| `net/dicomweb/oidc` | Optional OpenID Connect discovery, PKCE/device logins, refresh-token `TokenRefreshFunc`, and JWT/JWKS verification (standard library only). |
| `net/telemetry` | PHI-safe operational events for UL and DIMSE. |
| `net/audit` | Optional structured audit-event seam for service wrappers. |

Network packages implement protocol mechanics, not deployment policy. DIMSE
defaults to plain TCP unless callers supply TLS configuration. DICOMweb Basic
authentication requires certificate-verified HTTPS. Applications must provide
authorization, protected credential storage, endpoint policy, durable audit
retention, retries, and user-facing job state.

## Commands

Run command help before connecting to a peer:

```sh
go run ./cmd/dcmdump -h
go run ./cmd/dicomweb -h
go run ./cmd/echoscu -h
go run ./cmd/storescu -h
go run ./cmd/findscu -h
go run ./cmd/dicom-go-retrieve -h
go run ./cmd/dicom-go-codify -h
```

File tools to transcode, de-identify, write a DICOMDIR, compare files,
extract or insert raw values, scan attributes of a tree and summarize a file
are described in [docs/CLI_TOOLS.md](docs/CLI_TOOLS.md),
together with tools to render frames (`dcm2jpg`), validate IODs
(`dcmvalidate`), convert between Enhanced and single-frame images
(`emf2sf`) and between series, NIfTI volumes and Segmentations (`dcm2nii`,
`nii2seg`, `seg2nii`), the MPPS, Storage Commitment, UPS and Instance Availability
Notification requestors (`mppsscu`, `stgcmtscu`, `upsscu`, `ianscu`) and
`qrscp`, a read-only Query/Retrieve SCP over indexed folders for tests
without a PACS.

The command tree also contains SCP counterparts, UID and dictionary utilities,
codec-manifest tooling, and repository-maintenance generators. Live-peer
interoperability is opt-in; follow the
[Orthanc runbook](./docs/INTEROP_ORTHANC.md) and
[interoperability matrix](./docs/INTEROP_MATRIX.md). Never use real patient data
in repository fixtures or default tests.

## Authoritative documentation

- [Current capability and limitation scope](./docs/CAPABILITIES.md)
- [Authored GDCM quirk corpus, strict/recover stage matrix and gaps](./docs/GDCM_QUIRK_CORPUS.md)
- [Architecture and dependency direction](./docs/ARCHITECTURE.md)
- [Validation](./docs/VALIDATION.md), [character sets](./docs/CHARACTER_SETS.md),
  [metadata indexing](./docs/DICOM_METADATA_INDEX.md), and
  [DICOM file-sets](./docs/DICOM_FILE_SETS.md)
- [Codec capability matrix](./docs/CODEC_CAPABILITY_MATRIX.md),
  [codec dependency policy](./docs/CODEC_DEPENDENCY_POLICY.md),
  [pixel transcoding](./docs/PIXEL_TRANSCODING.md), and
  [stored samples](./docs/STORED_SAMPLES.md)
- [Asynchronous DIMSE](./docs/ASYNCHRONOUS_OPERATIONS.md),
  [C-STORE sessions](./docs/CSTORE_SESSION.md),
  [retrieve proxy](./docs/RETRIEVE_PROXY.md), and
  [network observability](./docs/NETWORK_OBSERVABILITY.md)
- [DICOMweb client CLI](./docs/DICOMWEB_CLI.md),
  [client response limits](./docs/DICOMWEB_CLIENT.md),
  [embeddable server](./docs/DICOMWEB_SERVER.md), and
  [DICOMweb ownership boundary](./docs/DICOMWEB_EXTRACTION.md)
- [Optional XDS-I.b transaction clients](./docs/XDSI_PROFILE.md) and the
  [historical scope decision](./docs/XDSI_SCOPE_DECISION.md)
- [De-identification](./docs/DEIDENTIFICATION.md),
  [Modality Worklist](./docs/MODALITY_WORKLIST.md),
  [HL7 v2 and MLLP](./docs/HL7_MLLP.md),
  [Unified Procedure Step](./docs/UNIFIED_PROCEDURE_STEP.md), and
  [Structured Reporting templates](./docs/SR_TEMPLATES_AND_REFERENCES.md)
- [NIfTI export](./docs/NIFTI_EXPORT.md),
  [NIfTI import](./docs/NIFTI_IMPORT.md),
  [volumetric presentation state](./docs/VOLUMETRIC_PRESENTATION_STATE.md), and
  [geometry guardrails](./docs/GEOMETRY_GUARDRAILS.md)

## Development

```sh
make fmt-check
make vet
make test
make build
make check
```

`make check` uses the read-only `fmt-check`: malformed source, tool errors, and
unformatted files fail with diagnostics, without rewriting source files.
Use `make fmt` explicitly to apply formatting.

`make race` and `make leak` are opt-in offline concurrency gates documented in
[Concurrency Testing](./docs/CONCURRENCY_TESTING.md). Longer fuzz campaigns are
documented in [Fuzzing](./docs/FUZZING.md). Large parser and Part 10 benchmark
comparisons use the informational, benchstat-based
[large-dataset workflow](./docs/LARGE_DATASET_BENCHMARKS.md). Regenerate the
standard dictionary with `go generate ./dictionary/std`.

## License

Copyright 2026 Thales Matheus Mendonça Santos.

Unless a file states otherwise, this project is licensed under the
[Apache License, Version 2.0](LICENSE). Third-party data and components retain
their own licenses and attribution requirements; see
[third-party notices](THIRD_PARTY_NOTICES.md) and the notices accompanying
individual files. Every derived artifact is recorded with commit, source lines
and hashes in [`THIRD_PARTY_PROVENANCE.json`](THIRD_PARTY_PROVENANCE.json) and
verified locally; see [third-party provenance](docs/THIRD_PARTY_PROVENANCE.md).

[Window value domains](docs/WINDOW_VALUE_DOMAIN.md) provide finite rescaled/measured intervals and bounded Parametric Map preparation for application controls.

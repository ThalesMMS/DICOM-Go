# Canonical VolumeStore

`render.VolumeStore` is the ownership boundary between dicom-go ingest and CPU,
WebGPU or native render engines. It implements the frozen VolumeSnapshot V1
contract in DICOM patient LPS millimetres.

## Ownership model

`Volume.AcquireAcquiredSnapshotContext` is a separate transport boundary for
irregular acquisitions. Its `AcquiredVolumeSnapshot` retains decoded original
samples and exposes an `AcquiredVolumeDescriptor` without a patient affine.
It does not call `RegularGrid`, `VolumeGeneration`, or a spatial sampler.
`RegularizationPlan` returns an owned metadata-only target and source-coordinate
table; a backend must interpolate that target itself. Generic acquired-affine
`AcquireSourceSnapshotContext` continues to reject stacks that need per-slice
geometry. Independent `AcquireSnapshotContext` keeps its existing headless CPU
regularization behavior, without mutating an already held acquisition lease.

Admit the estimate from `EstimateDirectPreparation` before acquisition, retain
the acquisition lease through upload, and release it afterward. Packed signed
and unsigned 16-bit samples preserve their shared rescale; other acquisitions
use modality float32. `WritePayloadTo` copies these samples unchanged, while
`WriteModalityF32Context` only normalizes individual acquired values. Neither
method resamples coordinates. Cancellation, release and volume closure use the
same immutable store lifetime rules as canonical snapshots. Planning caps both
the acquired and target depth at 65,536 planes.

- A `Stack` owns references to caller-owned `Frame.PixelBytes`. Those raw
  frames remain available to the independent 2D path and are not silently
  freed when a 3D volume is evicted.
- The first regular-volume sampler decodes one tightly packed allocation that
  `VolumeStore` owns. Grayscale stacks whose slices all hold 16-bit stored
  samples (BitsAllocated 16, at most 16 bits stored) with one rescale and one
  signedness keep the stored samples as `I16_STORED_LE` or `U16_STORED_LE`
  with the descriptor's `RescaleSlope`/`RescaleIntercept` (the `STORED`
  sample domain): half the bytes of float32, measured 117 MiB instead of
  234 MiB for the 512×512×234 head CT of `DICOM_Example`. Samplers
  compute modality values exactly in float64. Other stacks, and derived
  generations (resampled, prefiltered), are `F32_MODALITY_LE`. MPR and VR acquire
  read leases on the same generation; there is no second per-renderer voxel
  cache.
- `Volume.InteractiveVolumeContext` keeps a 2× BinShrink level of
  the volume for interaction frames in a separate store owned by the volume:
  float32 averages of 2×2×2 blocks (axes under four voxels are kept), an
  eighth of the voxels, with the origin moved half an input voxel per shrunk
  axis as ITK's BinShrinkImageFilter does. `Volume.Close` closes it.
- `Volume.PrepareSnapshotProgressiveContext` builds the same canonical
  generation as `PrepareSnapshotContext` in a viewer-friendly order: the even
  slices first, then a 2× preview volume (BinShrink average of the decoded
  even slices, owned and closed by the observer), then the odd slices, each
  completed contiguous run reported as a full-resolution z-band with a
  `VolumeBandReader` (modality float32 or stored 16-bit rows) for GPU
  uploaders. The generation is installed only when every slice is decoded,
  so leases never see a partial volume; `Volume.SnapshotReady` tells whether
  it is. A volume already prepared reports only the completion.
- `Volume.AcquireReader` holds one lease for an entire CPU render and supports
  concurrent read-only sampling. Pixel loops must not acquire a snapshot per
  sample; the reader is closed only after all rendering workers have joined.
- Irregular spacing, gaps and stable gantry tilt are decoded into a transient
  source buffer and regularized by dicom-go. Only the resulting regular affine
  grid is returned by `Volume.AcquireSnapshot`. It is a new generation whose
  descriptor records the source generation as `ParentGeneration`.
- `VolumeSnapshot` exposes a value-only descriptor and scalar/read-copy
  methods. It never exposes a writable voxel slice. Native and GPU transports
  copy through `WritePayloadTo` (the stored bytes), `WriteModalityF32To`
  (float32 modality values whatever the scalar format) or, for 16-bit stored
  formats, `WriteStored16Context` (the stored samples as tight rows, for
  backends that rescale while sampling); they must not retain a Go pointer.
- RGBA render buffers, raw-frame ownership transfers and backend/native copies
  can be attached with `TrackMemory`. The returned memory lease is the owner of
  that accounting entry and must be released only when the real allocation is
  no longer live.
- VOI, LUT, window/level, transfer functions, cameras and other presentation
  state are not fields of `VolumeSnapshot` and do not advance its generation.

`Stack.Close`, `Volume.Close`, `Series.CloseVolumeStore` and
`Study.CloseVolumeStores` retire normalized generations. A renderer that
already holds a lease remains valid; the last lease or pin release performs the
actual reclamation. These methods and lease releases are idempotent.

## Exact byte accounting

`VolumeStoreStats.LiveBytes` is the exact sum of:

- store-owned voxel backing arrays, using their readable allocation length;
- explicitly tracked live raw, RGBA and backend-copy allocations.

`ByKind` separates `raw_frames`, `normalized_volume`,
`regularized_volume`, `rgba_render_buffer` and `backend_copy`.
`ActiveLeases` and `ActivePins` expose the exact reference counts; byte totals
count each protected allocation once rather than multiplying it by readers.

The number deliberately excludes Go map buckets, mutexes, interface headers,
descriptors and lease objects. `MetadataRecords` and `TrackedRecords` expose the
counts needed to bound that implementation overhead without pretending it is
pixel residency. Go allocator size-class slack and runtime bookkeeping are also
outside payload accounting and must be measured with heap/RSS profiles.

`MaxLiveBytes == 0` means no hard ceiling. A non-zero ceiling is an admission
limit: replacement or external allocation fails with
`ErrVolumeBudgetExceeded` if safe retired-generation eviction cannot make it
fit. A failed admission does not replace the current generation. Leased or
pinned bytes are never reused or evicted. Applications install such a store
before the first stack volume build with `Stack.SetVolumeStore`.

## Lifecycle rules

1. Acquire a `VolumeLease`, or a `VolumeReader` for repeated CPU sampling, and
   retain it for the complete render or upload.
2. Read the descriptor and samples only through its `VolumeSnapshot`.
3. Attach every independently allocated native/GPU/RGBA copy with
   `TrackMemory`; do not count aliases twice.
4. Release copy leases when those allocations are actually freed.
5. Release the volume lease after the renderer has stopped reading.
6. Close the series/study owner during replacement or viewer shutdown.

The store starts no goroutines. Release callbacks run after its mutex is
unlocked, so callbacks may safely query telemetry or enter backend teardown.

# Planning and releasing canonical volume residency

`render.Volume.EstimateDirectPreparation` examines captured frame metadata
without decoding pixels. It uses the same compact-format planner as snapshot
preparation: eligible uniform 16-bit stored samples use two bytes per voxel,
including signedness and common rescale; other supported inputs use float32.
The estimate reports payload bytes, a conservative scalar-plane workspace for
the configured decode workers, and whether the geometry requires resampling.
Dimension and byte arithmetic is checked before reporting an estimate.

The estimate does not include source frame buffers, external codec runtime
workspace, allocator overhead or process RSS. A caller must account these
separately and preserve the captured source/geometry through allocation. It is
not a request to reconstruct a regular grid or render a frame. Application
policy may refuse geometries requiring GPU reconstruction.

`Volume.OnSnapshotRelease` attaches bookkeeping to an existing prepared
canonical generation. It does not decode an absent snapshot or add a lease.
The callback runs once outside store locks after the generation is removed,
including the last lease/pin release after `Close`. This lets an application
keep a pre-allocation reservation until its actual backing bytes become
reclaimable. Repeated `Close`/lease release does not repeat the callback.

Focused checks:

```sh
go test ./render -run '^(TestVolumePreparationEstimate|TestVolumeSnapshotReleaseObserver)' -count=1
```

The shared application uses these primitives for
background MPR preparation.
No rendering backend, snapshot descriptor layout or GPU wire contract changed.

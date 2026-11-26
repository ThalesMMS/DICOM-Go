# Progressive preview source and geometry

`PlanVolumeProgressivePreview` supplies copied metadata for an original
descriptor, a compact unchanged even-slice source and a float32 display target.
It accepts a loading descriptor with generation zero without loading samples.
The target follows the existing headless preview: 2x2 in-plane averaging,
floor-sized odd borders, slices 0, 2, 4, and a half-block physical origin shift.
Singleton axes remain unchanged; one preview slice retains the original slice
spacing. Affines and their inverses are recomputed without sampling a volume.

`VolumeDecodeOptions.ReportEvenSlices` independently opts into one
`VolumeDecodeEvenSlices` event before odd-slice decoding. `EvenSlices.Retain`
captures immutable backing and geometry headers. Its row writers select original
slices only and perform no spatial averaging. The retained source remains valid
after cancellation, a new decode attempt, or closing the volume. The default
library preview and bands-only event sequence remain unchanged unless this
option is selected. Consumers needing device preparation also set `SkipPreview`.


Worker protocol 27 can prepare and adopt the GPU display target from unchanged
even slices. An opaque worker-bound receipt carries copied geometry and GPU
extrema; it accepts unfiltered VR frames only. Preparation preserves pending
full-resolution bands, and their final commit invalidates the preview receipt.
Budget refusal leaves the displayed target intact. Native tests compare preview
frames with independently averaged test data and final frames with a whole
original upload.


The manager sends completion after its final band commit attempt, so UI cleanup
cannot cancel that commit prematurely. The original complete snapshot supplies
the final clinical generation; an interrupted band upload retains the existing
whole-snapshot GPU upload path. Native tests pause decoding before completion
and verify preview publication and subsequent original-generation publication
in both software-Fyne frontends using the Windows GPU worker. Final source
generations remain unchanged and CPU-derived application previews remain disabled.

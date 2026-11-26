# Bounded morphological contour proposals

`roi.InterpolateContours` is an opt-in detached proposal API. Existing
`InterpolateMask` and `InterpolateBetweenSlices` retain their bounding-box aligned
signed-distance behavior. The new request explicitly chooses the method, one
nonzero label, slice axis 2, a common patient-space `roi.Grid`, and all manual
slice masks (including explicitly empty masks). Inputs remain immutable and the
result owns its manual/generated masks and component correspondence records.

## Independent mathematical contract

The morphological method discovers four-connected components. Overlapping
components form a bipartite graph: connected groups identify one-to-one,
branch, merge or complex relations. Unmatched components may align by integer
centroid translation only when they are mutually nearest, within the caller's
millimetre bound, and have no tied candidate. Translation is off at zero.

Aligned endpoint intersections seed conditional four-neighbour dilations inside
each endpoint. An indexed Dijkstra queue computes arrival distances using column
and row spacing in millimetres. With physical gap fraction `t`, the proposal
retains the intersection, the first endpoint's reachable pixels with distance
at most `(1-t)*maximumFirst`, and the second endpoint's pixels with distance at
most `t*maximumLast`. Integer alignment is moved back proportionally. This is a
discrete geodesic schedule, not ITK's recursive median-contour algorithm. Shared
cavities remain excluded. Disconnected seed support, ambiguous correspondence or
clipped alignment marks the gap unresolved; it contributes no generated masks.
`ValidateComplete` refuses a result with any unresolved gap.

`ContourReportUnmatched` is the default. Optional `ContourCloseUnmatched`
erodes unmatched components from their boundary centres toward empty. Both
`EndLengthMM` and `MaxEndSlices` bound closing inside the annotated interval;
nothing is extrapolated outside its manual endpoints. Closing is recorded as a
relation, not inferred as anatomically correct.

Irregular ascending slice offsets determine `t`. Orthogonal anisotropic grids
are accepted. Shear/tilt, nonfinite or unrepresentable physical distances, invalid
axes/labels and incompatible mask dimensions are refused; callers resample first.
The caller owns Frame of Reference compatibility and stable input lifetime.

## Resources and cancellation

Defaults are 512 slices, 64 components per endpoint, 16 Mi voxels, 256 MiB
estimated workspace/output and 128 Mi work units. Hard slice/component ceilings
are 4096/256. Admission precedes mask copies. Conservative byte accounting covers
run output, dense component/distance/queue buffers and correspondence metadata.
Dense dilation buffers are reused between component groups. The indexed heap
holds at most one pending entry per pixel; alignment uses no exhaustive search.
Pixel/edge traversal work and cancellation are checked throughout processing.
An error or cancellation returns no proposal and changes no source masks.

## Local evidence

Windows amd64, Go 1.26.4, Intel i7-14700K. Focused race fixtures cover immutable
manual slices, sparse gaps, cavities, translation, irregular physical fractions,
scale, branch/merge, ambiguous candidates, anisotropic analytical distances,
bounded closing, resource admission and cancellation during dilation. SEG
per-frame derivation descriptions survive a Part 10 write/read round trip.

```powershell
go test -race ./roi ./seg -run '^Test(ContourInterpolation|FrameDerivationDescription|BinarySegmentationRoundTripsRoiMasks)' -count=1
go test ./roi -run '^$' -bench '^BenchmarkContourInterpolationMethods$' -benchtime=300ms -count=3
$env:CONTOUR_ITK_PYTHON = 'C:/absolute/path/python.exe'
go test ./roi -run '^TestContourInterpolationITKOracle$' -count=1 -v
```

The optional oracle requires ITK 5.4.7 and
`itk-morphologicalcontourinterpolation` 2.1.2. The live local run matched an
unchanged cavity at all 2400 voxels. Scale differed at 58 voxels and branching at
48; these are reported comparisons, not assertions of mathematical equivalence.
The algorithms differ in their dilation schedule and correspondence policy.
See the [ITK public interface](https://github.com/InsightSoftwareConsortium/ITK/blob/be770907fc62a2ae0b1395a1f092465d7ab9b535/Modules/Filtering/MorphologicalContourInterpolation/include/itkMorphologicalContourInterpolator.h).

For a fixed 128×128 unchanged rectangle with seven generated slices, three
300 ms benchmark samples gave median 4.322 ms / 3.911 MB for signed distance and
0.628 ms / 1.032 MB for the morphological method (592 allocations versus 1448).
This synthetic identity fixture does not establish general speed or clinical
superiority. Full baseline status is reported separately from focused evidence.

The Twin workflow supplies source
selection, conflict review and transactional application/undo. Mask processing
does not add a CPU 3D rendering path.

# Discrete medial thickness of a binary volume

`roi.VolumeMask.MedialThickness` composes the existing Lee curve skeleton and
physical Euclidean foreground-boundary distance. It returns owned geometry,
sorted sparse samples, original 26-connected foreground components and an
all-sample/per-component distribution. It never changes the segmentation.

The quantity is **twice the distance, in mm, at skeleton voxel centres**. A
boundary site is a foreground centre with a background voxel in its 26-neighbour
stencil. This is not distance to voxel faces or to the nearest background centre.
Only regular orthogonal axes are supported, including anisotropic spacing;
adjacent steps must agree within a relative tolerance of `1e-6`. Coordinates
must be finite and increasing. Each axis needs at least three centres.

`result.At(x,y,z)` returns `(NaN,false)` outside the skeleton/domain. A boundary
centre with a real thickness of zero returns `(0,true)`. An empty mask has no
components/samples, zero distribution count and NaN summary values. A one-voxel
component retains its valid zero sample. Components that yield no curve samples
remain in the report with undefined statistics. Cavities contribute boundary
sites. Foreground touching any original image edge is refused: missing anatomy
and an exterior halo are not inferred.

The distribution contains count, minimum, maximum, mean, median, P5/P95 with
linear interpolation, and twelve equal-width histogram bins. It describes curve
samples, not volume-weighted tissue. A slab's curve skeleton may collapse to one
point; it is not a medial surface. This quantity is not automatically cortical
thickness, CPR lumen diameter, or maximal-sphere diameter propagated over every
foreground voxel. It has no clinical calibration or automatic interpretation.

## Admission, ownership and cancellation

Zero options select finite caps: **1,048,576 grid voxels, 256 MiB reserved managed
arrays and 1,024 components**. Positive overrides may only lower these caps.
Admission counts background voxels too and checks products before allocation.
`MedialThicknessReservation` provides the same dimension/byte admission before
a caller allocates a crop. The conservative reservation is
`160*paddedVoxelCount + 1024*longestAxis + 256*componentCap + 65536` bytes. It
covers the input, padded thinning and growing candidate lists, labels/queue,
distance transform with up to sixteen line workers, sparse output, sorting,
owned axes and summaries; it is not a process RSS guarantee.

The caller keeps input stable during the call. The result owns all arrays,
including axes that the older standalone `Thin` API borrows. Cancellation is
checked during validation, labelling, existing thinning subiterations, distance
passes, output scans and before/after distribution sorting. Existing thinning
candidate sweeps and a bounded sort finish before their next context check;
cancellation is not an immediate interrupt of those loops. Every error or
cancellation returns a nil result. No partial map is published.


On Windows 11 amd64, Go 1.26.4, the authored 25-cubed synthetic corpus has fifteen
cases: tube, sphere, slab, branching tube and spherical cavity at spacings
`(1,1,1)`, `(0.5,0.5,0.5)` and `(0.5,0.8,1.2)` mm. Two independent operations
were executed separately: scikit-image 0.26.0 Lee skeletonization and SimpleITK
2.5.3 / ITK 5.4 Signed Maurer distance, with NumPy 2.3.5. No reference
implementation was transcribed. The generator, versions, digest, all foreground
distances and independent skeleton indices are in the
[small authored corpus](../roi/testdata/medial_thickness/README.md).

All fifteen skeletons had **zero symmetric difference**. Every foreground
distance was checked, independently of skeleton selection. The maximum distance
delta was **2.1324806311895372e-6 mm** on anisotropic cases; isotropic cases were
at most `5.43623579e-8 mm`. ITK's float32 distance/coordinate arithmetic can leave
tiny nonzero boundary values. The test allows four float32 epsilons times the
largest physical span, and checks the composition against twice the Go distance.
This is a scoped comparison of the two primitives, not qualification against the
entire ITK MedialThickness filter or clinical anatomy.

A separate authored 31-cubed corpus compares continuous known surfaces at the
returned skeleton centres. Cylinders have radius 3 mm and half-height 4 mm,
spheres radius 3 mm, slabs half-widths `(4,4,1.2)` mm. A branch joins a radius
1.5 mm straight trunk to an inclined tube; its negative-z trunk, including its
bottom cap, has a known analytic distance. The junction has no single diameter
and is excluded from that analytic width comparison, while its entire discrete
mask remains in the independent distance/skeleton comparison.

| Spacing mm | Shape / known region | Samples compared | Mean signed bias mm | Maximum absolute error mm |
|---|---|---:|---:|---:|
| 1,1,1 | Tube | 7 | -0.857142857 | 2 |
| 1,1,1 | Sphere | 2 | -2.26794919 | 2.53589838 |
| 1,1,1 | Slab | 1 | -0.4 | 0.4 |
| 1,1,1 | Branch trunk | 4 | -0.5 | 1 |
| 0.5,0.5,0.5 | Tube | 11 | -0.454545455 | 1 |
| 0.5,0.5,0.5 | Sphere | 2 | -1.33788346 | 1.41742431 |
| 0.5,0.5,0.5 | Slab | 1 | -0.4 | 0.4 |
| 0.5,0.5,0.5 | Branch trunk | 7 | -0.714285714 | 1 |
| 0.5,0.8,1.2 | Tube | 3 | -1.34699089 | 1.61365756 |
| 0.5,0.8,1.2 | Sphere | 7 | -1.75459409 | 2.9471325 |
| 0.5,0.8,1.2 | Slab | 9 | approximately 0 | 1.33226763e-15 |
| 0.5,0.8,1.2 | Branch trunk | 1 | -1.4 | 1.4 |

All errors stayed within the declared test bound of twice a voxel diagonal.
For the 6 mm-diameter tube, peak measurements were 4, 5 and 4.38634244 mm;
for the sphere they were 3.46410162, 4.58257569 and 3.0528675 mm in the same
spacing order. These are substantial discretization biases, especially on small
or coarse objects; the API does not silently add a voxel-size correction.

The focused command passed with race detection:

```sh
go test -race ./roi -run '^TestMedialThickness' -v
```

It also checks owned output, empty/small/diagonal components, valid zero versus
undefined, cavity policy, image-edge and irregular-grid refusals, byte/voxel/
component admission, dimension overflow and cancellation during computation.

The public mathematical definition was checked in the
[ITK filter header](https://github.com/InsightSoftwareConsortium/ITK/blob/be770907fc62a2ae0b1395a1f092465d7ab9b535/Modules/Filtering/Thickness3D/include/itkMedialThicknessImageFilter3D.h).
Independent SDK contracts are documented by
[SimpleITK Signed Maurer](https://simpleitk.org/doxygen/latest/html/classitk_1_1simple_1_1SignedMaurerDistanceMapImageFilter.html)
and [scikit-image skeletonize](https://scikit-image.org/docs/0.26.x/api/skimage.morphology.html#skimage.morphology.skeletonize).

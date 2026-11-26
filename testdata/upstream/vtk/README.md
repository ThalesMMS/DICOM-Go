# VTK numerical fixtures

Small JSON baselines computed by VTK 9.6.2 (tag commit
`f49a1dbafa60b58ef22f6292ec58370453162192`, the msys2 UCRT64 build) on
synthetic phantoms defined with integer arithmetic, so the Go tests rebuild
the same voxels exactly. They are test data only; VTK is BSD-3-Clause
([LICENSE.VTK](LICENSE.VTK)) and each file's provenance, with the VTK sources
that computed it, is in `THIRD_PARTY_PROVENANCE.json`.

| File | VTK | Consumed by | Comparison |
|---|---|---|---|
| `mesh.json` | `vtkSurfaceNets3D` (smoothing off), `vtkFlyingEdges3D`, `vtkMassProperties` | `render` `TestISOSurfaceMatchesVTKSurfaceNets` | voxel-face iso-surface: triangle count, area and volume equal SurfaceNets; volume equals the voxel count and stays within 4% of the analytic solid; the FlyingEdges baselines (for smooth meshes) within 2% (volume) and 1% (area) of the analytic sphere and ellipsoid |
| `mesh.json` | `vtkFlyingEdges3D`, `vtkSurfaceNets3D` (smoothing off), `vtkMassProperties` | `mesh` `TestMarchingCubesMatchesVTKFlyingEdges`, `TestVoxelFacesMatchesVTKSurfaceNets` | Marching Cubes on the same sphere and ellipsoid fields: identical triangle count, area and volume within 0.1% of FlyingEdges and 1% of the analytic solid, open or closed boundary; the indexed voxel-face mesh equals SurfaceNets (count, area, volume) |
| `meshpost.json` | `vtkWindowedSincPolyDataFilter` (20 iterations, Nuttall and Hamming, pass bands 0.1 and 0.01), `vtkQuadricDecimation` (target reduction 0.9; MaximumError 0 and 1e-7 on a 70×70 `vtkSphereSource`), `vtkImplicitPolyDataDistance`, `vtkMassProperties` | `mesh` `TestWindowedSincMatchesVTK`, `TestQuadricDecimationMatchesVTK`, `TestQuadricDecimationMaximumError` | smoothing of the voxel sphere (equal to the SurfaceNets input) within 0.3% of VTK's volume and 1.5% of its area, without shrinkage; decimation of the Marching Cubes sphere to the same triangle count with area and volume within 1% and the input points no farther than 1.5× VTK's largest distance; a zero maximum error keeps every triangle, 1e-7 collapses only within it |
| `reslice.json` | `vtkImageInterpolator` nearest, linear, cubic | `render` `TestTrilinearSamplesMatchVTK` | `VolumeReader.TrilinearAt` and `SampleAt` in every mode equal VTK (clamp border) to 1e-9 once the point is rounded to 2^-16 as `vtkInterpolationMath::Floor` does |
| `resample.json` | `vtkImageReslice` scale-and-translation resampling, nearest, linear, cubic | `render` `TestResampleSmallDimensionsMatchVTK` | volumes with axes of 2, 3 and 4 voxels (the path VTK 9496ad00e3 fixed for cubic); `SampleAt` equals every output sample to 1e-9 |
| `prefilter.json` | `vtkImageGaussianSmooth` (3D, sigma 1 and 2 mm on 0.8×0.6×1.5 mm voxels, radius factor 3), `vtkImageMedian3D` 3×3×3 | `render` `TestPrefiltersMatchVTK` | `SmoothGaussian3D` within 1e-3 (float32 passes against VTK's float64), `Median3x3x3` exactly |
| `slab.json` | `vtkImageReslice` slab max, min, mean, sum; trapezoid integration and half slice-spacing samples | `render` `TestAxialSlabsMatchVTK`, `TestObliqueSlabSamplingMatchesVTK` | axial MIP, MinIP, mean and Sum slabs equal to 1e-9 over interior slices; oblique scalar slabs with `SlabSum`, `SlabSampling.Trapezoid` and `SpacingFraction` 0.5 (linear) equal to 1e-6 |
| `stencil.json` | `vtkLassoStencilSource`, `vtkImageEllipsoidSource` | `roi` `TestPolygonRasterMatchesVTKStencil`, `TestEllipseRasterMatchesVTKEllipsoidSource` | polygon masks equal except pixel centres exactly on an edge (both tie rules are valid), pixel count within half the perimeter of the shoelace area; ellipse masks equal |


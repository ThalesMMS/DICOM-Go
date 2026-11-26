# ITK HDF5 transform fixtures

Synthetic transform files for `ReadITKHDF5` and `internal/hdf5`. No
patient data; no file is copied from ITK or another project.


| File | Format | Content |
|---|---|---|
| `affine.h5` | superblock 2 (ITK's HDF5 1.8 bounds) | the affine of `testdata/upstream/itk/transforms/affine.tfm` |
| `composite.h5` | superblock 2 | the composite of `composite.tfm` (affine, Euler) |
| `composite_v0.h5` | superblock 0, symbol-table groups | the same composite |
| `affine_float_misspelled_v0.h5` | superblock 0 | `AffineTransform_float_3_3`, float32 parameters under old ITK's `Tranform*` names |
| `affine_field.h5` | superblock 2 | ANTs-like composite: affine and a 6 × 5 × 4 `DisplacementFieldTransform`, deflated in one chunk |
| `field_chunks_v0.h5` | superblock 0 | the displacement field alone, one-element chunks, shuffle and deflate (multi-level chunk B-tree) |
| `bspline.h5` | superblock 2 | `BSplineTransform_double_3_3`, 5 × 5 × 4 control points |
| `many_v0.h5` | superblock 0 | a composite of nine Euler transforms (several symbol-table nodes) |
| `many_dense.h5` | superblock 2 | the same ten records in dense link storage |

The displacement field values are `((37i mod 101) − 50)/64` and the B-spline
coefficients `((13i mod 29) − 14)/32`, dyadic fractions the tests recompute
exactly. Object headers record creation times, so regenerating the files
changes their bytes but not their content.

The temporary generator workspace has been retired. These committed fixtures
remain available to the library tests; the linked source records their origin.

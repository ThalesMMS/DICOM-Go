# ITK HDF5 transform fixtures

Synthetic transform files for `ReadITKHDF5` and `internal/hdf5` (#1252). No
patient data; no file is copied from ITK or another project.

They were written by the HDF5 C library (1.14, the copy bundled with ITK at
commit `519a94d954`, `Modules/ThirdParty/HDF5`, built locally) through
the [historical HDF5 generator](https://github.com/ThalesMMS/go-dev/blob/3ddf654b09757f879330ce6c9ff3d40ceffc29a0/dicom-go-validation/scripts/itk_hdf5_transforms/h5gen.c), which reproduces
the layout of ITK's `HDF5TransformIO`: version strings at the root, then
`/TransformGroup/<N>/TransformType`, `TransformFixedParameters` and
`TransformParameters`, a composite transform first and its components in
ITK's queue order. ITK itself was not built (the full toolkit did not fit on
the build machine), so the files are ITK's layout written by HDF5, not files
written by ITK's own writer.

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

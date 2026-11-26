# Linear MINC XFM import

`registration.ReadTransformFile` and `ReadTransformFileContext` recognize the
`MNI Transform File` header. Each `Transform_Type = Linear;` record contains
12 finite, row-major values representing a 3 × 4 affine. An optional
`Invert_Flag = True;` or `Invert_Flag = False;` precedes that matrix. The
default is forward. Whitespace and comments beginning with `%` or `#` are
accepted in the body. A single record returns an `Affine`. Multiple records
return a `Composite` that applies the first record first. A flag inverts its
own component before composition. [libminc XFM reader](https://github.com/BIC-MNI/libminc/blob/15d994def83274b715711afffa73268167ec2cc6/volume_io/MNI_formats/gen_xf_io.c)

The encoded coordinates are physical RAS millimetres. With
`D = diag(-1, -1, 1)`, an encoded mapping `q = M p + t` becomes
`qLPS = (D M D) pLPS + D t`. The file does not identify which image is fixed
or moving. Match the transform's direction to the image roles before using
registration resampling, which maps fixed points to moving points. An XFM
produced in the opposite direction requires the inverse of the composed
mapping. [ITK MINC coordinate conversion](https://github.com/InsightSoftwareConsortium/ITK/blob/v5.4.6/Modules/IO/TransformMINC/src/itkMINCTransformIO.cxx)

The reader admits at most 16 MiB encoded, 256 bytes per token and 1,024
components. Each component consumes 12 values from the existing aggregate
`MaxParameters` budget. Cancellation returns no transform. Malformed data
wraps `ErrTransformFile`, including truncation, non-finite values and an
absent terminator. A singular or ill-conditioned matrix also wraps
`ErrSingularMatrix`. Grid, thin-plate spline and other nonlinear records
wrap `ErrUnsupportedTransform`. External displacement images are never
opened. The profile covers reading linear XFM and excludes XFM writing.

## Independent landmark reference

The three [synthetic fixtures](../registration/testdata/mincxfm/) and
[reference harness](../registration/testdata/mincxfm/oracle.c) cover a
general affine with shear and reflection, its flagged inverse, and a
noncommuting chain with mixed inverse flags. Debian amd64 libminc
`2.4.06-2.1+b4` read the files through `input_transform_file`. For LPS
landmarks `(0, 0, 0)`, `(1.5, -2.25, 3.75)` and
`(-7, 9.5, -11.25)`, the reference calls were
`general_transform_point(&xfm, -x, -y, z, &rx, &ry, &rz)`, comparing
`(-rx, -ry, rz)`. The Windows amd64 Go test uses those nine reference
points with a Euclidean tolerance of 1e-12 mm. The default tests use the
recorded landmarks and require no native MINC dependency.

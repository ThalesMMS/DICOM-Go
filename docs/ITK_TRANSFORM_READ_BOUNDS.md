# Bounded ITK transform reading

`registration.ReadTransformFile` recognizes ITK text, MATLAB Level 4 or the
supported HDF5 subset from encoded content. ITK transforms map fixed points to
moving points in LPS millimetres. The same entry point reads the
[MINC XFM linear profile](MINC_XFM.md) with its own direction and ordering
conventions.

`ReadTransformFileContext(ctx, data, TransformFileReadOptions{MaxParameters: n})`
adds cooperative cancellation at encoded input reads and an aggregate budget
across all component parameter vectors. Zero uses 2^24 values; explicit budgets
must be between 1 and 2^24. HDF5 checks the numeric dataset size before decoding
the payload. Text vectors are parsed incrementally; repeated `Parameters` lines
also consume the total budget. Fixed parameter vectors remain capped at 64
values. Readers admit at most 1,024 transform records. MATLAB holds small
linear vectors and is also subject to the transform count and total budget.

The direct text and HDF5 readers use the default aggregate budget, including
their 2D counterparts. Dense 3D families are cubic B-spline and displacement
field transforms. Bounded dense HDF5 hard-link groups are covered by the
[dense-group profile](../registration/testdata/itk1845/README.md). External
component files remain unsupported. The caller bounds its encoded byte
buffer separately. Parameter counts bound numeric payloads, and peak RSS
depends on the input and runtime.

Cancellation is checked before parsing and after decoding, and a cancelled
read returns no transform. Input read checkpoints do not interrupt arithmetic
inside an already buffered text-vector parse. `TransformDisplacementField`
also checks cancellation before allocating a field and between conversion rows.
It samples numerical displacement vectors, not rendered images.


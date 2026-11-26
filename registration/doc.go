// Package registration provides parametric 3D spatial transforms for image
// registration: Euler angles (ZXY and ZYX orders), versor rigid, similarity,
// affine, translation, scale-versor, scale-skew-versor and composite transforms.
// A separate Transform2D API provides translation, Euler, similarity, affine
// and linear composites in a caller-defined two-dimensional basis. Every
// transform maps a point, exposes a
// flat parameter vector for optimizers, its fixed parameters (the centre of
// rotation) and the Jacobian of the mapped point with respect to the
// parameters and to the position.
//
// # Conventions
//
// The transforms follow the Insight Toolkit (ITK, Apache-2.0, Copyright
// NumFOCUS; see ITK's NOTICE) at the historical fixture/reference commit
// 35b196e13c366e0386389a71c936a0436c568cea: the class names returned by
// Kind, the parameter and fixed-parameter layouts, the centre of rotation
// kept as a fixed parameter (x ↦ M·(x − c) + c + t, so the offset is
// t + c − M·c), the Euler angle orders and the versor update rule of
// Modules/Core/Transform (Euler3DTransform, VersorRigid3DTransform,
// Similarity3DTransform, AffineTransform, MatrixOffsetTransformBase and
// CompositeTransform). This package is an independent implementation: the
// rotation Jacobians are the product-rule derivatives of the rotation
// matrices cached with each parameter change, and the differences from ITK
// are documented on the types (a versor with a negative scalar part is
// negated so its right part keeps describing the same rotation, the
// matrix-to-versor conversion keeps the scalar sign in every branch, and
// invalid input returns an error instead of throwing).
//
// Three-dimensional coordinates are DICOM patient coordinates (LPS,
// millimetres), which are also ITK's physical space, so no axis flip is
// involved. Point2 is independent: no implicit DICOM XY slice, dimensional
// lifting, 2D registration engine or 2D DICOM Spatial Registration is supplied.
// Translation3D has three translation parameters and no fixed parameters.
// ScaleVersor3D has versor-right, translation and three scales; its matrix is
// R+diag(scale-1). ScaleSkewVersor3D additionally has six off-diagonal additive
// skew entries in xy,xz,yx,yz,zx,zy order. Finite zero/negative scales are valid
// forward mappings, while inverses and positive-Jacobian certificates have
// stricter domains. These new layouts and native 2D interchange are qualified
// against public SimpleITK 2.5.3/ITK 5.4 APIs using own synthetic fixtures.
//
// # Direction
//
// As in ITK, a registration transform maps points of the fixed image into
// the moving image: TransformPoint(fixed) = moving. The DICOM Frame of
// Reference Transformation Matrix (PS3.3 C.20.2) maps the opposite way, from
// the Source Frame of Reference (the moving image) into the registered
// Frame of Reference (the fixed image). DICOMMatrix therefore returns the
// inverse of the transform, and FromDICOMMatrix and FromAffineRegistration
// invert a stored matrix back into a fixed-to-moving transform.
//
// # Similarity metrics
//
// MeanSquares, Correlation and MattesMutualInformation compare a fixed
// Image with a moving Image mapped by a Transform, over every fixed sample
// or sampled points, optionally within fixed and moving masks
// (MetricDomain). Each returns its value and its exact gradient with
// respect to the transform parameters (not ITK's negated "derivative"):
// the moving gradient is the derivative of the linear interpolation the
// metric compares, and it combines with JacobianWithRespectToParameters.
// Fixed points that map more than half a sample outside the moving image
// do not count, as in ITK, so a metric jumps when points cross that border.
// The points are split in fixed chunks reduced in order, so results do not
// depend on the number of workers. The Mattes metric reproduces the values
// of ITK's MattesMutualInformationImageToImageMetricv4 (see mattes.go).
// ImageFromSnapshot builds an Image from a render.VolumeSnapshot in
// modality values.
// NeighborhoodCorrelation adds rectangular local negative squared Pearson
// correlation, with explicit voxel/physical radii, pair masks, clipped borders,
// constant-neighborhood exclusion and complete parameter derivatives. It uses
// bounded deterministic serial reduction, rather than ANTs' center-gradient
// approximation; see docs/SYN_NEIGHBORHOOD_CORRELATION.md.
//
// Objective adapts a metric and a transform to the optimize package, and
// PhysicalShiftScales estimates the per-parameter scales (and the step
// scale for learning-rate estimation) from the physical shift each
// parameter causes at CornerPoints, as ITK's
// RegistrationParameterScalesFromPhysicalShift does.
//
// # Registration method
//
// Method registers a moving image to a fixed one over resolution levels,
// after ITK's ImageRegistrationMethodv4: per level it smooths both images
// with a recursive Gaussian of σ in millimetres (imagefilter), shrinks the
// fixed grid into the compared points while the images keep their
// resolution, optionally samples them (regular with jitter, or random,
// seeded), estimates the scales and learning rate from physical shifts and
// optimizes the transform from the previous level's result. CenterTransform
// initializes a transform from the geometric centres or the centres of
// mass. Run is cancelled with its context, reports every evaluation through
// Progress, and returns per-level iterations, values and stop reasons. The
// result is a proposal to review: nothing here stores a Spatial
// Registration.
//
// # Symmetric normalization
//
// SyN produces bounded greedy halfway proposals using local correlation,
// exponential field composition, optional physical membrane regularization and
// continuous-cell gradient admission. MiddleToFixed and MiddleToMoving share
// the fixed virtual grid; their inverse compositions yield FixedToMoving and
// MovingToFixed on the corresponding input grids. Undefined vectors remain
// triple NaN with explicit validity masks, including through Deformable Spatial
// Registration interchange. Cancellation returns no proposal. Positive local
// Jacobian certificates and synthetic landmark tests do not establish clinical
// correspondence, a global inverse across undefined regions or ANTs trajectory
// equivalence. No application registration is performed automatically.
//
// # Free-form deformation
//
// BSpline is a cubic B-spline deformation with ITK's BSplineTransform
// layouts (control-point displacements as parameters; grid size, origin,
// spacing and direction as the eighteen fixed parameters): its Jacobian
// with respect to the parameters is the sparse set of B-spline weights,
// and Refine doubles the control mesh exactly. FreeFormDeformation
// registers a moving image to a fixed one of the same or another modality
// with it: Mattes mutual information, whose derivative uses the sparse
// Jacobian in two passes, resolution levels that smooth and shrink as
// Method does and refine the mesh, an optional membrane-energy
// regularization, and L-BFGS (optimize.LBFGS) per level; a seed makes
// sampled runs repeatable and the context cancels them. An Initial
// transform (a rigid or affine result) is applied after the deformation
// and kept fixed. The result maps fixed to moving points, for
// TransformMapping and Resample, and TransformDisplacementField samples it
// as a dense DisplacementField whose Grid writes a Deformable Spatial
// Registration.
//
// # Transform files
//
// ReadTransformFile reads ITK's text, MATLAB and HDF5 transform files (see
// itkfile.go and itkhdf5.go); DisplacementFieldTransform is the dense
// non-linear stage of ANTs composites.
// ReadTransformFile2D and the typed text/MATLAB/HDF5 readers preserve native
// 2D coordinates and reject 3D input; the 3D readers reject 2D input. Text and
// HDF5 preserve component order, reversing ITK queue order once to obtain Go
// application order. Serialization does not preserve optimization flags.
// Composite2D refuses duplicate component references; use Clone for independent
// parameter blocks of the same mapping. This does not alter Composite in 3D.
// MATLAB writing supports one linear transform, not a composite.
//
// WriteITKHDF5 and WriteITKHDF52D write canonical uncompressed classic HDF5
// in pure Go. The Context variants admit the entire encoded file against
// MaxBytes (zero defaults to 256 MiB), including bounded metadata, before
// writing. They borrow stable dense parameter storage; callers must not mutate
// a transform concurrently. Output uses IEEE little-endian float64 vectors and
// ASCII variable-length metadata strings (at most 1024 bytes, one bounded
// 4096-byte global heap each), matching the public ITK reader's charset.
// At most 1024 records, including a composite marker, are written; nested and
// empty composites, unknown implementations and malformed/nonfinite states
// are refused. Writes are at most 64 KiB, check cancellation between chunks,
// and preserve IO/context errors. Partial output can remain after IO failure
// or cancellation; a blocking caller Writer cannot be forcibly interrupted.
//
// Small linears must describe the same concrete family, matrix and offset
// after their native parameters are reread. The coefficient tolerance is
// 32 float64 epsilons times max(1, absolute actual/reconstructed coefficient).
// Use the New constructors: uninitialized linear states or versor boundary
// states whose native setter clamp changes geometry are refused before Write;
// an explicit affine representation can retain such a finite forward mapping.
// The byte cap bounds encoded output. The reader separately admits the
// unfiltered managed dense hard-link profile used by larger ITK composites.
// General HDF5, append,
// compression, string arrays and generic variable-length sequences are not
// supported by the writer. This is offline library interchange only.
//
// # Resampling and subtraction
//
// Resample puts a moving image on the grid of a fixed one (ITK's
// ResampleImageFilter): every output sample is a fixed point mapped by a
// Mapping into the moving image — TransformMapping for a transform,
// FieldMapping for a Demons displacement field, ComposeMappings for both,
// or any function such as a Spatial Registration's Registered-to-Source
// mapping — and read there trilinearly, by the nearest sample, or by the
// label with the largest Gaussian share for label images. Offline modality
// resampling also supports cubic B-spline and radius-five Lanczos/Welch
// windowed-sinc (border-sample extension, no weight renormalization). Sinc
// rejects non-finite stencil samples/results and uses a 512 MiB output-payload
// limit unless MaxOutputBytes supplies a positive cap; the original kernels
// remain uncapped at zero. This does not limit total RSS or input storage.
// Points that map
// more than half a sample outside the moving grid, or where the mapping is
// undefined, get the explicit ResampleOptions.Padding and are marked
// outside; the result also lists the moving slices each output slice read.
// Subtract then gives minuend − subtrahend on the shared grid, in the
// images' own units (Hounsfield units for CT), with the padding where the
// resampled image is outside its field. The grid geometry (Grid) is LPS,
// like the images'.
//
// # Motion correction
//
// CorrectMotion aligns the phases of a dynamic (4D) series: every phase is
// registered rigidly (Euler3D, Mattes mutual information by default, which
// tolerates contrast uptake) to a reference phase, each from the motion of
// its neighbour nearer the reference, on block-averaged images, and then
// resampled at full resolution onto the reference grid. Phases are read
// through a MotionSource and handed to a MotionSink one at a time, so only
// the reference and the current phase are held; ctx cancels the run. See
// motion.go.
//
// # Point-based registration
//
// RegisterLandmarks fits paired fixed and moving points in closed form
// (rigid, similarity or affine) and reports the residuals and the fiducial
// registration error; IterativeClosestPoint aligns two point clouds, such
// as mesh vertices, by refitting to closest-point pairs. See landmark.go
// and icp.go.
package registration

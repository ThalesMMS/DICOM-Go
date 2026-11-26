// Package imagefilter holds image and volume filters: display-domain
// enhancement (CLAHE), dense recursive Gaussian filtering with derivatives,
// histogram matching for comparing examinations, and multiscale Hessian
// vesselness.
//
// # Display enhancement
//
// CLAHE operates on presentation values (P-values after the modality and VOI
// transforms), never on stored or modality pixel values, and it is intended
// for on-screen enhancement only: its output is not a measurement domain and
// must not be written back into DICOM objects, presentation states or
// derived series as if it were the acquired image.
//
// CLAHE implements Contrast Limited Adaptive Histogram Equalization as
// described by K. Zuiderveld ("Contrast Limited Adaptive Histogram
// Equalization", Graphics Gems IV, 1994): the image is divided into a grid of
// contextual regions (tiles), each tile histogram is clipped at a limit and
// the excess is redistributed over the bins, each clipped histogram becomes a
// cumulative gray-level mapping, and every pixel is mapped by bilinear
// interpolation of the mappings of the four nearest tile centres. The cost is
// linear in the number of pixels plus a per-tile term proportional to the
// number of histogram bins. The implementation was written from the published
// algorithm; it is not derived from the ITK adaptive histogram equalization
// filter, which implements Stark's generalization with a sliding window.
//
// # Recursive Gaussian filtering
//
// SmoothRecursiveGaussian, RecursiveGaussian (any derivative order 0–2 per
// axis), GradientRecursiveGaussian, GradientMagnitudeRecursiveGaussian,
// LaplacianRecursiveGaussian and HessianRecursiveGaussian filter a Volume —
// dense float32 samples, x-fastest, with a spacing in millimetres; a 2-D
// image is a volume of one slice — with separable fourth-order recursive
// (IIR) Gaussians after Deriche and Farnebäck–Westin. σ is given in
// millimetres per axis, so anisotropic voxels are smoothed equally in patient
// space, and the cost per voxel does not depend on σ. Lines are filtered in
// parallel, the filters honour context cancellation, borders replicate the
// edge sample (zero-flux), and derivatives are in intensity units per
// millimetre, optionally scale-normalized (NormalizeAcrossScale) as in ITK.
// These filters work in the measurement domain (modality values such as HU)
// and are the building blocks for vesselness, denoising, bias-field
// correction, registration and rendering prefilters. See recursive_gaussian.go
// for the method, the normalization and the measured accuracy.
//
// # Histogram matching
//
// HistogramMatch maps the intensities of a source image onto the intensity
// scale of a reference image by quantile landmarks, adapted from ITK's
// HistogramMatchingImageFilter: each image gets an IntensityHistogram of the
// samples at or above its mean (the MR background is left out of the
// quantiles), the thresholds, MatchPoints evenly spaced quantiles and the
// maxima of both histograms form a landmark table, and source values are
// interpolated linearly between landmarks. The mapping is monotonic and
// continuous; values below the source threshold map linearly from the
// source minimum to the threshold and values above the source maximum map
// to the reference maximum. MR intensities are not calibrated, so a prior
// and a current examination usually need different windows; after matching
// the prior to the current one, the current window serves both.
//
// Matching operates on modality values and returns new values
// (MatchHistograms, HistogramMatch.Apply, or HistogramMatch.Map through
// render.RenderCache.RenderFrameRemapped); it never modifies its inputs or
// stored pixel data. Matched values are a display and comparison aid, not
// measurements: callers must report the transform wherever they show them
// and must not write matched values into DICOM objects as if acquired.
//
// # Experimental phase and wavelet analysis
//
// DecomposeWavelet2D and ReconstructWavelet2D expose a bounded physical 2D
// Gaussian-difference frequency partition. PhaseSymmetry2D adds isotropic
// Riesz phase and an explicit validity mask. This research profile stays
// experimental: the fixed held-out ridge campaign failed its promotion gate
// against the existing Frangi filter. It changes no existing filter/default.
// See docs/PHASE_WAVELET_2D.md for extension, precision and measured limits.
//
// # Vesselness
//
// Vesselness enhances tubular structures (angiography, airways) from the
// eigenvalues of the σ²-normalized Hessian at several scales, keeping per
// voxel the strongest response and, optionally, its scale: Frangi's
// objectness (tubes, blobs or plates, bright or dark) or Sato's line
// measure, after ITK's HessianToObjectnessMeasureImageFilter,
// Hessian3DToVesselnessMeasureImageFilter and
// MultiScaleHessianBasedMeasureImageFilter. A tube of radius r answers best at
// σ = r for a Gaussian profile and σ = r/√2 for a solid cylinder. The Hessian
// is never held for the whole volume: slabs of slices, with a 6σ margin
// along z, keep the working memory under VesselnessOptions.MaxWorkingBytes.
// The response is a feature map for display, path finding (a CPR speed map)
// and segmentation seeds, not a measurement. See vesselness.go.
//
// # Edge-preserving denoising and sharpening
//
// GradientAnisotropicDiffusion (Perona–Malik, with the conductance of ITK's
// GradientNDAnisotropicDiffusionFunction relative to the image's RMS
// gradient), CurvatureAnisotropicDiffusion (ITK's modified curvature
// diffusion equation, which does not turn noisy slopes into steps), Bilateral (Gaussian domain kernel in mm, Gaussian range kernel
// in modality units) and UnsharpMask (I + Amount·(I − G∗I)) filter modality
// values with the grid spacing, so low-dose CT and MR are denoised or
// sharpened in HU or the rescaled scale rather than on windowed 8-bit
// pixels. Diffusion takes its time step in mm² and is stable below
// 1/(2·Σ 1/hᵢ²) (half of that for the curvature scheme). The results are display aids, not measurements. See
// edge_preserving.go.
//
// # Median
//
// Median replaces each sample by the median of a box of (2r+1) samples per
// axis, the radius in millimetres per axis rounded to whole samples and the
// borders replicated, as ITK's MedianImageFilter. It removes impulsive
// noise (dead and hot pixels, speckle spikes in XA, US and CR) that the
// edge-preserving filters keep as edges. Integer data spanning at most
// 65536 values use Huang's sliding histogram with a two-level (coarse and
// fine) histogram; other data select the median by quickselect. Both return
// the exact median, an input sample. See median.go.
//
// # Slab-wise execution
//
// FilterSlabs runs a neighbourhood filter (Median, Bilateral,
// NonLocalMeans, or an in-plane UnsharpMask) on a volume read slab by slab
// from a SliceReader into a SliceWriter, each output slab computed from the
// input slab plus a halo of the filter's z reach, under
// SlabOptions.MaxWorkingBytes. Kernels address samples in whole-volume
// coordinates and the halo covers every slice they read, so the result is
// bit-identical to the whole-volume filter. Anisotropic diffusion, whose
// conductance depends on the whole volume at each iteration, bounds its
// scratch with two slab buffers (AnisotropicDiffusionOptions.MaxWorkingBytes)
// and gives the same result. See slab.go.
//
// # Non-local means
//
// NonLocalMeans is Coupé et al.'s blockwise non-local means: blocks of
// (2r+1) samples per axis on a grid of step n are each restored as the
// average of the blocks centred on every sample of a search window,
// weighted by exp(−‖Bᵢ − Bⱼ‖²/(|B|·2βσ²)) with σ the noise of the image (for
// example the median absolute difference of neighbours over 0.6745·√2),
// and every sample becomes the mean of the estimates of the blocks covering
// it. It removes the Gaussian-like noise of low-dose CT and MR while
// keeping edges and texture: +13.8 dB PSNR on a 2-D phantom with σ = 40
// (bilateral +11.1 dB). It works slice by slice or in 3-D, streams with a
// halo of 2·r_z + s_z slices, and denoises a 512×512 slice in about 35 ms.
// See nlm.go.
package imagefilter

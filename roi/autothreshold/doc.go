// Package autothreshold computes automatic intensity thresholds from a
// histogram: Otsu and multi-level Otsu, Huang, Intermodes, Minimum, IsoData,
// Kittler–Illingworth, Li, maximum entropy (Kapur), Mean, Moments, Rényi
// entropy, Shanbhag, Triangle and Yen.
//
// The calculators work on a Histogram, which can be built from any re-iterable
// sequence of sample values (a slice, an image grid, the pixels inside a
// roi.RasterMask or the voxels of a roi.Segmentation3D) with a chosen number of
// bins, a fixed bin width, or an explicit value range. The same histogram can
// be thresholded by every method, so a global threshold and one restricted to
// a region of interest differ only by the values fed to NewHistogram.
//
// # Threshold convention
//
// A Threshold splits the histogram bins into a lower class (bins 0..Bin) and
// an upper class (bins Bin+1..). Value is the lower edge of bin Bin+1, so a
// sample v belongs to the upper class exactly when v >= Value, which is the
// same rule the histogram used to bin it. Center is the centre of bin Bin,
// the value ITK's calculators and scikit-image report, where the upper class
// is v > Center.
//
// # Provenance
//
// The methods are implemented from the original publications, not ported from
// ITK, ImageJ/Fiji or scikit-image:
//
//   - Otsu, "A threshold selection method from gray-level histograms", IEEE
//     Trans. SMC 9(1), 1979; multi-level search as in Liao, Chen and Chung,
//     J. Inf. Sci. Eng. 17(5), 2001, solved exactly by dynamic programming.
//   - Huang and Wang, "Image thresholding by minimizing the measures of
//     fuzziness", Pattern Recognition 28(1), 1995.
//   - Prewitt and Mendelsohn, "The analysis of cell images", Ann. N.Y. Acad.
//     Sci. 128, 1966 (Intermodes and Minimum).
//   - Ridler and Calvard, "Picture thresholding using an iterative selection
//     method", IEEE Trans. SMC 8(8), 1978 (IsoData).
//   - Kittler and Illingworth, "Minimum error thresholding", Pattern
//     Recognition 19(1), 1986.
//   - Li and Lee, "Minimum cross entropy thresholding", Pattern Recognition
//     26(4), 1993, with the iteration of Li and Tam, Pattern Recognition
//     Letters 19(8), 1998.
//   - Kapur, Sahoo and Wong, "A new method for gray-level picture thresholding
//     using the entropy of the histogram", CVGIP 29(3), 1985 (MaxEntropy).
//   - Tsai, "Moment-preserving thresholding", CVGIP 29(3), 1985.
//   - Sahoo, Wilkins and Yeager, "Threshold selection using Renyi's entropy",
//     Pattern Recognition 30(1), 1997.
//   - Shanbhag, "Utilization of information measure as a means of image
//     thresholding", CVGIP: Graphical Models and Image Processing 56(5), 1994.
//   - Zack, Rogers and Latt, "Automatic measurement of sister chromatid
//     exchange frequency", J. Histochem. Cytochem. 25(7), 1977 (Triangle).
//   - Yen, Chang and Chang, "A new criterion for automatic multilevel
//     thresholding", IEEE Trans. Image Process. 4(3), 1995.
//
// The default tests compare Otsu, multi-Otsu, Li, Yen, IsoData, Triangle,
// Mean and Minimum with scikit-image 0.26.0 on synthetic histograms
// (testdata/upstream/skimage) and check every method on analytic bimodal
// mixtures. An opt-in test compares the ITK expected thresholds for
// cthead1.png when DICOMGO_CTHEAD1_PNG names a local copy of that image.
package autothreshold

// Package spectroscopy reads DICOM MR Spectroscopy Data (5600,0020) as owned
// float32 values, preserving its representation and spectral/spatial axes.
// Spectroscopy Data is an OF signal stream, not Pixel Data. The package uses
// object.Object for DICOM access; it does not parse a file, apply an FFT or
// infer an absolute frequency origin. See DICOM PS3.3 C.8.14 and C.8.14.4.
package spectroscopy

// Package jpegls provides qualified pure-Go DICOM JPEG-LS decoders
// and explicitly registered lossless and near-lossless encoders.
//
// New and Register support 1.2.840.10008.1.2.4.80 strictly with NEAR=0 and
// ILV=0, plus unsigned RGB ILV=1/2 with 2-16 stored bits in 8/16-bit containers,
// HighBit=BitsStored-1, PlanarConfiguration=0 and matching codestream precision.
// The decoder accepts default coding parameters and LSE preset coding
// parameters with ID=1. NewNearLossless and RegisterNearLossless additionally
// support .81 with unsigned MONOCHROME1/2 ILV=0 or RGB ILV=0/1/2, matching
// precision and full-range MAXVAL. Signed .81, palette color under .81,
// restart markers and other JPEG-LS extensions are outside
// this package's subset. Interleaved scans must contain all three components in
// frame-header order, with MAXVAL=2^precision-1. YBR_FULL and signed RGB are not
// qualified. Encoders write ILV=0 by default; EncoderOptions.Interleave selects
// line (ILV=1) or sample (ILV=2) interleaving for unsigned RGB frames, lossless
// or near-lossless. NewEncoderWithOptions and
// RegisterNearLosslessEncoder require explicit NEAR and lossy authorization;
// NewEncoder, zero Encoder and RegisterEncoder remain lossless. See
// docs/JPEGLS_INTERLEAVE.md and docs/JPEGLS_NEAR_LOSSLESS.md for the independent
// corpora and parameter limits.
// DecodeRecovery.JPEGLSHPColorTransforms separately opts into nonstandard
// APP8 mrfx HP1/HP2/HP3 compatibility: only .80, NEAR=0, unsigned RGB8/RGB16
// at full precision, ILV=1/2 and full-range MAXVAL. Applied transforms report
// individual diagnostics; strict decoding and standard encoder output remain
// unchanged. See docs/JPEGLS_HP_COMPATIBILITY.md for the independent evidence.
package jpegls

// UID is the DICOM JPEG-LS Lossless transfer syntax UID.
const UID = "1.2.840.10008.1.2.4.80"

// NearLosslessUID is the JPEG-LS Near-Lossless transfer syntax, enabled for
// encoding only through explicit options and a caller-owned registry.
const NearLosslessUID = "1.2.840.10008.1.2.4.81"

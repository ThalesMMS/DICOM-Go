// Package jpegxl is a pure-Go JPEG XL codec (ISO/IEC 18181-1 and -2) for the
// DICOM JPEG XL transfer syntaxes: JPEG XL Lossless 1.2.840.10008.1.2.4.110,
// JPEG XL JPEG Recompression 1.2.840.10008.1.2.4.111 and JPEG XL
// 1.2.840.10008.1.2.4.112 (PS3.5 Section 8.2.16 and Annex A.4.12). It was
// written from the ISO/IEC 18181 specifications; libjxl serves only as an
// opt-in external test oracle.
//
// Profile:
//
//   - Decode: single-frame images with a modular frame (every predictor,
//     the self-correcting predictor, MA trees, RCT, Palette without delta
//     entries and Squeeze, 1-16 bits, grey or RGB) or a JPEG-compatible VarDCT frame (DCT8 blocks with a
//     RAW JPEG quantization table, no XYB), plus static 8-bit sRGB XYB DCT8
//     images up to 2048 pixels per axis, with the default opsin and AC
//     quantization matrices and no restoration filters. Other XYB profiles,
//     animation, extra channels, patches, splines and noise return ErrUnsupported.
//   - ReconstructJPEG restores the original JPEG file of a JPEG-recompressed
//     image (jbrd box) byte for byte, for sequential and progressive JPEGs.
//   - EncodeLossless writes an exact modular codestream; EncodeLossy writes a
//     JPEG-compatible VarDCT codestream at a JPEG quality; RecompressJPEG
//     writes a reversible recompression of a sequential Huffman 8-bit JPEG and
//     verifies the reconstruction before returning.
//   - Container boxes (jxlc, jxlp, jbrd, Exif, xml, brob) are read; brob
//     payloads use the pure-Go Brotli decoder in internal/brotli.
//
// DICOM integration is explicit: Register adds the decoder for the three
// UIDs, RegisterLosslessEncoder and RegisterLossyEncoder add the .110 and .112
// encoders (the latter only for callers that allow lossy compression), and
// Recompressor is a pixeldata.CodestreamTranscoder converting JPEG Baseline
// and Extended frames to .111 and back without decoding pixels. Signed
// monochrome samples are stored with an offset-binary level shift (value +
// 2^(BitsStored-1)).
package jpegxl

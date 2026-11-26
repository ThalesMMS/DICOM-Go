// Package deflateframe is the pure Go codec for the DICOM Deflated Image Frame
// Compression transfer syntax, UID 1.2.840.10008.1.2.8.1 (PS3.5 Section 8.2.16
// and Annex A.4.13).
//
// Under this transfer syntax the data set itself is plain Explicit VR Little
// Endian and Pixel Data is encapsulated. Each frame is the native, little-endian
// frame byte stream (exactly as it would appear in native Pixel Data for that
// frame, starting on a byte boundary) compressed with the DEFLATE algorithm of
// RFC 1951, without a zlib (RFC 1950) or gzip (RFC 1952) wrapper, and stored in
// exactly one fragment. A compressed stream of odd length is padded with one
// trailing NULL byte so the fragment has even length. This is unrelated to the
// data-set deflate of Deflated Explicit VR Little Endian (1.2.840.10008.1.2.1.99).
//
// Decoding inflates each fragment into a buffer bounded by the expected frame
// size (Rows x Columns x Samples per Pixel x Bits Allocated / 8, rounded up to
// whole bytes). A stream that would produce more bytes is refused with an
// *ExpansionLimitError (errors.Is(err, ErrExpansionLimit)) before any output
// beyond that bound is produced, which defeats decompression bombs.
//
// Register the directions explicitly:
//
//	deflateframe.Register(decoders)        // or pixeldata/builtin.Register
//	deflateframe.RegisterEncoder(encoders) // for TranscodeDataSet targets
package deflateframe

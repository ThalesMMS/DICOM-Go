package pixeldata

// DetectableDecodeRecovery enables every decode recovery whose use is
// decided from the data itself and reported in DecompressReport (#1136): a
// retry profile for a decode that failed strictly. It leaves out
// JPEGLosslessPredictor6Overflow and JPEGLSJAIThresholds, which cannot be
// detected from the stream and misdecode conforming frames, so they stay
// reserved for sources known to be affected.
// Native planar/OW overrides, reversed RLE byte segments and retired color
// conventions also require explicit producer knowledge and remain excluded.
// Wrong palette tags can also describe retired large palettes, so their
// reinterpretation is never selected by this automatic retry profile.
func DetectableDecodeRecovery() DecodeRecovery {
	return DecodeRecovery{
		IgnoreInconsistentBasicOffsetTable: true,
		DefaultImagePixelModule:            true,
		Unpack12BitPacked:                  true,
		JPEGLosslessEncoderQuirks:          true,
		JPEGSequentialScanQuirks:           true,
		RLESegmentOverrun:                  true,
		ReconcileWithCodestream:            true,
		UnwrapJP2Container:                 true,
		NativePixelDataUnderEncapsulated:   true,
		DefinedLengthFragments:             true,
		RepairBitCountMasks:                true,
	}
}

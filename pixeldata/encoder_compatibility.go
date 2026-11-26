package pixeldata

// EncoderCompatibilityProfile identifies explicitly selected output outside
// the standard DICOM attribute profile. Empty means no compatibility profile
// was used; it is not a general DICOM conformance certification.
type EncoderCompatibilityProfile string

const (
	// EncoderCompatibilityJPEGExtendedColor is SOF1 unsigned RGB8/RGB12 or
	// YBR_FULL12, 4:4:4. PS3.5 Table 8.2.1-1 lists only mono for .51.
	EncoderCompatibilityJPEGExtendedColor EncoderCompatibilityProfile = "jpeg-extended-color"
	// EncoderCompatibilityJPEGExtendedReducedPrecision preserves the old
	// mono9..11 API with precision12 in the codestream and reduced BitsStored.
	EncoderCompatibilityJPEGExtendedReducedPrecision EncoderCompatibilityProfile = "jpeg-extended-reduced-precision"
)

func validEncoderCompatibilityProfile(profile EncoderCompatibilityProfile) bool {
	return profile == EncoderCompatibilityJPEGExtendedColor || profile == EncoderCompatibilityJPEGExtendedReducedPrecision
}

func validateEncoderCompatibility(capabilities EncoderCapabilities, encoded EncodedFrame) error {
	if encoded.CompatibilityProfile == "" {
		return nil
	}
	for _, profile := range capabilities.CompatibilityProfiles {
		if profile == encoded.CompatibilityProfile {
			return nil
		}
	}
	return ErrEncoderOutputInvalid
}

func encoderCompatibilityProfiles(profile EncoderCompatibilityProfile) []EncoderCompatibilityProfile {
	if profile == "" {
		return nil
	}
	return []EncoderCompatibilityProfile{profile}
}

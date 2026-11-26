package transfer

import "encoding/binary"

// Legacy native encodings. They are not registered in DefaultRegistry, so the
// ordinary readers keep rejecting them; the transfer syntax recovery readers
// accept them on request and normalize the data set to Implicit VR Little
// Endian after reading.
var (
	// ImplicitVRBigEndianACRNEMA is the Implicit VR Big Endian encoding of
	// ACR-NEMA 1.0/2.0 files. No UID was ever assigned; UID is a fixed
	// non-numeric identifier that cannot collide with a real UID.
	ImplicitVRBigEndianACRNEMA = Syntax{
		UID:            "ACR-NEMA-Implicit-VR-Big-Endian",
		Name:           "ACR-NEMA Implicit VR Big Endian",
		ExplicitVR:     false,
		ByteOrder:      binary.BigEndian,
		Supported:      true,
		CodecAvailable: true,
	}
	// PrivateGEImplicitVRBigEndianPixelData is GE's private syntax
	// 1.2.840.113619.5.2: an Implicit VR Little Endian data set whose native
	// Pixel Data is Big Endian (DCMTK EXS_PrivateGE_LEI_WithBigEndianPixelData,
	// GDCM ImplicitVRBigEndianPrivateGE).
	PrivateGEImplicitVRBigEndianPixelData = Syntax{
		UID:            "1.2.840.113619.5.2",
		Name:           "Private GE Implicit VR Little Endian with Big Endian Pixel Data",
		ExplicitVR:     false,
		ByteOrder:      binary.LittleEndian,
		Supported:      true,
		CodecAvailable: true,
	}
)

// LegacySyntax returns the legacy encoding identified by uid.
func LegacySyntax(uid string) (Syntax, bool) {
	switch NormalizeUID(uid) {
	case ImplicitVRBigEndianACRNEMA.UID:
		return ImplicitVRBigEndianACRNEMA, true
	case PrivateGEImplicitVRBigEndianPixelData.UID:
		return PrivateGEImplicitVRBigEndianPixelData, true
	}
	return Syntax{}, false
}

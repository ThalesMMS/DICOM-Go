package vps

import "github.com/ThalesMMS/dicom-go/internal/iccprofile"

// SRGBICCProfile returns a fresh deterministic ICC v4 matrix/TRC monitor
// profile for the sRGB colour space. Callers may safely mutate the result.
func SRGBICCProfile() []byte { return iccprofile.SRGB() }

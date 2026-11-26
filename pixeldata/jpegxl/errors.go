package jpegxl

import (
	"errors"
	"fmt"
)

var (
	// ErrInvalidCodestream reports a malformed or truncated JPEG XL file,
	// container or codestream.
	ErrInvalidCodestream = errors.New("jpegxl: invalid codestream")
	// ErrUnsupported reports a valid JPEG XL feature this pure-Go profile
	// does not implement. Callers may fall back to another backend.
	ErrUnsupported = errors.New("jpegxl: unsupported feature")
	// ErrNoReconstructionData reports a JPEG XL file without a JPEG bitstream
	// reconstruction (jbrd) box, so no original JPEG can be restored.
	ErrNoReconstructionData = errors.New("jpegxl: no JPEG reconstruction data")
	// ErrNotReversible reports a JPEG that cannot be recompressed so that
	// reconstruction reproduces its exact bytes.
	ErrNotReversible = errors.New("jpegxl: JPEG cannot be recompressed reversibly")
	// ErrLimit reports input exceeding a resource bound.
	ErrLimit = errors.New("jpegxl: resource limit exceeded")
)

var errTruncated = fmt.Errorf("%w: truncated bit stream", ErrInvalidCodestream)

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidCodestream, fmt.Sprintf(format, args...))
}

// recoverCorrupt turns a runtime panic on hostile input into
// ErrInvalidCodestream at the public decoder boundary, so a malformed file
// cannot crash the caller. Panics are bugs; the fuzz and corruption tests
// look for them.
func recoverCorrupt(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("%w: internal decoder fault: %v", ErrInvalidCodestream, r)
	}
}

func unsupported(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, fmt.Sprintf(format, args...))
}

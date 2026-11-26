package jpip

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Target is what a JPIP target name resolves to: the JPEG 2000 codestreams
// of one image, in frame order. The JPIP stream identifier N (one-based, as
// jpip.Client sends Frame+1) selects Codestreams[N-1].
//
// A Server treats a resolved Target as immutable while it is cached, which
// suits DICOM instances: their pixel data never changes under one SOP
// Instance UID.
type Target struct {
	Codestreams [][]byte
}

// TargetProvider resolves the opaque target names of JPIP requests. Names
// can identify patients or studies; the server never logs them, and a
// provider should not either.
type TargetProvider interface {
	Target(ctx context.Context, name string) (Target, error)
}

// ErrTargetNotFound is returned by providers for unknown names; the server
// answers 404 Not Found.
var ErrTargetNotFound = errors.New("dicom: JPIP target not found")

// ErrUnsupportedTarget reports pixel data that cannot be served over JPIP.
var ErrUnsupportedTarget = errors.New("dicom: pixel data cannot be served as JPIP codestreams")

// StaticTargets is a fixed in-memory provider.
type StaticTargets map[string]Target

// Target implements TargetProvider.
func (s StaticTargets) Target(_ context.Context, name string) (Target, error) {
	target, ok := s[name]
	if !ok {
		return Target{}, ErrTargetNotFound
	}
	return target, nil
}

// TargetFromObject returns the frames of encapsulated JPEG 2000 or HTJ2K
// Pixel Data as a Target, one codestream per frame, without their Item pad
// byte.
func TargetFromObject(ctx context.Context, transferSyntaxUID string, obj *object.Object) (Target, error) {
	if !transfer.IsJPEG2000TransferSyntax(transferSyntaxUID) {
		return Target{}, fmt.Errorf("%w: transfer syntax is not JPEG 2000", ErrUnsupportedTarget)
	}
	pixel, err := pixeldata.Extract(obj)
	if err != nil {
		return Target{}, errors.Join(ErrUnsupportedTarget, err)
	}
	if !pixel.Encapsulated {
		return Target{}, fmt.Errorf("%w: pixel data is not encapsulated", ErrUnsupportedTarget)
	}
	metadata, err := pixeldata.ExtractMetadata(obj)
	if err != nil {
		return Target{}, errors.Join(ErrUnsupportedTarget, err)
	}
	frames := max(metadata.NumberOfFrames, 1)
	plan, err := encapsulated.FromFragments(ctx, pixel.Sequence, obj, frames, encapsulated.JPEG2000, encapsulated.Limits{})
	if err != nil {
		return Target{}, errors.Join(ErrUnsupportedTarget, err)
	}
	target := Target{Codestreams: make([][]byte, plan.Len())}
	for i := range target.Codestreams {
		view, err := plan.Frame(ctx, i)
		if err != nil {
			return Target{}, errors.Join(ErrUnsupportedTarget, err)
		}
		data := view.Data
		if view.Padded {
			data = data[:len(data)-1]
		}
		target.Codestreams[i] = append([]byte(nil), data...)
	}
	return target, nil
}

// targetID derives the JPIP target identifier (tid) from the codestream
// bytes, so it changes when the data does and carries no identifiers.
func targetID(target Target) string {
	h := sha256.New()
	var length [8]byte
	for _, codestream := range target.Codestreams {
		binary.BigEndian.PutUint64(length[:], uint64(len(codestream)))
		h.Write(length[:])
		h.Write(codestream)
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

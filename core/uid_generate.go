package core

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
)

// UUIDRoot is the root of UIDs derived from a UUID (PS3.5 B.2, ISO/IEC
// 9834-8): "2.25." followed by the UUID as one unsigned decimal integer.
const UUIDRoot = "2.25"

// MinRootedUIDDigits is the fewest random digits NewUIDWithRoot appends to an
// organizational root (about 66 bits).
const MinRootedUIDDigits = 20

// ErrUIDRoot reports an organizational root that is not a valid UID or leaves
// too little room below the 64-character limit.
var ErrUIDRoot = errors.New("dicom: invalid UID root")

// NewUID returns a new UID under 2.25 from a random (version 4) UUID drawn
// from crypto/rand.
func NewUID() (string, error) {
	return NewUIDFromReader(rand.Reader)
}

// NewUIDFromReader is NewUID with the 16 random bytes read from r.
func NewUIDFromReader(r io.Reader) (string, error) {
	var uuid [16]byte
	if _, err := io.ReadFull(r, uuid[:]); err != nil {
		return "", fmt.Errorf("dicom: generate UID: %w", err)
	}
	return UIDFromUUID(setUUIDVersion(uuid, 4)), nil
}

// UIDFromUUID returns the 2.25 UID of uuid, whose version and variant bits
// the caller has set.
func UIDFromUUID(uuid [16]byte) string {
	return UUIDRoot + "." + new(big.Int).SetBytes(uuid[:]).String()
}

// NameUID returns a deterministic UID under 2.25 for the name parts: a
// name-based UUID of version 8 (RFC 9562 B.2) from the first 128 bits of the
// SHA-256 of the parts, each prefixed by its length so that part boundaries
// count. The same parts always give the same UID.
func NameUID(parts ...string) string {
	return UIDFromUUID(setUUIDVersion(nameDigest(sha256.New(), parts), 8))
}

// KeyedNameUID is NameUID with HMAC-SHA-256 under key, so that the UID does
// not reveal its parts to anyone without the key: the stable replacement of
// an identifying UID in de-identification.
func KeyedNameUID(key []byte, parts ...string) string {
	return UIDFromUUID(setUUIDVersion(nameDigest(hmac.New(sha256.New, key), parts), 8))
}

// NewUIDWithRoot returns a new UID under an organizational root: root, a dot
// and random decimal digits (no leading zero) filling the 64-character limit,
// at most 39 of them (about 128 bits). The root must be a valid UID that
// leaves room for MinRootedUIDDigits.
func NewUIDWithRoot(root string) (string, error) {
	return NewUIDWithRootFromReader(root, rand.Reader)
}

// NewUIDWithRootFromReader is NewUIDWithRoot with the random digits drawn
// from r.
func NewUIDWithRootFromReader(root string, r io.Reader) (string, error) {
	if !IsValidUID(root) {
		return "", fmt.Errorf("%w: %q is not a UID", ErrUIDRoot, root)
	}
	digits := min(64-len(root)-1, 39)
	if digits < MinRootedUIDDigits {
		return "", fmt.Errorf("%w: %q leaves %d digits, fewer than %d", ErrUIDRoot, root, max(digits, 0), MinRootedUIDDigits)
	}
	// A uniform value in [10^(digits-1), 10^digits) has exactly digits digits.
	low := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits-1)), nil)
	span := new(big.Int).Sub(new(big.Int).Mul(low, big.NewInt(10)), low)
	value, err := rand.Int(r, span)
	if err != nil {
		return "", fmt.Errorf("dicom: generate UID: %w", err)
	}
	return root + "." + value.Add(value, low).String(), nil
}

// setUUIDVersion sets the version nibble and the RFC 9562 variant bits (10).
func setUUIDVersion(uuid [16]byte, version byte) [16]byte {
	uuid[6] = uuid[6]&0x0f | version<<4
	uuid[8] = uuid[8]&0x3f | 0x80
	return uuid
}

func nameDigest(h hash.Hash, parts []string) [16]byte {
	var length [8]byte
	for _, part := range parts {
		n := uint64(len(part))
		for i := range length {
			length[i] = byte(n >> (56 - 8*i))
		}
		h.Write(length[:])
		io.WriteString(h, part)
	}
	var out [16]byte
	copy(out[:], h.Sum(nil))
	return out
}

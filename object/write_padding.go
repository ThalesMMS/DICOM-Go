package object

import (
	"compress/flate"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type PaddingPolicy = parser.PaddingPolicy
type PaddingOptions = parser.PaddingOptions

const (
	PaddingPreserve        = parser.PaddingPreserve
	PaddingRemove          = parser.PaddingRemove
	PaddingCreate          = parser.PaddingCreate
	DefaultMaxPaddingBytes = parser.DefaultMaxPaddingBytes
)

// ValidateSerializationOptions validates the Deflate and padding controls for
// a target syntax without writing or reading values. DeflateLevel is copied by
// each writer at entry; the caller must not mutate it concurrently with entry.
func ValidateSerializationOptions(syntax transfer.Syntax, level *int, padding PaddingOptions) error {
	var err error
	syntax, err = validateWritableSyntax(syntax)
	if err != nil {
		return err
	}
	if err := parser.ValidatePaddingOptions(padding); err != nil {
		return err
	}
	if level != nil {
		if *level < 0 || *level > 9 {
			return fmt.Errorf("dicom: DeflateLevel must be between 0 and 9")
		}
		if !syntax.Deflated {
			return fmt.Errorf("dicom: DeflateLevel requires Deflated Explicit VR Little Endian")
		}
	}
	return nil
}

func serializationLevel(syntax transfer.Syntax, level *int, padding PaddingOptions) (int, error) {
	value := flate.DefaultCompression
	if level != nil {
		value = *level
		level = &value
	}
	if err := ValidateSerializationOptions(syntax, level, padding); err != nil {
		return 0, err
	}
	return value, nil
}

type serializationCounter struct {
	destination io.Writer
	n           int64
}

func (c *serializationCounter) Write(p []byte) (int, error) {
	n, err := c.destination.Write(p)
	c.n += int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

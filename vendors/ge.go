package vendors

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ThalesMMS/dicom-go/object"
)

// GEProtocolEntry is one KEY "VALUE" line of a GE Protocol Data Block.
type GEProtocolEntry struct {
	Key, Value string
}

// GEProtocolDataBlock is a decoded GE Protocol Data Block, (0025,xx1B) of
// "GEMS_SERS_01": a 32-bit little endian length followed by a gzip stream
// holding either KEY "VALUE" lines or, on newer systems, an XML document.
type GEProtocolDataBlock struct {
	// Name is the file name recorded in the gzip header, when present.
	Name string
	// Text is the decompressed block.
	Text string
	// XML reports an XML document; Entries is set only for KEY "VALUE" text.
	XML     bool
	Entries []GEProtocolEntry
	// Truncated reports a gzip stream that ends before its trailer, as some
	// scanners write it: Text is everything the stream decoded, and its
	// checksum could not be verified.
	Truncated bool
}

// Value returns the value of the first entry with key.
func (b *GEProtocolDataBlock) Value(key string) (string, bool) {
	if b == nil {
		return "", false
	}
	for _, entry := range b.Entries {
		if entry.Key == key {
			return entry.Value, true
		}
	}
	return "", false
}

// GEProtocolDataBlockOf decodes the Protocol Data Block of dataset.
func GEProtocolDataBlockOf(dataset *object.Object, limits Limits) (*GEProtocolDataBlock, error) {
	limits = limits.normalized()
	raw, _, err := privateValue(dataset, 0x0025, []string{"GEMS_SERS_01"}, 0x1B, limits)
	if err != nil {
		return nil, err
	}
	return ParseGEProtocolDataBlock(raw, limits)
}

// ParseGEProtocolDataBlock decodes a Protocol Data Block value.
func ParseGEProtocolDataBlock(data []byte, limits Limits) (*GEProtocolDataBlock, error) {
	limits = limits.normalized()
	if len(data) > limits.MaxBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrLimit, len(data))
	}
	c := cursor{data: data}
	length, err := c.uint32()
	if err != nil {
		return nil, err
	}
	// The value is padded to an even length, so one byte may follow.
	if int64(length) > int64(c.remaining()) {
		return nil, fmt.Errorf("%w: Protocol Data Block declares %d bytes, %d remain", ErrInvalid, length, c.remaining())
	}
	reader, err := gzip.NewReader(bytes.NewReader(data[4 : 4+length]))
	if err != nil {
		return nil, fmt.Errorf("%w: Protocol Data Block: %w", ErrInvalid, err)
	}
	reader.Multistream(false)
	text, err := io.ReadAll(io.LimitReader(reader, int64(limits.MaxDecodedBytes)+1))
	block := &GEProtocolDataBlock{Name: reader.Name}
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF) && len(text) > 0:
		block.Truncated = true
	case err != nil:
		return nil, fmt.Errorf("%w: Protocol Data Block: %w", ErrInvalid, err)
	}
	if len(text) > limits.MaxDecodedBytes {
		return nil, fmt.Errorf("%w: Protocol Data Block decodes to more than %d bytes", ErrLimit, limits.MaxDecodedBytes)
	}
	block.Text = string(text)
	if trimmed := strings.TrimLeft(block.Text, " \t\r\n\uFEFF"); strings.HasPrefix(trimmed, "<") {
		block.XML = true
		return block, nil
	}
	for _, line := range strings.Split(block.Text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(block.Entries) == limits.MaxEntries {
			return nil, fmt.Errorf("%w: more than %d entries", ErrLimit, limits.MaxEntries)
		}
		key, value, _ := strings.Cut(line, " ")
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		block.Entries = append(block.Entries, GEProtocolEntry{Key: key, Value: value})
	}
	return block, nil
}

package hl7

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

var (
	// ErrBatchEnvelope reports a missing, misplaced or malformed batch envelope.
	ErrBatchEnvelope = fmt.Errorf("%w: invalid batch envelope", ErrSyntax)
	// ErrBatchCount reports an invalid advertised batch or message count.
	ErrBatchCount = fmt.Errorf("%w: invalid batch trailer count", ErrSyntax)
)

// BatchError locates a failure without exposing envelope or message values.
type BatchError struct {
	// Offset is the zero-based byte offset of the segment or message at fault.
	Offset int64
	// Batch and Message are one-based. Zero identifies a file-level error or
	// a batch envelope error, respectively.
	Batch, Message int
	Err            error
}

func (e *BatchError) Error() string {
	return fmt.Sprintf("hl7: batch file at byte %d, batch %d, message %d: %v",
		e.Offset, e.Batch, e.Message, e.Err)
}

func (e *BatchError) Unwrap() error { return e.Err }

// BatchOptions bounds the complete input independently of the options used
// when decoding each contained message. Zero values take the defaults.
type BatchOptions struct {
	MaxBytes       int // default 32 MiB, including envelopes and line endings
	MaxBatches     int // default 1000
	MaxMessages    int // default 10000, cumulative across all batches
	MessageOptions ParseOptions
}

// BatchFile contains one or more complete batches in input order. Header and
// Trailer are nil when the file does not use a paired FHS/FTS envelope.
type BatchFile struct {
	Header, Trailer *Segment
	Batches         []Batch
}

// Batch contains the BHS/BTS pair and the messages between them.
type Batch struct {
	Header, Trailer Segment
	Messages        []*Message
}

// ParseBatch decodes a strict ER7 batch profile: one or more paired BHS/BTS
// ranges, optionally enclosed by one FHS/FTS pair. Present BTS-1 and FTS-1
// counts must match the file. Messages use their own MSH framing and character
// set; envelope text uses MessageOptions.DefaultCharacterSet. CR, LF and CRLF
// are accepted. The caller owns input. Results are returned only after the
// complete file, including its trailing boundary, has been validated.
// Cancellation is checked between reads; it cannot interrupt a blocked Reader.
func ParseBatch(ctx context.Context, input io.Reader, options BatchOptions) (*BatchFile, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if input == nil {
		return nil, &BatchError{Err: ErrStreamRead}
	}
	if options.MaxBytes < 0 || options.MaxBatches < 0 || options.MaxMessages < 0 {
		return nil, &BatchError{Err: fmt.Errorf("%w: negative batch limit", ErrLimit)}
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = 32 << 20
	}
	if options.MaxBatches == 0 {
		options.MaxBatches = 1000
	}
	if options.MaxMessages == 0 {
		options.MaxMessages = 10000
	}
	options.MessageOptions = options.MessageOptions.withDefaults()
	parser := batchParser{ctx: ctx, input: newER7Input(input), options: options}
	return parser.parse()
}

type batchParser struct {
	ctx      context.Context
	input    *er7Input
	options  BatchOptions
	messages int
}

func (p *batchParser) parse() (*BatchFile, error) {
	var result BatchFile
	name, err := p.nextEnvelopeName()
	if err != nil {
		return nil, &BatchError{Offset: p.input.offset, Err: err}
	}

	var fileDelimiters Delimiters
	if name == "FHS" {
		offset := p.input.offset
		header, err := p.readEnvelope("FHS", &fileDelimiters)
		if err != nil {
			return nil, &BatchError{Offset: offset, Err: err}
		}
		result.Header = &header
		name, err = p.nextEnvelopeName()
		if err != nil {
			return nil, &BatchError{Offset: p.input.offset, Err: err}
		}
	}
	for name == "BHS" {
		number := len(result.Batches) + 1
		if number > p.options.MaxBatches {
			return nil, &BatchError{Offset: p.input.offset, Batch: number, Err: fmt.Errorf("%w: number of batches", ErrLimit)}
		}
		batch, err := p.readBatch(number)
		if err != nil {
			return nil, err
		}
		result.Batches = append(result.Batches, batch)
		name, err = p.nextEnvelopeName()
		if err != nil {
			return nil, &BatchError{Offset: p.input.offset, Err: err}
		}
	}
	if len(result.Batches) == 0 {
		return nil, &BatchError{Offset: p.input.offset, Err: fmt.Errorf("%w: missing BHS", ErrBatchEnvelope)}
	}
	if result.Header != nil {
		offset := p.input.offset
		if name != "FTS" {
			return nil, &BatchError{Offset: offset, Err: fmt.Errorf("%w: expected FTS", ErrBatchEnvelope)}
		}
		trailer, err := p.readEnvelope("FTS", &fileDelimiters)
		if err == nil {
			err = validateBatchCount(trailer, len(result.Batches))
		}
		if err != nil {
			return nil, &BatchError{Offset: offset, Err: err}
		}
		result.Trailer = &trailer
		name, err = p.nextEnvelopeName()
		if err != nil {
			return nil, &BatchError{Offset: p.input.offset, Err: err}
		}
	}
	if name != "" {
		return nil, &BatchError{Offset: p.input.offset, Err: fmt.Errorf("%w: data after the last trailer", ErrBatchEnvelope)}
	}
	return &result, nil
}

func (p *batchParser) readBatch(number int) (Batch, error) {
	var batch Batch
	var delimiters Delimiters
	offset := p.input.offset
	var err error
	batch.Header, err = p.readEnvelope("BHS", &delimiters)
	if err != nil {
		return Batch{}, &BatchError{Offset: offset, Batch: number, Err: err}
	}
	for {
		name, err := p.nextEnvelopeName()
		offset = p.input.offset
		if err != nil {
			return Batch{}, &BatchError{Offset: offset, Batch: number, Err: err}
		}
		switch name {
		case "MSH":
			if p.messages >= p.options.MaxMessages {
				err = fmt.Errorf("%w: number of messages", ErrLimit)
				return Batch{}, &BatchError{Offset: offset, Batch: number, Message: len(batch.Messages) + 1, Err: err}
			}
			message, err := p.readMessage()
			if err != nil {
				return Batch{}, &BatchError{Offset: offset, Batch: number, Message: len(batch.Messages) + 1, Err: err}
			}
			batch.Messages = append(batch.Messages, message)
			p.messages++
		case "BTS":
			batch.Trailer, err = p.readEnvelope("BTS", &delimiters)
			if err == nil {
				err = validateBatchCount(batch.Trailer, len(batch.Messages))
			}
			if err != nil {
				return Batch{}, &BatchError{Offset: offset, Batch: number, Err: err}
			}
			return batch, nil
		default:
			return Batch{}, &BatchError{Offset: offset, Batch: number, Err: fmt.Errorf("%w: expected MSH or BTS", ErrBatchEnvelope)}
		}
	}
}

// nextEnvelopeName ignores blank separator lines while charging their bytes
// to the file budget. An empty name denotes the end of input.
func (p *batchParser) nextEnvelopeName() (string, error) {
	var blank [1]byte
	for {
		name, err := p.peekName()
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if name != "" && name[0] != '\r' && name[0] != '\n' {
			return name, nil
		}
		if _, err := p.readSegment(blank[:0], len(blank)); err != nil {
			return "", err
		}
	}
}

func (p *batchParser) peekName() (string, error) {
	name, err := p.input.peekName(p.ctx)
	if err == nil && p.input.offset >= int64(p.options.MaxBytes) {
		return "", fmt.Errorf("%w: batch file bytes", ErrLimit)
	}
	return name, err
}

func (p *batchParser) readSegment(dst []byte, maximum int) ([]byte, error) {
	remaining := p.options.MaxBytes - int(p.input.offset)
	return p.input.readSegment(p.ctx, dst, min(maximum, len(dst)+remaining))
}

func (p *batchParser) readMessage() (*Message, error) {
	options := p.options.MessageOptions
	var raw []byte
read:
	for {
		var err error
		raw, err = p.readSegment(raw, options.MaxBytes)
		if err != nil {
			return nil, err
		}
		name, peekErr := p.input.peekName(p.ctx)
		if errors.Is(peekErr, io.EOF) {
			break
		}
		if peekErr != nil {
			return nil, peekErr
		}
		switch name {
		case "MSH", "FHS", "BHS", "BTS", "FTS":
			break read
		}
	}
	return ParseWithOptions(raw, options)
}

func (p *batchParser) readEnvelope(name string, delimiters *Delimiters) (Segment, error) {
	options := ParseOptions{DefaultCharacterSet: p.options.MessageOptions.DefaultCharacterSet}.withDefaults()
	data, err := p.readSegment(nil, options.MaxBytes)
	if err != nil {
		return Segment{}, err
	}
	header := name == "FHS" || name == "BHS"
	if header {
		*delimiters, err = readHeaderDelimiters(data, name)
		if err != nil {
			return Segment{}, fmt.Errorf("%w: %w", ErrBatchEnvelope, err)
		}
	}
	text, err := decodeCharacterSet(options.DefaultCharacterSet, data)
	if err != nil {
		return Segment{}, err
	}
	line := strings.TrimRight(text, "\r\n")
	parts := splitSegment(line, *delimiters, header, options.MaxFields)
	if parts[0] != name {
		return Segment{}, fmt.Errorf("%w: invalid %s field separator", ErrBatchEnvelope, name)
	}
	segment, parseErr := parseSegmentFields(parts, *delimiters, options.DefaultCharacterSet, header, options)
	if parseErr != nil {
		return Segment{}, parseErr
	}
	return segment, nil
}

func validateBatchCount(trailer Segment, actual int) error {
	count := trailer.Field(1)
	if count == nil {
		return nil
	}
	if len(count) != 1 || len(count[0]) != 1 || len(count[0][0]) != 1 {
		return fmt.Errorf("%w: expected a single value in %s-1", ErrBatchCount, trailer.Name)
	}
	value := count.String()
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || n != uint64(actual) {
		return fmt.Errorf("%w: %s-1 must equal the observed count", ErrBatchCount, trailer.Name)
	}
	return nil
}

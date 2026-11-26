package hl7

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

func TestParseBatchWireAcrossEverySplit(t *testing.T) {
	latin := message(
		`MSH|^~\&|A|B|C|D|20261115120000||ADT^A08|L1|P|2.5.1||||||8859/1`,
		"PID|1||ID1||\xC9rable^Anne|"+`\XE9\\F\`,
	)
	utf := []byte("MSH#$*@%#A#B#C#D#20261115120000##ADT$A08#U1#P#2.5.1######UNICODE UTF-8\r\n" +
		"PID#1##ID2##青木$春#left@F@right\r\n")
	asci := message(`MSH|^~\&|A|B|C|D|20261115120000||ADT^A08|A1|P|2.5.1`, "PID|1||ID3")
	wire := bytes.Join([][]byte{
		[]byte("FHS#^~\\&#SyntheticFile\r\nBHS!^~\\&!\\ZFirst!Batch\\!Next\n"),
		latin,
		utf,
		[]byte("BTS!2!Group1\r\n\rBHS|^~\\&|Second\r\n"),
		asci,
		[]byte("BTS|01\r\nFTS#2#SyntheticTrailer"),
	}, nil)
	expected := make([]*Message, 0, 3)
	for _, data := range [][]byte{latin, utf, asci} {
		m, err := Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		expected = append(expected, m)
	}
	check := func(reader io.Reader) {
		t.Helper()
		file, err := ParseBatch(context.Background(), reader, BatchOptions{MaxBytes: len(wire)})
		if err != nil {
			t.Fatal(err)
		}
		if file.Header.Name != "FHS" || file.Header.Field(1).String() != "#" ||
			file.Header.Field(2).String() != `^~\&` || file.Header.Field(3).String() != "SyntheticFile" ||
			file.Trailer.Name != "FTS" || file.Trailer.Field(1).String() != "2" || len(file.Batches) != 2 {
			t.Fatalf("file envelope: %+v", file)
		}
		first, second := file.Batches[0], file.Batches[1]
		if first.Header.Name != "BHS" || first.Header.Field(1).String() != "!" ||
			first.Header.Field(3).String() != "\\ZFirst!Batch\\" || first.Header.Field(4).String() != "Next" || first.Trailer.Name != "BTS" ||
			first.Trailer.Field(1).String() != "2" || second.Trailer.Field(1).String() != "01" {
			t.Fatalf("batch envelopes: %+v / %+v", first, second)
		}
		if !reflect.DeepEqual(first.Messages, expected[:2]) || !reflect.DeepEqual(second.Messages, expected[2:]) {
			t.Fatal("batch decoding differs from decoding the individual messages")
		}
		pid, _ := first.Messages[1].Segment("PID")
		if pid.Value(5, 1, 1) != "青木" || pid.Field(6).String() != "left#right" {
			t.Fatalf("custom UTF-8 delimiters or escape changed: %+v", pid)
		}
	}
	for cut := 0; cut <= len(wire); cut++ {
		check(io.MultiReader(bytes.NewReader(wire[:cut]), bytes.NewReader(wire[cut:])))
	}
	check(iotest.OneByteReader(bytes.NewReader(wire)))
}

func TestParseBatchBareAndEmptyCounts(t *testing.T) {
	const wire = "\r\nBHS|^~\\&|Empty\nBTS\rBHS|^~\\&|Periodic\r\nBTS||Optional comment\n"
	file, err := ParseBatch(nil, strings.NewReader(wire), BatchOptions{})
	if err != nil || file.Header != nil || file.Trailer != nil || len(file.Batches) != 2 {
		t.Fatalf("bare batches: %+v, %v", file, err)
	}
	for _, batch := range file.Batches {
		if len(batch.Messages) != 0 || batch.Trailer.Field(1) != nil {
			t.Fatalf("empty batch or omitted count changed: %+v", batch)
		}
	}

	const wrapped = "FHS|^~\\&\rBHS|^~\\&\rBTS|0\rFTS||Optional comment\r"
	file, err = ParseBatch(nil, strings.NewReader(wrapped), BatchOptions{})
	if err != nil || file.Trailer.Field(1) != nil || file.Trailer.Field(2).String() != "Optional comment" {
		t.Fatalf("omitted file count: %+v, %v", file, err)
	}
}

func TestParseBatchChecksEnvelopeAndCountsAtomically(t *testing.T) {
	const patientMarker = "SYNTHETIC_CLINICAL_CANARY"
	const fh = "FHS|^~\\&|Synthetic\r"
	const bh = "BHS|^~\\&|Synthetic\r"
	const msh = "MSH|^~\\&|A|B|C|D|20261115120000||ADT^A08|C1|P|2.5.1\r"
	const data = msh + "PID|1||ID1||" + patientMarker + "\r"
	const good = bh + data + "BTS|1\r"
	for _, tc := range []struct {
		name    string
		source  string
		offset  int
		batch   int
		message int
		want    error
	}{
		{"empty", "", 0, 0, 0, ErrBatchEnvelope},
		{"standalone_message", data, 0, 0, 0, ErrBatchEnvelope},
		{"file_without_batch", fh + "FTS|0", len(fh), 0, 0, ErrBatchEnvelope},
		{"file_missing_trailer", fh + good, len(fh + good), 0, 0, ErrBatchEnvelope},
		{"file_trailer_without_header", good + "FTS|1", len(good), 0, 0, ErrBatchEnvelope},
		{"batch_missing_trailer", bh + data, len(bh + data), 1, 0, ErrBatchEnvelope},
		{"nested_batch", bh + data + good, len(bh + data), 1, 0, ErrBatchEnvelope},
		{"wrong_file_trailer_order", fh + bh + data + "FTS|1\rBTS|1", len(fh + bh + data), 1, 0, ErrBatchEnvelope},
		{"extra_message_after_trailer", fh + good + "FTS|1\r" + data, len(fh + good + "FTS|1\r"), 0, 0, ErrBatchEnvelope},
		{"bad_header_delimiters", "BHS|^^\\&|" + patientMarker + "\rBTS|0", 0, 1, 0, ErrBatchEnvelope},
		{"wrong_trailer_separator", bh + data + "BTS!1!" + patientMarker, len(bh + data), 1, 0, ErrBatchEnvelope},
		{"wrong_message_count", bh + data + "BTS|0|" + patientMarker, len(bh + data), 1, 0, ErrBatchCount},
		{"wrong_batch_count", fh + good + "FTS|2|" + patientMarker, len(fh + good), 0, 0, ErrBatchCount},
		{"non_decimal_count", bh + data + "BTS|1.5|" + patientMarker, len(bh + data), 1, 0, ErrBatchCount},
		{"compound_count", bh + data + "BTS|1^0|" + patientMarker, len(bh + data), 1, 0, ErrBatchCount},
		{"overflow_count", fh + good + "FTS|18446744073709551616|" + patientMarker, len(fh + good), 0, 0, ErrBatchCount},
		{"malformed_second_message", bh + data + "MSH|^^\\&|" + patientMarker + "\rBTS|2", len(bh + data), 1, 2, ErrSyntax},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ParseBatch(context.Background(), strings.NewReader(tc.source), BatchOptions{})
			var detail *BatchError
			if result != nil || !errors.Is(err, tc.want) || !errors.As(err, &detail) {
				t.Fatalf("returned %+v, %v. Expected no file and %v", result, err, tc.want)
			}
			if detail.Offset != int64(tc.offset) || detail.Batch != tc.batch || detail.Message != tc.message {
				t.Fatalf("wrong error location: %+v", detail)
			}
			if strings.Contains(err.Error(), patientMarker) {
				t.Fatal("error exposed a field value")
			}
		})
	}
}

func TestParseBatchBudgetScopesAndExactBytes(t *testing.T) {
	const bh = "BHS|^~\\&|Synthetic\r"
	const msh = "MSH|^~\\&|A|B|C|D|20261115120000||ADT^A08|C1|P|2.5.1\r"
	const messageWire = msh + "PID|1||ID1\r"
	const batchWire = bh + messageWire + "BTS|1\r"
	const fileWire = "FHS|^~\\&\r" + batchWire + batchWire + "FTS|2\r"
	for _, tc := range []struct {
		name    string
		opts    BatchOptions
		wantErr bool
		batch   int
		message int
	}{
		{"exact_caps", BatchOptions{MaxBytes: len(fileWire), MaxBatches: 2, MaxMessages: 2,
			MessageOptions: ParseOptions{MaxBytes: len(messageWire), MaxSegments: 2}}, false, 0, 0},
		{"one_byte_short_file", BatchOptions{MaxBytes: len(fileWire) - 1}, true, 0, 0},
		{"one_byte_short_message", BatchOptions{MessageOptions: ParseOptions{MaxBytes: len(messageWire) - 1}}, true, 1, 1},
		{"too_many_batches", BatchOptions{MaxBatches: 1}, true, 2, 0},
		{"cumulative_message_cap", BatchOptions{MaxMessages: 1}, true, 2, 1},
		{"per_message_segment_cap", BatchOptions{MessageOptions: ParseOptions{MaxSegments: 1}}, true, 1, 1},
		{"per_message_field_cap", BatchOptions{MessageOptions: ParseOptions{MaxFields: 1}}, true, 1, 1},
		{"negative_max_bytes", BatchOptions{MaxBytes: -1}, true, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ParseBatch(context.Background(), strings.NewReader(fileWire), tc.opts)
			if !tc.wantErr {
				if err != nil || len(result.Batches) != 2 || len(result.Batches[1].Messages) != 1 {
					t.Fatalf("exact-limit result: %+v, %v", result, err)
				}
				return
			}
			var detail *BatchError
			if result != nil || !errors.Is(err, ErrLimit) || !errors.As(err, &detail) ||
				detail.Batch != tc.batch || detail.Message != tc.message {
				t.Fatalf("limited result: %+v, %v", result, err)
			}
		})
	}
}

func TestParseBatchUsesSeparateEnvelopeCharacterSet(t *testing.T) {
	const wire = "BHS|^~\\&|Réception\rMSH|^~\\&|A|B|||||ADT^A08|C|P|2.5.1||||||8859/1\rPID|1||ID1||\xC9rable\rBTS|1\r"
	result, err := ParseBatch(context.Background(), strings.NewReader(wire), BatchOptions{
		MessageOptions: ParseOptions{DefaultCharacterSet: "UNICODE UTF-8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	batch := result.Batches[0]
	pid, _ := batch.Messages[0].Segment("PID")
	if batch.Header.Field(3).String() != "Réception" || pid.Field(5).String() != "Érable" {
		t.Fatal("envelope and message character sets were not kept independent")
	}
	result, err = ParseBatch(context.Background(), strings.NewReader(wire), BatchOptions{})
	if result != nil || !errors.Is(err, ErrCharset) || strings.Contains(err.Error(), "Réception") {
		t.Fatalf("default envelope charset result: %+v, %v", result, err)
	}
}

func TestParseBatchReadFailureAndCancellation(t *testing.T) {
	const wire = "FHS|^~\\&\rBHS|^~\\&\rBTS|0\rFTS|1\r"
	ctx, cancel := context.WithCancel(context.Background())
	result, err := ParseBatch(ctx, batchReaderFunc(func(p []byte) (int, error) {
		cancel()
		return copy(p, wire), io.EOF
	}), BatchOptions{})
	if result != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled result: %+v, %v", result, err)
	}

	const marker = "SYNTHETIC_IO_CANARY"
	result, err = ParseBatch(context.Background(), io.MultiReader(strings.NewReader(wire), batchReaderFunc(func([]byte) (int, error) {
		return 0, errors.New(marker)
	})), BatchOptions{})
	if result != nil || !errors.Is(err, ErrStreamRead) || strings.Contains(err.Error(), marker) {
		t.Fatalf("I/O failure result: %+v, %v", result, err)
	}
}

type batchReaderFunc func([]byte) (int, error)

func (f batchReaderFunc) Read(p []byte) (int, error) { return f(p) }

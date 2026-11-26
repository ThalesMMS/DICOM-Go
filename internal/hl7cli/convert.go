package hl7cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/hl7"
)

func convert(ctx context.Context, command string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(stderr)
	output := f.String("output", "", "destination file (atomic creation) or - for stdout")
	max := f.Int("max-bytes", 8<<20, "input and output byte limit (1..67108864)")
	nodes := f.Int("max-nodes", 100000, "structural XML token/node limit (1..1000000)")
	f.Usage = func() {
		fmt.Fprintf(f.Output(), "Usage: %s -output destination [options] input (or - for stdin)\n", command)
		f.PrintDefaults()
	}
	code, ok := parseFlags(f, args)
	if !ok {
		return code
	}
	if f.NArg() != 1 || *output == "" || !validBytes(*max) || *nodes < 1 || *nodes > 1000000 {
		return usage(stderr, command+": require one input, output and valid finite limits")
	}
	data, err := readInput(f.Arg(0), stdin, *max)
	if err != nil {
		return refusal(stderr, command+": input unavailable or over limit")
	}
	if ctx.Err() != nil {
		return refusal(stderr, command+": canceled")
	}
	var result []byte
	options := hl7.XMLOptions{MaxBytes: *max, MaxNodes: *nodes}
	if command == "hl72xml" {
		var message *hl7.Message
		message, err = hl7.ParseWithOptions(data, hl7.ParseOptions{MaxBytes: *max})
		if err == nil {
			result, err = hl7.MarshalXMLMessage(message, options)
		}
	} else {
		var message *hl7.Message
		message, err = hl7.ParseXMLMessage(data, options)
		if err == nil {
			result, err = message.Marshal()
		}
	}
	if err != nil || len(result) > *max {
		return refusal(stderr, command+": malformed, unsupported or over-limit message")
	}
	if ctx.Err() != nil {
		return refusal(stderr, command+": canceled")
	}
	if err = writeOutput(*output, result, stdout); err != nil {
		return refusal(stderr, command+": output could not be published")
	}
	return 0
}

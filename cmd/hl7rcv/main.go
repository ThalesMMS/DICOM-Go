package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/ThalesMMS/dicom-go/internal/hl7cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := hl7cli.Run(ctx, "hl7rcv", os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

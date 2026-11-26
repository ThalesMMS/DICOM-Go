package main

import (
	"context"
	"github.com/ThalesMMS/dicom-go/internal/clisignal"
	"github.com/ThalesMMS/dicom-go/internal/retrievecli"
	"os"
)

func main() {
	ctx, stop := clisignal.NotifyInterruptContext(context.Background())
	defer stop()
	os.Exit(retrievecli.RunWithContext(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

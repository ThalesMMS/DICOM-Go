package main

import (
	"bytes"
	"context"
	"github.com/ThalesMMS/dicom-go/internal/presentationcli"
	"github.com/ThalesMMS/dicom-go/internal/wrappercli"
	"testing"
)

func TestHelpAndUnknownOption(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
	}{{[]string{"-h"}, 0}, {[]string{"-unknown-option"}, 2}} {
		var out, errout bytes.Buffer
		if got := wrappercli.Run(context.Background(), "dcmp2pgm", tc.args, &out, &errout, presentationcli.Execute); got != tc.want {
			t.Fatalf("exit %d want %d: %s", got, tc.want, errout.String())
		}
		if errout.Len() == 0 {
			t.Fatal("missing help/diagnostic")
		}
	}
}

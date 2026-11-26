package main

import (
	"bytes"
	"context"
	"github.com/ThalesMMS/dicom-go/internal/srcli"
	"github.com/ThalesMMS/dicom-go/internal/wrappercli"
	"testing"
)

func TestHelpAndUnknownOption(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
	}{{[]string{"-h"}, 0}, {[]string{"-unknown-option"}, 2}} {
		var out, errout bytes.Buffer
		if got := wrappercli.Run(context.Background(), "dsrdump", tc.args, &out, &errout, srcli.Execute); got != tc.want {
			t.Fatalf("exit %d want %d: %s", got, tc.want, errout.String())
		}
		if errout.Len() == 0 {
			t.Fatal("missing help/diagnostic")
		}
	}
}

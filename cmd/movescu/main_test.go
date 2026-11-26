package main

import (
	"context"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/retrievecli"
)

func TestMoveAliasRefusesGetBeforeNetwork(t *testing.T) {
	if got := retrievecli.RunMoveWithContext(context.Background(), []string{"-method", "get", "-study-uid", "1.2.3"}, io.Discard, io.Discard); got != 2 {
		t.Fatalf("GET under movescu exit=%d", got)
	}
}

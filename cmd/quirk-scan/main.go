// quirk-scan generates and scans only the authored non-PHI GDCM quirk analogues.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/quirkcorpus"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

func run() error {
	dir := flag.String("dir", "testdata/quirks", "directory of authored fixtures")
	generate := flag.Bool("generate", false, "write fixtures and expected manifest; refuse different existing files")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	fixtures, e := quirkcorpus.Fixtures()
	if e != nil {
		return e
	}
	if *generate {
		if e = os.MkdirAll(*dir, 0755); e != nil {
			return e
		}
		for _, f := range fixtures {
			if e = writeOwn(filepath.Join(*dir, f.ID+".dcm"), f.Wire); e != nil {
				return e
			}
		}
		manifest, e := json.MarshalIndent(quirkcorpus.NewManifest(fixtures), "", "  ")
		if e != nil {
			return e
		}
		return writeOwn(filepath.Join(*dir, "manifest.json"), append(manifest, '\n'))
	}
	report := struct {
		Schema  int
		Runtime string
		Results []quirkcorpus.Result
	}{Schema: 1, Runtime: runtime.GOOS + "/" + runtime.GOARCH + " " + runtime.Version()}
	var failures int
	for _, f := range fixtures {
		for _, recover := range []bool{false, true} {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			result, err := quirkcorpus.Scan(ctx, filepath.Join(*dir, f.ID+".dcm"), f, recover)
			cancel()
			expected := f.Strict
			if recover {
				expected = f.Recover
			}
			if err != nil {
				result.Diagnostic = err.Error()
				failures++
			} else if result.Stages != expected {
				failures++
			}
			report.Results = append(report.Results, result)
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if e := encoder.Encode(report); e != nil {
		return e
	}
	if failures > 0 {
		return fmt.Errorf("%d unexpected stage/metadata/admission results", failures)
	}
	return nil
}

func writeOwn(path string, data []byte) error {
	if prior, e := os.ReadFile(path); e == nil {
		if !bytes.Equal(prior, data) {
			return fmt.Errorf("refusing to replace different existing file: %s", path)
		}
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	return os.WriteFile(path, data, 0644)
}

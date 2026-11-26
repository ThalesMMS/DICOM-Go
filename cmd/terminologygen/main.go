// Command terminologygen generates the PS3.16 terminology of the codes
// package: the DICOM codes of Annex D, the codes of the Context Groups and
// templates of other schemes, and the Context Groups of Annex B.
//
// Extraction reads a local copy of the official DocBook (part16.xml, from
// https://dicom.nema.org/medical/dicom/current/source/docbook/part16/) and
// writes codes/terms.tsv and codes/cid.tsv, which record the edition and the
// SHA-256 of their source. Generation turns the TSV files into the named
// constants of codes/dcm, codes/sct, codes/ucum, codes/umls and codes/ln
// (go generate in codes/). The command has no network client.
//
//	go run ./cmd/terminologygen -extract part16.xml -dir codes
//	go run ./cmd/terminologygen -dir codes
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("terminologygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	extract := fs.String("extract", "", "part16.xml to extract into the TSV files first")
	dir := fs.String("dir", "codes", "directory of the codes package")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: terminologygen [-extract part16.xml] [-dir codes]\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *extract != "" {
		x, err := extractFile(*extract)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "terminologygen: %v\n", err)
			return 1
		}
		if err := writeTSVFiles(*dir, x); err != nil {
			_, _ = fmt.Fprintf(stderr, "terminologygen: %v\n", err)
			return 1
		}
		members := 0
		for _, group := range x.ContextGroups {
			members += len(group.Codes)
		}
		_, _ = fmt.Fprintf(stdout, "terminologygen: %s: %d DCM codes, %d other codes, %d context groups, %d members\n",
			x.Edition, len(x.DCM), len(x.Others), len(x.ContextGroups), members)
	}
	files, err := generateFromDir(*dir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "terminologygen: %v\n", err)
		return 1
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			_, _ = fmt.Fprintf(stderr, "terminologygen: %v\n", err)
			return 1
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			_, _ = fmt.Fprintf(stderr, "terminologygen: %v\n", err)
			return 1
		}
	}
	return 0
}

func extractFile(path string) (Extraction, error) {
	source, err := os.Open(path)
	if err != nil {
		return Extraction{}, err
	}
	defer func() { _ = source.Close() }()
	return Extract(source)
}

func writeTSVFiles(dir string, x Extraction) error {
	for name, write := range map[string]func(io.Writer, Extraction) error{"terms.tsv": writeTerms, "cid.tsv": writeContextGroups} {
		file, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if err := write(file, x); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}

// generateFromDir reads the TSV files of dir and returns the generated
// files by path.
func generateFromDir(dir string) (map[string][]byte, error) {
	terms, err := os.Open(filepath.Join(dir, "terms.tsv"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = terms.Close() }()
	groups, err := os.Open(filepath.Join(dir, "cid.tsv"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = groups.Close() }()
	x, err := readTSV(terms, groups)
	if err != nil {
		return nil, err
	}
	return generate(dir, x)
}

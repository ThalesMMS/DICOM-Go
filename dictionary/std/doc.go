// Package std exposes the generated standard DICOM dictionary.
//
// Regenerate with:
//
//	go generate ./dictionary/std
package std

//go:generate go run ../../cmd/dicomdictgen -input ../../internal/standard/dicom.dic -commit 39fcab076dfabc5d51295141ecd46d88dc1e95d2 -output std_gen.go

// Package clitag resolves CLI attribute names without linking codecs or networking.
package clitag

import (
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
)

// Parse accepts a PS3.6 keyword, (gggg,eeee), gggg,eeee or ggggeeee.
func Parse(name string) (core.Tag, error) {
	if tag, err := core.ParseTag(name); err == nil {
		return tag, nil
	}
	if entry, ok := std.Dictionary.ByKeyword(name); ok {
		return entry.Tag, nil
	}
	return core.Tag{}, fmt.Errorf("unknown attribute %q", name)
}

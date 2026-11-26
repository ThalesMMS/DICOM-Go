package pixeldata

import (
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
)

func validateTranscodeUIDRoot(root string) error {
	if root == "" {
		return nil
	}
	if !core.IsValidUID(root) || len(root) > 64-1-core.MinRootedUIDDigits ||
		root == "1.2.840.10008" || strings.HasPrefix(root, "1.2.840.10008.") ||
		root == core.UUIDRoot || strings.HasPrefix(root, core.UUIDRoot+".") {
		return fmt.Errorf("%w: %w", ErrTranscodeUnsupported, core.ErrUIDRoot)
	}
	return nil
}

func newTranscodeUID(root string) (string, error) {
	if err := validateTranscodeUIDRoot(root); err != nil {
		return "", err
	}
	if root == "" {
		return core.NewUID()
	}
	return core.NewUIDWithRoot(root)
}

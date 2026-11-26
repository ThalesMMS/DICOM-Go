package object

import "os"

// OpenSourceFile opens a read-only DICOM source descriptor. On Windows it permits
// replacement/removal of its directory entry while deferred readers keep the
// selected file alive. Callers must close the descriptor and verify size/time
// when concurrent writes would invalidate their snapshot.
func OpenSourceFile(path string) (*os.File, error) { return openSourceFile(path) }

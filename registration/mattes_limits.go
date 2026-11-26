package registration

import (
	"errors"
	"fmt"
	"math"
)

// ErrMattesWorkspaceLimit reports an invalid or oversized Mattes histogram or
// derivative array. Each float64 array is limited to 256 MiB; the arrays one
// evaluation holds together are bounded by MattesWorkspaceBytes.
var ErrMattesWorkspaceLimit = errors.New("registration: Mattes workspace limit exceeded")

// MattesWorkspaceBytes is the total budget, in bytes, of the histogram and
// derivative accumulators one Mattes evaluation holds at once: the merged
// total plus the chunk partials evaluated but not yet merged. Each partial
// of a transform without local support holds a bins × bins × parameters
// derivative array (a local-support transform's holds one value per
// parameter), so large partials lower the number of chunks in flight, and
// with it the workers, after ITK's MaximumJointPDFDerivativesBytes (#2025).
// One partial is always allowed, so a serial evaluation of arrays within the
// 256 MiB per-array limit still runs. The budget changes only the
// concurrency, never the result: chunks are merged in index order.
const MattesWorkspaceBytes = 512 << 20

const maxMattesArrayBytes = 256 << 20

func checkMattesArraySize(rows, columns int) error {
	const float64Bytes = 8
	if rows < 1 || columns < 0 {
		return fmt.Errorf("%w: invalid %d × %d array dimensions", ErrMattesWorkspaceLimit, rows, columns)
	}
	if columns > math.MaxInt/float64Bytes/rows {
		return fmt.Errorf("%w: %d × %d array byte size cannot be represented as int", ErrMattesWorkspaceLimit, rows, columns)
	}
	if columns > maxMattesArrayBytes/float64Bytes/rows {
		return fmt.Errorf("%w: %d × %d array exceeds %d bytes", ErrMattesWorkspaceLimit, rows, columns, maxMattesArrayBytes)
	}
	return nil
}

// mattesChunksInFlight is the number of chunk partials of partialFloats
// float64 values each that budget bytes hold beside the merged total of
// totalFloats values: at least one, so that a serial evaluation always runs.
func mattesChunksInFlight(budget int64, totalFloats, partialFloats int) int {
	const float64Bytes = 8
	partial := int64(max(1, partialFloats)) * float64Bytes
	free := budget - int64(totalFloats)*float64Bytes
	if free < partial {
		return 1
	}
	return int(min(free/partial, math.MaxInt32))
}

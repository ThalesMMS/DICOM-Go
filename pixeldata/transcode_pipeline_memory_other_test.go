//go:build !windows

package pixeldata_test

import "errors"

func transcode1294ProcessMemory() (transcode1294Memory, error) {
	return transcode1294Memory{}, errors.New("Windows process memory counters unavailable")
}

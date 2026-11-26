package jpegls

import "sync"

// maxPooledSamples bounds the sample planes kept for reuse (a 4096×4096
// plane), so one very large frame does not pin its memory.
const maxPooledSamples = 4096 * 4096

// samplePlanes recycles the per-frame sample planes of the codec. A plane
// holds a frame's samples as int, eight bytes each; allocating one for
// every frame of a multi-frame object made the garbage collector a large
// share of a transcode (#1277).
var samplePlanes sync.Pool

// Keep decoding rows separate: borrowing an encoder's whole-frame buffer
// would retain that capacity even when the decoder only needs three rows.
var sampleRows sync.Pool

// getSamples returns a zeroed plane of n samples.
func getSamples(n int) []int {
	return getSampleBuffer(&samplePlanes, n)
}

func getSampleBuffer(pool *sync.Pool, n int) []int {
	if p, ok := pool.Get().(*[]int); ok && cap(*p) >= n {
		s := (*p)[:n]
		clear(s)
		return s
	}
	return make([]int, n)
}

// putSamples hands a plane back for reuse; the caller must not use it
// afterwards.
func putSamples(s []int) {
	putSampleBuffer(&samplePlanes, s)
}

func putSampleBuffer(pool *sync.Pool, s []int) {
	if cap(s) == 0 || cap(s) > maxPooledSamples {
		return
	}
	pool.Put(&s)
}

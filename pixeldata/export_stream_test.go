package pixeldata

// CountTranscodeStreams counts the outputs TranscodePath stages through its
// streaming route until the returned function restores the previous hook.
func CountTranscodeStreams(count *int) (restore func()) {
	previous := transcodeStreamStaged
	transcodeStreamStaged = func() { *count++ }
	return func() { transcodeStreamStaged = previous }
}

// ObserveTranscodeProgress tells observe the bytes of each frame
// TranscodePath's streaming route has encoded to its spool and of each write
// of its output before it is made, until the returned function restores the
// previous hook.
func ObserveTranscodeProgress(observe func(bytes int)) (restore func()) {
	previous := transcodeStreamProgress
	transcodeStreamProgress = observe
	return func() { transcodeStreamProgress = previous }
}

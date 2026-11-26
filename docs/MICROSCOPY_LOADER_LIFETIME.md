# Microscopy loader lifetime

A microscopy Loader shares worker admission across viewport generations.
Superseding a request cancels it but does not lend its source slot to a new
request until FetchTile physically returns. Cache bytes remain bounded by
the configured ceiling.

Request completion now reports context cancellation for a superseded viewport.
Callers must not compose or publish its partial layers as a successful current
image. The current viewport still has its own cancellation and completion.

Close prevents new requests, cancels the current generation, joins every
registered request and only then clears the cache. A source must eventually
return after cancellation; Close cannot safely release a source still running.
Do not call Close from a publication callback. Wrappers must release locks
that their source may need before joining; Twin MicroscopySeries swaps/detaches
the loader under its lock and closes the old one after unlocking.

TestLoaderSupersessionRetainsPhysicalAdmissionAndCloseJoins deliberately holds
a source after cancellation to verify occupied admission, no old publications,
join and final cache cleanup. Full microscopy tests pass with the race detector.

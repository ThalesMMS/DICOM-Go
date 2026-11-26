// Package tractography reads and writes materialized DICOM Tractography Results
// (PS3.3 C.8.33). It stores results; it performs no tract reconstruction or
// rendering. Read and Write return independent results or nil on error.
//
// Inputs and options, including borrowed Object payloads, must remain stable
// during calls. Other Object methods must not run while its initial snapshot is
// taken. Calls in this package serialize that snapshot and use private facades.
// Deferred retained values are refused without IO; root image bulk is ignored.
// See docs/TRACTOGRAPHY_RESULTS.md for limits, conditional origin, and IOD scope.
package tractography

# j2kcore provenance


## Adapted files

SHA-256 of each upstream file at the pinned commit:

| File | Upstream SHA-256 |
|---|---|
| `codestream.go` | `bdc492722f9ef9245002a557997d3f844d063f22b3cb1c44a3e0feef3e689e34` |
| `decode.go` | `3585e93edc2eab4bdda30536114ee888b244aa32700a4ff3edd3a039fa91598e` |
| `dwt.go` | `e6f3f75591ff3943e83740a84185b0e8664bb656bfbd6e5e06ff7c0d52c7e3e7` |
| `geometry.go` | `c96f650e9cdf0104e2e5637f143e72421b5ba50e8d631cf7a140de993e4c79e9` |
| `mq.go` | `56b634702e88142a20a389dfcb9353031382cf2f4efff17c26fad12f0164f14b` |
| `packet.go` | `b7e5836123f4f7ca551f0ad177a7aefc43c2dcb069a05e58543b96a32840f104` |
| `tagtree.go` | `ba304e98655351715acbec204fcd92f61c22373251ee22c3d63248176cd5b71a` |
| `tier1.go` | `9b513e9c09e394432e41c0f15a3e3072851d99e78fbde70d2606152a4cf0680b` |

Upstream tests, fuzz corpora and `testdata` codestreams were not copied. The
local tests use the repository's redistribution-safe pydicom corpus and its
independent native twins.

## Local modifications

- Package renamed to `j2kcore`; each file starts with an attribution header.
- `DecodeContext(ctx, codestream, Limits)` added; `Decode` delegates to it
  with `DefaultLimits`. Cancellation is checked before every tile,
  tile-component, packet, code-block and wavelet level (`packet.go`,
  `decode.go`, `dwt.go`), and no image is returned after an error.
- `limits.go` (new): caller budgets for components, tiles, resolutions, output
  samples, per-tile float32 work bytes and code-blocks per tile, checked with
  overflow-safe int64 arithmetic before allocation. The upstream fixed ceilings
  remain as an outer bound.


- Written for dicom-go from ISO/IEC 15444-15 (no upstream code): the CAP
  marker segment of Part 15 is accepted and other parts' capabilities are
  refused (`codestream.go`); HT code-blocks (code-block style 0x40) are
  decoded by one HT cleanup pass through the base module's `internal/htj2k`
  (`tier1.go`), and HT refinement or placeholder passes are refused while
  reading packet headers (`packet.go`).

- Written for dicom-go from ISO/IEC 15444-1 (no upstream code): partial
  decoding. `DecodeOptions.Layers` keeps the first quality layers: every
  packet header is still read (B.10.4, B.10.7: later headers of a precinct
  build on earlier ones), but the bodies of later layers are not attached to
  their code-blocks, which then decode the passes of the kept layers alone.
  The same packet filter skips the bodies of the resolution levels a
  reduction drops, and reading stops after the last packet the decode needs
  (`packet.go`, `decode.go`). `OutputOrigin` reports where the decoded image
  starts on the output grid (B.5).


The upstream algorithms for the style-0 path (MQ decoding, tier-1 passes,
wavelet synthesis, dequantization and component transforms) are otherwise
unchanged.


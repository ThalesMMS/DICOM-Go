# JPIP Server and Data-Bin Indexing

The `jpip` package serves JPEG 2000 and HTJ2K codestreams progressively over
JPIP (ISO/IEC 15444-9) and turns the responses back into codestreams on the
client side. It complements `jpip.Client`, which retrieves the
`Pixel Data Provider URL` of the JPIP Referenced transfer syntaxes.

## Data-bin index

`jpip.IndexCodestream` reads a codestream's marker structure through
`encapsulated.ParseJ2KLayout` (the same structural framing rules as
`encapsulated.ValidateFrame`) and locates the data-bins of ISO/IEC 15444-9
Annex A:

| Class | Content | In-class identifier |
|---|---|---|
| Main header (6) | SOC through the last main header marker segment | 0 |
| Tile header (2) | Marker segments of the tile's tile-part headers, without SOT, SOD and PLT | tile index |
| Tile (4) | The tile's complete tile-parts, in order | tile index |
| Precinct (0) | The precinct's packets in layer order | `t + (c + s * Csiz) * numTiles` |
| Metadata (8) | Empty: a bare codestream has no metadata boxes | 0 |

Precinct data-bins need every packet's byte range. The indexer takes the
lengths from PLT marker segments when every tile-part has consistent ones, and
otherwise reads the Part 1 packet headers (tag trees, coding passes, Lblock,
the bypass and termination codeword segments, SOP and EPH) to measure each
packet. Packets are assigned to precincts by replaying the progression order
of COD or POC, including the position-driven RPCL, PCRL and CPRL orders with
tiles, offsets and sub-sampled components. The index fails closed: a
codestream whose packets do not exactly fill its tile-parts is malformed.

Precinct data-bins are not offered, and the codestream is served as tile
data-bins only, when packet headers are relocated by PPM or PPT, when Part 2
decomposition styles or code-block anchors change the geometry, when an
HTJ2K codestream has no PLT (HT packet headers are not measured), or when the
packet count exceeds `IndexLimits.MaxPackets`.

## Server

`jpip.NewServer` returns an `http.Handler`. A `TargetProvider` resolves the
`target` request field to codestreams; `jpip.TargetFromObject` builds one from
encapsulated JPEG 2000 Pixel Data, one codestream per frame, and
`jpip.StaticTargets` is an in-memory provider. The `stream` field is the
one-based frame number that `jpip.Client` sends, and message CSn values use the
same numbering.

| Request | Response |
|---|---|
| No `type`, no session | The complete codestream as `image/jp2` (`image/jphc` for HTJ2K), with HTTP byte ranges; this is what `jpip.Client` decodes by default. |
| `type=jpp-stream` | Main header, empty metadata-bin 0, tile headers, then precinct packets from the coarsest resolution and lowest layer up. |
| `type=jpt-stream` | Main header, empty metadata-bin 0, then whole tile data-bins. |
| `cnew=http` | Opens a session; `JPIP-cnew` returns `cid`, `path` and `transport=http`. The session's cache model sends each data-bin byte once. |
| `cid`, `cclose`, `tid` | Continue and close sessions; a `tid` other than the target's resets the cache model. |
| `fsiz`, `roff`, `rsiz` | The frame size (reference grid, with `round-down`, `round-up` or `closest`) chooses the resolution levels; the region selects the precincts or tiles that intersect it. |
| `comps`, `layers`, `len` | Component ranges, a quality-layer limit (precinct data-bins are then sent as incomplete prefixes) and a byte limit. |

Every JPP/JPT response ends with an End Of Response message: image done,
window done, byte limit reached (`len`) or response limit reached
(`MaxResponseBytes`). Other request fields are ignored, which the standard
allows because the server may send more data than asked.

Limits: open sessions (`MaxSessions`, then 503), idle session expiry
(`SessionIdleTimeout`), requests in progress (`MaxConcurrentRequests`, then
503 with `Retry-After`), query size (`MaxQueryBytes`, then 414), response size
(`MaxResponseBytes`), cached target bytes (`MaxCacheBytes`, least recently
used targets are dropped) and per-codestream index limits (`IndexLimits`).
Cached targets are treated as immutable, which holds for DICOM instances.

`ServerConfig.OnEvent` receives one event per request and session change with
statuses, sizes, EOR reasons and durations. Events, error bodies and error
messages never contain target names, query strings or channel identifiers,
because target names can identify patients. Like the rest of this module's
network code the server has no authentication: run it on a trusted network or
behind a transport and access layer the deployment owns.

## Client side

`jpip.ParseStream` reads JPP/JPT messages, including the compressed
Bin-ID forms that reuse the previous class and codestream. `jpip.DataBinSet`
merges messages from any number of responses (out-of-order ranges are held
until contiguous) under a byte budget, and `Codestream` rebuilds a decodable
codestream:

- from tile data-bins, the complete tile-parts in tile order;
- from precinct data-bins, one tile-part per tile written in RPCL order, with
  COD set to RPCL, POC, TLM, PLM and PLT dropped, SOP marker segments removed,
  and absent precincts or layers coded as empty packets. Part 1 precinct
  data-bins are split into packets by reading their headers, so a data-bin
  holding only its first layers still contributes them; an HTJ2K precinct
  data-bin contributes only when complete.

`Reconstruction.Complete` reports whether every data-bin of the image was
present. `jpip.Client.DecodeFrame` with `Request.Type` set to
`StreamTypeJPP` or `StreamTypeJPT` decodes a response that ends with "image
done" or "window done"; `DecodedFrame.Partial` marks a reduced or partial
window. A response cut short by a byte or response limit stays the typed
`ErrIncrementalStream` limitation, since finishing it needs further requests.

## Validation

- `jpip` tests index every JPEG 2000 codestream of the codec corpus, check
  that the packets tile each tile-part exactly, that PLT and packet-header
  indexing agree, and that precinct and tile streams rebuild every packet.
  Server tests cover sessions under byte limits (each byte sent once), view
  windows, type negotiation, refusals, concurrency and session limits, idle
  expiry, the target cache and PHI-free events.
- `examples/codec-adapters/jpeg2000` decodes every Part 1 corpus codestream
  served through the server and retrieved by `jpip.Client` as a complete
  codestream, a JPT-stream and a JPP-stream, and compares the samples with
  the source; reduced `fsiz` windows decode, at that reduction, like the
  source.
- Opt-in OpenJPEG check: with `DICOMGO_JPIP_OPENJPEG=1` and `opj_compress`
  and `opj_decompress` on `PATH`, `go test ./jpip -run TestOpenJPEGInterop`
  encodes gray and colour images in all five progression orders, with and
  without PLT, SOP/EPH and tiles, checks PLT and packet-header indexing agree,
  and compares `opj_decompress` output for rebuilt and source codestreams at
  full size and two reductions. OpenJPEG's `opj_server` and `opj_dec_server`
  (the FastCGI JPIP server and its decoding proxy) are not driven.

# Pre-VOI 3×3 source convolution

`render.PrepareFrameConvolution3x3Context` prepares one decoded frame for
display. It borrows the original `Frame.PixelBytes` during the calculation.
`ConvolvedFrame.RenderContext` can then map a new window or VOI LUT over the
same result. Probe, ROI and acquired-image export still use the original frame.

The input supports 8- and 16-bit signed or unsigned MONOCHROME1/2, and unsigned
8- and 16-bit RGB in either interleaved or planar layout. The stored-sample
reader honors High Bit, Bits Stored and byte order. The API uses one decoded
frame payload, with the optional DICOM even-length padding byte when necessary.
Compressed input must already have been decoded by the caller or the frame's
context-aware loader. Unsupported photometric interpretations and containers
return a typed error.

Weights apply in row-major order as correlation. The border repeats the
nearest sample. Divide the weighted sum by the finite nonzero divisor and add
the stored-value bias. `MidrangeBias` adds the declared unsigned midpoint
uniformly across the image. Padding classification uses the acquired center
sample. It stays black after VOI and MONOCHROME1 inversion, and its stored value
participates when computing adjacent valid samples.

The grayscale intermediate saturates to signed or unsigned 16-bit range,
including for acquisitions in byte containers. The kernel can therefore create
a response outside the original Bits Stored range. Linear rescale follows
saturation. Fractional values survive the division and rescale until VOI maps
them. RGB saturates to the physical container range and uses its declared
channel range, with an optional ICC transform, to produce the display image.
The uniform midpoint and fractional arithmetic provide explicit, consistent
behavior across the interior and borders.

Preparation admits up to 16 Mi components before loading the frame. Each
worker uses at most three decoded rows of scratch, with at most eight workers
using the shared process budget. The detached result retains eight bytes per
grayscale pixel or four per RGB pixel, plus any owned default VOI entries.
`MemoryBytes` reports that retained payload for caller cache accounting.
The output image is independently owned. Cancellation drains the workers
before returning and never returns a partially prepared result.

Use `render.SourceConvolutionCache` to retain prepared frames by immutable
`Frame.CacheIdentity`, physical `FrameIndex`, and kernel. Change the identity
when the acquired samples or their retained interpretation change. The cache
keeps at most 16 entries within its configured byte budget and lets a cold
request finish once for concurrent waiters. `Lookup` needs just the identity,
frame index and kernel, so those requests can skip loading the source.
`Clear` prevents work already in flight from restoring old residency.

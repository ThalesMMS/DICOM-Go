# Movie export

The `render` package writes rendered frame sequences (VR orbits, MPR scrolls,
cine loops) as movies without external encoders. Both formats are pure Go and
use only the standard library codecs.

| Format | API | Codec | Bound |
| --- | --- | --- | --- |
| Motion-JPEG AVI | `EncodeMJPEGAVI`, streaming `NewAVIWriter` | baseline JPEG per frame (`image/jpeg`) | whole file at most `MaxAVIBytes` (1 GiB) |
| Animated GIF | `EncodeGIF`, `EncodeAdaptiveGIF`, streaming `NewGIFWriter`, `EncodeAdaptiveGIFProducer` | LZW, adaptive 256-colour palette or one fixed palette | GIF limits only |

MP4/H.264 output is not implemented here. Applications that want MP4 may call
an external encoder such as ffmpeg; the AVI writer is the encoder-free
alternative.

## Motion-JPEG AVI

The file is a classic AVI 1.0 RIFF with one video stream:

```text
RIFF 'AVI '
  LIST 'hdrl'
    'avih'  MainAVIHeader, 56 bytes, AVIF_HASINDEX
    LIST 'strl'
      'strh' AVIStreamHeader, 56 bytes: 'vids', handler 'MJPG', rate = fps, scale = 1
      'strf' BITMAPINFOHEADER, 40 bytes: 24 bit, compression 'MJPG'
  LIST 'movi'
    '00dc' JPEG frame, padded with one zero byte when its size is odd
    ...
  'idx1' one 16-byte entry per frame: '00dc', AVIIF_KEYFRAME, offset, size
```

- Every frame is an independent baseline JFIF image, so every index entry is a
  keyframe. Frames are always three-component YCbCr (grayscale sources are
  expanded) because several MJPEG decoders reject one-component frames.
- `idx1` offsets are measured from the `movi` list-type FOURCC, the AVI 1.0
  reference convention (the first frame is at offset 4).
- All frames must share the first frame's dimensions (`ErrAVIFrameSize`).
  Frame rates are 1–240 fps; JPEG quality defaults to 90.
- `NewAVIWriter` streams frames into an `io.WriteSeeker` (such as `*os.File`)
  and rewrites the fixed-size header on `Close`. `EncodeMJPEGAVI` encodes all
  frames in memory and writes any `io.Writer` in one pass; both produce the
  same bytes for the same input.

### Size bound

The OpenDML AVI 2.0 extensions (`AVIX` continuation RIFFs, `indx`
super-indexes) are deliberately not written. Without them the RIFF size and
the `idx1` offsets are 32-bit, several readers treat them as signed, and the
OpenDML specification recommends closing the first RIFF at 1 GiB. The writer
therefore refuses to grow the file beyond `MaxAVIBytes` (1 GiB; callers can
choose a lower `AVIOptions.MaxBytes`). The check runs before a frame is
written: `EncodeMJPEGAVI` returns `ErrAVITooLarge` without writing anything,
and `AVIWriter.WriteFrame` returns `ErrAVITooLarge` while the frames already
accepted remain closable as a valid, shorter movie. A broken, over-long AVI is
never produced.

### Validation

Tests parse the output with an independent RIFF walker that checks the chunk
tree, sizes and even alignment, pad bytes, `avih`/`strh`/`strf` fields, frame
counts, that each `idx1` entry points at the matching `00dc` chunk, and that
every frame decodes with `image/jpeg` at the right size and mean colour. An
opt-in check runs a local `ffprobe` when `DICOMGO_FFPROBE=1` is set, and
`DICOMGO_MOVIE_SAMPLE_DIR` writes sample AVI and GIF files for manual playback.

## Streaming GIF

`EncodeGIF` and `EncodeAdaptiveGIF` build the palette from the frames, so they
need every frame in memory. When the palette is known up front (a grayscale
cine), `NewGIFWriter` writes the header with the first frame and each later
frame as it arrives, holding one frame at a time. The first frame fixes the
movie size (`ErrGIFFrameSize` for later mismatches), and every pixel must index
the writer's 2..256-colour palette. Tests decode the output with `image/gif`
and compare pixels, palette, delays and loop count.

## Adaptive GIF

A fixed palette (the former Plan9 quantization) keeps only a handful of levels
along a smooth VR shading ramp, which shows as bands. `EncodeAdaptiveGIF`
instead:

1. Builds a colour histogram of exact 24-bit colours from a bounded,
   staggered sample (at most 2^19 pixels per palette).
2. Uses the exact colours when there are no more than the palette size.
   Otherwise it runs median cut: the box with the largest total squared error
   is split at the population median along its highest-variance channel, and
   each final box contributes its population-weighted mean colour.
3. Maps pixels with serpentine Floyd–Steinberg error diffusion (7/16, 3/16,
   5/16, 1/16) and an exact nearest-colour search; `NoDither` disables the
   diffusion.

Palette choice: by default one global palette is sampled from all frames and
written once as the GIF global colour table. Orbit and cine movies keep one
transfer function across frames, so a shared palette avoids frame-to-frame
palette flicker and keeps files small. `GIFPalettePerFrame` builds one palette
per frame (local colour tables) for sequences whose colours change
substantially. `EncodeGIF` uses the global palette with dithering.

On synthetic smooth gradients (encode, then decode with `image/gif`), the mean
absolute error after an 8x8 box blur, which is what stays visible as banding
once the eye integrates dither noise, falls from about 7–11 levels with Plan9
to about 0.2 levels with median cut and dithering.

## Bounded adaptive producer

`EncodeAdaptiveGIFProducer(ctx, out, total, frameAt, options, progress)` accepts
an immutable, context-aware, random-access frame source. It samples up to 32
evenly spaced temporal frames (including the endpoints), with at most 2^19
spatial samples shared between them, then replays the sequence through the
streaming GIF writer. One median-cut global palette and the existing serpentine
Floyd–Steinberg mapper are used for the entire movie. Identical frames therefore
produce identical indices, without artificial palette flicker. Rare colours
between temporal samples can be approximated; this is explicitly a different
sampling policy from `EncodeAdaptiveGIF`, which samples all supplied frames.
Per-frame palettes are refused by the producer API rather than silently changed.

`MaxFramePixels` defaults to 16 Mi pixels. `MaxWorkspaceBytes` defaults to
384 MiB, conservatively accounting for 64 MiB of histogram/sample work,
16 bytes per full-resolution pixel, two 12-byte-per-column error rows and 1 MiB
of encoder I/O. The limit is checked before conversion/quantization; the source
must enforce its own allocation and GPU residency budget before producing a
frame. These are explicit allocation/admission estimates, not an OS RSS limit
or a guarantee about Go garbage collector retention. No movie-sized image list
is retained. Counts up to 2^30 are accepted by the library; application limits
may be lower. All frames must have the same dimensions; differing dimensions
return `ErrGIFFrameSize` in either pass. Delay and loop count use the existing
GIF stream contract. Cancellation is checked during sampling and every mapping
row, at output writes and before the trailer. Output errors propagate; the
caller owns atomic publication, source cleanup and renderer shutdown.

The original slice API remains compatible. Applications that cannot replay an
immutable source must freeze/stage frames with explicit disk and per-frame
bounds; this API does not silently allocate an unbounded staging collection.
Tests compare a smooth colour ramp to the original in-memory encoder, decode
40 equal frames to check palette/pixel identity and cadence, and inject
production, quantization, writing, finalization and disk failures.

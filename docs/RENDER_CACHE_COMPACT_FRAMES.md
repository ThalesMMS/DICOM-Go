# Compact frames in the render cache

`render.RenderCache` keeps each decoded grayscale frame so that window/level
changes and revisits do not decode it again. previously, an 8- or 16-bit frame
enters the cache in compact form from its first decode:

- `display.DecodeModalityLevels` reads each stored sample once. It keeps a
  2-byte index per pixel (the stored value minus the smallest one) and one
  float32 modality value per stored level. Each level value is computed with
  the arithmetic of `DecodeModalityFloat32` (rescale, or the modality LUT), so
  `levels[index[i]]` equals the float32 decode bit for bit.
- A window maps each level once and gathers the output pixels through the
  index. The output is identical to mapping the float32 plane pixel by pixel.
- The cache charges 2 bytes per pixel plus 4 bytes per level, instead of
  4 bytes per pixel.

Frames whose stored range spans more than 65,536 values or more values than
the frame has pixels, 32-bit containers and precision-preserving frames keep
the float32 (or float64) plane. A float32 plane still builds its level index
only when its window changes, so scrolling it never pays for the index.

Concurrent requests for the same frame (a render, a read-ahead, a volume
build) wait for one decode instead of each decoding it.

## Measurements

i7-14700K, Windows, synthetic signed 12-bit CT-like frames with a
-1024 intercept, six runs each, `benchstat` medians, base 379c7ffc:

| Benchmark | Base | Compact |
| --- | --- | --- |
| `BenchmarkRenderCacheFirstWindow` 512x512, time | 1.044 ms | 0.900 ms (-13.8%) |
| `BenchmarkRenderCacheFirstWindow` 512x512, cached bytes | 1024 KiB | 528 KiB (-48%) |
| `BenchmarkRenderCacheFirstWindow` 2048x2048, time | 14.15 ms | 12.82 ms (-9.4%) |
| `BenchmarkRenderCacheFirstWindow` 2048x2048, cached bytes | 16.0 MiB | 8.0 MiB (-50%) |
| `BenchmarkRenderCacheWindowDrag` 512x512 | 115.0 µs | 114.9 µs (no change) |
| `BenchmarkRenderCacheWindowDrag` 2048x2048 | 662.2 µs | 639.3 µs (-3.5%) |

The first window decodes a frame the cache has not seen and renders it at its
default window (scrolling onto a slice); the window drag renders a resident
frame at a new window.

```sh
go test ./pixeldata/display -run 'DecodeModalityLevels' -count=1
go test ./render -run 'RenderCacheDecodesCompactFramesOnce|RenderCacheDecodesConcurrentRequestsOnce|WindowLevels' -count=1
go test ./render -run '^$' -bench 'BenchmarkRenderCache(FirstWindow|WindowDrag)$' -benchmem -count=6
```

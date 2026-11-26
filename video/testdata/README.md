# AVC codec fixture

avc-high41.mp4 is a synthetic six-frame 96 by 64, 25 fps clip. Luma is
16 + ((4*x + 3*y + 15*frame) mod 220); each U plane is 100 + 2*frame,
and each V plane is 150 - 2*frame. It contains no patient information.
The generated sample bytes and their source formula are project-authored.

The source was encoded on Windows with caller-owned FFmpeg
n8.1.3-35-gf1acb0a266-20261115 using libx264. Flags include
-pix_fmt yuv420p -profile:v high -level:v 4.1 -preset medium -crf 18
-bf 0 -refs 3 -color_range tv -color_primaries bt709 -color_trc bt709
-colorspace bt709 -use_editlist 0 -map_metadata -1 -f mp4.
The source video is used only as a synthetic codec fixture, and no FFmpeg
implementation or runtime is included.

The reproducible PowerShell generator is generate-avc.ps1. Its arguments select
the external executable and require the caller's exact SHA-256.

SHA-256 of avc-high41.mp4 is
623b7a17b991dba0b6f6e6944de0a4a34b4167e9e4f7a08ddab01aa32bae221b.

The other vt-* files are existing VideoToolbox remux fixtures.

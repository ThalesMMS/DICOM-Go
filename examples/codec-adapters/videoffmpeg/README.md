# Optional AVC video backend

This nested module invokes an explicitly configured FFmpeg executable. Importing
dicom-go alone adds no executable dependency, registration or video decoder.
The command adapter uses local file/pipe protocols and fixed arguments. It
does not link FFmpeg libraries, copy their implementation or package a binary.
Applications own installation, licensing, patching and deployment of the selected
FFmpeg build and its companion DLLs.

Create a backend with an absolute executable path and its expected SHA-256.
Both video directions use the exact profile in the shared video package.

```go
backend, err := videoffmpeg.New(executablePath, expectedSHA256)
if err != nil {
    return err
}
frames, err := video.DecodeAVCMP4(ctx, source, backend, video.AVCCodecLimits{})
derived, report, err := video.TranscodeAVCMP4(ctx, source, backend, backend,
    video.AVCTranscodeOptions{AllowLossy: true, CRF: 23})
```

The encoder needs FFmpeg with libx264. The decoder is FFmpeg's software H.264
decoder. No playback or app integration is enabled by these APIs. The full
profile, resource limits and metadata treatment are in
[VIDEO_CODEC.md](../../../docs/VIDEO_CODEC.md).

## Verification

Normal tests need no executable. The interoperability test enables external
tools only when their explicit environment paths are supplied.

```powershell
$env:DICOMGO_VIDEO_FFMPEG = 'C:/owned-ffmpeg/bin/ffmpeg.exe'
$env:DICOMGO_VIDEO_FFPROBE = 'C:/owned-ffmpeg/bin/ffprobe.exe'
go test -p 2 -v -count=1 ./...
```

For an independent Windows decoder, compile the small Media Foundation test
program with the Windows SDK or MinGW-w64. This tool lives in testdata and
is never part of the package. No codecs are installed by the test.

```powershell
g++ -std=c++17 -O2 -municode testdata/mf-video-reader.cpp -o "$env:TEMP/mf-video-reader.exe" -lmfplat -lmfreadwrite -lmfuuid -lole32
$env:DICOMGO_VIDEO_MF_READER = "$env:TEMP/mf-video-reader.exe"
go test -p 2 -v -count=1 ./...
```

The optional Media Foundation test requires identical presentation timestamps,
dimensions and NV12 bytes to FFmpeg on the checked-in synthetic clip.
DICOMGO_VIDEO_ARTIFACT_DIR can retain the derived DICOM, MP4 and source decoded
samples in a caller-chosen directory for further independent examination.

## Recorded Windows run


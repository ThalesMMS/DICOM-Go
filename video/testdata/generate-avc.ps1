# Generate only the project-authored synthetic AVC fixture.
param(
    [Parameter(Mandatory = $true)][string] $FFmpeg,
    [Parameter(Mandatory = $true)][string] $ExpectedSHA256
)
$ErrorActionPreference = 'Stop'
if (!(Test-Path -LiteralPath $FFmpeg -PathType Leaf) -or
    (Get-FileHash -LiteralPath $FFmpeg).Hash -ine $ExpectedSHA256) {
    throw 'The explicitly selected FFmpeg executable does not match its SHA-256'
}
$raw = New-Object byte[] (96 * 64 * 3 / 2 * 6)
for ($frame = 0; $frame -lt 6; $frame++) {
    $at = $frame * 96 * 64 * 3 / 2
    for ($y = 0; $y -lt 64; $y++) {
        for ($x = 0; $x -lt 96; $x++) {
            $raw[$at + $y * 96 + $x] = 16 + (($x * 4 + $y * 3 + $frame * 15) % 220)
        }
    }
    for ($i = 0; $i -lt 96 * 64 / 4; $i++) {
        $raw[$at + 96 * 64 + $i] = [byte](100 + $frame * 2)
        $raw[$at + 96 * 64 * 5 / 4 + $i] = [byte](150 - $frame * 2)
    }
}
$inputFile = [IO.Path]::GetTempFileName()
try {
    [IO.File]::WriteAllBytes($inputFile, $raw)
    $arguments = @(
        '-hide_banner', '-loglevel', 'error', '-y', '-f', 'rawvideo',
        '-pixel_format', 'yuv420p', '-video_size', '96x64', '-framerate', '25',
        '-i', $inputFile, '-an', '-c:v', 'libx264', '-threads', '1',
        '-profile:v', 'high', '-level:v', '4.1', '-preset', 'medium', '-crf', '18',
        '-bf', '0', '-refs', '3', '-color_range', 'tv',
        '-color_primaries', 'bt709', '-color_trc', 'bt709', '-colorspace', 'bt709',
        '-use_editlist', '0', '-map_metadata', '-1', '-f', 'mp4',
        (Join-Path $PSScriptRoot 'avc-high41.mp4')
    )
    & $FFmpeg @arguments
    if ($LASTEXITCODE -ne 0) {
        throw "Fixture encoding failed with exit $LASTEXITCODE"
    }
} finally {
    Remove-Item -LiteralPath $inputFile -Force
}

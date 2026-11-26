param(
    [string]$OutputDirectory = (Join-Path $env:TEMP ("dicom-incremental-" + [guid]::NewGuid().ToString("N")))
)
$ErrorActionPreference = 'Stop'
$moduleRoot = Split-Path -Parent $PSScriptRoot
$outputRoot = [IO.Path]::GetFullPath($OutputDirectory)
[void](New-Item -ItemType Directory -Path $outputRoot -Force)
$variableNames = @('GOMAXPROCS', 'DICOM_INCREMENTAL_SERIES_MODE', 'DICOM_INCREMENTAL_SERIES_COUNT', 'DICOM_INCREMENTAL_SEG_MODE', 'DICOM_INCREMENTAL_SEG_COUNT')
$savedVariables = @{}
foreach ($name in $variableNames) { $savedVariables[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
Push-Location $moduleRoot
try {
    $env:GOMAXPROCS = '4'
    foreach ($kind in @('imagebuild', 'seg')) {
        & go test -c "./$kind" -o (Join-Path $outputRoot "$kind.exe")
        if ($LASTEXITCODE -ne 0) { throw "Compilation failed for $kind" }
    }
    $rows = @()
    foreach ($kind in @('imagebuild', 'seg')) {
        foreach ($count in @(32, 128, 512)) {
            foreach ($mode in @('whole', 'stream')) {
                if ($kind -eq 'imagebuild') {
                    $env:DICOM_INCREMENTAL_SERIES_MODE = $mode
                    $env:DICOM_INCREMENTAL_SERIES_COUNT = [string]$count
                    $selector = '^TestWriteSeriesMemoryCampaign$'
                } else {
                    $env:DICOM_INCREMENTAL_SEG_MODE = $mode
                    $env:DICOM_INCREMENTAL_SEG_COUNT = [string]$count
                    $selector = '^TestSEGWriteFramesMemoryCampaign$'
                }
                $stdoutPath = Join-Path $outputRoot "$kind-$count-$mode.out"
                $stderrPath = Join-Path $outputRoot "$kind-$count-$mode.err"
                $child = Start-Process -WindowStyle Hidden -FilePath (Join-Path $outputRoot "$kind.exe") -ArgumentList @('-test.run', $selector, '-test.v') -RedirectStandardOutput $stdoutPath -RedirectStandardError $stderrPath -PassThru
                $observedPeak = [int64]0
                while (-not $child.HasExited) {
                    $child.Refresh()
                    $observedPeak = [Math]::Max($observedPeak, $child.PeakWorkingSet64)
                    Start-Sleep -Milliseconds 10
                }
                $child.WaitForExit()
                if ($child.ExitCode -ne 0) { throw "Campaign failed: $kind/$count/$mode; see $stdoutPath and $stderrPath" }
                $line = Get-Content -LiteralPath $stdoutPath | Where-Object { $_.StartsWith('{') } | Select-Object -First 1
                if (-not $line) { throw "Campaign emitted no receipt: $kind/$count/$mode" }
                $entry = $line | ConvertFrom-Json
                $entry | Add-Member -NotePropertyName kind -NotePropertyValue $kind
                $entry | Add-Member -NotePropertyName observed_process_peak_working_set_bytes -NotePropertyValue $observedPeak
                $rows += $entry
                $entry | ConvertTo-Json -Compress
                $child.Dispose()
            }
        }
    }
    $receiptPath = Join-Path $outputRoot 'memory-campaign.json'
    $rows | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $receiptPath
    Write-Output "Receipt: $receiptPath"
} finally {
    Pop-Location
    foreach ($name in $variableNames) { [Environment]::SetEnvironmentVariable($name, $savedVariables[$name], 'Process') }
}

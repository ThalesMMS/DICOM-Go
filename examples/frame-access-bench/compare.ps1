param(
    [string]$OutputDirectory = (Join-Path ([IO.Path]::GetTempPath()) ('frame-access-' + [guid]::NewGuid().ToString('N'))),
    [string]$Baseline,
    [ValidateRange(1, 30)][int]$Trials = 5,
    [ValidateRange(1, 100000)][int]$Iterations = 64,
    [switch]$VerifyOnly
)

$ErrorActionPreference = 'Stop'
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)
New-Item -ItemType Directory -Force -Path $OutputDirectory | Out-Null
if ($Baseline) {
    $Baseline = (Resolve-Path -LiteralPath $Baseline).Path
}
$candidate = Join-Path $OutputDirectory 'frame-access-go.exe'
$dcmtk = Join-Path $OutputDirectory 'frame-access-dcmtk.exe'
$fixtureDir = Join-Path $OutputDirectory 'fixture'

function Assert-NativeSuccess([string]$Operation) {
    if ($LASTEXITCODE -ne 0) {
        throw "$Operation exited with code $LASTEXITCODE"
    }
}

function Invoke-Adapter([string]$Executable, [string[]]$ArgumentList, [string]$EvidenceFile) {
    & $Executable @ArgumentList 2>> $EvidenceFile
    Assert-NativeSuccess "$Executable $ArgumentList"
}

$previousProcs = [Environment]::GetEnvironmentVariable('GOMAXPROCS')
Push-Location -LiteralPath (Join-Path $PSScriptRoot '../..')
try {
    $env:GOMAXPROCS = '1'
    & go build -p 1 -o $candidate ./examples/frame-access-bench
    Assert-NativeSuccess 'Build Go adapter'
    & g++ -O2 -DNDEBUG -std=c++17 (Join-Path $PSScriptRoot 'dcmtk/main.cpp') -o $dcmtk -ldcmjpls -ldcmdata -loflog -lofstd -lcrypto
    Assert-NativeSuccess 'Build DCMTK adapter'
    & $candidate prepare $fixtureDir
    Assert-NativeSuccess 'Generate deterministic fixtures'

    $fixtures = @('rle-bot', 'rle-empty', 'jls-bot', 'jls-split-bot', 'jls-split-empty')
    $storageModes = @('resident', 'deferred')
    $fixtureHashes = foreach ($fixture in $fixtures) {
        $file = Join-Path $fixtureDir "$fixture.dcm"
        [pscustomobject]@{fixture = $fixture; sha256 = (Get-FileHash -Algorithm SHA256 -LiteralPath $file).Hash.ToLowerInvariant()}
        foreach ($storage in $storageModes) {
            $prefix = Join-Path $OutputDirectory "$fixture-$storage"
            $goRows = @(Invoke-Adapter $candidate @($file, $storage, 'frame', 0) "$prefix-frame.txt")
            $goInto = @(Invoke-Adapter $candidate @($file, $storage, 'into', 0) "$prefix-into.txt")
            $dcRows = @(Invoke-Adapter $dcmtk @($file, $storage, 0) "$prefix-dcmtk.txt")
            if ($goRows.Count -ne 4 -or $goInto.Count -ne 4 -or $dcRows.Count -ne 4) {
                throw "Wrong verification row count for $fixture $storage"
            }
            $verification = foreach ($row in 0..3) {
                if ($goRows[$row] -cne $goInto[$row]) {
                    throw "Frame/FrameInto mismatch for $fixture $storage step $row"
                }
                if ($fixture -eq 'jls-split-empty' -and $row -eq 2) {
                    if ($dcRows[$row] -cne '63,error,Cannot determine start fragment: basic offset table is empty') {
                        throw "Expected DCMTK to decline an unhinted seek across fragmented JPEG-LS with empty BOT"
                    }
                } elseif ($goRows[$row] -cne $dcRows[$row]) {
                    throw "DCMTK frame mismatch for $fixture $storage step $row"
                }
                [pscustomobject]@{step = $row; go = $goRows[$row]; dcmtk = $dcRows[$row]}
            }
            $verification | Export-Csv -LiteralPath "$prefix-parity.csv" -NoTypeInformation
            if ($Baseline) {
                foreach ($api in 'frame', 'into') {
                    $prior = @(Invoke-Adapter $Baseline @($file, $storage, $api, 0) "$prefix-baseline-$api.txt")
                    if ($prior.Count -ne 4 -or (Compare-Object $goRows $prior -CaseSensitive -SyncWindow 0)) {
                        throw "Baseline frame mismatch for $fixture $storage $api"
                    }
                }
            }
        }
    }
    $fixtureHashes | Export-Csv -LiteralPath (Join-Path $OutputDirectory 'fixtures.csv') -NoTypeInformation
    Write-Output "Verified signed 16-bit frames and BOT controls in $OutputDirectory"
    if ($VerifyOnly) {
        return
    }

    $headers = @('ns_per_op', 'alloc_bytes_per_op', 'allocs_per_op', 'readerat_calls_per_op', 'payload_bytes_per_op',
        'setup_ns', 'setup_read_calls', 'setup_read_bytes', 'frame0_ns', 'next_ns', 'seek_ns', 'return_ns')
    $records = foreach ($fixture in @('rle-bot', 'jls-bot', 'jls-split-bot')) {
        $file = Join-Path $fixtureDir "$fixture.dcm"
        foreach ($storage in $storageModes) {
            $variants = @(
                @{name = 'dcmtk'; path = $dcmtk; args = @($file, $storage, $Iterations)},
                @{name = 'go-frame'; path = $candidate; args = @($file, $storage, 'frame', $Iterations)},
                @{name = 'go-into'; path = $candidate; args = @($file, $storage, 'into', $Iterations)},
                @{name = 'go-raw'; path = $candidate; args = @($file, $storage, 'raw', $Iterations)},
                @{name = 'go-locate'; path = $candidate; args = @($file, $storage, 'locate', $Iterations)}
            )
            if ($Baseline) {
                $variants += @(
                    @{name = 'baseline-frame'; path = $Baseline; args = @($file, $storage, 'frame', $Iterations)},
                    @{name = 'baseline-into'; path = $Baseline; args = @($file, $storage, 'into', $Iterations)}
                )
            }
            for ($trial = 1; $trial -le $Trials; $trial++) {
                if ($trial -gt 1) {
                    [array]::Reverse($variants)
                }
                foreach ($variant in $variants) {
                    $log = Join-Path $OutputDirectory "$fixture-$storage-$($variant.name)-trial$trial.txt"
                    $line = Invoke-Adapter $variant.path $variant.args $log
                    if ($line -isnot [string] -or $line.Split(',').Count -ne $headers.Count) {
                        throw "Unexpected timing output '$line'"
                    }
                    $result = [ordered]@{fixture = $fixture; storage = $storage; trial = $trial; variant = $variant.name}
                    $row = $line | ConvertFrom-Csv -Header $headers
                    foreach ($header in $headers) {
                        $result[$header] = $row.$header
                    }
                    [pscustomobject]$result
                }
            }
        }
    }
    $results = Join-Path $OutputDirectory 'results.csv'
    $records | Export-Csv -LiteralPath $results -NoTypeInformation
    Write-Output $results
} finally {
    Pop-Location
    [Environment]::SetEnvironmentVariable('GOMAXPROCS', $previousProcs)
}

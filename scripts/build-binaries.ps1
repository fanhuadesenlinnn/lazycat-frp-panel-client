# Windows 本地构建脚本：与管理器一起产出 content/bin 目录结构。
#
#   pwsh -File scripts\build-binaries.ps1 -UpstreamDir ..\frp-panel
#
# 需要 Go 1.24+。上游客户端默认从本地源码交叉编译（可复现）。
[CmdletBinding()]
param(
  [string]$UpstreamDir = (Join-Path (Split-Path -Parent $PSScriptRoot) '..\frp-panel'),
  [string]$Go = 'go',
  [string[]]$Arches = @('amd64', 'arm64'),
  [switch]$SkipUpstream
)

$ErrorActionPreference = 'Stop'
$Root = Split-Path -Parent $PSScriptRoot

$Version = 'dev'
try { $Version = (& git -C $Root describe --tags --always --dirty 2>$null) } catch { }
if (-not $Version) { $Version = 'dev' }
$BuildDate = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
$LdFlags = "-s -w -X main.version=$Version+$BuildDate"

foreach ($arch in $Arches) {
  $dir = Join-Path $Root "content/bin/linux-$arch"
  New-Item -ItemType Directory -Force -Path $dir | Out-Null

  Write-Host "==> 编译管理器 linux/$arch"
  $env:CGO_ENABLED = '0'
  $env:GOOS = 'linux'
  $env:GOARCH = $arch
  Push-Location $Root
  & $Go build -trimpath -ldflags $LdFlags -o (Join-Path $dir 'lzc-frpp-client') .
  Pop-Location
  if ($LASTEXITCODE -ne 0) { throw "管理器编译失败 ($arch)" }

  if ($SkipUpstream) { continue }
  if (-not (Test-Path (Join-Path $UpstreamDir 'cmd/frppc'))) {
    throw "未找到上游源码 $UpstreamDir\cmd\frppc"
  }
  Write-Host "==> 编译上游客户端 linux/$arch ($UpstreamDir)"
  Push-Location $UpstreamDir
  & $Go build -o (Join-Path $dir 'frp-panel-client') ./cmd/frppc
  Pop-Location
  if ($LASTEXITCODE -ne 0) { throw "上游客户端编译失败 ($arch)" }
  Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
}

Write-Host "`n构建完成：" -ForegroundColor Green
Get-ChildItem -Recurse -File (Join-Path $Root 'content/bin') | ForEach-Object { "{0,10}  {1}" -f $_.Length, $_.FullName }

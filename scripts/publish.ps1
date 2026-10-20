# publish.ps1 — FlowEdge 一键发布到 GitHub
#
# 前置（只需做一次）：
#   1. 安装 GitHub CLI：winget install --id GitHub.cli  （或 https://cli.github.com）
#   2. 登录：          gh auth login
#   3. 确保 dist/FlowEdge.exe 已是最新：python scripts/build-windows.py
#
# 用法：
#   pwsh scripts/publish.ps1                 # 公开仓库，默认名 flowedge
#   pwsh scripts/publish.ps1 -Repo my-engine # 指定仓库名
#   pwsh scripts/publish.ps1 -Private        # 建私有仓库
#   pwsh scripts/publish.ps1 -RemoteOrigin https://github.com/你/已存在的仓库.git  # 推到已有仓库
#
# 脚本只做：建仓(可选) → 推送 main + tags → 用 CHANGELOG.md 创建 GitHub Release 并附 exe。
# 不会动你本地数据、不会删东西。

param(
  [string]$Repo = "flowedge",
  [switch]$Private,
  [string]$RemoteOrigin = "",     # 若已手动加过 remote，留空即可
  [string]$Tag = "v2026.10.20",
  [string]$ExePath = "dist/FlowEdge.exe"
)

$ErrorActionPreference = "Stop"

function Fail($msg) { Write-Host "❌ $msg" -ForegroundColor Red; exit 1 }

# 1) gh 可用性 + 登录态
if (-not (Get-Command gh -ErrorAction SilentlyContinue)) {
  Fail "未找到 gh。先装：winget install --id GitHub.cli，然后 gh auth login"
}
$auth = & gh auth status 2>&1
if ($LASTEXITCODE -ne 0) { Fail "gh 未登录。请先运行：gh auth login`n$auth" }
Write-Host "✅ gh 已登录" -ForegroundColor Green

# 2) 仓库根
$root = Resolve-Path (Join-Path $PSScriptRoot "..")
Push-Location $root

# 3) 检查 exe 产物
if (-not (Test-Path $ExePath)) { Fail "找不到 $ExePath，先跑：python scripts/build-windows.py" }
$exeSize = (Get-Item $ExePath).Length / 1MB
Write-Host ("✅ 发现发布产物 {0}（{1:N1} MB）" -f $ExePath, $exeSize) -ForegroundColor Green

# 4) remote
if ($RemoteOrigin -ne "") {
  git remote remove origin 2>$null
  git remote add origin $RemoteOrigin
  Write-Host "✅ 已设置 remote origin -> $RemoteOrigin" -ForegroundColor Green
} elseif (-not (git remote get-url origin 2>$null)) {
  $vis = if ($Private) { "--private" } else { "--public" }
  Write-Host "📦 创建 GitHub 仓库 $Repo ..." -ForegroundColor Cyan
  & gh repo create $Repo $vis --source . --remote origin --push
  if ($LASTEXITCODE -ne 0) { Fail "gh repo create 失败" }
  Write-Host "✅ 仓库已创建并推送" -ForegroundColor Green
} else {
  Write-Host ("✅ 已存在 remote origin -> {0}" -f (git remote get-url origin)) -ForegroundColor Green
}

# 5) 若上一步没自动 push，这里补推 main + tags
$branch = git branch --show-current
$pushed = & git rev-parse --verify --quiet "origin/$branch"
if ($pushed -eq $null) {
  Write-Host "📤 推送 $branch ..." -ForegroundColor Cyan
  & git push -u origin $branch; if ($LASTEXITCODE -ne 0) { Fail "git push 失败" }
}
Write-Host "📤 推送 tags ..." -ForegroundColor Cyan
& git push --tags; if ($LASTEXITCODE -ne 0) { Fail "git push --tags 失败" }

# 6) Release（若已存在则更新）
$relExists = & gh release view $Tag 2>$null
if ($LASTEXITCODE -eq 0) {
  Write-Host "🔁 Release $Tag 已存在，重新上传产物..." -ForegroundColor Yellow
  & gh release upload $Tag $ExePath --clobber
} else {
  Write-Host "🚀 创建 Release $Tag ..." -ForegroundColor Cyan
  & gh release create $Tag $ExePath --title $Tag --notes-file CHANGELOG.md
  if ($LASTEXITCODE -ne 0) { Fail "gh release create 失败" }
}
Write-Host "🎉 完成。Release 页面：`gh release view $Tag --web`" -ForegroundColor Green
Pop-Location

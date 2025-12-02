#Requires -Version 5.1

<#
.SYNOPSIS
  一个用于构建、测试、打包和发布多平台 Docker 镜像的 PowerShell 脚本。
.DESCRIPTION
  此脚本自动化了以下流程：
  1. 解析版本号（从命令行参数、Git 标签或时间戳生成）。
  2. 准备构建环境，包括清理旧产物和复制配置文件。
  3. 构建前端 Web 应用。
  4. 交叉编译 Go 后端应用。
  5. 校验构建产物。
  6. 使用 Docker buildx 构建并推送 linux/amd64 和 linux/arm64 平台的镜像。
  7. 创建并推送多架构的 Docker manifest list。
  8. 输出最终的不可变镜像标签和清单摘要，并提供部署建议。
.PARAMETER Version
  可选参数，用于手动指定要发布的版本号。如果未提供，脚本将尝试从 Git 标签或时间戳获取。
.EXAMPLE
  .\build_docker.ps1
  # 自动确定版本号并开始构建发布流程。
.EXAMPLE
  .\build_docker.ps1 "1.2.3"
  # 使用指定的版本号 "1.2.3" 进行构建和发布。
#>
[CmdletBinding()]
param (
  [string]$Version
)

# ===== 基本配置 =====
$APP_NAME = "onehub"
$IMAGE_NAME = "ahhhliu/onehub"
$PLATFORMS = "linux/amd64,linux/arm64"

# ===== 脚本初始化与辅助函数 =====

# 设置严格模式，有助于捕获常见错误
Set-StrictMode -Version Latest

# 确保脚本出错时立即停止
$ErrorActionPreference = "Stop"

# 辅助函数：带颜色的日志输出
function Write-Log {
  param (
    [Parameter(Mandatory=$true)]
    [string]$Message,
    [ValidateSet("INFO", "WARN", "ERROR", "SUCCESS")]
    [string]$Level = "INFO"
  )

  $colorMap = @{
    INFO    = "Cyan"
    WARN    = "Yellow"
    ERROR   = "Red"
    SUCCESS = "Green"
  }
  Write-Host "[$Level] $Message" -ForegroundColor $colorMap[$Level]
}

# 辅助函数：执行外部命令并检查错误 (使用 Start-Process 增强稳定性)
function Invoke-CommandAndCheck {
  param (
    [Parameter(Mandatory=$true)]
    [string]$Command,
    [Parameter(Mandatory=$true)]
    [string[]]$Arguments,
    [string]$ErrorMessage
  )

  Write-Host "[CMD] $Command $($Arguments -join ' ')" -ForegroundColor Gray

  $exe = $Command
  $argumentsList = $Arguments

  # 如果是 cmd/bat/cmdlet 则使用 cmd.exe 执行
  if ($Command -match "\.cmd$" -or $Command -match "\.bat$" -or $Command -notmatch "\.exe$") {
    $exe = "cmd.exe"
    $argumentsList = @("/c", $Command) + $Arguments
  }

  $process = Start-Process -FilePath $exe -ArgumentList $argumentsList -Wait -PassThru -NoNewWindow

  if ($process.ExitCode -ne 0) {
    Write-Log -Level ERROR -Message "$ErrorMessage (退出码: $($process.ExitCode))"
    exit 1
  }
}


Write-Log -Message "构建开始..."

# =========================================
# 版本号解析
# =========================================
if (-not [string]::IsNullOrEmpty($Version)) {
  Write-Log "使用命令行参数指定的版本。"
}
if ([string]::IsNullOrEmpty($Version)) {
  $timestamp = Get-Date -Format "yyyyMMdd.HHmm"
  $rand4 = (Get-Random -Minimum 1000 -Maximum 9999).ToString()
  $Version = "$timestamp-$rand4"
  Write-Log "未提供版本，使用时间戳生成版本。"
}

Write-Log -Level INFO -Message "解析到 VERSION: $Version"

# Step 0: 取 Git 短 SHA
$GIT_SHA = (git rev-parse --short HEAD 2>$null)
if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrEmpty($GIT_SHA)) {
  $GIT_SHA = "nogit"
}
$IMMUTABLE_TAG = "$Version-$GIT_SHA"

Write-Log -Level INFO -Message "版本号："
Write-Host "       不可变: $IMMUTABLE_TAG"
Write-Host "       指  针: $Version / latest"


# Step 1: 清理构建产物
Write-Log -Level "INFO" -Message "[Step 1] 清理 target/ ..."
$targetDir = ".\target"
if (Test-Path $targetDir) {
  Remove-Item -Path $targetDir -Recurse -Force
}
New-Item -Path $targetDir -ItemType Directory -Force | Out-Null
$webtargetDir = ".\web\build"
if (Test-Path $webtargetDir) {
  Remove-Item -Path $webtargetDir -Recurse -Force
}
New-Item -Path $webtargetDir -ItemType Directory -Force | Out-Null


## Step 1.5: 复制 configs
#Write-Log -Level "INFO" -Message "[Step 1.5] 准备构建上下文（复制 .\configs -> target\configs）..."
#$configsDir = ".\configs"
#if (-not (Test-Path $configsDir)) {
#  Write-Log -Level ERROR -Message "未找到目录 configs ，请检查。"
#  exit 1
#}
#Copy-Item -Path $configsDir -Destination "$targetDir\configs" -Recurse -Force
#

# Step 1.8: 构建 Web 前端
Write-Log -Level "INFO" -Message "[Step 1.8] 构建 Web 前端..."
$webDir = ".\web"
if (-not (Test-Path $webDir)) {
  Write-Log -Level ERROR -Message "未找到 web 目录，请检查。"
  exit 1
}

Push-Location $webDir
try {
  if (-not (Test-Path ".\package.json")) {
    Write-Log -Level ERROR -Message "web 目录下缺少 package.json"
    exit 1
  }

  # 安装依赖：优先使用 yarn
  $yarnExists = $null -ne (Get-Command yarn -ErrorAction SilentlyContinue)
  if ($yarnExists) {
    Write-Log "使用 yarn 安装依赖..."
    Invoke-CommandAndCheck "yarn" @("--frozen-lockfile") "yarn 安装依赖失败"
  } else {
    Write-Log "未找到 yarn，改用 npm install ..."
    Invoke-CommandAndCheck "npm" @("install") "npm 安装依赖失败"
  }

  # 传递版本号等环境变量
  $env:DISABLE_ESLINT_PLUGIN = "true"
  $env:VITE_APP_VERSION = $Version
  Write-Log "VITE_APP_VERSION=$($env:VITE_APP_VERSION)"

  Invoke-CommandAndCheck "npm" @("run", "build") "npm run build 失败"

} finally {
  Pop-Location
}


## Step 1.9: 复制 Web 构建产物
#Write-Log -Level "INFO" -Message "[Step 1.9] 复制 Web 构建产物到 target\web ..."
#$webBuildSource = ".\web\build"
#$webBuildDest = ".\target\web"
#Copy-Item -Path $webBuildSource -Destination $webBuildDest -Recurse -Force



# Step 2: 交叉编译 Go
Write-Log -Level "INFO" -Message "[Step 2] 预编译 Go..."
# 尝试 xgo，如果失败则回退
try {
  # --- 最终修正 ---
  # 将 ldflags 的值用单引号包围，使其成为一个包含双引号的、不可分割的单一参数字符串。
  $ldflagsValue = '"-s -w -X main.VERSION=' + $Version + '"'

  Invoke-CommandAndCheck "xgo" @(
    "-targets=$PLATFORMS",
    "-ldflags", $ldflagsValue, # 将标志名和它的值作为两个独立的参数传递
    "-out", "target/$APP_NAME",
    "."
  ) "xgo 编译失败"
  # --- 修正结束 ---

} catch {
  Write-Log -Level WARN -Message "xgo 失败或未安装，回退 go build 双架构..."

  $env:CGO_ENABLED="0"
  $env:GOOS="linux"

  # --- 对 go build 也应用同样的修正逻辑 ---
  $ldflagsValue = '"-s -w -X main.VERSION=' + $Version + '"'

  Write-Log "正在构建 amd64..."
  $env:GOARCH="amd64"
  Invoke-CommandAndCheck "go" @("build", "-ldflags", $ldflagsValue, "-o", "target/$($APP_NAME)-linux-amd64", ".") "amd64 构建失败"

  Write-Log "正在构建 arm64..."
  $env:GOARCH="arm64"
  Invoke-CommandAndCheck "go" @("build", "-ldflags", $ldflagsValue, "-o", "target/$($APP_NAME)-linux-arm64", ".") "arm64 构建失败"
}

# Step 3: 产物校验
Write-Log -Level "INFO" -Message "[Step 3] 校验产物..."
Get-ChildItem -Path $targetDir | Select-Object -ExpandProperty Name
foreach ($arch in @("amd64", "arm64")) {
  $binaryPath = "$targetDir\$($APP_NAME)-linux-$arch"
  if (-not (Test-Path $binaryPath)) {
    Write-Log -Level ERROR -Message "缺少产物 $binaryPath"
    exit 1
  }
  $fileSize = (Get-Item $binaryPath).Length
  Write-Log "$arch 大小: $fileSize bytes"
  if ($fileSize -lt 1000000) {
    Write-Log -Level WARN -Message "$arch 体积异常（可能构建有误）"
  }
}


# Step 4: 检查 Dockerfile
Write-Log -Level "INFO" -Message "[Step 4] 检查 Dockerfile.prod ..."
$dockerfilePath = ".\Dockerfile.prod"
if (-not (Test-Path $dockerfilePath)) {
  Write-Log -Level ERROR -Message "缺少 Dockerfile.prod"
  exit 1
}
Select-String -Path $dockerfilePath -Pattern "COPY" | ForEach-Object { $_.ToString() } | Write-Host


# Step 5: 准备 buildx 构建器
Write-Log -Level "INFO" -Message "[Step 5] 准备 buildx ..."
$builderCheck = docker buildx inspect mybuilder 2>$null
if ($LASTEXITCODE -ne 0) {
  Write-Log "构建器 'mybuilder' 不存在，正在创建..."
  Invoke-CommandAndCheck "docker" @("buildx", "create", "--use", "--name", "mybuilder") "创建 buildx 失败"
} else {
  Invoke-CommandAndCheck "docker" @("buildx", "use", "mybuilder") "切换到 buildx 构建器失败"
}
Invoke-CommandAndCheck "docker" @("buildx", "inspect", "--bootstrap") "Bootstrap buildx 失败"


# ====== 核心：构建与推送 ======
$commonBuildxArgs = @(
  "--push",
  "-f", "Dockerfile.prod",
  "--build-arg", "APP=$APP_NAME"
)
$contextPath = "target"

Write-Log -Level "INFO" -Message "[Step 6] 构建并推送 amd64 ..."
$amd64Args = @( "buildx", "build", "--platform", "linux/amd64", "-t", "${IMAGE_NAME}:${IMMUTABLE_TAG}-amd64", "--build-arg", "TARGETARCH=amd64" ) + $commonBuildxArgs + $contextPath
Invoke-CommandAndCheck "docker" $amd64Args "amd64 镜像构建失败"

Write-Log -Level "INFO" -Message "[Step 7] 构建并推送 arm64 ..."
$arm64Args = @( "buildx", "build", "--platform", "linux/arm64", "-t", "${IMAGE_NAME}:${IMMUTABLE_TAG}-arm64", "--build-arg", "TARGETARCH=arm64" ) + $commonBuildxArgs + $contextPath
Invoke-CommandAndCheck "docker" $arm64Args "arm64 镜像构建失败"


# ====== 组装多平台 manifest ======
$amd64Image = "${IMAGE_NAME}:${IMMUTABLE_TAG}-amd64"
$arm64Image = "${IMAGE_NAME}:${IMMUTABLE_TAG}-arm64"

Write-Log -Level "INFO" -Message "[Step 8] 组装并推送多平台清单（不可变 tag）..."
Invoke-CommandAndCheck "docker" @("buildx", "imagetools", "create", "-t", "${IMAGE_NAME}:${IMMUTABLE_TAG}", $amd64Image, $arm64Image) "创建不可变 tag 清单失败"

Write-Log -Level "INFO" -Message "[Step 9] 组装并推送多平台清单（版本指针）..."
Invoke-CommandAndCheck "docker" @("buildx", "imagetools", "create", "-t", "${IMAGE_NAME}:${Version}", $amd64Image, $arm64Image) "创建版本指针清单失败"

Write-Log -Level "INFO" -Message "[Step 10] 组装并推送多平台清单（latest 指针）..."
Invoke-CommandAndCheck "docker" @("buildx", "imagetools", "create", "-t", "${IMAGE_NAME}:latest", $amd64Image, $arm64Image) "创建 latest 指针清单失败"


# ====== 读取多平台 manifest 摘要（sha256）======
$MANIFEST_DIGEST = ""
try {
  $inspectOutput = docker buildx imagetools inspect "${IMAGE_NAME}:${IMMUTABLE_TAG}"
  if ($inspectOutput -match '(?m)^Digest: (sha256:[a-f0-9]+)') {
    $MANIFEST_DIGEST = $matches[1]
  }
}
catch {
  Write-Log -Level WARN -Message "解析 manifest 摘要时出错: $($_.Exception.Message)"
}


Write-Host ""
Write-Log -Level SUCCESS -Message "发布完成！"
Write-Host "不可变 tag : ${IMAGE_NAME}:${IMMUTABLE_TAG}"

if (-not [string]::IsNullOrEmpty($MANIFEST_DIGEST)) {
  Write-Log -Message "Manifest 摘要: $MANIFEST_DIGEST"
  Write-Host ""
  $yamlSnippet = @"
---------- 复制到 stack.yml 的推荐写法 ----------
services:
  app:
    image: ${IMAGE_NAME}@${MANIFEST_DIGEST}
    platform: "linux/amd64"
    deploy:
      update_config:
        order: start-first
        parallelism: 1
        delay: 10s
        failure_action: rollback
      rollback_config:
        parallelism: 1
        delay: 5s
-----------------------------------------------
"@
  Write-Host $yamlSnippet
} else {
  Write-Log -Level WARN -Message "未能自动解析摘要，请手动执行："
  Write-Host "       docker buildx imagetools inspect ${IMAGE_NAME}:${IMMUTABLE_TAG}"
  Write-Host "       从第一条 `"Digest: sha256:...`" 复制到 stack.yml： image: ${IMAGE_NAME}@sha256:xxxx"
}

exit 0

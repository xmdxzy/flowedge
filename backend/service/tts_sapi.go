package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Windows 内置语音合成（SAPI），零 Python 依赖。
//
// 为什么值得做：此前 TTS 只能走 Python 服务（要装 Python + 跑脚本），
// 而 Windows 自带 SAPI 语音引擎，一句 PowerShell 就能出 wav。
// 这样「裸 exe 双击即可用」的承诺才真正覆盖到语音合成。
//
// 实现走 COM（SAPI.SpVoice / SAPI.SpFileStream）而不是 System.Speech：
//  - 不需要 Add-Type 编译程序集，冷启动快一个数量级（约 0.3s vs 2s+）
//  - 不依赖 .NET Framework 版本
//  - 文本经 UTF-8 临时文件传入，彻底绕开命令行引号/中文编码坑

// sapiScript 生成 wav 的 PowerShell 脚本（写文件执行，避免引号地狱）
const sapiScript = `param(
  [Parameter(Mandatory=$true)][string]$TextPath,
  [Parameter(Mandatory=$true)][string]$OutPath,
  [string]$VoiceName = '',
  [int]$Rate = 0
)
$ErrorActionPreference = 'Stop'
$voice = New-Object -ComObject SAPI.SpVoice
$stream = New-Object -ComObject SAPI.SpFileStream
# SSFMCreateForWrite = 3
$stream.Open($OutPath, 3, $false)
try {
  $voice.AudioOutputStream = $stream
  if ($Rate -lt -10) { $Rate = -10 }
  if ($Rate -gt 10) { $Rate = 10 }
  $voice.Rate = $Rate
  if ($VoiceName -ne '') {
    $tokens = $voice.GetVoices()
    for ($i = 0; $i -lt $tokens.Count; $i++) {
      $d = $tokens.Item($i).GetDescription()
      if ($d -like "*$VoiceName*") { $voice.Voice = $tokens.Item($i); break }
    }
  }
  $text = [IO.File]::ReadAllText($TextPath, [Text.Encoding]::UTF8)
  $null = $voice.Speak($text)
} finally {
  $stream.Close()
}
`

// SAPIAvailable 当前机器能否用 SAPI 合成（仅 Windows，且能找到 powershell）
func SAPIAvailable() bool {
	if runtime.GOOS != "windows" {
		return false
	}
	if os.Getenv("FLOWEDGE_TTS_SAPI") == "0" {
		return false
	}
	_, err := exec.LookPath("powershell.exe")
	return err == nil
}

// SAPIInfo 探测已安装语音（供能力面板显示「哪几个嗓子可用」）。
//
// 结果在进程内缓存一次：探测本身要 spawn 一个 powershell，
// 而 ProbeCapabilities 会在每次 /api/capabilities 请求时调用本函数——
// 前端挂载、窗口聚焦、依赖启停后都会刷能力，不缓存就会周期性在桌面上闪黑框。
// 语音引擎列表在进程生命周期内不会变，缓存零风险。
var (
	sapiInfoOnce  sync.Once
	sapiInfoCache string
)

func SAPIInfo() string {
	sapiInfoOnce.Do(func() {
		sapiInfoCache = detectSAPIInfo()
	})
	return sapiInfoCache
}

// detectSAPIInfo 真正执行一次探测（单次 powershell，超时 6 秒）
func detectSAPIInfo() string {
	if !SAPIAvailable() {
		return ""
	}
	script := `$v = New-Object -ComObject SAPI.SpVoice; $t = $v.GetVoices(); $out = @(); for ($i=0; $i -lt $t.Count; $i++) { $out += $t.Item($i).GetDescription() }; $out -join ' | '`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = hiddenWindowAttr()
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	case <-time.After(6 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return ""
	}
}

// synthesizeSAPI 用 Windows 内置语音把文本合成到 outputPath（wav）
func synthesizeSAPI(text, outputPath, voiceName string, rate int) error {
	if !SAPIAvailable() {
		return errors.New("当前环境不支持 Windows 内置语音合成")
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("TTS 文本为空")
	}
	if len([]rune(text)) > 8000 {
		return errors.New("TTS 文本过长（上限 8000 字），请分段合成")
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "flowedge-tts-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	textPath := filepath.Join(dir, "in.txt")
	scriptPath := filepath.Join(dir, "tts.ps1")
	if err := os.WriteFile(textPath, []byte(text), 0600); err != nil {
		return err
	}
	if err := os.WriteFile(scriptPath, []byte(sapiScript), 0600); err != nil {
		return err
	}

	// 输出路径必须是绝对路径：SAPI 解析相对路径时以进程 CWD 为基准，容易写错地方
	absOut, err := filepath.Abs(outputPath)
	if err != nil {
		absOut = outputPath
	}
	if err := os.MkdirAll(filepath.Dir(absOut), 0755); err != nil {
		return err
	}

	cmd := exec.Command("powershell.exe",
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-File", scriptPath,
		"-TextPath", textPath,
		"-OutPath", absOut,
		"-VoiceName", voiceName,
		"-Rate", fmt.Sprintf("%d", rate),
	)
	cmd.SysProcAttr = hiddenWindowAttr()

	done := make(chan error, 1)
	var output []byte
	go func() {
		output, err = cmd.CombinedOutput()
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("SAPI 合成失败：%w（%.300s）", err, string(output))
		}
	case <-time.After(120 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return errors.New("SAPI 合成超时（120 秒）")
	}

	st, err := os.Stat(absOut)
	if err != nil {
		return fmt.Errorf("SAPI 未产出音频文件：%w", err)
	}
	if st.Size() < 1000 {
		return fmt.Errorf("SAPI 产出的音频过小（%d 字节），疑似合成失败", st.Size())
	}
	return nil
}

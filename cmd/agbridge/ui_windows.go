package main

import (
	_ "embed"
	"log"
	"os"
	"strings"

	"fyne.io/systray"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// shim.exe is produced by build.ps1 / build.sh before this package is built.
//
//go:embed shim.exe
var shimBinary []byte

const isGUI = true

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

func runUI(a *app, openConsole bool) {
	systray.Run(func() {
		systray.SetIcon(trayIcon())
		systray.SetTitle("AGBridge")
		systray.SetTooltip("AGBridge · Antigravity 第三方模型代理")
		mOpen := systray.AddMenuItem("打开控制台", "")
		mInstall := systray.AddMenuItem("接入 / 修复 Antigravity", "")
		systray.AddSeparator()
		mAuto := systray.AddMenuItemCheckbox("开机自启", "", autostartEnabled(a.exe))
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("退出", "")
		if openConsole {
			openBrowser(a.consoleURL())
		}
		go func() {
			for {
				select {
				case <-mOpen.ClickedCh:
					openBrowser(a.consoleURL())
				case <-mInstall.ClickedCh:
					if err := a.install(); err != nil {
						message("接入失败", err.Error(), true)
					} else if a.status().RestartNeeded {
						message("接入完成", "Antigravity 正在运行，完全退出（包括托盘）后重新打开，第三方模型才会出现在模型下拉框里。", false)
					} else {
						message("接入完成", "现在打开 Antigravity，第三方模型会出现在模型下拉框里。", false)
					}
				case <-mAuto.ClickedCh:
					on := !mAuto.Checked()
					if err := setAutostart(a.exe, on); err != nil {
						message("设置失败", err.Error(), true)
						continue
					}
					if on {
						mAuto.Check()
					} else {
						mAuto.Uncheck()
					}
				case <-mQuit.ClickedCh:
					systray.Quit()
					return
				}
			}
		}()
	}, func() {})
}

func openBrowser(url string) {
	if err := windows.ShellExecute(0, windows.StringToUTF16Ptr("open"), windows.StringToUTF16Ptr(url), nil, nil, windows.SW_SHOWNORMAL); err != nil {
		log.Printf("open browser: %v", err)
	}
}

func message(title, text string, isErr bool) {
	flags := uint32(windows.MB_OK | windows.MB_SETFOREGROUND)
	if isErr {
		flags |= windows.MB_ICONERROR
	} else {
		flags |= windows.MB_ICONINFORMATION
	}
	windows.MessageBox(0, windows.StringToUTF16Ptr(text), windows.StringToUTF16Ptr("AGBridge · "+title), flags)
}

func fail(msg string) {
	log.Print(msg)
	message("出错了", msg, true)
	os.Exit(1)
}

func autostartEnabled(exe string) bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue("AGBridge")
	return err == nil && strings.Contains(strings.ToLower(v), strings.ToLower(exe))
}

func setAutostart(exe string, on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		if err := k.DeleteValue("AGBridge"); err != nil && err != registry.ErrNotExist {
			return err
		}
		return nil
	}
	return k.SetStringValue("AGBridge", `"`+exe+`" --background`)
}

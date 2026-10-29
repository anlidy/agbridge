// agbridge is a local proxy that adds third-party models to Antigravity next
// to the subscription models. On Windows it runs as a tray app.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"sync/atomic"
	"time"

	"agbridge/internal/applog"
	"agbridge/internal/config"
	"agbridge/internal/hook"
	"agbridge/internal/install"
	"agbridge/internal/proxy"
)

var version = "0.1.0"

type app struct {
	store   *config.Store
	server  *proxy.Server
	logFile string
	exe     string
	http    *http.Server
	// restartNeeded: the hook went in while an unhooked language server was
	// running; cleared when a language server registers through the shim.
	restartNeeded atomic.Bool
}

func main() {
	background := flag.Bool("background", false, "start without opening the console")
	flag.Parse()

	dir := config.Dir()
	logFile, err := applog.Setup(dir, "agbridge.log", !isGUI)
	if err != nil {
		fail("无法写入日志目录 " + dir + ": " + err.Error())
	}
	store, err := config.Open(filepath.Join(dir, "config.json"))
	if err != nil {
		fail("读取配置失败: " + err.Error())
	}
	exe, _ := os.Executable()
	a := &app{store: store, logFile: logFile, exe: exe}

	switch flag.Arg(0) {
	case "install":
		exitOn(a.install())
		fmt.Println("installed")
		return
	case "uninstall":
		exitOn(a.uninstall())
		fmt.Println("uninstalled")
		return
	case "status":
		fmt.Printf("%+v\n", a.status())
		return
	case "", "serve":
	default:
		fail("未知命令: " + flag.Arg(0))
	}

	cfg := store.Get()
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(cfg.Port))
	if err != nil {
		// Another agbridge already serves this port: just bring up its console.
		if alreadyRunning(cfg.Port) {
			if !*background {
				openBrowser(a.consoleURL())
			}
			return
		}
		fail(fmt.Sprintf("端口 %d 被其他程序占用，请在 %s 里修改 port 后重试", cfg.Port, store.Path()))
	}
	log.Printf("agbridge %s listening on %s (config %s)", version, ln.Addr(), store.Path())

	ring := applog.NewRing(200)
	a.server = proxy.New(store, ring, proxy.Hooks{
		Status:     func() any { return a.status() },
		Install:    a.install,
		Uninstall:  a.uninstall,
		OnRegister: func() { a.restartNeeded.Store(false) },
		LogFile:    logFile,
	}, version)
	a.http = &http.Server{Handler: a.server, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		if err := a.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http server: %v", err)
		}
	}()
	go a.autoRepair()

	runUI(a, !*background)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = a.http.Shutdown(ctx)
}

func (a *app) consoleURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(a.store.Get().Port) + "/ui/"
}

func (a *app) shimLog() string { return filepath.Join(config.Dir(), "shim.log") }

func (a *app) status() install.Status {
	st := install.Detect(a.store.Get(), shimBinary)
	st.RestartNeeded = st.Hooked && st.Running && a.restartNeeded.Load()
	return st
}

func (a *app) install() error {
	if err := a.applyHook(install.Detect(a.store.Get(), shimBinary)); err != nil {
		return err
	}
	return a.store.Update(func(c *config.Config) error { c.HookWanted = true; return nil })
}

// applyHook installs or repairs the hook; before is the status beforehand.
func (a *app) applyHook(before install.Status) error {
	err := install.Install(a.store.Get(), shimBinary, a.exe, a.shimLog())
	// Patching held a few hundred MB of language server copies; hand it back
	// now rather than idling in the tray with it.
	debug.FreeOSMemory()
	if err != nil {
		return err
	}
	if before.Running && !before.Hooked {
		a.restartNeeded.Store(true)
	}
	log.Printf("hook installed into %s (Antigravity %s, running=%v)", before.Dir, before.Version, before.Running)
	return nil
}

func (a *app) uninstall() error {
	if err := install.Uninstall(a.store.Get()); err != nil {
		return err
	}
	a.restartNeeded.Store(false)
	log.Printf("hook removed")
	return a.store.Update(func(c *config.Config) error { c.HookWanted = false; return nil })
}

// autoRepair re-applies the hook after Antigravity updates overwrite it, and
// keeps the shim pointing at this executable if agbridge was moved. It polls
// cheap file stats and only acts on a state that held for a whole round, so
// it never backs up a language server the updater is still writing.
func (a *app) autoRepair() {
	var last, handled string
	var retryAt time.Time
	for ; ; time.Sleep(5 * time.Second) {
		cfg := a.store.Get()
		if !cfg.HookWanted || shimBinary == nil {
			continue
		}
		fp := install.Fingerprint(cfg)
		stable := fp == last
		last = fp
		if !stable || fp == handled || time.Now().Before(retryAt) {
			continue
		}
		st := install.Detect(cfg, shimBinary)
		switch {
		case st.Found && (!st.Hooked || !st.ShimCurrent):
			if err := a.applyHook(st); err != nil {
				log.Printf("auto-repair failed: %v", err)
				retryAt = time.Now().Add(time.Minute)
				continue
			}
		case st.Hooked:
			a.syncShimConfig(st.Dir)
		}
		handled = install.Fingerprint(cfg)
	}
}

func (a *app) syncShimConfig(root string) {
	path := filepath.Join(root, "resources", "bin", hook.ShimConfig)
	sc, err := hook.ReadShim(path)
	if err != nil || sc.AgbridgeExe == a.exe {
		return
	}
	sc.AgbridgeExe = a.exe
	if b, err := jsonIndent(sc); err == nil {
		if err := os.WriteFile(path, b, 0o644); err == nil {
			log.Printf("shim config now points to %s", a.exe)
		}
	}
}

func alreadyRunning(port int) bool {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func exitOn(err error) {
	if err != nil {
		fail(err.Error())
	}
}

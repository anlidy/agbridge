// ls-shim replaces Antigravity's language_server.exe. It points the real
// language server at the local agbridge proxy and otherwise behaves exactly
// like it: same arguments, stdio and exit code. When agbridge cannot be
// reached it runs the original binary untouched.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"agbridge/internal/hook"
)

// marker lets the installer recognise this binary; keep it referenced.
var marker = hook.ShimMarker

// Endpoint flags rewritten to the proxy, in the language server's own syntax.
var endpointFlags = []string{"cloud_code_endpoint", "api_server_url", "inference_api_server_url"}

func main() {
	self, err := os.Executable()
	if err != nil {
		fatal(err)
	}
	dir := filepath.Dir(self)
	cfg, cfgErr := hook.ReadShim(filepath.Join(dir, hook.ShimConfig))
	original := filepath.Join(dir, hook.OrigName)
	if cfg != nil && cfg.Original != "" {
		original = filepath.Join(dir, cfg.Original)
	}
	if cfg != nil && cfg.LogFile != "" {
		if f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			log.SetOutput(f)
			defer f.Close()
		} else {
			// Stays on stderr, which Antigravity keeps in language_server.log.
			log.Printf("[shim] cannot open log file: %v", err)
		}
	} else {
		log.SetOutput(nullWriter{})
	}
	args := os.Args[1:]
	log.Printf("[shim %s] start pid=%d args=%d", marker, os.Getpid(), len(args))

	bin, runArgs := original, args
	if cfgErr != nil {
		log.Printf("[shim] no config (%v); running original", cfgErr)
	} else if passthrough(args) {
		// --stamp / --help: nothing to redirect.
	} else if ensureProxy(cfg) {
		orig := currentValues(args)
		register(cfg.Proxy, hook.Registration{
			CloudCode: orig["cloud_code_endpoint"],
			APIServer: orig["api_server_url"],
			Inference: orig["inference_api_server_url"],
		})
		runArgs = rewrite(args, cfg.Proxy)
		if cfg.Patched != "" {
			if p := filepath.Join(dir, cfg.Patched); exists(p) {
				bin = p
			}
		}
		log.Printf("[shim] routed via %s using %s", cfg.Proxy, filepath.Base(bin))
	} else {
		log.Printf("[shim] agbridge unreachable at %s; running original unchanged", cfg.Proxy)
	}
	os.Exit(run(bin, runArgs))
}

func passthrough(args []string) bool {
	for _, a := range args {
		switch strings.TrimLeft(a, "-") {
		case "stamp", "help", "h", "version":
			return true
		}
	}
	return false
}

func flagName(a string) (name, value string, inline bool) {
	if !strings.HasPrefix(a, "-") {
		return "", "", false
	}
	a = strings.TrimLeft(a, "-")
	if i := strings.IndexByte(a, '='); i >= 0 {
		return a[:i], a[i+1:], true
	}
	return a, "", false
}

// currentValues extracts the endpoint flags Antigravity passed.
func currentValues(args []string) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(args); i++ {
		name, value, inline := flagName(args[i])
		if !isEndpoint(name) {
			continue
		}
		if !inline && i+1 < len(args) {
			value = args[i+1]
			i++
		}
		out[name] = value
	}
	return out
}

// rewrite points every endpoint flag at the proxy, adding missing ones.
func rewrite(args []string, proxy string) []string {
	out := make([]string, 0, len(args)+len(endpointFlags))
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		name, _, inline := flagName(args[i])
		if !isEndpoint(name) {
			out = append(out, args[i])
			continue
		}
		seen[name] = true
		out = append(out, "--"+name+"="+proxy)
		if !inline && i+1 < len(args) {
			i++
		}
	}
	for _, f := range endpointFlags {
		if !seen[f] {
			out = append(out, "--"+f+"="+proxy)
		}
	}
	return out
}

func isEndpoint(name string) bool {
	for _, f := range endpointFlags {
		if name == f {
			return true
		}
	}
	return false
}

func healthy(proxy string) bool {
	c := http.Client{Timeout: 700 * time.Millisecond}
	resp, err := c.Get(proxy + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var v struct {
		App string `json:"app"`
	}
	return json.NewDecoder(resp.Body).Decode(&v) == nil && v.App == "agbridge"
}

// ensureProxy starts agbridge in the background if it is not running yet.
func ensureProxy(cfg *hook.Shim) bool {
	if healthy(cfg.Proxy) {
		return true
	}
	if cfg.AgbridgeExe == "" || !exists(cfg.AgbridgeExe) {
		return false
	}
	cmd := exec.Command(cfg.AgbridgeExe, "--background")
	detach(cmd, true)
	if err := cmd.Start(); err != nil {
		// The parent job may forbid breakaway; children escape it silently anyway.
		cmd = exec.Command(cfg.AgbridgeExe, "--background")
		detach(cmd, false)
		if err2 := cmd.Start(); err2 != nil {
			log.Printf("[shim] start agbridge: %v / %v", err, err2)
			return false
		}
	}
	_ = cmd.Process.Release()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		if healthy(cfg.Proxy) {
			return true
		}
	}
	return false
}

func register(proxy string, reg hook.Registration) {
	b, _ := json.Marshal(reg)
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Post(proxy+"/_agb/register", "application/json", bytes.NewReader(b))
	if err != nil {
		log.Printf("[shim] register: %v", err)
		return
	}
	resp.Body.Close()
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "agbridge shim:", err)
	os.Exit(1)
}

type nullWriter struct{}

func (nullWriter) Write(p []byte) (int, error) { return len(p), nil }

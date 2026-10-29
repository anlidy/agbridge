package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agbridge/internal/hook"
)

func TestRewrite(t *testing.T) {
	in := []string{"--standalone", "--api_server_url", "https://generativelanguage.googleapis.com",
		"--cloud_code_endpoint=https://daily-cloudcode-pa.googleapis.com", "-csrf_token", "abc"}
	got := strings.Join(rewrite(in, "http://127.0.0.1:1"), " ")
	want := "--standalone --api_server_url=http://127.0.0.1:1 --cloud_code_endpoint=http://127.0.0.1:1 -csrf_token abc --inference_api_server_url=http://127.0.0.1:1"
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	vals := currentValues(in)
	if vals["cloud_code_endpoint"] != "https://daily-cloudcode-pa.googleapis.com" || vals["api_server_url"] != "https://generativelanguage.googleapis.com" {
		t.Fatalf("values = %v", vals)
	}
}

// TestShimEndToEnd builds the shim and runs it against a fake proxy and a
// fake language server that echoes its arguments.
func TestShimEndToEnd(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "language_server")
	if out, err := exec.Command("go", "build", "-o", shim, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	fake := "#!/bin/sh\necho \"$0 $@\"\nexit 7\n"
	for _, name := range []string{hook.OrigName, hook.PatchedName} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var reg hook.Registration
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Write([]byte(`{"app":"agbridge"}`))
		case "/_agb/register":
			json.NewDecoder(r.Body).Decode(&reg)
		}
	}))
	defer proxy.Close()
	cfg, _ := json.Marshal(hook.Shim{Proxy: proxy.URL, Original: hook.OrigName, Patched: hook.PatchedName})
	os.WriteFile(filepath.Join(dir, hook.ShimConfig), cfg, 0o644)

	cmd := exec.Command(shim, "--cloud_code_endpoint", "https://daily-cloudcode-pa.googleapis.com", "--https_server_port", "0")
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 7 {
		t.Fatalf("exit code not propagated: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, hook.PatchedName) || !strings.Contains(s, "--cloud_code_endpoint="+proxy.URL) || !strings.Contains(s, "--https_server_port 0") {
		t.Fatalf("unexpected invocation: %s", s)
	}
	if reg.CloudCode != "https://daily-cloudcode-pa.googleapis.com" {
		t.Fatalf("registration = %+v", reg)
	}

	// --stamp never touches the proxy and uses the original binary.
	out, _ = exec.Command(shim, "--stamp").Output()
	if !strings.Contains(string(out), hook.OrigName+" --stamp") {
		t.Fatalf("stamp: %s", out)
	}

	// Proxy down: run the original with arguments untouched.
	proxy.Close()
	out, _ = exec.Command(shim, "--cloud_code_endpoint", "https://x.googleapis.com").Output()
	if !strings.Contains(string(out), hook.OrigName+" --cloud_code_endpoint https://x.googleapis.com") {
		t.Fatalf("fallback: %s", out)
	}
}

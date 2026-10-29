// Package hook holds the contract shared by the installer, the language
// server shim and the proxy: file names, the shim config and binary patches.
package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	LSName      = "language_server.exe"
	OrigName    = "language_server.agb-orig.exe"
	PatchedName = "language_server.agb-patched.exe"
	ShimConfig  = "agbridge-shim.json"
	// ShimMarker is embedded in the shim binary so the installer can tell it
	// apart from the vendor language server.
	ShimMarker = "AGBRIDGE-LS-SHIM/1"
)

// Shim is the configuration written next to the installed shim.
type Shim struct {
	Proxy       string `json:"proxy"`       // e.g. http://127.0.0.1:50999
	AgbridgeExe string `json:"agbridgeExe"` // started when the proxy is not running
	Original    string `json:"original"`    // vendor binary (file name in the same dir)
	Patched     string `json:"patched"`     // patched copy, empty when unused
	LogFile     string `json:"logFile"`
}

func ReadShim(path string) (*Shim, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Shim
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Registration is what the shim tells the proxy about the endpoints
// Antigravity originally passed to the language server.
type Registration struct {
	CloudCode string `json:"cloudCode,omitempty"`
	APIServer string `json:"apiServer,omitempty"`
	Inference string `json:"inference,omitempty"`
}

// Rule is a hardcoded endpoint inside the language server that is replaced by
// a same-length loopback URL. Since 2.0.6 the model list is fetched from the
// hardcoded daily Cloud Code URL, ignoring --cloud_code_endpoint.
type Rule struct {
	Original string
	Marker   string // path prefix identifying the original upstream
}

var Rules = []Rule{
	{Original: "https://daily-cloudcode-pa.googleapis.com", Marker: "/_agbd"},
}

// MarkerFor maps known Google origins to path markers (used when rewriting
// response bodies so follow-up calls keep their upstream).
var MarkerFor = map[string]string{
	"https://daily-cloudcode-pa.googleapis.com": "/_agbd",
	"https://cloudcode-pa.googleapis.com":       "/_agbp",
}

// PatchedURL returns a loopback URL padded to exactly len(r.Original) bytes.
func (r Rule) PatchedURL(port int) (string, error) {
	base := fmt.Sprintf("http://127.0.0.1:%d%s/", port, r.Marker)
	pad := len(r.Original) - len(base)
	if pad < 1 {
		return "", fmt.Errorf("port %d does not fit the %d-byte endpoint", port, len(r.Original))
	}
	return base + strings.Repeat("x", pad), nil
}

// StripMarker removes a marker (and its padding) from a request path and
// returns the upstream origin it stands for.
func StripMarker(path string) (rest, origin string) {
	for o, m := range MarkerFor {
		if path == m || strings.HasPrefix(path, m+"/") {
			rest = strings.TrimPrefix(path, m)
			// Drop "/xxxx" padding left by a binary patch.
			if strings.HasPrefix(rest, "/x") {
				i := 1
				for i < len(rest) && rest[i] == 'x' {
					i++
				}
				if i == len(rest) || rest[i] == '/' {
					rest = rest[i:]
				}
			}
			if rest == "" {
				rest = "/"
			}
			return rest, o
		}
	}
	return path, ""
}

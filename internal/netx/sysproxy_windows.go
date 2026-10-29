package netx

import (
	"net/url"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// systemProxy reads the WinINET per-user proxy (what Clash/v2rayN "system
// proxy" mode sets). TUN mode needs no proxy and leaves ProxyEnable at 0.
func systemProxy(target *url.URL) (*url.URL, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return nil, nil
	}
	defer k.Close()
	enabled, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || enabled == 0 {
		return nil, nil
	}
	server, _, err := k.GetStringValue("ProxyServer")
	if err != nil || server == "" {
		return nil, nil
	}
	if override, _, err := k.GetStringValue("ProxyOverride"); err == nil && bypassed(target.Hostname(), override) {
		return nil, nil
	}
	// Either "host:port" or "http=host:port;https=host:port;socks=host:port".
	addr := server
	if strings.Contains(server, "=") {
		addr = ""
		entries := map[string]string{}
		for _, part := range strings.Split(server, ";") {
			if kv := strings.SplitN(part, "=", 2); len(kv) == 2 {
				entries[strings.ToLower(strings.TrimSpace(kv[0]))] = strings.TrimSpace(kv[1])
			}
		}
		switch {
		case entries["https"] != "":
			addr = entries["https"]
		case entries["http"] != "":
			addr = entries["http"]
		case entries["socks"] != "":
			return url.Parse("socks5://" + entries["socks"])
		}
	}
	if addr == "" {
		return nil, nil
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return url.Parse(addr)
}

func bypassed(host, override string) bool {
	for _, pat := range strings.Split(override, ";") {
		pat = strings.ToLower(strings.TrimSpace(pat))
		switch {
		case pat == "":
		case pat == "<local>":
			if !strings.Contains(host, ".") {
				return true
			}
		case strings.HasSuffix(pat, "*"):
			if strings.HasPrefix(host, strings.TrimSuffix(pat, "*")) {
				return true
			}
		case strings.HasPrefix(pat, "*"):
			if strings.HasSuffix(host, strings.TrimPrefix(pat, "*")) {
				return true
			}
		case host == pat:
			return true
		}
	}
	return false
}

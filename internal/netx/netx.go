// Package netx builds outbound HTTP clients that honour agbridge's proxy
// settings ("auto", "direct" or an explicit URL).
package netx

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ProxyFunc resolves a proxy setting to a Transport.Proxy function.
func ProxyFunc(setting string) func(*http.Request) (*url.URL, error) {
	switch s := strings.TrimSpace(setting); s {
	case "", "auto":
		return func(r *http.Request) (*url.URL, error) {
			if u, err := http.ProxyFromEnvironment(r); u != nil || err != nil {
				return u, err
			}
			return systemProxy(r.URL)
		}
	case "direct":
		return nil
	default:
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return func(*http.Request) (*url.URL, error) { return nil, err }
		}
		return http.ProxyURL(u)
	}
}

var (
	mu      sync.Mutex
	clients = map[string]*http.Client{}
)

// Client returns a shared streaming-friendly client for a proxy setting.
// Timeouts are applied per request by the caller's context.
func Client(setting string) *http.Client {
	mu.Lock()
	defer mu.Unlock()
	if c, ok := clients[setting]; ok {
		return c
	}
	t := &http.Transport{
		Proxy:                 ProxyFunc(setting),
		DialContext:           (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 0,
		ExpectContinueTimeout: time.Second,
	}
	c := &http.Client{Transport: t}
	clients[setting] = c
	return c
}

// Reset drops cached clients after a proxy setting change.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	for _, c := range clients {
		c.CloseIdleConnections()
	}
	clients = map[string]*http.Client{}
}

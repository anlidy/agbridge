// Package proxy is the loopback server the language server talks to instead
// of Google: native traffic is forwarded, custom models are answered locally.
package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"agbridge/internal/applog"
	"agbridge/internal/config"
	"agbridge/internal/hook"
	"agbridge/internal/netx"
)

const maxBody = 64 << 20

// Hooks lets the host app provide install actions without an import cycle.
type Hooks struct {
	Status    func() any
	Install   func() error
	Uninstall func() error
	// OnRegister runs when a language server starts through the shim.
	OnRegister func()
	LogFile    string
}

type Server struct {
	Version string
	store   *config.Store
	ring    *applog.Ring
	hooks   Hooks
	native  *httputil.ReverseProxy

	mu          sync.Mutex
	nativeModel []NativeModel
	catalogAt   time.Time
	injected    []string // slugs added to the last catalog served
	tiers       map[string]tierRoute
}

// tierRoute remembers which Cloud Code host an account belongs to. Pointed at
// a custom URL, the language server skips its own UpdateEndpointURL step, so
// the proxy replays it: GCP-ToS tiers use production, the rest daily.
type tierRoute struct {
	origin  string
	expires time.Time
}

type ctxKey int

const targetKey ctxKey = 0

func New(store *config.Store, ring *applog.Ring, hooks Hooks, version string) *Server {
	s := &Server{Version: version, store: store, ring: ring, hooks: hooks}
	s.native = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			target := pr.In.Context().Value(targetKey).(*url.URL)
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.URL.Path = target.Path
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = target.RawQuery
			pr.Out.Host = ""
			// Let the transport negotiate gzip and hand us plain bodies.
			pr.Out.Header.Del("Accept-Encoding")
		},
		Transport:      roundTripper{s},
		FlushInterval:  -1,
		ModifyResponse: s.rewriteNativeResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			log.Printf("[native] %s %s: %v", r.Method, r.URL.Path, err)
			writeError(w, http.StatusBadGateway, "agbridge 转发到 Google 失败: "+err.Error())
		},
	}
	return s
}

// roundTripper picks the transport for the current global proxy setting.
type roundTripper struct{ s *Server }

func (rt roundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return netx.Client(rt.s.store.Get().Proxy).Transport.RoundTrip(r)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.allowedHost(r) {
		writeError(w, http.StatusForbidden, "forbidden host")
		return
	}
	path, origin := hook.StripMarker(r.URL.Path)
	r.URL.Path, r.URL.RawPath = path, ""

	switch {
	case path == "/health" || path == "/healthz":
		writeJSON(w, http.StatusOK, map[string]any{"app": "agbridge", "version": s.Version, "port": s.store.Get().Port})
		return
	case path == "/_agb/register" && r.Method == http.MethodPost:
		s.handleRegister(w, r)
		return
	case path == "/" && r.Method == http.MethodGet:
		http.Redirect(w, r, "/ui/", http.StatusFound)
		return
	case strings.HasPrefix(path, "/ui/"):
		s.serveUI(w, r)
		return
	case strings.HasPrefix(path, "/api/"):
		s.serveAPI(w, r)
		return
	}

	cloud := strings.HasPrefix(path, "/v1internal")
	upstream := s.upstream(r, cloud, origin)
	if r.Method == http.MethodPost {
		switch {
		case path == "/v1internal:fetchAvailableModels":
			s.handleCatalog(w, r, upstream)
			return
		case path == "/v1internal:streamGenerateContent" || path == "/v1internal:generateContent" || path == "/v1internal:countTokens":
			s.handleCustom(w, r, true, "", path[len("/v1internal:"):], upstream)
			return
		default:
			if m := publicGen.FindStringSubmatch(path); m != nil {
				s.handleCustom(w, r, false, m[1], m[2], upstream)
				return
			}
		}
	}
	s.forwardNative(w, r, upstream)
}

var publicGen = regexp.MustCompile(`^/v1[a-z0-9]*/models/([^/:]+):(streamGenerateContent|generateContent|countTokens)$`)

// allowedHost blocks DNS-rebinding: only loopback Host headers are served.
func (s *Server) allowedHost(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	switch host {
	case "127.0.0.1", "localhost", "::1", "[::1]":
	default:
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host != r.Host {
			return false
		}
	}
	return true
}

// upstream returns the Google origin for a request.
func (s *Server) upstream(r *http.Request, cloud bool, marker string) string {
	if marker != "" {
		return marker
	}
	up := s.store.Get().Upstreams
	if cloud {
		if o := s.tierOrigin(r.Header); o != "" {
			return o
		}
		if up.CloudCode != "" {
			return up.CloudCode
		}
		return config.DefaultCloudCode
	}
	if up.APIServer != "" {
		return up.APIServer
	}
	return config.DefaultAPIServer
}

func (s *Server) forwardNative(w http.ResponseWriter, r *http.Request, origin string) {
	target, err := url.Parse(origin)
	if err != nil {
		writeError(w, http.StatusBadGateway, "invalid upstream "+origin)
		return
	}
	target.Path = strings.TrimRight(target.Path, "/") + r.URL.Path
	target.RawQuery = r.URL.RawQuery
	s.native.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), targetKey, target)))
}

// forwardBuffered replays an already-read request body to the native upstream.
func (s *Server) forwardBuffered(w http.ResponseWriter, r *http.Request, body []byte, origin string) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	s.forwardNative(w, r, origin)
}

var googleOrigin = regexp.MustCompile(`https://(daily-cloudcode-pa|cloudcode-pa)\.googleapis\.com`)

// rewriteNativeResponse keeps follow-up Cloud Code URLs pointed at the proxy.
func (s *Server) rewriteNativeResponse(resp *http.Response) error {
	p := resp.Request.URL.Path
	if !strings.HasPrefix(p, "/v1internal") || strings.Contains(p, "GenerateContent") || strings.Contains(p, "stream") ||
		!strings.Contains(resp.Header.Get("Content-Type"), "json") || resp.ContentLength > 4<<20 {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	resp.Body.Close()
	if err != nil {
		return err
	}
	if p == "/v1internal:loadCodeAssist" && resp.StatusCode/100 == 2 {
		s.observeTier(resp.Request.Header, b)
	}
	if len(b) <= 4<<20 && googleOrigin.Match(b) {
		port := s.store.Get().Port
		b = googleOrigin.ReplaceAllFunc(b, func(m []byte) []byte {
			return []byte("http://127.0.0.1:" + strconv.Itoa(port) + hook.MarkerFor[string(m)])
		})
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	resp.ContentLength = int64(len(b))
	resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
	resp.Header.Del("Content-Encoding")
	return nil
}

func callerKey(h http.Header) string {
	auth := h.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(auth[7:])))
	return hex.EncodeToString(sum[:])
}

func (s *Server) observeTier(h http.Header, body []byte) {
	key := callerKey(h)
	if key == "" {
		return
	}
	var v struct {
		PaidTier *struct {
			UsesGcpTos *bool `json:"usesGcpTos"`
		} `json:"paidTier"`
	}
	if json.Unmarshal(body, &v) != nil {
		return
	}
	origin := "https://daily-cloudcode-pa.googleapis.com"
	if v.PaidTier != nil && v.PaidTier.UsesGcpTos != nil && *v.PaidTier.UsesGcpTos {
		origin = "https://cloudcode-pa.googleapis.com"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tiers == nil || len(s.tiers) > 64 {
		s.tiers = map[string]tierRoute{}
	}
	if prev := s.tiers[key]; prev.origin != origin {
		log.Printf("[tier] account routed to %s", origin)
	}
	s.tiers[key] = tierRoute{origin: origin, expires: time.Now().Add(time.Hour)}
}

func (s *Server) tierOrigin(h http.Header) string {
	key := callerKey(h)
	if key == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tiers[key]; ok && time.Now().Before(t.expires) {
		return t.origin
	}
	return ""
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var reg hook.Registration
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&reg); err != nil {
		writeError(w, http.StatusBadRequest, "bad registration")
		return
	}
	for _, u := range []string{reg.CloudCode, reg.APIServer, reg.Inference} {
		if u != "" && !isGoogleURL(u) {
			writeError(w, http.StatusBadRequest, "only https://*.googleapis.com upstreams are accepted")
			return
		}
	}
	err := s.store.Update(func(c *config.Config) error {
		if reg.CloudCode != "" {
			c.Upstreams.CloudCode = strings.TrimRight(reg.CloudCode, "/")
		}
		if reg.APIServer != "" {
			c.Upstreams.APIServer = strings.TrimRight(reg.APIServer, "/")
		}
		c.Upstreams.Inference = strings.TrimRight(reg.Inference, "/")
		return nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("[shim] language server registered: cloudCode=%s apiServer=%s inference=%s", reg.CloudCode, reg.APIServer, reg.Inference)
	if s.hooks.OnRegister != nil {
		s.hooks.OnRegister()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func isGoogleURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && (strings.HasSuffix(u.Hostname(), ".googleapis.com") || strings.HasSuffix(u.Hostname(), ".google.com"))
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large or unreadable")
		return nil, false
	}
	return b, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError answers in the Google API error shape the language server parses.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code": status, "message": msg, "status": googleStatus(status)}})
}

func googleStatus(code int) string {
	switch code {
	case 400:
		return "INVALID_ARGUMENT"
	case 401:
		return "UNAUTHENTICATED"
	case 403:
		return "PERMISSION_DENIED"
	case 404:
		return "NOT_FOUND"
	case 429:
		return "RESOURCE_EXHAUSTED"
	case 499:
		return "CANCELLED"
	case 503:
		return "UNAVAILABLE"
	case 504:
		return "DEADLINE_EXCEEDED"
	}
	if code >= 500 {
		return "INTERNAL"
	}
	return "UNKNOWN"
}

// Package config holds agbridge's persistent settings: providers, models,
// upstream endpoints and the native model-picker identities.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
)

const (
	DefaultPort          = 50999
	DefaultCloudCode     = "https://daily-cloudcode-pa.googleapis.com"
	DefaultAPIServer     = "https://generativelanguage.googleapis.com"
	PlaceholderMax       = 1650 // MODEL_PLACEHOLDER_M0..M1650 in Antigravity 2.25 language server
	placeholderAllocBase = 1650 // allocate from the top down, far from native placeholders
)

// Provider protocol types.
const (
	TypeGemini    = "gemini"    // Gemini API (generateContent / streamGenerateContent)
	TypeOpenAI    = "openai"    // OpenAI-compatible /chat/completions
	TypeAnthropic = "anthropic" // Anthropic /v1/messages
)

type Provider struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	BaseURL string            `json:"baseUrl"`
	APIKey  string            `json:"apiKey,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// AuthMode for gemini: "header" (x-goog-api-key, default), "bearer", "query" (?key=).
	AuthMode string `json:"authMode,omitempty"`
	// Proxy: "" inherit global, "direct", or an http/https/socks5 URL.
	Proxy          string `json:"proxy,omitempty"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

type Model struct {
	ID              string `json:"id"`
	ProviderID      string `json:"providerId"`
	UpstreamModel   string `json:"upstreamModel"`
	DisplayName     string `json:"displayName"`
	Enabled         bool   `json:"enabled"`
	MaxTokens       int    `json:"maxTokens,omitempty"`
	MaxOutputTokens int    `json:"maxOutputTokens,omitempty"`
	SupportsImages  *bool  `json:"supportsImages,omitempty"`
	SupportsThink   *bool  `json:"supportsThinking,omitempty"`
	// ReasoningEffort: "", minimal, low, medium, high. Sent as reasoning_effort
	// (openai), a thinking budget (anthropic) or thinkingLevel (gemini).
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	// ThoughtSignature handling for gemini upstreams: "keep" (default), "dummy", "strip".
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
	// Efforts, when set, lists the reasoning efforts offered as separate picker
	// entries "Name (Low)", "Name (High)"…, which Antigravity groups into one
	// model with an effort switch, like its native Gemini models.
	Efforts []string `json:"efforts,omitempty"`
	// Placeholder is the native MODEL_PLACEHOLDER_M<n> index this model owns in
	// the picker; EffortPlaceholders holds those of its effort entries.
	Placeholder        *int           `json:"placeholder,omitempty"`
	EffortPlaceholders map[string]int `json:"effortPlaceholders,omitempty"`
}

// EffortLevels are the accepted reasoning efforts, lowest first.
var EffortLevels = []string{"minimal", "low", "medium", "high"}

// Entry is one item a model puts into the picker.
type Entry struct {
	Slug        string
	Label       string // suffix for the display name, e.g. " (High)"
	Effort      string // "" for the model's plain entry
	Placeholder *int
}

// Slug is the catalog key Antigravity uses to refer to the model.
func (m *Model) Slug() string { return "agb-" + m.ID }

// Entries lists the model's picker entries: one per effort when Efforts is
// set, else the plain entry.
func (m *Model) Entries() []Entry {
	if len(m.Efforts) == 0 {
		return []Entry{{Slug: m.Slug(), Placeholder: m.Placeholder}}
	}
	var out []Entry
	for _, e := range EffortLevels {
		if !slices.Contains(m.Efforts, e) {
			continue
		}
		en := Entry{Slug: m.Slug() + "-" + e, Label: " (" + strings.ToUpper(e[:1]) + e[1:] + ")", Effort: e}
		if n, ok := m.EffortPlaceholders[e]; ok {
			en.Placeholder = &n
		}
		out = append(out, en)
	}
	return out
}

func PlaceholderName(n int) string { return fmt.Sprintf("MODEL_PLACEHOLDER_M%d", n) }

type Upstreams struct {
	CloudCode string `json:"cloudCode,omitempty"`
	APIServer string `json:"apiServer,omitempty"`
	Inference string `json:"inference,omitempty"`
}

type Config struct {
	Port int `json:"port"`
	// Proxy for Google/native traffic and providers that inherit: "auto" (env, then
	// Windows system proxy), "direct", or an explicit URL.
	Proxy     string     `json:"proxy,omitempty"`
	Upstreams Upstreams  `json:"upstreams"`
	Providers []Provider `json:"providers"`
	Models    []Model    `json:"models"`
	// ReservedPlaceholders are placeholder indexes observed in the native catalog.
	ReservedPlaceholders []int `json:"reservedPlaceholders,omitempty"`
	// AntigravityDir overrides auto-detection of the install directory.
	AntigravityDir string `json:"antigravityDir,omitempty"`
	// HookWanted records that the user installed the hook, enabling auto-repair after updates.
	HookWanted bool `json:"hookWanted,omitempty"`
	// PatchBinary enables the patched language server copy (needed for the model list).
	PatchBinary *bool  `json:"patchBinary,omitempty"`
	APIToken    string `json:"apiToken,omitempty"`
}

func (c *Config) PatchEnabled() bool { return c.PatchBinary == nil || *c.PatchBinary }

func (c *Config) Provider(id string) *Provider {
	for i := range c.Providers {
		if c.Providers[i].ID == id {
			return &c.Providers[i]
		}
	}
	return nil
}

func (c *Config) Model(id string) *Model {
	for i := range c.Models {
		if c.Models[i].ID == id {
			return &c.Models[i]
		}
	}
	return nil
}

// Clone returns a deep copy safe to use without holding the store lock.
func (c *Config) Clone() *Config {
	b, _ := json.Marshal(c)
	var out Config
	_ = json.Unmarshal(b, &out)
	return &out
}

func NewID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Dir returns the per-user data directory (%APPDATA%\agbridge on Windows).
func Dir() string {
	if d := os.Getenv("AGBRIDGE_HOME"); d != "" {
		return d
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "agbridge")
}

// Store guards the config and persists every change atomically.
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  *Config
}

func Open(path string) (*Store, error) {
	s := &Store{path: path}
	cfg := &Config{}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	for i := range cfg.Providers {
		cfg.Providers[i].APIKey = unseal(cfg.Providers[i].APIKey)
		for k, v := range cfg.Providers[i].Headers {
			cfg.Providers[i].Headers[k] = unseal(v)
		}
	}
	changed := normalize(cfg)
	s.cfg = cfg
	if changed || errors.Is(err, os.ErrNotExist) {
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func normalize(c *Config) bool {
	changed := false
	if c.Port == 0 {
		c.Port, changed = DefaultPort, true
	}
	if c.Proxy == "" {
		c.Proxy, changed = "auto", true
	}
	if c.APIToken == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		c.APIToken, changed = hex.EncodeToString(b), true
	}
	if c.Providers == nil {
		c.Providers = []Provider{}
	}
	if c.Models == nil {
		c.Models = []Model{}
	}
	return changed
}

func (s *Store) Path() string { return s.path }

// Get returns a snapshot of the configuration.
func (s *Store) Get() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Clone()
}

// Update applies fn to the live config and persists it if fn returns nil.
func (s *Store) Update(fn func(*Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cfg.Clone()
	if err := fn(next); err != nil {
		return err
	}
	normalize(next)
	if err := validate(next); err != nil {
		return err
	}
	prev := s.cfg
	s.cfg = next
	if err := s.saveLocked(); err != nil {
		s.cfg = prev
		return err
	}
	return nil
}

func validate(c *Config) error {
	if c.Port < 1024 || c.Port > 65535 {
		return fmt.Errorf("端口必须在 1024-65535 之间")
	}
	ids := map[string]bool{}
	for _, p := range c.Providers {
		if p.ID == "" || ids[p.ID] {
			return fmt.Errorf("提供方 ID 为空或重复: %q", p.ID)
		}
		ids[p.ID] = true
		switch p.Type {
		case TypeGemini, TypeOpenAI, TypeAnthropic:
		default:
			return fmt.Errorf("提供方 %s 的类型无效: %q", p.Name, p.Type)
		}
		if !strings.HasPrefix(p.BaseURL, "http://") && !strings.HasPrefix(p.BaseURL, "https://") {
			return fmt.Errorf("提供方 %s 的地址必须以 http:// 或 https:// 开头", p.Name)
		}
	}
	mids := map[string]bool{}
	owners := map[int]string{}
	for _, m := range c.Models {
		if m.ID == "" || mids[m.ID] {
			return fmt.Errorf("模型 ID 为空或重复: %q", m.ID)
		}
		mids[m.ID] = true
		if !ids[m.ProviderID] {
			return fmt.Errorf("模型 %s 引用了不存在的提供方", m.DisplayName)
		}
		if strings.TrimSpace(m.UpstreamModel) == "" {
			return fmt.Errorf("模型 %s 缺少上游模型名", m.DisplayName)
		}
		for i, e := range m.Efforts {
			if !slices.Contains(EffortLevels, e) || slices.Contains(m.Efforts[:i], e) {
				return fmt.Errorf("模型 %s 的档位 %q 无效或重复", m.DisplayName, e)
			}
		}
		for _, e := range m.Entries() {
			if e.Placeholder == nil {
				continue
			}
			if prev, ok := owners[*e.Placeholder]; ok {
				return fmt.Errorf("模型 %s 与 %s 占用了同一个原生编号", m.DisplayName, prev)
			}
			owners[*e.Placeholder] = m.DisplayName
		}
	}
	return nil
}

func (s *Store) saveLocked() error {
	out := s.cfg.Clone()
	for i := range out.Providers {
		out.Providers[i].APIKey = seal(out.Providers[i].APIKey)
		for k, v := range out.Providers[i].Headers {
			out.Providers[i].Headers[k] = seal(v)
		}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// AssignPlaceholders gives every model a native placeholder that is not
// reserved by the upstream catalog. It returns true when anything changed.
func AssignPlaceholders(c *Config, upstreamUsed []int) bool {
	changed := false
	reserved := map[int]bool{}
	for _, r := range c.ReservedPlaceholders {
		reserved[r] = true
	}
	for _, r := range upstreamUsed {
		if !reserved[r] {
			reserved[r] = true
			c.ReservedPlaceholders = append(c.ReservedPlaceholders, r)
			changed = true
		}
	}
	sort.Ints(c.ReservedPlaceholders)

	// A model holds either its plain placeholder or one per effort entry;
	// release whatever it no longer shows.
	for i := range c.Models {
		m := &c.Models[i]
		if len(m.Efforts) > 0 && m.Placeholder != nil {
			m.Placeholder = nil
			changed = true
		}
		for e := range m.EffortPlaceholders {
			if !slices.Contains(m.Efforts, e) {
				delete(m.EffortPlaceholders, e)
				changed = true
			}
		}
	}
	type slot struct {
		m      *Model
		effort string
	}
	get := func(s slot) (int, bool) {
		if s.effort == "" {
			if s.m.Placeholder == nil {
				return 0, false
			}
			return *s.m.Placeholder, true
		}
		n, ok := s.m.EffortPlaceholders[s.effort]
		return n, ok
	}
	set := func(s slot, n int, ok bool) {
		changed = true
		switch {
		case s.effort == "" && ok:
			s.m.Placeholder = &n
		case s.effort == "":
			s.m.Placeholder = nil
		case ok:
			if s.m.EffortPlaceholders == nil {
				s.m.EffortPlaceholders = map[string]int{}
			}
			s.m.EffortPlaceholders[s.effort] = n
		default:
			delete(s.m.EffortPlaceholders, s.effort)
		}
	}
	var slots []slot
	for i := range c.Models {
		m := &c.Models[i]
		for _, e := range m.Entries() {
			slots = append(slots, slot{m, e.Effort})
		}
	}

	used := map[int]bool{}
	for _, s := range slots {
		n, ok := get(s)
		if ok && (reserved[n] || used[n] || n < 0 || n > PlaceholderMax) {
			// Google started using this identity, or it collides: never let a native
			// selection route to a custom provider.
			set(s, 0, false)
			ok = false
		}
		if ok {
			used[n] = true
		}
	}
	next := placeholderAllocBase
	for _, s := range slots {
		if _, ok := get(s); ok {
			continue
		}
		for next >= 0 && (reserved[next] || used[next]) {
			next--
		}
		if next < 0 {
			break
		}
		set(s, next, true)
		used[next] = true
	}
	return changed
}

func IsWindows() bool { return runtime.GOOS == "windows" }

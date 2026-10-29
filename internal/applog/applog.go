// Package applog provides the process log file and an in-memory ring of
// recent custom-model requests for the web console.
package applog

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const maxLogBytes = 5 << 20

// Setup sends the standard logger to dir/name, rotating once at 5 MB.
func Setup(dir, name string, alsoStderr bool) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if st, err := os.Stat(path); err == nil && st.Size() > maxLogBytes {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", err
	}
	var w io.Writer = f
	if alsoStderr {
		w = io.MultiWriter(f, os.Stderr)
	}
	log.SetOutput(w)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	return path, nil
}

// Record describes one routed request, shown on the console's activity list.
type Record struct {
	Time       time.Time `json:"time"`
	Kind       string    `json:"kind"` // custom | native-catalog | error
	Model      string    `json:"model"`
	Provider   string    `json:"provider,omitempty"`
	Status     int       `json:"status"`
	DurationMs int64     `json:"durationMs"`
	InTokens   int       `json:"inTokens,omitempty"`
	OutTokens  int       `json:"outTokens,omitempty"`
	Error      string    `json:"error,omitempty"`
}

type Ring struct {
	mu   sync.Mutex
	buf  []Record
	next int
	full bool
}

func NewRing(n int) *Ring { return &Ring{buf: make([]Record, n)} }

func (r *Ring) Add(rec Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = rec
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

// List returns records newest first.
func (r *Ring) List() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.next
	if r.full {
		n = len(r.buf)
	}
	out := make([]Record, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, r.buf[(r.next-i+len(r.buf))%len(r.buf)])
	}
	return out
}

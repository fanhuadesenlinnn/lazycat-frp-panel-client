// Package logbuf implements the bounded, incrementally readable log buffer that
// backs the log view in the web UI.
package logbuf

import (
	"strings"
	"sync"
	"time"
)

// Line is a single log line with a monotonically increasing sequence number so
// that the browser can poll for "everything after N".
type Line struct {
	Seq  uint64    `json:"seq"`
	Time time.Time `json:"time"`
	Text string    `json:"text"`
}

// Ring is a bounded in-memory log buffer.
type Ring struct {
	mu       sync.Mutex
	lines    []Line
	nextSeq  uint64
	capacity int
	maxLine  int
	partial  []byte
	// discarding is set while the rest of a runaway (unterminated, oversized)
	// line is being thrown away, so that its tail can not resurface as a line
	// once the terminating newline finally arrives.
	discarding bool
	redact     func(string) string
}

// New creates a Ring holding at most capacity lines; longer lines are truncated
// to maxLine bytes before redaction is applied by redact (which may be nil).
func New(capacity, maxLine int, redact func(string) string) *Ring {
	if capacity <= 0 {
		capacity = 500
	}
	if maxLine <= 0 {
		maxLine = 8192
	}
	if redact == nil {
		redact = func(s string) string { return s }
	}
	return &Ring{
		lines:    make([]Line, 0, capacity),
		capacity: capacity,
		maxLine:  maxLine,
		redact:   redact,
	}
}

// Write implements io.Writer. It splits the incoming bytes on newlines and
// stores complete lines. It never returns an error so that it can be used as a
// child process stdout/stderr sink.
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := len(p)
	for len(p) > 0 {
		if r.discarding {
			idx := indexByte(p, '\n')
			if idx < 0 {
				// Still inside the discarded line: swallow the bytes.
				return n, nil
			}
			p = p[idx+1:]
			r.discarding = false
			continue
		}
		idx := indexByte(p, '\n')
		if idx < 0 {
			r.partial = append(r.partial, p...)
			p = nil
			if len(r.partial) > r.maxLine*4 {
				r.partial = nil
				r.discarding = true
				r.pushLocked("检测到超长日志行（长时间没有换行），已丢弃该行内容")
			}
			break
		}
		r.partial = append(r.partial, p[:idx]...)
		line := r.partial
		r.partial = nil
		p = p[idx+1:]
		r.appendLocked(string(line))
	}
	return n, nil
}

// Flush stores a trailing partial line. Call it when the child process exits.
func (r *Ring) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.discarding {
		// The last line was a discarded runaway line; do not resurrect it.
		r.discarding = false
		r.partial = nil
		return
	}
	if len(r.partial) == 0 {
		return
	}
	line := r.partial
	r.partial = nil
	r.appendLocked(string(line))
}

func (r *Ring) appendLocked(raw string) {
	text := strings.TrimRight(raw, "\r")
	if strings.TrimSpace(text) == "" {
		return
	}
	// Redact the whole line before any truncation: cutting first could leave a
	// partial secret sitting exactly at the cut point.
	text = r.redact(text)
	if len(text) > r.maxLine {
		short := text[:r.maxLine]
		// Never end the visible line with a partial replacement marker.
		if strings.HasSuffix(short, "*") {
			short = strings.TrimRight(short, "*")
		}
		text = short + " ...(已截断)"
	}
	r.pushLocked(text)
}

// pushLocked stores an already redacted line.
func (r *Ring) pushLocked(text string) {
	r.nextSeq++
	r.lines = append(r.lines, Line{
		Seq:  r.nextSeq,
		Time: time.Now(),
		Text: text,
	})
	if len(r.lines) > r.capacity {
		overflow := len(r.lines) - r.capacity
		r.lines = append(r.lines[:0], r.lines[overflow:]...)
	}
}

// Since returns every buffered line with Seq greater than seq, oldest first.
func (r *Ring) Since(seq uint64) []Line {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Line, 0, len(r.lines))
	for _, l := range r.lines {
		if l.Seq > seq {
			out = append(out, l)
		}
	}
	return out
}

// Tail returns the newest n lines, oldest first.
func (r *Ring) Tail(n int) []Line {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.lines) {
		n = len(r.lines)
	}
	out := make([]Line, n)
	copy(out, r.lines[len(r.lines)-n:])
	return out
}

// LastSeq returns the highest sequence number handed out so far.
func (r *Ring) LastSeq() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nextSeq
}

// Clear drops every buffered line.
func (r *Ring) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = r.lines[:0]
	r.partial = nil
	r.discarding = false
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

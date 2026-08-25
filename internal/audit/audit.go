package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Event struct {
	ID, ProjectID, Action, Actor, RequestID string
	At                                      time.Time
	Data                                    map[string]interface{}
	PreviousHash, Hash                      string
}
type Logger struct {
	path string
	mu   sync.Mutex
	last string
}

func New(dir string) *Logger {
	l := &Logger{path: filepath.Join(dir, "audit.jsonl")}
	if b, err := os.ReadFile(l.path); err == nil {
		lines := splitLines(b)
		if len(lines) > 0 {
			var e Event
			if json.Unmarshal(lines[len(lines)-1], &e) == nil {
				l.last = e.Hash
			}
		}
	}
	return l
}
func (l *Logger) Append(e Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.PreviousHash = l.last
	raw, _ := json.Marshal(e)
	h := sha256.Sum256(raw)
	e.Hash = hex.EncodeToString(h[:])
	line, _ := json.Marshal(e)
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(append(line, '\n')); err != nil {
		return err
	}
	l.last = e.Hash
	return nil
}
func (l *Logger) List(project string) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, err := os.ReadFile(l.path)
	if err != nil {
		return nil
	}
	var out []Event
	for _, line := range splitLines(b) {
		var e Event
		if json.Unmarshal(line, &e) == nil && (project == "" || e.ProjectID == project) {
			out = append(out, e)
		}
	}
	return out
}

func (l *Logger) Verify(project string) bool {
	events := l.List("")
	previous := ""
	for _, e := range events {
		if e.PreviousHash != previous {
			return false
		}
		want := e.Hash
		e.Hash = ""
		raw, _ := json.Marshal(e)
		h := sha256.Sum256(raw)
		if hex.EncodeToString(h[:]) != want {
			return false
		}
		previous = want
	}
	_ = project
	return true
}

func (l *Logger) VerifyDiagnostic(project string) (bool, string, string) {
	events := l.List("")
	previous := ""
	for _, e := range events {
		if e.PreviousHash != previous {
			return false, e.ID, previous
		}
		want := e.Hash
		e.Hash = ""
		raw, _ := json.Marshal(e)
		h := sha256.Sum256(raw)
		got := hex.EncodeToString(h[:])
		if got != want {
			return false, e.ID, got
		}
		previous = want
	}
	return true, "", ""
}
func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, b[start:i])
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

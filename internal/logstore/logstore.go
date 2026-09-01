// Package logstore holds the recent proxied-request records served by the
// dashboard log view. It is a small mutex-guarded ring buffer.
package logstore

import (
	"sync"
	"time"
)

// Level is the entry severity.
type Level string

const (
	LevelInfo  Level = "info"
	LevelError Level = "error"
)

// Entry is one proxied-request record.
type Entry struct {
	Time       time.Time `json:"time"`
	Level      Level     `json:"level"`
	Path       string    `json:"path"`
	Model      string    `json:"model"`
	Format     string    `json:"format"` // "openai" | "anthropic" | ""
	Status     int       `json:"status"`
	DurationMs int64     `json:"durationMs"`
	TokensIn   int       `json:"tokensIn"`
	TokensOut  int       `json:"tokensOut"`
	CacheRead  int       `json:"cacheRead"`
	CacheWrite int       `json:"cacheWrite"`
	Error      string    `json:"error"`
}

// Store is a mutex-guarded ring buffer of the most recent entries.
type Store struct {
	mu       sync.Mutex
	entries  []Entry
	next     int
	capacity int
	full     bool
}

// NewStore returns a store holding up to capacity entries (<= 0 -> 500).
func NewStore(capacity int) *Store {
	if capacity <= 0 {
		capacity = 500
	}
	return &Store{entries: make([]Entry, capacity), capacity: capacity}
}

// Add appends an entry, dropping the oldest when the ring is full.
func (s *Store) Add(e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[s.next] = e
	s.next = (s.next + 1) % s.capacity
	if s.next == 0 {
		s.full = true
	}
}

// List returns a copy of the stored entries, newest first. Mutating the
// returned slice never affects the store.
func (s *Store) List() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, s.lenLocked())
	for i := 0; i < s.lenLocked(); i++ {
		out = append(out, s.entries[(s.next-1-i+s.capacity)%s.capacity])
	}
	return out
}

// lenLocked is the current entry count; caller holds s.mu.
func (s *Store) lenLocked() int {
	if s.full {
		return s.capacity
	}
	return s.next
}

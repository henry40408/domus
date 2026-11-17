package auth

import (
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	// freeAttempts is the number of failures allowed before the first lock.
	freeAttempts = 5
	baseLockSecs = 60
	maxLockSecs  = 900
	// maxEntries bounds the map: past it, idle entries are dropped on the
	// next failure.
	maxEntries = 10_000
	// keyNameRunes bounds the name part of a key so junk names cannot bloat
	// the map.
	keyNameRunes = 64
	maxDoublings = 4
)

type entry struct {
	failures    int
	lockedUntil int64
	lastFailure int64
}

// LoginGuard is in-memory login throttling: exponential back-off per key,
// never a permanent lock. The zero value is ready to use.
type LoginGuard struct {
	mu      sync.Mutex
	entries map[string]*entry
}

// Check returns the seconds to wait before key may try again, or 0 if it may
// try now.
func (g *LoginGuard) Check(key string, now int64) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.entries[key]
	if !ok {
		return 0
	}
	return max(e.lockedUntil-now, 0)
}

// Fail records a failed attempt. From the fifth in a row the lock doubles each
// time, from 1 minute up to 15.
func (g *LoginGuard) Fail(key string, now int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.entries == nil {
		g.entries = make(map[string]*entry)
	}
	if len(g.entries) >= maxEntries {
		g.pruneIdle(now)
	}
	e, ok := g.entries[key]
	if !ok {
		e = &entry{lastFailure: now}
		g.entries[key] = e
	}
	// Strictly greater: at exactly the limit the old failures still count.
	if now-e.lastFailure > maxLockSecs {
		e.failures = 0
	}
	e.failures++
	e.lastFailure = now
	if e.failures < freeAttempts {
		return
	}
	doublings := min(e.failures-freeAttempts, maxDoublings)
	e.lockedUntil = now + min(int64(baseLockSecs)<<doublings, maxLockSecs)
}

// Succeed forgets key's failures.
func (g *LoginGuard) Succeed(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.entries, key)
}

func (g *LoginGuard) pruneIdle(now int64) {
	for key, e := range g.entries {
		if e.lockedUntil <= now && now-e.lastFailure >= maxLockSecs {
			delete(g.entries, key)
		}
	}
}

// ThrottleKey is the normalised key for scope and name: case-insensitive and
// bounded. Go's ToLower and Rust's to_lowercase differ for a few special
// characters (final sigma); a different key only changes which attempts share
// a counter, so the difference is accepted.
func ThrottleKey(scope, name string) string {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > keyNameRunes {
		name = string([]rune(name)[:keyNameRunes])
	}
	return scope + ":" + strings.ToLower(name)
}

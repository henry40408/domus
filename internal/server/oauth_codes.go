package server

import (
	"sync"
	"time"

	"github.com/henry40408/domus/internal/auth"
)

const (
	// codeTTLSecs is the lifetime of an authorization code.
	codeTTLSecs = 60
	// maxCodes is how many pending codes the table may hold before expired
	// ones are dropped on the next insert.
	maxCodes = 1000
)

type pendingCode struct {
	userID   int64
	clientID string
	expires  int64
}

// authCodes holds one-time authorization codes in memory only: they live for
// a minute. Keys are the SHA-256 of the code.
type authCodes struct {
	mu    sync.Mutex
	now   func() time.Time
	codes map[string]pendingCode
}

func newAuthCodes(now func() time.Time) *authCodes {
	return &authCodes{now: now, codes: map[string]pendingCode{}}
}

// issue stores a new code for userID, usable only by clientID.
func (c *authCodes) issue(userID int64, clientID string) string {
	code := auth.RandomHex(16)
	now := c.now().Unix()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.codes) >= maxCodes {
		for key, pending := range c.codes {
			if pending.expires <= now {
				delete(c.codes, key)
			}
		}
	}
	c.codes[auth.SHA256Hex(code)] = pendingCode{userID: userID, clientID: clientID, expires: now + codeTTLSecs}
	return code
}

// redeem consumes code: it works once, and only for the client it was issued
// to. A guess with the wrong client burns the code as well.
func (c *authCodes) redeem(code, clientID string) (userID int64, ok bool) {
	key := auth.SHA256Hex(code)
	c.mu.Lock()
	pending, found := c.codes[key]
	delete(c.codes, key)
	c.mu.Unlock()
	if !found {
		return 0, false
	}
	if pending.expires <= c.now().Unix() || pending.clientID != clientID {
		return 0, false
	}
	return pending.userID, true
}

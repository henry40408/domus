// Package auth holds the credential primitives: randomness, hashing, password
// storage, login throttling and the username and password rules.
package auth

import (
	"crypto/rand"
	"encoding/hex"
)

// base32Alphabet is Crockford base32 without I, L, O, U: nothing that is
// easily misread when typed from a log.
const base32Alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// RandomBytes returns n bytes from the system CSPRNG.
func RandomBytes(n int) []byte {
	b := make([]byte, n)
	// Since Go 1.24 rand.Read never returns an error; it crashes the program
	// instead when the system source fails.
	_, _ = rand.Read(b)
	return b
}

// RandomHex returns 2*n hex characters.
func RandomHex(n int) string {
	return hex.EncodeToString(RandomBytes(n))
}

// RandomBase32 returns chars random characters, 5 bits of entropy each.
func RandomBase32(chars int) string {
	out := make([]byte, chars)
	for i, b := range RandomBytes(chars) {
		out[i] = base32Alphabet[b&0x1f]
	}
	return string(out)
}

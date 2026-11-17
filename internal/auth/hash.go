package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
)

// SHA256Hex returns the lowercase hex SHA-256 of the UTF-8 string.
func SHA256Hex(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}

// ConstantTimeEqual compares in time that depends only on the length. Slices
// of different length are never equal.
func ConstantTimeEqual(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

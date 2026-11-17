package auth

import (
	"encoding/hex"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Argon2id v0x13 with the argon2 crate's default parameters. Changing any of
// them would stop every stored password from verifying.
const (
	argonTime    = 2
	argonMemory  = 19456 // KiB
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16
)

// dummyHash is computed on first use so the unknown-user path costs the same
// argon2 time as a real check.
var dummyHash = sync.OnceValue(func() string {
	return HashPassword("domus dummy password")
})

func derive(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
}

// HashPassword returns "<salt hex>$<argon2id hash hex>". This is not a PHC
// string; the format is shared with the Rust server's stored hashes.
func HashPassword(password string) string {
	salt := RandomBytes(saltLen)
	return hex.EncodeToString(salt) + "$" + hex.EncodeToString(derive(password, salt))
}

// VerifyPassword reports whether password matches a hash made by HashPassword.
func VerifyPassword(password, stored string) bool {
	saltHex, hashHex, found := strings.Cut(stored, "$")
	if !found {
		return false
	}
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return false
	}
	got := hex.EncodeToString(derive(password, salt))
	return ConstantTimeEqual([]byte(got), []byte(hashHex))
}

// VerifyDummy burns the same argon2 time as a real check, for usernames that
// do not exist, so response time does not reveal which usernames are valid.
func VerifyDummy(password string) {
	VerifyPassword(password, dummyHash())
}

// WarmDummy computes the dummy hash up front so the first unknown-user login
// is not faster than later ones.
func WarmDummy() {
	VerifyDummy("")
}

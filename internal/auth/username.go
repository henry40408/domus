package auth

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	minPasswordLen = 12
	maxPasswordLen = 128
	maxUsernameLen = 64

	// UsernameHint and PasswordHint are shown to users by the web app, so
	// their wording is part of the API.
	UsernameHint = "Username must be 1 to 64 characters and must not contain control characters."
	PasswordHint = "Password must be 12 to 128 characters and must not be the username."
)

// ValidUsername expects the already trimmed name. Length counts characters,
// not bytes, so non-ASCII names are not penalised; control characters would
// forge audit log lines.
func ValidUsername(name string) bool {
	if name == "" || utf8.RuneCountInString(name) > maxUsernameLen {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ValidPassword counts characters, not bytes, and refuses the username in any
// case.
func ValidPassword(password, username string) bool {
	n := utf8.RuneCountInString(password)
	if n < minPasswordLen || n > maxPasswordLen {
		return false
	}
	return !strings.EqualFold(password, username)
}

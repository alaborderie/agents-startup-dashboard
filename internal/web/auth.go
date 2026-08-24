package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"strings"
)

func validBearer(header, expected string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	providedHash := sha256.Sum256([]byte(strings.TrimPrefix(header, prefix)))
	expectedHash := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(providedHash[:], expectedHash[:]) == 1
}

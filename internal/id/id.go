// Package id creates opaque identifiers for persisted domain objects.
package id

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
)

var encoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// New returns an opaque identifier with the supplied human-readable prefix.
func New(prefix string) (string, error) {
	if prefix == "" || strings.Contains(prefix, "_") {
		return "", fmt.Errorf("invalid ID prefix %q", prefix)
	}

	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("read random ID bytes: %w", err)
	}
	return prefix + "_" + strings.ToLower(encoding.EncodeToString(random)), nil
}

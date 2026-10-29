//go:build !windows

package config

// Secrets are stored as-is outside Windows (development and tests only);
// the file itself is written with 0600 permissions.
func seal(v string) string   { return v }
func unseal(v string) string { return v }

//go:build !windows

package install

// Running is only meaningful on Windows; elsewhere installs are test fixtures.
func Running(string) bool { return false }

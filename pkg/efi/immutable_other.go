//go:build !linux

package efi

// clearImmutable is a no-op where efivarfs does not exist.
func clearImmutable(string) (func(), error) { return func() {}, nil }

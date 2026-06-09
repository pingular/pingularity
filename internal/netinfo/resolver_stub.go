//go:build !linux && !darwin && !windows

package netinfo

// rawResolvers has no implementation for this platform (the *BSDs, plan9, ...).
// Returning nil means "unavailable"; the DNS panel falls back to the probe-based
// resolver discovery, same as when the files/APIs yield nothing.
func rawResolvers() []string { return nil }

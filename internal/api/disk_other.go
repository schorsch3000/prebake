//go:build !unix

package api

// fsSpace is unknown here (the daemon runs on Linux; this only keeps the CLI buildable elsewhere).
func fsSpace(string) (free, total uint64) { return 0, 0 }

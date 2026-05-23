//go:build !windows

package main

func isDexgramCodexAppServerProcess(_ int, _ string) bool {
	return false
}

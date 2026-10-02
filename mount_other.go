//go:build !windows

package main

import "log"

// mount is Windows-only; elsewhere the volume list main already printed is
// the whole output. Use --walk to read every file.
func mount(vols []volume, debug bool) error {
	log.Printf("not on Windows: %d volume(s) detected, nothing mounted (try --walk)", len(vols))
	return nil
}

func sysInfo(fileInfo) any { return nil }

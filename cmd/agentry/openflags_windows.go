//go:build windows

package main

// openNoFollow is nothing on Windows, which has no such flag; the runhost's
// job files it guards are written on Linux alone.
const openNoFollow = 0

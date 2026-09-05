//go:build malgo

package main

// Side-effect import: registers the miniaudio (cgo) audio backend.
import _ "remote-au/internal/audio/malgo"

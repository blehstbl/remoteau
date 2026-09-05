//go:build windows

package main

// Side-effect import: registers the pure-Go WASAPI audio backend.
import _ "remote-au/internal/audio/wasapi"

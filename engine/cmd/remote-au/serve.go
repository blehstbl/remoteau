package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/skip2/go-qrcode"

	"remote-au/internal/audio"
	"remote-au/internal/engine"
	"remote-au/internal/logging"
	"remote-au/internal/pairing"
	"remote-au/internal/profiles"
	"remote-au/internal/protocol/v2"
	"remote-au/internal/relay"
	"remote-au/internal/transport"
	"remote-au/internal/transport/v2"
)

// runServe hosts the v2 engine: QUIC + datagrams, pairing, discovery.
func runServe(args []string, stdout, stderr io.Writer, backend audio.Backend, format audio.Format, logger logging.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fmt.Sprintf(":%d", 47010)
	sourceName := "loopback"
	deviceSelector := ""
	name := defaultHostname()
	fs.StringVar(&addr, "addr", addr, "v2 listen address")
	fs.StringVar(&sourceName, "source", sourceName, "capture source: mic or loopback")
	fs.StringVar(&deviceSelector, "device", deviceSelector, "capture device selector (see devices)")
	fs.StringVar(&name, "name", name, "host name shown to receivers")
	recordDir := ""
	profileName := ""
	relayAddr := ""
	toneModeName := "sine"
	fs.StringVar(&recordDir, "record", recordDir, "record captured audio to WAV files in this directory")
	fs.StringVar(&profileName, "profile", profileName, "apply a named profile (see profiles)")
	fs.StringVar(&relayAddr, "relay", relayAddr, "register with this relay for WAN access")
	fs.StringVar(&toneModeName, "tone-mode", toneModeName, "test-tone pattern: sine or click (with --source testtone)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("serve takes no positional arguments: %v", fs.Args())
	}

	source, err := parseCaptureSource(sourceName)
	if err != nil {
		return err
	}
	var toneMode audio.ToneMode
	if source == audio.SourceTestTone {
		switch strings.ToLower(toneModeName) {
		case "click", "impulse":
			toneMode = audio.ToneClick
		default:
			toneMode = audio.ToneSine
		}
	}

	store, err := pairing.NewWindowsStore()
	if err != nil {
		return fmt.Errorf("open trust store: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pairingCode := func(code string) {
		fmt.Fprintf(stdout, "\n*** PAIRING: enter code %s on the device ***\n\n", code)
	}
	pairingInfo := func(code, url string) {
		fmt.Fprintf(stdout, "\n*** PAIRING ***\n")
		fmt.Fprintf(stdout, "Code: %s\n", code)
		fmt.Fprintf(stdout, "URL:  %s\n", url)
		if qr, qerr := qrcode.New(url, qrcode.Medium); qerr == nil {
			fmt.Fprintln(stdout, qr.ToSmallString(false))
		}
		fmt.Fprintln(stdout, "Scan the QR in the RemoteAU app, or type the code.")
	}

	// Profile (Phase 14): presets sync naturally with the GUIs.
	if profileName != "" {
		storeDir, _ := os.UserConfigDir()
		ps, perr := profiles.NewStore(filepath.Join(storeDir, "RemoteAU"))
		if perr == nil {
			list, lerr := ps.List()
			if lerr == nil {
				if p, ok := profiles.ByName(list, profileName); ok {
					logger.Infof("profile %q: preset=%s target=%dms", p.Name, p.Preset, p.TargetMs)
				} else {
					return fmt.Errorf("profile %q not found", profileName)
				}
			}
		}
	}

	host, err := engine.NewHost(engine.HostOptions{
		Name:           name,
		Store:          store,
		Backend:        backend,
		ListenAddr:     addr,
		CaptureSource:  source,
		DeviceSelector: deviceSelector,
		Format:         format,
		OnPairingCode:  pairingCode,
		OnPairingInfo:  pairingInfo,
		RecordDir:      recordDir,
		RelayAddr:      relayAddr,
		ToneMode:       toneMode,
		Logger:         logger,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "serve: %s, %s, listening on %s\n", format, sourceName, addr)
	if recordDir != "" {
		fmt.Fprintf(stdout, "recording to %s\n", recordDir)
	}
	fmt.Fprintln(stdout, "Press Ctrl-C to stop.")
	return host.Run(ctx)
}

// runRecv2 is the v2 test client (desktop): connects to a v2 host, pairs,
// and plays the received stream locally.
func runRecv2(args []string, stdout, stderr io.Writer, backend audio.Backend, format audio.Format, logger logging.Logger) error {
	fs := flag.NewFlagSet("recv2", flag.ContinueOnError)
	fs.SetOutput(stderr)
	hostAddr := ""
	name := defaultHostname()
	playbackSelector := ""
	fs.StringVar(&hostAddr, "to", hostAddr, "host address, e.g. 192.168.1.10:47010")
	fs.StringVar(&name, "name", name, "client name shown to the host")
	fs.StringVar(&playbackSelector, "device", playbackSelector, "playback device selector (see devices)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("recv2 takes no positional arguments: %v", fs.Args())
	}
	if hostAddr == "" {
		return fmt.Errorf("recv2 requires --to <host:port>")
	}

	store, err := pairing.NewWindowsStore()
	if err != nil {
		return fmt.Errorf("open trust store: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Jitter playback: pull from a bounded ring fed by OnMedia.
	ring := audio.NewRing(48_000*4, 4) // 500ms @48k stereo
	pull := func(out []byte, frames uint32) int {
		n := ring.Read(out)
		if n < len(out) {
			clear(out[n:])
			n = len(out)
		}
		return n
	}

	playbackOpts := audio.PlaybackOptions{Format: format, Pull: pull}
	if playbackSelector != "" {
		playbackOpts.DeviceSelector = playbackSelector
	}
	playback, err := backend.OpenPlayback(playbackOpts)
	if err != nil {
		return err
	}
	defer func() { _ = playback.Close() }()
	if err := playback.Start(); err != nil {
		return err
	}

	client, err := engine.NewClient(engine.ClientOptions{
		HostAddr: hostAddr,
		Name:     name,
		Store:    store,
		PINProvider: func() (string, error) {
			fmt.Fprint(stdout, "Enter the pairing code shown on the PC: ")
			var code string
			if _, err := fmt.Fscanln(os.Stdin, &code); err != nil {
				return "", err
			}
			return code, nil
		},
		OnMedia: func(pcm []byte) {
			ring.TryWrite(pcm)
		},
		RequestedCaps: protocolv2.Caps{
			Codec:      protocolv2.CodecPCMS16LE,
			SampleRate: uint32(format.Rate),
			Channels:   uint8(format.Channels),
			FrameMs:    5,
		},
		Logger: logger,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "recv2: connecting to %s\n", hostAddr)
	return client.Run(ctx)
}

// runRelay is the Phase 12 relay process: splices phone↔PC QUIC connections
// without interpreting application data (payloads are sealed end-to-end).
func runRelay(args []string, stdout, stderr io.Writer, backend audio.Backend, format audio.Format, logger logging.Logger) error {
	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := ":47020"
	logLevel := "info"
	fs.StringVar(&addr, "addr", addr, "relay listen address")
	fs.StringVar(&logLevel, "log-level", logLevel, "relay log level")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("relay takes no positional arguments: %v", fs.Args())
	}

	// The relay uses its own throwaway identity; it never sees plaintext.
	id, err := pairing.NewIdentity()
	if err != nil {
		return err
	}
	cert, err := transportv2.DeviceCertificate(id)
	if err != nil {
		return err
	}
	tlsCfg := transportv2.TLSConfig(cert, transportv2.FingerprintVerifier(nil, true))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	l := func(line string) { logger.Infof("relay: %s", line) }
	fmt.Fprintf(stdout, "relay listening on %s\n", addr)
	return relay.RunRelay(ctx, addr, tlsCfg, l)
}

var _ = transport.TransportUDP
var _ = time.Second

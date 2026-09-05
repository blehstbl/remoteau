# RemoteAU WAN mode (Phase 12 — design)

LAN quality comes first. WAN support must not reshape the LAN architecture;
it rides on the same v2 stack.

## Preferred direction

1. **Direct secure QUIC when reachable.** The v2 host already speaks QUIC;
   if the phone can reach `host:47010` (port forward, VPN like Tailscale/
   WireGuard, IPv6), everything works unchanged — pairing, stats, resume
   included. Document the port; no new code path.

2. **Secure relay fallback.** A small `remote-au relay` process:

   ```
   phone ──QUIC──► relay ◄──QUIC── pc
   ```

   - The relay terminates TLS both ways but only ever sees ciphertext-inside-
     application: the phone re-encrypts media with the **pairing secret**
     (AES-256-GCM, keys already shared during pairing) before handing frames
     to the relay. The relay cannot decode audio it cannot authenticate.
   - Control messages: same rule — the session payload carries an inner
     AEAD envelope keyed from the pairing secret.
   - Enrollment: PC publishes a signed rendezvous record (deviceID → host
     endpoint) to the relay; the phone looks it up by deviceID. Discovery
     still never conveys trust — the fingerprint + pairing handshake do.

3. **Direct NAT traversal (optional later).** UDP hole punching coordinated
   through the same relay (STUN-lite): exchange candidate address sets over
   the encrypted control channel, race direct vs relayed paths, prefer
   direct. This is ICE-like *without* WebRTC; it reuses the QUIC transport.

## Non-goals

- No WebRTC/SDP/ICE stack (kept out per plan).
- No media relay trust: relays are dumb byte forwarders for encrypted
  envelopes.

## Implementation checklist

- [ ] `internal/transport/v2`: pluggable `Dial` target already abstracts the
      address — add `RelayDial(relayAddr, hostDeviceID)` that performs the
      rendezvous + inner AEAD wrapping.
- [ ] `cmd/remote-au relay`: relay process (accept two QUIC conns, splice
      control streams + datagram pipes; zero media decryption).
- [ ] Session-layer envelope codec using the stored pairing secret.
- [ ] UI: host discovery gains a "via relay" state; latency stats label the
      path.

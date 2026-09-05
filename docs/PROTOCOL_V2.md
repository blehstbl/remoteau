# RemoteAU Protocol v2 ("RAU2")

v1 (stock remote-au: TCP `RAU1` / UDP `RAUU`) remains available as **legacy
mode**. v2 is a separate protocol; nothing in v1 changes.

## Goals

- negotiated capabilities (codec, rate, channels, frame duration, FEC, DTX…)
- encrypted by default, authenticated via paired device identity
- reliable ordered control channel + unreliable media datagrams (no
  head-of-line blocking on audio)
- receiver feedback (stats) so the sender can adapt
- resume after transient drops without a full re-handshake

## Transport

QUIC (TLS 1.3) with QUIC DATAGRAM extension (RFC 9221):

- **Control**: one bidirectional QUIC stream. Small JSON-less binary messages,
  little state machine, both roles send.
- **Media**: QUIC datagrams, one datagram = one media packet ≤ ~1200 B
  (QUIC-safe MTU), never retransmitted.
- Identity: each device holds a long-term ECDSA P-256 identity key. The
  sender presents a certificate chain (`rau2` leaf, self-signed, stable
  fingerprint); the receiver only trusts fingerprints it paired with (or
  explicitly accepts in legacy/trust-first mode).

Fallback transport (same messages, different carrier): UDP with per-packet
AES-256-GCM using keys derived from the pairing secret. Selected during
capability exchange so an iOS build without QUIC datagram support can still do
v2 securely.

## Control messages (little-endian)

Framing: `msgType:u16 | flags:u16 | requestId:u32 | payloadLen:u32 | payload`

| Type | Name | Direction | Payload |
|---|---|---|---|
| 0x0001 | HELLO | both | `{protoVersion:u16, minProto:u16, deviceName:str16, deviceId:16B, caps:Caps}` |
| 0x0002 | HELLO_OK | both | chosen `{profile}` + `resumeToken:32B` |
| 0x0010 | PAIR_BEGIN | recv→send | `{nonce:16B}` |
| 0x0011 | PAIR_CHALLENGE | send→recv | `{ecdhPub:65B, salt:16B, verify:32B}` |
| 0x0012 | PAIR_CONFIRM | recv→send | `{ecdhPub:65B, verify:32B}` |
| 0x0013 | PAIR_RESULT | both | `{ok:u8, peerName:str16, peerId:16B}` |
| 0x0020 | STREAM_START | recv→send | `{request:Caps}` |
| 0x0021 | STREAM_ACK | send→recv | `{active:Caps, formatGen:u32}` |
| 0x0022 | STREAM_STOP | recv→send | `{}` |
| 0x0030 | FORMAT_UPDATE | send→recv | `{formatGen:u32, caps:Caps}` (codec/quality change) |
| 0x0031 | FORMAT_ACK | recv→send | `{formatGen:u32, applied:u8}` |
| 0x0040 | STATS | recv→send | `{loss%, late%, jitterUs:u32, rttUs:u32, bufDepthMs:u16, underruns:u32, driftPpm:i32}` |
| 0x0041 | STATS_ACK | send→recv | `{}` |
| 0x0050 | VOLUME | recv→send | `{volumeQ16:u16, mute:u8}` |
| 0x0060 | PING/PONG | both | `{clientTimeUs:u64}` (echoed) |
| 0x0070 | RESUME | recv→send | `{resumeToken:32B, lastSeq:u64}` |
| 0x0071 | RESUME_OK | send→recv | `{nextSeq:u64, formatGen:u32}` |

`Caps` (also used inside HELLO / STREAM_START / STREAM_ACK):

```
codec:u8        0=PCM_S16LE 1=Opus
sampleRate:u32  channels:u8   frameMs:u8 (2,5,10,20)
opusBitrate:u32 fec:u8 (0/1)  dtx:u8 (0/1)
complexity:u8   appId:u8 (0=audio 1=voip 2=lowdelay)
```

Sender HELLO lists what it can produce; STREAM_START picks one; STREAM_ACK
confirms the active format and bumps `formatGen`. Media datagrams carry the
`formatGen` so the receiver can switch decoders atomically.

## Media datagram (unreliable)

```
magic:4 "RAU2" | version:1 | type:1 (=1 audio) | streamId:u8 |
formatGen:u16 | flags:u8 (bit0= FEC-eligible, bit1 = DTX-silence) |
seq:u32 | captureTsUs:u40 (mod 2^40) | payloadLen:u16 | payload
```

- seq is per-stream, wraps, u32 is plenty (2^32 × 5 ms ≈ 248 days).
- captureTsUs is the sender's capture-clock timestamp of the first frame in
  the payload (drift correction references this, not the wall clock).
- Payload = codec frame (PCM S16LE interleaved, or one Opus packet).

## Security

- Pairing (first use): sender displays 6-digit code; control channel runs
  PAIR_BEGIN/CHALLENGE/CONFIRM: ECDH P-256 with both sides proving knowledge
  of the code via HKDF-SHA256(salt, ECDH-secret, "rau2-pin-verify") compared
  constant-time; transcript is mixed into the HKDF info so the code check
  binds the full exchange. Result: `pairingSecret` (32 B).
- Persistent trust: both sides generate/keep `deviceId:u16-byte` +
  identity key; they store (peerId, peerName, pairingSecret, senderCertFp).
  Subsequent connections: sender cert fingerprint must match the stored one;
  control channel is protected with AEAD keys derived from
  HKDF(pairingSecret, both deviceIds, "rau2-transport").
- Keychain (iOS) / DPAPI (Windows) storage of all secrets.

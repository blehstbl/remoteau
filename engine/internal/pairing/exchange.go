package pairing

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/big"

	"golang.org/x/crypto/hkdf"
)

// Pairing exchange (mutual PIN authentication, transcript-bound):
//
//	receiver → PAIR_BEGIN   { nonce16 }
//	sender   → PAIR_CHALLENGE { pubS65, salt16 }
//	receiver → PAIR_CONFIRM { pubR65, tagR32 }   // proves receiver knows PIN
//	sender   → PAIR_RESULT  { ok, tagS32, name, peerID } // proves sender knows PIN
//
// tagR/tagS are HMAC-SHA256 over the transcript with keys derived from the
// ECDH shared point, the salt and the PIN. Comparison is constant-time. On
// success both sides derive the same 32-byte pairing secret:
//
//	pairingSecret = HKDF-SHA256(sharedX, salt, "rau2-pairing-secret-v1")
const (
	pinVerifyInfo  = "rau2-pin-verify-v1"
	pairSecretInfo = "rau2-pairing-secret-v1"

	tagLen    = 32
	secretLen = 32
	pubLen    = 65
	saltLen   = 16
	nonceLen  = 16
)

// ErrPinMismatch is returned when the PINs did not match.
var ErrPinMismatch = errors.New("pairing pin mismatch")

// Role selects which side of the exchange this instance plays.
type Role int

const (
	RoleSender   Role = iota // the PC: displays the code
	RoleReceiver             // the iPhone: user enters/confirms the code
)

// Exchange runs one side of a pairing exchange.
type Exchange struct {
	role      Role
	pin       string
	localName string

	ephemeral *ecdsa.PrivateKey

	peerX, peerY *big.Int
	salt         [saltLen]byte
	nonce        [nonceLen]byte

	tr     []byte // transcript
	secret []byte
}

func NewExchange(role Role, pin, localName string) (*Exchange, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("pairing ecdh: %w", err)
	}
	e := &Exchange{role: role, pin: pin, localName: localName, ephemeral: key}
	if _, err := rand.Read(e.salt[:]); err != nil {
		return nil, err
	}
	return e, nil
}

// Secret returns the derived pairing secret after a successful exchange.
func (e *Exchange) Secret() []byte { return e.secret }

// SetPIN sets (or replaces) the PIN; the receiver obtains it from the user
// after initiating the exchange.
func (e *Exchange) SetPIN(pin string) {
	if len(pin) >= 4 {
		e.pin = pin
	}
}

func (e *Exchange) transcriptAdd(parts ...[]byte) {
	for _, p := range parts {
		e.tr = append(e.tr, p...)
	}
}

func (e *Exchange) transcriptHash() []byte {
	sum := sha256.Sum256(e.tr)
	return sum[:]
}

func (e *Exchange) localPub() []byte {
	return elliptic.Marshal(elliptic.P256(), e.ephemeral.PublicKey.X, e.ephemeral.PublicKey.Y)
}

// sharedAndKeys computes the ECDH shared point and derives (secret, macKey).
func (e *Exchange) sharedAndKeys() (secret, macKey []byte, err error) {
	if e.peerX == nil {
		return nil, nil, errors.New("pairing: peer key not set")
	}
	sx, _ := elliptic.P256().ScalarMult(e.peerX, e.peerY, e.ephemeral.D.Bytes())
	if sx == nil {
		return nil, nil, errors.New("pairing: ecdh failed")
	}

	ikm := make([]byte, 0, len(sx.Bytes())+32)
	ikm = append(ikm, sx.Bytes()...)
	ikm = append(ikm, e.transcriptHash()...)

	secret = make([]byte, secretLen)
	if _, err := io.ReadFull(hkdf.New(sha256.New, ikm, e.salt[:], []byte(pairSecretInfo)), secret); err != nil {
		return nil, nil, err
	}
	macKey = make([]byte, tagLen)
	if _, err := io.ReadFull(hkdf.New(sha256.New, ikm, e.salt[:], []byte(pinVerifyInfo+"|"+e.pin)), macKey); err != nil {
		return nil, nil, err
	}
	return secret, macKey, nil
}

// BeginMessage (receiver → sender): PAIR_BEGIN payload.
func (e *Exchange) BeginMessage() ([]byte, error) {
	if e.role != RoleReceiver {
		return nil, errors.New("only the receiver begins pairing")
	}
	if _, err := rand.Read(e.nonce[:]); err != nil {
		return nil, err
	}
	e.transcriptAdd([]byte("begin"), e.nonce[:])
	out := make([]byte, nonceLen)
	copy(out, e.nonce[:])
	return out, nil
}

// OnBegin (sender): consumes PAIR_BEGIN.
func (e *Exchange) OnBegin(payload []byte) error {
	if e.role != RoleSender {
		return errors.New("only the sender consumes begin")
	}
	if len(payload) != nonceLen {
		return errors.New("pair begin payload malformed")
	}
	copy(e.nonce[:], payload)
	e.transcriptAdd([]byte("begin"), e.nonce[:])
	return nil
}

// ChallengeMessage (sender → receiver): PAIR_CHALLENGE payload.
func (e *Exchange) ChallengeMessage() ([]byte, error) {
	if e.role != RoleSender {
		return nil, errors.New("only the sender sends the challenge")
	}
	pub := e.localPub()
	e.transcriptAdd([]byte("challenge"), pub, e.salt[:])

	out := make([]byte, 0, pubLen+saltLen)
	out = append(out, pub...)
	out = append(out, e.salt[:]...)
	return out, nil
}

// OnChallenge (receiver): consumes PAIR_CHALLENGE.
func (e *Exchange) OnChallenge(payload []byte) error {
	if e.role != RoleReceiver {
		return errors.New("only the receiver consumes the challenge")
	}
	if len(payload) != pubLen+saltLen {
		return errors.New("pair challenge payload malformed")
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), payload[:pubLen])
	if x == nil {
		return errors.New("invalid challenge public key")
	}
	e.peerX, e.peerY = x, y
	copy(e.salt[:], payload[pubLen:])
	e.transcriptAdd([]byte("challenge"), payload[:pubLen], payload[pubLen:])
	return nil
}

// ConfirmMessage (receiver → sender): PAIR_CONFIRM payload with tagR.
func (e *Exchange) ConfirmMessage() ([]byte, error) {
	if e.role != RoleReceiver {
		return nil, errors.New("only the receiver sends confirm")
	}
	if e.peerX == nil {
		return nil, errors.New("confirm before challenge")
	}
	pub := e.localPub()
	e.transcriptAdd([]byte("confirm"), pub)

	_, macKey, err := e.sharedAndKeys()
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, macKey)
	mac.Write(e.transcriptHash())
	mac.Write([]byte("R"))

	out := make([]byte, 0, pubLen+tagLen)
	out = append(out, pub...)
	out = append(out, mac.Sum(nil)...)
	return out, nil
}

// OnConfirm (sender): consumes PAIR_CONFIRM, verifies the receiver's PIN.
func (e *Exchange) OnConfirm(payload []byte) error {
	if e.role != RoleSender {
		return errors.New("only the sender consumes confirm")
	}
	if len(payload) != pubLen+tagLen {
		return errors.New("pair confirm payload malformed")
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), payload[:pubLen])
	if x == nil {
		return errors.New("invalid confirm public key")
	}
	e.transcriptAdd([]byte("confirm"), payload[:pubLen])

	_, macKey, err := e.sharedAndKeysWithPeer(x, y)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, macKey)
	mac.Write(e.transcriptHash())
	mac.Write([]byte("R"))
	if !hmac.Equal(mac.Sum(nil), payload[pubLen:]) {
		return ErrPinMismatch
	}
	e.peerX, e.peerY = x, y
	return nil
}

// ResultMessage (sender → receiver): PAIR_RESULT payload with tagS.
// peerID is the sender's stable device ID.
func (e *Exchange) ResultMessage(peerID [16]byte) ([]byte, error) {
	secret, macKey, err := e.sharedAndKeys()
	if err != nil {
		return nil, err
	}
	e.secret = secret

	mac := hmac.New(sha256.New, macKey)
	mac.Write(e.transcriptHash())
	mac.Write([]byte("S"))

	name := []byte(e.localName)
	if len(name) > 255 {
		name = name[:255]
	}
	out := make([]byte, 0, 1+tagLen+2+len(name)+16)
	out = append(out, 1)
	out = append(out, mac.Sum(nil)...)
	out = append(out, byte(len(name)>>8), byte(len(name)))
	out = append(out, name...)
	out = append(out, peerID[:]...)
	return out, nil
}

// OnResult (receiver): consumes PAIR_RESULT, verifies the sender's PIN and
// derives the pairing secret. Returns the sender's name and device ID.
func (e *Exchange) OnResult(payload []byte) (peerName string, peerID [16]byte, err error) {
	if len(payload) < 1+tagLen+2+16 {
		return "", peerID, errors.New("pair result payload malformed")
	}
	if payload[0] != 1 {
		return "", peerID, ErrPinMismatch
	}
	tag := payload[1 : 1+tagLen]
	nameLen := int(payload[1+tagLen])<<8 | int(payload[2+tagLen])
	rest := payload[3+tagLen:]
	if len(rest) != nameLen+16 {
		return "", peerID, errors.New("pair result length mismatch")
	}
	peerName = string(rest[:nameLen])
	copy(peerID[:], rest[nameLen:])

	secret, macKey, err := e.sharedAndKeys()
	if err != nil {
		return "", peerID, err
	}
	mac := hmac.New(sha256.New, macKey)
	mac.Write(e.transcriptHash())
	mac.Write([]byte("S"))
	if !hmac.Equal(mac.Sum(nil), tag) {
		return "", peerID, ErrPinMismatch
	}
	e.secret = secret
	return peerName, peerID, nil
}

// sharedAndKeysWithPeer computes keys with an explicit peer key (used by the
// sender at confirm time, before storing the peer key).
func (e *Exchange) sharedAndKeysWithPeer(x, y *big.Int) ([]byte, []byte, error) {
	oldX, oldY := e.peerX, e.peerY
	e.peerX, e.peerY = x, y
	defer func() { e.peerX, e.peerY = oldX, oldY }()
	return e.sharedAndKeys()
}

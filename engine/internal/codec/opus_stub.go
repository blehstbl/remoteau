//go:build !opus

package codec

import "remote-au/internal/logging"

// opusAvailable is false without the opus build tag.
const opusAvailable = false

// newOpusCodec is only available with the opus build tag (libopus via cgo).
func newOpusCodec(cfg Config, logger logging.Logger) (Codec, error) {
	return nil, ErrOpusUnavailable
}

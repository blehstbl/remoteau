package discovery

import (
	"crypto/rand"
	"fmt"
)

// NewInstanceID generates a random 16-byte discovery instance identifier.
func NewInstanceID() (InstanceID, error) {
	var id InstanceID
	if _, err := rand.Read(id[:]); err != nil {
		return id, fmt.Errorf("generate discovery instance id: %w", err)
	}
	return id, nil
}

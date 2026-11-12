package store

import (
	"crypto/rand"
	"encoding/hex"
)

// newID produces an opaque configuration item identifier.
//
// It is deliberately not derived from a hostname, an address or a serial.
// Every one of those changes during the life of a machine, and a key that
// changes is a key that forks one machine into two records.
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("kryostate: no entropy available for identifiers: " + err.Error())
	}
	return hex.EncodeToString(b)
}

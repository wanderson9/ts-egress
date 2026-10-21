package gateway

import (
	"crypto/rand"
	"encoding/binary"
)

// NewRevision starts each process/configuration lifetime in a fresh namespace.
// Keep values exactly representable by JavaScript numbers, including increments.
func NewRevision() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return (binary.LittleEndian.Uint64(b[:]) & ((1 << 52) - 1)) + 1
}

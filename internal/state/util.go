package state

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}
func nowISO() string { return time.Now().UTC().Format(time.RFC3339Nano) }

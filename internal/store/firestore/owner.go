package firestore

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"
)

// newOwner identifies one lease holder. The hostname makes a stuck lock
// traceable to a pod; the random suffix keeps two leases from the same pod
// distinct.
func newOwner() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}

	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s-%d", host, time.Now().UnixNano())
	}
	return host + "-" + hex.EncodeToString(b[:])
}

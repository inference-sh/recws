package recws

import (
	"sync"
	"time"
)

// keepAliveResponse records when the peer last showed it was alive: a pong,
// or any message read through RecConn.
type keepAliveResponse struct {
	lastResponse time.Time
	sync.RWMutex
}

func (k *keepAliveResponse) setLastResponse() {
	k.Lock()
	defer k.Unlock()

	k.lastResponse = time.Now()
}

func (k *keepAliveResponse) getLastResponse() time.Time {
	k.RLock()
	defer k.RUnlock()

	return k.lastResponse
}

// reset starts the clock for a new connection. The connect time counts as
// the last response, so the first check measures from the dial and a
// connection is never judged dead before it has been given a full timeout.
func (k *keepAliveResponse) reset() {
	k.setLastResponse()
}

// defaultPingDivisor sets the default ping interval to a third of the
// keepalive timeout: two pings can be lost, or answered late, before the
// timeout trips.
const defaultPingDivisor = 3

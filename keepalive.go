package recws

import (
	"sync"
	"time"
)

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

// reset resets the keep-alive response state
func (k *keepAliveResponse) reset() {
	k.Lock()
	defer k.Unlock()

	k.lastResponse = time.Time{} // Set to zero time
}

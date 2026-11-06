package register

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

const tempMailLOLMinPollInterval = 5 * time.Second

// Share pacing for the same inbox even across separate jobs/provider instances.
// An API-key cooldown is applied to every inbox after a 429 response.
type tempMailLOLReadGate struct {
	mu       sync.Mutex
	next     time.Time
	interval time.Duration
}

var tempMailLOLReadGates sync.Map

func tempMailLOLGate(key string, interval time.Duration) *tempMailLOLReadGate {
	hash := sha256.Sum256([]byte(key))
	value, _ := tempMailLOLReadGates.LoadOrStore(hex.EncodeToString(hash[:]), &tempMailLOLReadGate{interval: interval})
	return value.(*tempMailLOLReadGate)
}

func (p *tempMailLOLProvider) inboxGate(token string) *tempMailLOLReadGate {
	return tempMailLOLGate("inbox\x00"+p.base+"\x00"+token, tempMailLOLMinPollInterval)
}

func (gate *tempMailLOLReadGate) wait(ctx context.Context, cooldown *tempMailLOLReadGate) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		gate.mu.Lock()
		cooldown.mu.Lock()
		until := gate.next
		if cooldown.next.After(until) {
			until = cooldown.next
		}
		wait := time.Until(until)
		if wait <= 0 {
			gate.next = time.Now().Add(gate.interval)
			cooldown.mu.Unlock()
			gate.mu.Unlock()
			return nil
		}
		cooldown.mu.Unlock()
		gate.mu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (gate *tempMailLOLReadGate) pause(duration time.Duration) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	until := time.Now().Add(duration)
	if until.After(gate.next) {
		gate.next = until
	}
}

package deepai

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// Instance and challenges are process-local, like the official client. Never
// persist challenges or share them between accounts, machines or API origins.
type laneProtocol struct {
	instance string
	started  time.Time
	mu       sync.Mutex
	states   map[[32]byte]*laneCheckIn
}

type laneCheckIn struct {
	mu          sync.Mutex
	gate        chan struct{}
	nonce       string
	interval    time.Duration
	seen        time.Time
	lastAttempt time.Time
	lastRelay   time.Time
}

func newLaneProtocol() *laneProtocol {
	return &laneProtocol{instance: RandomHex(16), started: time.Now(), states: make(map[[32]byte]*laneCheckIn)}
}

var processLaneProtocol = newLaneProtocol()
var checkInNoncePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (c *Client) laneProtocol() *laneProtocol {
	if c.protocol != nil {
		return c.protocol
	}
	return processLaneProtocol
}

func (c *Client) checkInState(acct *Account) *laneCheckIn {
	key, machine := "", c.Machine
	if acct != nil {
		key = acct.APIKey
		if fp := MachineFingerprint(acct.MachineID); fp != "" {
			machine = fp
		}
	}
	id := sha256.Sum256([]byte(c.Origin + "\x00" + key + "\x00" + machine))
	protocol := c.laneProtocol()
	protocol.mu.Lock()
	defer protocol.mu.Unlock()
	state := protocol.states[id]
	if state == nil {
		state = &laneCheckIn{gate: make(chan struct{}, 1)}
		protocol.states[id] = state
	}
	return state
}

func (s *laneCheckIn) note(body []byte, now time.Time) {
	var payload struct {
		CheckIn struct {
			Nonce   string  `json:"nonce"`
			Seconds float64 `json:"seconds"`
		} `json:"check_in"`
	}
	if json.Unmarshal(body, &payload) != nil || !checkInNoncePattern.MatchString(payload.CheckIn.Nonce) || payload.CheckIn.Seconds < 1 || payload.CheckIn.Seconds > 86400 {
		return
	}
	s.mu.Lock()
	s.nonce = payload.CheckIn.Nonce
	s.interval = time.Duration(payload.CheckIn.Seconds * float64(time.Second))
	s.seen = now
	s.mu.Unlock()
}

func (c *Client) checkInHeaders(req *http.Request, state *laneCheckIn) {
	state.mu.Lock()
	nonce, interval, lastRelay := state.nonce, state.interval, state.lastRelay
	state.mu.Unlock()
	if nonce != "" {
		req.Header.Set(HeaderCheckIn, nonce)
	}
	if interval == 0 {
		interval = 600 * time.Second
	}
	active := 0
	if !lastRelay.IsZero() && time.Since(lastRelay) < interval {
		active = 1
	}
	// Headless operation has no window-focus state. Only actual inference
	// attempts contribute activity; idle quota queries never manufacture it.
	beacon := fmt.Sprintf("v=%s;up=%d;pi=%d", ProtocolClientVersion, int64(time.Since(c.laneProtocol().started)/time.Second), active)
	req.Header.Set(HeaderBeacon, beacon)
}

func (c *Client) checkInBeforeModel(ctx context.Context, acct *Account) {
	state := c.checkInState(acct)
	now := time.Now()
	state.mu.Lock()
	state.lastRelay = now
	due := state.nonce != "" && now.Sub(state.seen) >= state.interval/2 && now.Sub(state.lastAttempt) >= 30*time.Second
	if due {
		state.lastAttempt = now
	}
	state.mu.Unlock()
	if due {
		// Match the official transport: refresh a due challenge before relaying,
		// but a failed quota refresh must not prevent free-model inference.
		refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _ = c.Theos(refreshCtx, acct)
		cancel()
	}
}

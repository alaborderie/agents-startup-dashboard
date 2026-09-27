package web

import (
	"sync"
	"time"
)

// bridgeOfflineAfter is how long the laptop bridge may go without polling for
// commands before the HQ shows it offline. Its long poll returns every 25 s.
const bridgeOfflineAfter = 60 * time.Second

// subscriberBuffer bounds how far a slow browser may fall behind before it is
// dropped. It reconnects and gets a fresh full state, which is always correct.
const subscriberBuffer = 32

type subscriber struct {
	messages chan []byte
	// dropped is closed when the hub gives up on a subscriber that stopped
	// reading, so its writer can end the connection.
	dropped chan struct{}
}

type bridgeStatus struct {
	Online   bool       `json:"online"`
	LastSeen *time.Time `json:"last_seen"`
}

// hub fans real-time events out to connected HQ browsers and wakes the
// laptop's long poll when a command is queued. It is in-process state: the
// deployment runs one replica, and a restart only costs a reconnect.
type hub struct {
	mu          sync.Mutex
	subscribers map[*subscriber]struct{}
	commandWake chan struct{}
	activePolls int
	lastPoll    time.Time
	online      bool
	now         func() time.Time
}

func newHub(now func() time.Time) *hub {
	return &hub{
		subscribers: make(map[*subscriber]struct{}),
		commandWake: make(chan struct{}),
		now:         now,
	}
}

func (h *hub) subscribe() *subscriber {
	s := &subscriber{
		messages: make(chan []byte, subscriberBuffer),
		dropped:  make(chan struct{}),
	}
	h.mu.Lock()
	h.subscribers[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	h.removeLocked(s)
	h.mu.Unlock()
}

func (h *hub) removeLocked(s *subscriber) {
	if _, ok := h.subscribers[s]; ok {
		delete(h.subscribers, s)
		close(s.dropped)
	}
}

func (h *hub) hasSubscribers() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subscribers) > 0
}

// broadcast never blocks: a subscriber whose buffer is full is dropped.
func (h *hub) broadcast(message []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subscribers {
		select {
		case s.messages <- message:
		default:
			h.removeLocked(s)
		}
	}
}

func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subscribers {
		h.removeLocked(s)
	}
}

// commandSignal returns a channel closed on the next queued command. Take it
// before querying so a command queued in between is never missed.
func (h *hub) commandSignal() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.commandWake
}

func (h *hub) notifyCommand() {
	h.mu.Lock()
	defer h.mu.Unlock()
	close(h.commandWake)
	h.commandWake = make(chan struct{})
}

// pollStarted records a bridge long poll and reports whether the bridge just
// came online.
func (h *hub) pollStarted() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.activePolls++
	h.lastPoll = h.now()
	cameOnline := !h.online
	h.online = true
	return cameOnline
}

func (h *hub) pollEnded() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.activePolls--
	h.lastPoll = h.now()
}

// bridge reports the laptop bridge's presence.
func (h *hub) bridge() bridgeStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.bridgeLocked()
}

func (h *hub) bridgeLocked() bridgeStatus {
	status := bridgeStatus{Online: h.online}
	if !h.lastPoll.IsZero() {
		seen := h.lastPoll.UTC()
		status.LastSeen = &seen
	}
	return status
}

// checkBridge flips the bridge offline once it stopped polling, and reports
// the new status when it changed.
func (h *hub) checkBridge() (bridgeStatus, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.online || h.activePolls > 0 || h.now().Sub(h.lastPoll) < bridgeOfflineAfter {
		return bridgeStatus{}, false
	}
	h.online = false
	return h.bridgeLocked(), true
}

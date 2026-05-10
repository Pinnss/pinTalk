package room

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

const (
	MaxPeersPerRoom = 2
	roomIDByteLen   = 16 // → 22 chars base64url, ~128 bits
	peerIDByteLen   = 6  // → 8 chars base64url, ~48 bits, only needs to be unique per room
	roomMaxAge      = 24 * time.Hour
)

var (
	ErrRoomFull     = errors.New("room is full")
	ErrPeerNotFound = errors.New("peer not found")
)

type Peer struct {
	ID   string
	Name string
	Role string // "host" | "guest"

	// Send is buffered; the WS write loop drains it. If full, drop the connection.
	Send chan []byte

	closeOnce sync.Once
	closed    chan struct{}
}

func NewPeer(name, role string) *Peer {
	return &Peer{
		ID:     newID(peerIDByteLen),
		Name:   name,
		Role:   role,
		Send:   make(chan []byte, 64),
		closed: make(chan struct{}),
	}
}

func (p *Peer) Close() {
	p.closeOnce.Do(func() {
		close(p.closed)
		close(p.Send)
	})
}

func (p *Peer) Closed() <-chan struct{} { return p.closed }

type Room struct {
	ID           string
	HostUsername string
	Created      time.Time

	mu    sync.Mutex
	peers []*Peer // ordered by join time (1st = first joiner)
}

func newRoom(host string) *Room {
	return &Room{
		ID:           newID(roomIDByteLen),
		HostUsername: host,
		Created:      time.Now(),
	}
}

// AddPeer registers a peer; returns the index it received (0 = first joiner).
// The 2nd joiner (index 1) is the WebRTC offer initiator.
func (r *Room) AddPeer(p *Peer) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.peers) >= MaxPeersPerRoom {
		return -1, ErrRoomFull
	}
	idx := len(r.peers)
	r.peers = append(r.peers, p)
	return idx, nil
}

func (r *Room) RemovePeer(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, p := range r.peers {
		if p.ID == id {
			r.peers = append(r.peers[:i], r.peers[i+1:]...)
			p.Close()
			return
		}
	}
}

// Snapshot returns a copy of the peer list (safe to iterate without lock).
func (r *Room) Snapshot() []*Peer {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Peer, len(r.peers))
	copy(out, r.peers)
	return out
}

func (r *Room) Empty() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.peers) == 0
}

type Registry struct {
	mu    sync.RWMutex
	rooms map[string]*Room
}

func NewRegistry() *Registry {
	r := &Registry{rooms: make(map[string]*Room)}
	go r.gcLoop()
	return r
}

func (r *Registry) Create(hostUsername string) *Room {
	room := newRoom(hostUsername)
	r.mu.Lock()
	r.rooms[room.ID] = room
	r.mu.Unlock()
	return room
}

func (r *Registry) Get(id string) *Room {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.rooms[id]
}

// ListByHost returns all rooms owned by the given host, sorted newest-first.
func (r *Registry) ListByHost(host string) []*Room {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Room, 0)
	for _, room := range r.rooms {
		if room.HostUsername == host {
			out = append(out, room)
		}
	}
	// newest first
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Created.After(out[i].Created) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// PeerCount returns the current peer count without exposing the peer list.
func (r *Room) PeerCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.peers)
}

func (r *Registry) Delete(id string) {
	r.mu.Lock()
	if room, ok := r.rooms[id]; ok {
		delete(r.rooms, id)
		for _, p := range room.peers {
			p.Close()
		}
	}
	r.mu.Unlock()
}

func (r *Registry) gcLoop() {
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		r.mu.Lock()
		for id, room := range r.rooms {
			room.mu.Lock()
			stale := now.Sub(room.Created) > roomMaxAge && len(room.peers) == 0
			room.mu.Unlock()
			if stale {
				delete(r.rooms, id)
			}
		}
		r.mu.Unlock()
	}
}

func newID(byteLen int) string {
	b := make([]byte, byteLen)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

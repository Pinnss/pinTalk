package signal

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/Pinnss/pinTalk/internal/auth"
	"github.com/Pinnss/pinTalk/internal/room"
)

type Handler struct {
	Auth     *auth.Store
	Rooms    *room.Registry
	Logger   *log.Logger
	DevMode  bool // skip Origin enforcement in dev
}

// Wire-format messages.
type wsMsg struct {
	Type    string          `json:"type"`
	PeerID  string          `json:"peerId,omitempty"`
	From    string          `json:"from,omitempty"`
	To      string          `json:"to,omitempty"`
	Peer    *peerInfo       `json:"peer,omitempty"`
	Peers   []peerInfo      `json:"peers,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Reason  string          `json:"reason,omitempty"`
}

type peerInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Initiator bool   `json:"initiator"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		http.Error(w, "missing room", http.StatusBadRequest)
		return
	}
	rm := h.Rooms.Get(roomID)
	if rm == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	// Determine role: cookie session → host; else guest with ?name=
	var name, role string
	if sess := h.Auth.FromRequest(r); sess != nil && sess.Username == rm.HostUsername {
		name = sess.Username
		role = "host"
	} else {
		name = strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			http.Error(w, "missing name", http.StatusBadRequest)
			return
		}
		if len(name) > 40 {
			name = name[:40]
		}
		role = "guest"
	}

	acceptOpts := &websocket.AcceptOptions{}
	if h.DevMode {
		acceptOpts.InsecureSkipVerify = true
	}
	conn, err := websocket.Accept(w, r, acceptOpts)
	if err != nil {
		// websocket.Accept already wrote a response on failure
		return
	}
	defer conn.Close(websocket.StatusInternalError, "shutdown")

	peer := room.NewPeer(name, role)
	idx, err := rm.AddPeer(peer)
	if err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}
	defer rm.RemovePeer(peer.ID)

	// 2nd joiner is the WebRTC offer initiator (deterministic for 1-on-1).
	initiator := idx == 1

	if h.Logger != nil {
		h.Logger.Printf("ws join room=%s peer=%s name=%q role=%s initiator=%v", rm.ID, peer.ID, name, role, initiator)
	}

	// Welcome: tell new peer about existing peers.
	existing := []peerInfo{}
	for _, p := range rm.Snapshot() {
		if p.ID == peer.ID {
			continue
		}
		existing = append(existing, peerInfo{ID: p.ID, Name: p.Name, Role: p.Role, Initiator: false})
	}
	send(peer, wsMsg{
		Type:   "welcome",
		PeerID: peer.ID,
		Peers:  existing,
		Peer:   &peerInfo{ID: peer.ID, Name: peer.Name, Role: peer.Role, Initiator: initiator},
	})

	// Notify existing peers about this newcomer.
	announce := wsMsg{
		Type: "peer-joined",
		Peer: &peerInfo{ID: peer.ID, Name: peer.Name, Role: peer.Role, Initiator: initiator},
	}
	announceBytes, _ := json.Marshal(announce)
	for _, p := range rm.Snapshot() {
		if p.ID == peer.ID {
			continue
		}
		select {
		case p.Send <- announceBytes:
		default:
			// peer's outbound buffer full — they'll get dropped when their writer notices
		}
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// writer
	go func() {
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-peer.Closed():
				return
			case msg, ok := <-peer.Send:
				if !ok {
					return
				}
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Write(wctx, websocket.MessageText, msg)
				wcancel()
				if err != nil {
					return
				}
			case <-ping.C:
				pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
				err := conn.Ping(pctx)
				pcancel()
				if err != nil {
					return
				}
			}
		}
	}()

	// reader
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			break
		}
		var m wsMsg
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		switch m.Type {
		case "signal":
			if m.To == "" {
				continue
			}
			// forward to target peer
			var target *room.Peer
			for _, p := range rm.Snapshot() {
				if p.ID == m.To {
					target = p
					break
				}
			}
			if target == nil {
				continue
			}
			out, _ := json.Marshal(wsMsg{Type: "signal", From: peer.ID, Payload: m.Payload})
			select {
			case target.Send <- out:
			default:
			}
		case "bye":
			conn.Close(websocket.StatusNormalClosure, "bye")
			break
		}
	}

	// notify others of departure
	leave, _ := json.Marshal(wsMsg{Type: "peer-left", PeerID: peer.ID})
	for _, p := range rm.Snapshot() {
		if p.ID == peer.ID {
			continue
		}
		select {
		case p.Send <- leave:
		default:
		}
	}
}

func send(p *room.Peer, m wsMsg) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	select {
	case p.Send <- b:
	default:
	}
}

var _ = errors.New // silence unused if branches change

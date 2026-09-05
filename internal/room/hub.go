package room

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"bomber-cli/internal/game"
)

type Session struct {
	ID     uint64
	Name   string
	Frames chan Snapshot
	room   *Room
}

type entry struct {
	room   *Room
	cancel context.CancelFunc
}

type Hub struct {
	mu                    sync.Mutex
	ctx                   context.Context
	cancel                context.CancelFunc
	sessions              map[uint64]*Session
	rooms                 map[uint64]entry
	nextSession, nextRoom uint64
	maxSessions, maxRooms int
}

func New(maxSessions, maxRooms int) *Hub {
	ctx, cancel := context.WithCancel(context.Background())
	return &Hub{ctx: ctx, cancel: cancel, sessions: make(map[uint64]*Session), rooms: make(map[uint64]entry), maxSessions: maxSessions, maxRooms: maxRooms}
}

func sanitizePlayerName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
			if b.Len() == 16 {
				break
			}
		}
	}
	if b.Len() == 0 {
		return "player"
	}
	return b.String()
}

func (h *Hub) Connect(name string) (*Session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctx.Err() != nil {
		return nil, errors.New("server stopping")
	}
	if len(h.sessions) >= h.maxSessions {
		return nil, errors.New("server is full")
	}
	name = h.uniquePlayerName(name)
	h.nextSession++
	s := &Session{ID: h.nextSession, Name: name, Frames: make(chan Snapshot, 1)}
	h.sessions[s.ID] = s
	slog.Info("player connected", "session", s.ID, "name", s.Name)
	return s, nil
}

func (h *Hub) uniquePlayerName(name string) string {
	base := sanitizePlayerName(name)
	name = base
	for suffix := 2; ; suffix++ {
		used := false
		for _, s := range h.sessions {
			if s.Name == name {
				used = true
				break
			}
		}
		if !used {
			break
		}
		name = fmt.Sprintf("%s-%d", base, suffix)
	}
	return name
}

func (h *Hub) Join(s *Session, id uint64, create bool) (*Room, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions[s.ID] != s || h.ctx.Err() != nil {
		return nil, errors.New("session closed")
	}
	if s.room != nil {
		return nil, errors.New("leave your current room first")
	}
	var r *Room
	var err error
	if !create {
		r, err = h.findRoomToJoin(id)
		if err != nil {
			return nil, err
		}
	}
	created := false
	if r == nil {
		r, err = h.createRoom()
		if err != nil {
			return nil, err
		}
		created = true
	}
	if err := r.change(control{member: Member{ID: s.ID, Name: s.Name}, frames: s.Frames}); err != nil {
		if created {
			h.rooms[r.ID].cancel()
			delete(h.rooms, r.ID)
		}
		return nil, err
	}
	s.room = r
	return r, nil
}

// Room selection and creation run while Join holds the hub lock.
func (h *Hub) findRoomToJoin(id uint64) (*Room, error) {
	if id != 0 {
		if e, ok := h.rooms[id]; ok {
			return e.room, nil
		}
		return nil, errors.New("room no longer exists")
	}
	for _, roomID := range h.sortedRoomIDs() {
		candidate := h.rooms[roomID].room
		snapshot := candidate.Snapshot()
		if snapshot.Phase == Waiting && snapshot.Count < game.MaxPlayers {
			return candidate, nil
		}
	}
	return nil, nil
}

func (h *Hub) createRoom() (*Room, error) {
	if len(h.rooms) >= h.maxRooms {
		return nil, errors.New("room limit reached")
	}
	h.nextRoom++
	r := &Room{ID: h.nextRoom, input: make(chan command, 64), control: make(chan control, 16), done: make(chan struct{})}
	r.latest.Store(Snapshot{ID: r.ID, Phase: Waiting})
	ctx, cancel := context.WithCancel(h.ctx)
	h.rooms[r.ID] = entry{r, cancel}
	go r.run(ctx, time.Now().UnixNano())
	return r, nil
}

func (h *Hub) leave(s *Session) {
	if s.room == nil {
		return
	}
	r := s.room
	_ = r.change(control{member: Member{ID: s.ID}, leave: true})
	s.room = nil
	if r.Snapshot().Count == 0 {
		h.rooms[r.ID].cancel()
		delete(h.rooms, r.ID)
	}
	select {
	case <-s.Frames:
	default:
	}
}

func (h *Hub) Leave(s *Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.leave(s)
}

func (h *Hub) Disconnect(s *Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.leave(s)
	delete(h.sessions, s.ID)
	slog.Info("player disconnected", "session", s.ID)
}

func (h *Hub) sortedRoomIDs() []uint64 {
	ids := make([]uint64, 0, len(h.rooms))
	for id := range h.rooms {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (h *Hub) List() []Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []Snapshot{}
	for _, id := range h.sortedRoomIDs() {
		out = append(out, h.rooms[id].room.Snapshot())
	}
	return out
}

func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cancel()
	// Actors do not acquire the hub lock. Keep it until they stop so concurrent
	// Close calls and late session cleanup cannot observe a partial shutdown.
	for _, e := range h.rooms {
		<-e.room.done
	}
	for _, s := range h.sessions {
		s.room = nil
		select {
		case <-s.Frames:
		default:
		}
	}
	clear(h.sessions)
	clear(h.rooms)
}

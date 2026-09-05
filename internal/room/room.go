// Package room coordinates rooms. Each room's mutable state belongs to its actor.
package room

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bomber-cli/internal/game"
)

type Phase string

const (
	Waiting   Phase = "waiting"
	Countdown Phase = "countdown"
	Playing   Phase = "playing"
	Result    Phase = "result"
)

type Member struct {
	ID    uint64
	Name  string
	Ready bool
	Score int
}
type Snapshot struct {
	ID       uint64
	Phase    Phase
	Members  [4]Member
	Count    int
	Deadline time.Time
	Game     game.View
	Message  string
}
type Action uint8

const (
	Ready Action = iota
	Up
	Down
	Left
	Right
	Bomb
)

type command struct {
	id     uint64
	action Action
}
type control struct {
	member Member
	frames chan Snapshot
	leave  bool
	reply  chan error
}
type Room struct {
	ID      uint64
	input   chan command
	control chan control
	done    chan struct{}
	latest  atomic.Value
}

func (r *Room) Snapshot() Snapshot { return r.latest.Load().(Snapshot) }
func (r *Room) Submit(id uint64, a Action) bool {
	select {
	case <-r.done:
		return false
	default:
	}
	select {
	case r.input <- command{id, a}:
		return true
	default:
		return false
	}
}
func (r *Room) change(c control) error {
	c.reply = make(chan error, 1)
	select {
	case r.control <- c:
	case <-r.done:
		return errors.New("room closed")
	}
	select {
	case err := <-c.reply:
		return err
	case <-r.done:
		return errors.New("room closed")
	}
}
func publish(ch chan Snapshot, s Snapshot) {
	select {
	case ch <- s:
	default:
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- s:
		default:
		}
	}
}

type actor struct {
	snapshot    Snapshot
	subscribers map[uint64]chan Snapshot
	match       *game.Game
	seed        int64
}

func (a *actor) membership(c control) error {
	s := &a.snapshot
	if c.leave {
		for i, m := range s.Members {
			if m.ID == c.member.ID {
				s.Members[i] = Member{}
				s.Count--
				delete(a.subscribers, m.ID)
				if a.match != nil {
					a.match.Remove(m.ID)
				}
				break
			}
		}
	} else {
		if s.Phase != Waiting && s.Phase != Countdown {
			return errors.New("round in progress")
		}
		if s.Count >= 4 {
			return errors.New("room is full")
		}
		for i, m := range s.Members {
			if m.ID == 0 {
				s.Members[i] = c.member
				s.Count++
				a.subscribers[c.member.ID] = c.frames
				break
			}
		}
	}
	if s.Phase == Countdown {
		s.Phase = Waiting
		s.Deadline = time.Time{}
	}
	for i := range s.Members {
		s.Members[i].Ready = false
	}
	return nil
}
func (a *actor) command(c command, now time.Time) bool {
	if c.id == 0 || c.action > Bomb {
		return false
	}
	s := &a.snapshot
	member := -1
	for i, m := range s.Members {
		if m.ID == c.id {
			member = i
			break
		}
	}
	if member < 0 {
		return false
	}
	// Resolve expired bombs and the round deadline before accepting an action.
	// Inputs are processed between timer ticks and must not let a player escape
	// an explosion that is already due.
	if s.Phase == Playing {
		a.tick(now)
		if s.Phase != Playing {
			return true
		}
	}
	if c.action == Ready && (s.Phase == Waiting || s.Phase == Countdown) {
		s.Members[member].Ready = !s.Members[member].Ready
		if s.Phase == Countdown {
			s.Phase = Waiting
			s.Deadline = time.Time{}
		}
		return true
	} else if s.Phase == Playing {
		switch c.action {
		case Up:
			return a.match.Move(c.id, 0, -1, now)
		case Down:
			return a.match.Move(c.id, 0, 1, now)
		case Left:
			return a.match.Move(c.id, -1, 0, now)
		case Right:
			return a.match.Move(c.id, 1, 0, now)
		case Bomb:
			return a.match.Place(c.id, now)
		}
	}
	return false
}
func (a *actor) tick(now time.Time) {
	s := &a.snapshot
	switch s.Phase {
	case Waiting:
		ready := s.Count >= 2
		for _, m := range s.Members {
			if m.ID != 0 && !m.Ready {
				ready = false
			}
		}
		if ready {
			s.Phase = Countdown
			s.Deadline = now.Add(3 * time.Second)
			s.Message = ""
		}
	case Countdown:
		if !now.Before(s.Deadline) {
			ids := []uint64{}
			for _, m := range s.Members {
				if m.ID != 0 {
					ids = append(ids, m.ID)
				}
			}
			a.match = game.New(ids, now, a.seed)
			a.seed++
			s.Phase = Playing
			slog.Info("match started", "room", s.ID, "players", s.Count)
		}
	case Playing:
		a.match.Tick(now)
		s.Game = a.match.View
		if a.match.Over {
			s.Phase = Result
			s.Deadline = now.Add(3 * time.Second)
			s.Message = "Draw!"
			for i, m := range s.Members {
				if m.ID != 0 && m.ID == a.match.Winner {
					s.Members[i].Score++
					s.Message = m.Name + " wins!"
				}
			}
			slog.Info("match ended", "room", s.ID, "winner", a.match.Winner)
		}
	case Result:
		if !now.Before(s.Deadline) {
			s.Phase = Waiting
			for i := range s.Members {
				s.Members[i].Ready = false
			}
			a.match = nil
		}
	}
	if a.match != nil {
		s.Game = a.match.View
	}
}
func (r *Room) run(ctx context.Context, seed int64) {
	defer close(r.done)
	a := actor{snapshot: r.Snapshot(), subscribers: make(map[uint64]chan Snapshot), seed: seed}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	emit := func() {
		r.latest.Store(a.snapshot)
		for _, ch := range a.subscribers {
			publish(ch, a.snapshot)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case c := <-r.control:
			err := a.membership(c)
			emit()
			c.reply <- err
		case c := <-r.input:
			if a.command(c, time.Now()) {
				if a.match != nil {
					a.snapshot.Game = a.match.View
				}
				emit()
			}
		case <-ticker.C:
			// A buffered ticker timestamp can predate an input just processed.
			a.tick(time.Now())
			emit()
		}
	}
}

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
func sanitize(name string) string {
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
	base := sanitize(name)
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
	h.nextSession++
	s := &Session{ID: h.nextSession, Name: name, Frames: make(chan Snapshot, 1)}
	h.sessions[s.ID] = s
	slog.Info("player connected", "session", s.ID, "name", s.Name)
	return s, nil
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
	if !create {
		if id != 0 {
			if e, ok := h.rooms[id]; ok {
				r = e.room
			} else {
				return nil, errors.New("room no longer exists")
			}
		} else {
			ids := h.ids()
			for _, rid := range ids {
				candidate := h.rooms[rid].room
				snap := candidate.Snapshot()
				if snap.Phase == Waiting && snap.Count < 4 {
					r = candidate
					break
				}
			}
		}
	}
	created := false
	if r == nil {
		if len(h.rooms) >= h.maxRooms {
			return nil, errors.New("room limit reached")
		}
		h.nextRoom++
		r = &Room{ID: h.nextRoom, input: make(chan command, 64), control: make(chan control, 16), done: make(chan struct{})}
		r.latest.Store(Snapshot{ID: r.ID, Phase: Waiting})
		ctx, cancel := context.WithCancel(h.ctx)
		h.rooms[r.ID] = entry{r, cancel}
		go r.run(ctx, time.Now().UnixNano())
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
func (h *Hub) Leave(s *Session) { h.mu.Lock(); defer h.mu.Unlock(); h.leave(s) }
func (h *Hub) Disconnect(s *Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.leave(s)
	delete(h.sessions, s.ID)
	slog.Info("player disconnected", "session", s.ID)
}
func (h *Hub) ids() []uint64 {
	ids := make([]uint64, 0, len(h.rooms))
	for id := range h.rooms {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
func (h *Hub) List() []Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []Snapshot{}
	for _, id := range h.ids() {
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

// Package room coordinates rooms. Each room's mutable state belongs to its actor.
package room

import (
	"context"
	"errors"
	"log/slog"
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
	Members  [game.MaxPlayers]Member
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
	if c.leave {
		if memberIndex(a.snapshot.Members, c.member.ID) >= 0 {
			a.snapshot = withoutMember(a.snapshot, c.member.ID)
			delete(a.subscribers, c.member.ID)
			if a.match != nil {
				a.match.Remove(c.member.ID)
			}
		}
	} else {
		next, err := withMember(a.snapshot, c.member)
		if err != nil {
			return err
		}
		if next.Count > a.snapshot.Count {
			a.subscribers[c.member.ID] = c.frames
		}
		a.snapshot = next
	}
	a.snapshot = withoutReadiness(withoutCountdown(a.snapshot))
	return nil
}

func (a *actor) command(c command, now time.Time) bool {
	if c.id == 0 || c.action > Bomb {
		return false
	}
	member := memberIndex(a.snapshot.Members, c.id)
	if member < 0 {
		return false
	}
	// Resolve expired bombs and the round deadline before accepting an action.
	// Inputs are processed between timer ticks and must not let a player escape
	// an explosion that is already due.
	if a.snapshot.Phase == Playing {
		a.tick(now)
		if a.snapshot.Phase != Playing {
			return true
		}
	}
	if c.action == Ready && (a.snapshot.Phase == Waiting || a.snapshot.Phase == Countdown) {
		a.snapshot = withReadinessToggled(a.snapshot, member)
		return true
	}
	if a.snapshot.Phase == Playing {
		return a.applyGameAction(c, now)
	}
	return false
}

func (a *actor) applyGameAction(c command, now time.Time) bool {
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
	default:
		return false
	}
}

func (a *actor) tick(now time.Time) {
	switch a.snapshot.Phase {
	case Waiting:
		if allReady(a.snapshot) {
			a.snapshot = countdownSnapshot(a.snapshot, now)
		}
	case Countdown:
		if !now.Before(a.snapshot.Deadline) {
			a.startRound(now)
		}
	case Playing:
		a.match.Tick(now)
		if a.match.Over {
			a.snapshot = resultSnapshot(a.snapshot, a.match.Winner, now)
		}
	case Result:
		if !now.Before(a.snapshot.Deadline) {
			a.snapshot = waitingSnapshot(a.snapshot)
			a.match = nil
		}
	}
	if a.match != nil {
		a.snapshot.Game = a.match.View
	}
}

func (a *actor) startRound(now time.Time) {
	a.match = game.New(playerIDs(a.snapshot), now, a.seed)
	a.seed++
	a.snapshot.Phase = Playing
}

func (r *Room) publishSnapshot(a *actor) {
	previous := r.Snapshot().Phase
	if a.snapshot.Phase != previous {
		switch a.snapshot.Phase {
		case Playing:
			slog.Info("match started", "room", r.ID, "players", a.snapshot.Count)
		case Result:
			slog.Info("match ended", "room", r.ID, "winner", a.snapshot.Game.Winner)
		}
	}
	r.latest.Store(a.snapshot)
	for _, ch := range a.subscribers {
		publish(ch, a.snapshot)
	}
}

func (r *Room) run(ctx context.Context, seed int64) {
	defer close(r.done)
	a := actor{snapshot: r.Snapshot(), subscribers: make(map[uint64]chan Snapshot), seed: seed}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case c := <-r.control:
			err := a.membership(c)
			r.publishSnapshot(&a)
			c.reply <- err
		case c := <-r.input:
			if a.command(c, time.Now()) {
				if a.match != nil {
					a.snapshot.Game = a.match.View
				}
				r.publishSnapshot(&a)
			}
		case <-ticker.C:
			// A buffered ticker timestamp can predate an input just processed.
			a.tick(time.Now())
			r.publishSnapshot(&a)
		}
	}
}

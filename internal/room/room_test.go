package room

import (
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"bomber-cli/internal/game"
)

func newActor() *actor {
	return &actor{snapshot: Snapshot{ID: 1, Phase: Waiting}, subscribers: make(map[uint64]chan Snapshot), seed: 42}
}

func TestRoomTransitionsPreserveTheirInput(t *testing.T) {
	now := time.Unix(1000, 0)
	original := Snapshot{
		ID: 1, Phase: Countdown, Count: 2, Deadline: now.Add(time.Second),
		Members: [game.MaxPlayers]Member{
			{ID: 1, Name: "alice", Ready: true, Score: 3},
			{ID: 2, Name: "bob", Ready: true},
		},
	}
	before := original
	unready := withReadinessToggled(original, 1)
	if unready.Phase != Waiting || !unready.Deadline.IsZero() || unready.Members[1].Ready || !unready.Members[0].Ready {
		t.Fatal("unready transition did not cancel the countdown")
	}
	result := resultSnapshot(original, 1, now)
	if result.Phase != Result || result.Members[0].Score != 4 || result.Message != "alice wins!" {
		t.Fatal("result transition did not award the winner")
	}
	if original != before || unready.Members[0].Score != 3 {
		t.Fatal("room transitions changed their input or another branch")
	}
	joined, err := withMember(original, Member{ID: 3, Name: "charlie"})
	if err != nil || joined.Count != 3 || original != before {
		t.Fatal("joining did not produce an independent snapshot")
	}
	rejected, err := withMember(result, Member{ID: 3})
	if err == nil || rejected != result {
		t.Fatal("rejected join changed the result snapshot")
	}
}

func TestCommandResolvesExpiredTimers(t *testing.T) {
	for _, action := range []Action{Right, Bomb} {
		t.Run(fmt.Sprint(action), func(t *testing.T) {
			a := newActor()
			if err := a.membership(member(1)); err != nil {
				t.Fatal(err)
			}
			if err := a.membership(member(2)); err != nil {
				t.Fatal(err)
			}
			now := time.Unix(1000, 0)
			a.snapshot.Phase = Playing
			a.match = game.New([]uint64{1, 2}, now, 42)
			a.match.Place(1, now)
			a.command(command{1, action}, now.Add(2*time.Second))
			if a.match.Players[0].Alive || a.match.Players[0].Pos != game.Spawns()[0] || a.match.BombCount != 0 {
				t.Fatal("input ran before overdue explosion")
			}
			if a.snapshot.Phase != Result || a.snapshot.Members[1].Score != 1 {
				t.Fatal("overdue explosion did not finish round")
			}
		})
	}
	a := newActor()
	if err := a.membership(member(1)); err != nil {
		t.Fatal(err)
	}
	if err := a.membership(member(2)); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	a.snapshot.Phase = Playing
	a.match = game.New([]uint64{1, 2}, now, 42)
	a.command(command{1, Bomb}, now.Add(3*time.Minute))
	if a.match.BombCount != 0 || a.snapshot.Phase != Result || a.match.Winner != 0 {
		t.Fatal("action accepted after round deadline")
	}
}

func TestInvalidCommandsDoNotChangeMembership(t *testing.T) {
	a := newActor()
	if err := a.membership(member(1)); err != nil {
		t.Fatal(err)
	}
	before := a.snapshot
	for _, c := range []command{{0, Ready}, {2, Ready}, {1, Action(255)}} {
		if a.command(c, time.Now()) || a.snapshot != before {
			t.Fatalf("invalid command changed room: %+v", c)
		}
	}
}

func member(id uint64) control {
	return control{member: Member{ID: id, Name: "player"}, frames: make(chan Snapshot, 1)}
}

func TestReadyMembershipCountdownAndCapacity(t *testing.T) {
	a := newActor()
	now := time.Unix(0, 0)
	for id := uint64(1); id <= 4; id++ {
		if err := a.membership(member(id)); err != nil {
			t.Fatal(err)
		}
	}
	if a.membership(member(5)) == nil {
		t.Fatal("fifth player admitted")
	}
	a.command(command{1, Ready}, now)
	a.tick(now)
	if a.snapshot.Phase != Waiting {
		t.Fatal("started without everyone ready")
	}
	for id := uint64(2); id <= 4; id++ {
		a.command(command{id, Ready}, now)
	}
	a.tick(now)
	if a.snapshot.Phase != Countdown {
		t.Fatal("no countdown")
	}
	a.tick(now.Add(2999 * time.Millisecond))
	if a.snapshot.Phase != Countdown {
		t.Fatal("early start")
	}
	c := member(4)
	c.leave = true
	if err := a.membership(c); err != nil {
		t.Fatal(err)
	}
	if a.snapshot.Phase != Waiting {
		t.Fatal("leave failed to cancel countdown")
	}
	for id := uint64(1); id <= 3; id++ {
		a.command(command{id, Ready}, now)
	}
	a.tick(now)
	if err := a.membership(member(4)); err != nil {
		t.Fatal(err)
	}
	if a.snapshot.Phase != Waiting {
		t.Fatal("join failed to cancel countdown")
	}
	for id := uint64(1); id <= 4; id++ {
		a.command(command{id, Ready}, now)
	}
	a.tick(now)
	a.tick(now.Add(3 * time.Second))
	if a.snapshot.Phase != Playing {
		t.Fatal("round did not start")
	}
	if a.membership(member(5)) == nil {
		t.Fatal("active join accepted")
	}
}

func TestRematchScoresAndDisconnect(t *testing.T) {
	a := newActor()
	now := time.Unix(0, 0)
	if err := a.membership(member(1)); err != nil {
		t.Fatal(err)
	}
	if err := a.membership(member(2)); err != nil {
		t.Fatal(err)
	}
	a.command(command{1, Ready}, now)
	a.command(command{2, Ready}, now)
	a.tick(now)
	a.command(command{2, Ready}, now)
	if a.snapshot.Phase != Waiting {
		t.Fatal("unready did not cancel")
	}
	a.command(command{2, Ready}, now)
	a.tick(now)
	a.tick(now.Add(3 * time.Second))
	a.match.Players[0].Capacity = 5
	a.match.Place(2, now.Add(3*time.Second))
	c := member(2)
	c.leave = true
	if err := a.membership(c); err != nil {
		t.Fatal(err)
	}
	a.tick(now.Add(3050 * time.Millisecond))
	if a.snapshot.Phase != Result || a.snapshot.Members[0].Score != 1 {
		t.Fatal("disconnect did not award survivor")
	}
	a.tick(now.Add(6050 * time.Millisecond))
	if a.snapshot.Phase != Waiting || a.snapshot.Members[0].Ready {
		t.Fatal("did not return to ready-up")
	}
	if err := a.membership(member(3)); err != nil {
		t.Fatal(err)
	}
	a.command(command{1, Ready}, now)
	a.command(command{3, Ready}, now)
	a.tick(now.Add(7 * time.Second))
	a.tick(now.Add(10 * time.Second))
	if a.match.Players[0].Capacity != 1 || a.snapshot.Members[0].Score != 1 {
		t.Fatal("rematch reset wrong state")
	}
}

func TestSoloNeverStartsAndDrawHasNoScore(t *testing.T) {
	a := newActor()
	now := time.Unix(0, 0)
	if err := a.membership(member(1)); err != nil {
		t.Fatal(err)
	}
	a.command(command{1, Ready}, now)
	a.tick(now)
	if a.snapshot.Phase != Waiting {
		t.Fatal("solo countdown")
	}
	if err := a.membership(member(2)); err != nil {
		t.Fatal(err)
	}
	a.snapshot.Phase = Playing
	a.match = game.New([]uint64{1, 2}, now, 1)
	a.match.Remove(1)
	a.match.Remove(2)
	a.tick(now)
	if a.snapshot.Message != "Draw!" || a.snapshot.Members[0].Score != 0 || a.snapshot.Members[1].Score != 0 {
		t.Fatal("draw scored")
	}
}

func TestHubLimitsCleanupNamesAndQuickJoin(t *testing.T) {
	h := New(5, 1)
	defer h.Close()
	players := []*Session{}
	for i := range 5 {
		s, err := h.Connect("\x1b[31mAlice / 🐈")
		if err != nil {
			t.Fatal(err)
		}
		players = append(players, s)
		for j := range i {
			if players[j].Name == s.Name || players[j].ID == s.ID {
				t.Fatal("duplicate identity")
			}
		}
	}
	if _, err := h.Connect("extra"); err == nil {
		t.Fatal("session limit")
	}
	if sanitizePlayerName("\x1b\n🐈") == "" || sanitizePlayerName("\x1b\n🐈") != "player" {
		t.Fatal("sanitization")
	}
	r, err := h.Join(players[0], 0, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range players[1:4] {
		other, err := h.Join(s, 0, false)
		if err != nil || r != other {
			t.Fatal("quick join failed")
		}
	}
	if _, err := h.Join(players[4], r.ID, false); err == nil {
		t.Fatal("full room joined")
	}
	if _, err := h.Join(players[4], 0, true); err == nil {
		t.Fatal("room limit")
	}
	for _, s := range players[:4] {
		h.Disconnect(s)
	}
	if len(h.List()) != 0 {
		t.Fatal("empty room retained")
	}
	select {
	case <-r.done:
	case <-time.After(time.Second):
		t.Fatal("actor leaked")
	}
	if _, err := h.Join(players[4], 0, false); err != nil {
		t.Fatal(err)
	}
}

func TestSlowClientAndBoundedInput(t *testing.T) {
	h := New(2, 1)
	defer h.Close()
	a, err := h.Connect("a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Connect("b")
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.Join(a, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Join(b, r.ID, false); err != nil {
		t.Fatal(err)
	}
	// Never consume a's frames. The other player still sees ready transitions.
	r.Submit(a.ID, Ready)
	deadline := time.After(time.Second)
	for {
		select {
		case s := <-b.Frames:
			if s.Members[0].Ready {
				goto ready
			}
		case <-deadline:
			t.Fatal("slow client blocked room")
		}
	}
ready:
	if len(a.Frames) != 1 {
		t.Fatal("latest frame not bounded")
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 10000 {
				r.Submit(a.ID, Up)
				_ = r.Snapshot()
			}
		})
	}
	wg.Wait()
	h.Leave(a)
	h.Leave(b)
	if len(h.List()) != 0 {
		t.Fatal("flood prevented cleanup")
	}
}

func TestInputPublishesWithoutWaitingForTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := New(2, 1)
		defer h.Close()
		player, err := h.Connect("responsive")
		if err != nil {
			t.Fatal(err)
		}
		r, err := h.Join(player, 0, true)
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		now := time.Now()
		r.Submit(player.ID, Ready)
		synctest.Wait()
		if time.Now() != now {
			t.Fatal("input required advancing the tick clock")
		}
		if !r.Snapshot().Members[0].Ready {
			t.Fatal("input was held until the next timer tick")
		}
		frame := <-player.Frames
		if !frame.Members[0].Ready {
			t.Fatal("updated snapshot was not published immediately")
		}
	})
}

func TestCloseReleasesRoomsAndSessions(t *testing.T) {
	h := New(16, 4)
	var players []*Session
	var rooms []*Room
	for i := range 16 {
		s, err := h.Connect(fmt.Sprint(i))
		if err != nil {
			t.Fatal(err)
		}
		r, err := h.Join(s, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		players = append(players, s)
		rooms = append(rooms, r)
	}
	h.Close()
	if len(h.List()) != 0 || len(h.sessions) != 0 {
		t.Fatal("closed hub retains rooms or sessions")
	}
	for i, s := range players {
		select {
		case <-rooms[i].done:
		default:
			t.Fatal("room still running")
		}
		if s.room != nil || len(s.Frames) != 0 {
			t.Fatal("closed session retains room or snapshot")
		}
		// Session handlers can finish after the hub has shut down.
		h.Leave(s)
		h.Disconnect(s)
	}
	h.Close()
	if _, err := h.Connect("late"); err == nil {
		t.Fatal("closed hub accepted session")
	}
}

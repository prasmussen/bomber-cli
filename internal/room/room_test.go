package room

import (
	"bomber-cli/internal/game"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func newActor() *actor {
	return &actor{snapshot: Snapshot{ID: 1, Phase: Waiting}, subscribers: make(map[uint64]chan Snapshot), seed: 42}
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
	a.membership(c)
	if a.snapshot.Phase != Waiting {
		t.Fatal("leave failed to cancel countdown")
	}
	for id := uint64(1); id <= 3; id++ {
		a.command(command{id, Ready}, now)
	}
	a.tick(now)
	a.membership(member(4))
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
	a.membership(member(1))
	a.membership(member(2))
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
	a.membership(c)
	a.tick(now.Add(3050 * time.Millisecond))
	if a.snapshot.Phase != Result || a.snapshot.Members[0].Score != 1 {
		t.Fatal("disconnect did not award survivor")
	}
	a.tick(now.Add(6050 * time.Millisecond))
	if a.snapshot.Phase != Waiting || a.snapshot.Members[0].Ready {
		t.Fatal("did not return to ready-up")
	}
	a.membership(member(3))
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
	a.membership(member(1))
	a.command(command{1, Ready}, now)
	a.tick(now)
	if a.snapshot.Phase != Waiting {
		t.Fatal("solo countdown")
	}
	a.membership(member(2))
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
	for i := 0; i < 5; i++ {
		s, err := h.Connect("\x1b[31mAlice / 🐈")
		if err != nil {
			t.Fatal(err)
		}
		players = append(players, s)
		for j := 0; j < i; j++ {
			if players[j].Name == s.Name || players[j].ID == s.ID {
				t.Fatal("duplicate identity")
			}
		}
	}
	if _, err := h.Connect("extra"); err == nil {
		t.Fatal("session limit")
	}
	if sanitize("\x1b\n🐈") == "" || sanitize("\x1b\n🐈") != "player" {
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
	a, _ := h.Connect("a")
	b, _ := h.Connect("b")
	r, _ := h.Join(a, 0, false)
	h.Join(b, r.ID, false)
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
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 10000; n++ {
				r.Submit(a.ID, Up)
				_ = r.Snapshot()
			}
		}()
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
		player, _ := h.Connect("responsive")
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

package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"bomber-cli/internal/game"
	"bomber-cli/internal/room"
)

func TestRenderingAndSmallWindow(t *testing.T) {
	h := room.New(4, 32)
	defer h.Close()
	s, err := h.Connect("player")
	if err != nil {
		t.Fatal(err)
	}
	m := New(h, s, 60, 24, true)
	r, err := h.Join(s, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	m.Room = r
	m.Snapshot = room.Snapshot{ID: r.ID, Phase: room.Playing, Count: 4, Game: game.New([]uint64{1, 2, 3, 4}, time.Now(), 1).View}
	for i := range m.Snapshot.Members {
		m.Snapshot.Members[i] = room.Member{ID: uint64(i + 1), Name: "abcdefghijklmnop-128", Score: 10}
	}
	view := ansi.Strip(m.View())
	lines := strings.Split(view, "\n")
	if len(lines) > 24 {
		t.Fatalf("too tall: %d", len(lines))
	}
	for _, line := range lines {
		if len(line) > 60 {
			t.Fatalf("too wide: %q", line)
		}
	}
	if !strings.Contains(view, "P1") || !strings.Contains(view, "##") || !strings.Contains(view, "[]") {
		t.Fatal("missing accessible arena symbols")
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 59, Height: 24})
	small := updated.(Model)
	if !strings.Contains(small.View(), "Resize terminal") {
		t.Fatal("no resize prompt")
	}
	updated, _ = small.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if updated.(Model).Room != nil {
		t.Fatal("small terminal cannot leave")
	}
}

func TestLobbyScrolling(t *testing.T) {
	h := room.New(40, 32)
	defer h.Close()
	for range 32 {
		s, err := h.Connect("p")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.Join(s, 0, true); err != nil {
			t.Fatal(err)
		}
	}
	s, err := h.Connect("viewer")
	if err != nil {
		t.Fatal(err)
	}
	m := New(h, s, 60, 24, false)
	m.selected = 33
	view := m.View()
	if !strings.Contains(view, "> Room 32") || len(strings.Split(view, "\n")) > 24 {
		t.Fatal("lobby selection not visible")
	}
}

func TestSnapshotUpdatesWithoutPolling(t *testing.T) {
	h := room.New(1, 1)
	defer h.Close()
	player, err := h.Connect("responsive")
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.Join(player, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	m := New(h, player, 60, 24, false)
	m.Room = r
	snapshot := r.Snapshot()
	snapshot.Members[0].Ready = true
	updated, cmd := m.Update(snapshot)
	if !updated.(Model).Snapshot.Members[0].Ready || cmd != nil {
		t.Fatal("snapshot not applied directly")
	}
	snapshot.ID++
	snapshot.Members[0].Ready = false
	updated, _ = updated.Update(snapshot)
	if !updated.(Model).Snapshot.Members[0].Ready {
		t.Fatal("snapshot from another room was applied")
	}
}

func TestBoardRenderingIsPureAndFlamesTakePrecedence(t *testing.T) {
	now := time.Unix(1000, 0)
	view := game.New([]uint64{1, 2}, now, 42).View
	view.Powers[1][1] = game.Capacity
	view.Bombs[0] = game.Bomb{Pos: game.Pos{X: 1, Y: 1}, Owner: 1}
	view.BombCount = 1
	view.Flames[1][1] = now.Add(time.Second)
	before := view
	plain := renderBoard(view, false)
	if plain != renderBoard(view, false) || view != before {
		t.Fatal("rendering changed state or produced inconsistent output")
	}
	if ansi.Strip(renderBoard(view, true)) != plain {
		t.Fatal("color changed board content")
	}
	row := strings.Split(plain, "\n")[1]
	if row[2:4] != "**" {
		t.Fatalf("flames should obscure the player, bomb, and pickup: %q", row)
	}
}

func TestBoardDisplayPriorityAndBombPulse(t *testing.T) {
	now := time.Unix(1000, 0)
	position := game.Pos{X: 1, Y: 1}
	view := game.View{Started: now, Now: now}
	view.Tiles[1][1] = game.Block
	view.Powers[1][1] = game.Speed
	view.Bombs[0] = game.Bomb{Pos: position}
	view.BombCount = 1
	view.Players[0] = game.Player{ID: 1, Pos: position, Alive: true}
	view.Players[1] = game.Player{ID: 2, Pos: position, Alive: true}
	view.Flames[1][1] = now.Add(time.Second)

	assertCell := func(want string) {
		t.Helper()
		row := strings.Split(renderBoard(view, false), "\n")[1]
		if got := row[2:4]; got != want {
			t.Fatalf("cell = %q, want %q", got, want)
		}
	}
	assertCell("**")
	view.Flames[1][1] = now
	assertCell("P2")
	view.Players[1].Alive = false
	assertCell("P1")
	view.Players[0].Alive = false
	assertCell("o*")
	view.Now = now.Add(250 * time.Millisecond)
	assertCell("O*")
	view.Now = now.Add(500 * time.Millisecond)
	assertCell("o*")
	view.BombCount = 0
	assertCell("S+")
	view.Powers[1][1] = game.Range
	assertCell("R+")
	view.Powers[1][1] = game.Capacity
	assertCell("B+")
	view.Powers[1][1] = game.None
	assertCell("[]")
	view.Tiles[1][1] = game.Wall
	assertCell("##")
	view.Tiles[1][1] = game.Floor
	assertCell("  ")
}

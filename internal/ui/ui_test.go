package ui

import (
	"bomber-cli/internal/game"
	"bomber-cli/internal/room"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"
	"time"
)

func TestRenderingAndSmallWindow(t *testing.T) {
	h := room.New(4, 32)
	defer h.Close()
	s, _ := h.Connect("player")
	m := New(h, s, 60, 24, true)
	r, _ := h.Join(s, 0, true)
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
	for i := 0; i < 32; i++ {
		s, _ := h.Connect("p")
		h.Join(s, 0, true)
	}
	s, _ := h.Connect("viewer")
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
	player, _ := h.Connect("responsive")
	r, _ := h.Join(player, 0, true)
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

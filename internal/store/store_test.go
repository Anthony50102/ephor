package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRoundTripAndDeletes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	s, err := Load(path)
	if err != nil || len(s.Projects) != 0 {
		t.Fatalf("missing file should load empty: %v", err)
	}
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	p, _ := s.AddProject("Payments", now)
	pid := p.ID
	th, _ := s.AddThread(pid, "Retries")
	tid := th.ID
	s.AssignSession("copilot:a", pid, tid, "a", now)
	s.AssignSession("copilot:b", pid, "", "b", now)
	s.SetHold("copilot:b", now, "waiting on PR review")
	s.NoRelativeNumbers = true
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Project(pid).Thread(tid).Name != "Retries" || s2.Assign["copilot:a"].Thread != tid {
		t.Fatalf("round trip lost data: %+v", s2)
	}
	if h, ok := s2.Holds["copilot:b"]; !ok || h.Reason != "waiting on PR review" || !s2.NoRelativeNumbers {
		t.Fatalf("round trip lost hold or rnu preference: holds=%+v noRnu=%v", s2.Holds, s2.NoRelativeNumbers)
	}
	s2.ClearHold("copilot:b")
	if _, ok := s2.Holds["copilot:b"]; ok {
		t.Fatal("ClearHold should remove the hold")
	}

	s2.DeleteThread(pid, tid)
	if a := s2.Assign["copilot:a"]; a.Project != pid || a.Thread != "" {
		t.Fatalf("deleting a thread should keep sessions in the project: %+v", a)
	}
	s2.DeleteProject(pid)
	if len(s2.Assign) != 0 || len(s2.Projects) != 0 {
		t.Fatalf("deleting a project should unassign its sessions: %+v", s2)
	}
	if _, err := s2.AddProject("  ", now); err == nil {
		t.Fatal("empty names must be rejected")
	}
}

func TestCorruptFileIsNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	_ = os.WriteFile(path, []byte("{not json"), 0o600)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "will not overwrite") {
		t.Fatalf("want a refusal, got %v", err)
	}
}

func TestUpdateKeepsOtherWritersChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Now()
	// Two instances start from the same file, then each makes one edit.
	if _, err := Update(path, func(s *State) error { _, err := s.AddProject("A", now); return err }); err != nil {
		t.Fatal(err)
	}
	st, err := Update(path, func(s *State) error { _, err := s.AddProject("B", now); return err })
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Projects) != 2 {
		t.Fatalf("second writer dropped the first writer's project: %+v", st.Projects)
	}
}

func TestSnapshotRebootAndSync(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	t0 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

	a := SavedSession{Key: "copilot:aaa", Kind: "copilot", ID: "aaa", Title: "train", WindowName: "nabu", Cwd: "/repo", SeenAt: t0}
	b := SavedSession{Key: "claude:bbb", Kind: "claude", ID: "bbb", Title: "eval", WindowName: "eval", Cwd: "/repo", SeenAt: t0}
	c := SavedSession{Key: "copilot:ccc", Kind: "copilot", ID: "ccc", Title: "fresh", WindowName: "fresh", Cwd: "/repo", SeenAt: t0.Add(time.Hour)}

	// First sync on server A saves both sessions.
	snap, err := UpdateSnapshot(path, func(s *Snapshot) (bool, error) {
		return s.Sync("100:1700000000", []SavedSession{a, b}, t0), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Live) != 2 || len(snap.Recoverable) != 0 {
		t.Fatalf("initial sync = %+v", snap)
	}

	// Steady-state sync on same server with unchanged metadata does not rewrite.
	if snap.Sync("100:1700000000", []SavedSession{a, b}, t0.Add(3*time.Second)) {
		t.Fatal("unchanged live set should not mark snapshot dirty")
	}

	// Closing session b on the same server drops it from Live without making it recoverable.
	if !snap.Sync("100:1700000000", []SavedSession{a}, t0.Add(time.Minute)) {
		t.Fatal("closing a window on the same server should update Live")
	}
	if len(snap.Live) != 1 || snap.Live[0].Key != a.Key || len(snap.Recoverable) != 0 {
		t.Fatalf("after normal close: %+v", snap)
	}

	// Reboot to server B where user already opened session c before Ephor started:
	// session a from dead server A moves to Recoverable instead of being clobbered.
	if !snap.Sync("200:1700009999", []SavedSession{c}, t0.Add(time.Hour)) {
		t.Fatal("new tmux server ID must transition snapshot")
	}
	if len(snap.Recoverable) != 1 || snap.Recoverable[0].Key != a.Key {
		t.Fatalf("expected a in Recoverable after reboot, got %+v", snap.Recoverable)
	}
	if len(snap.Live) != 1 || snap.Live[0].Key != c.Key {
		t.Fatalf("expected c in Live, got %+v", snap.Live)
	}

	// Second reboot to server C with 0 live sessions preserves BOTH a (still recoverable) and c (was live on B).
	if !snap.Sync("300:1700019999", nil, t0.Add(2*time.Hour)) {
		t.Fatal("second reboot must merge Live into Recoverable")
	}
	if len(snap.Recoverable) != 2 {
		t.Fatalf("expected 2 recoverable across back-to-back reboots, got %+v", snap.Recoverable)
	}

	// AllRecoverable returns Recoverable on matching server, and Recoverable+Live when tmux is down.
	if got := snap.AllRecoverable("300:1700019999"); len(got) != 2 {
		t.Fatalf("AllRecoverable(same) = %d, want 2", len(got))
	}
	snap.Live = []SavedSession{b}
	if got := snap.AllRecoverable(""); len(got) != 3 {
		t.Fatalf("AllRecoverable(no tmux) = %d, want 3", len(got))
	}

	// Resuming session a on server C automatically removes it from Recoverable.
	if !snap.Sync("300:1700019999", []SavedSession{a}, t0.Add(3*time.Hour)) {
		t.Fatal("resumed session should clear from Recoverable")
	}
	if len(snap.Recoverable) != 1 || snap.Recoverable[0].Key != c.Key {
		t.Fatalf("expected only c left in Recoverable, got %+v", snap.Recoverable)
	}

	// Dismiss removes c explicitly and round-trips through disk.
	if !snap.Dismiss(c.Key) || len(snap.Recoverable) != 0 {
		t.Fatalf("Dismiss failed: %+v", snap.Recoverable)
	}
	if err := snap.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSnapshot(path)
	if err != nil || loaded.ServerID != "300:1700019999" || len(loaded.Live) != 1 {
		t.Fatalf("round-trip snapshot = %+v err=%v", loaded, err)
	}
}

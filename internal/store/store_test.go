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

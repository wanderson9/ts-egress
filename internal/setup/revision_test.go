package setup

import "testing"

func TestSettingsRejectPreRestartRevision(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Claim("a long test password"); err != nil {
		t.Fatal(err)
	}
	v, stale := s.Snapshot()
	v.Email = "new@example.com"
	if err := s.Update(v); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	v.Email = "old@example.com"
	if err := restarted.UpdateAtRevision(v, stale, true); err == nil {
		t.Fatal("pre-restart revision accepted")
	}
	if restarted.Get().Email != "new@example.com" {
		t.Fatal("saved settings changed")
	}
}

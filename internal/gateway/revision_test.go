package gateway

import "testing"

func TestRevisionRejectsPreRestartEdits(t *testing.T) {
	old := []Route{{"app.example.com", "http://old:3000", true, "", "", false}}
	current := []Route{{"app.example.com", "http://new:3000", true, "", "", false}}
	first := New(nil)
	first.Replace(old)
	stale := first.Revision()
	first.Replace(current)
	restarted := New(nil)
	restarted.Replace(current)
	if restarted.ReplaceAtRevision(old, stale) {
		t.Fatal("pre-restart revision accepted")
	}
	if restarted.Revision() > 1<<53 {
		t.Fatal("revision not JavaScript-safe")
	}
}

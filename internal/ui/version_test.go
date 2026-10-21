package ui

import (
	"os"
	"strings"
	"testing"
)

func TestReleaseComparison(t *testing.T) {
	for _, tc := range []struct {
		latest string
		want   bool
	}{{"v0.1", false}, {"v0.1.0", false}, {"v0.1.1", true}, {"v0.2", true}, {"v0.10", true}, {"v1.0", true}, {"v0.0.9", false}, {"v0.2-beta", false}, {"", false}, {"garbage", false}} {
		if got := newerRelease(tc.latest, Version); got != tc.want {
			t.Errorf("%s newer than %s: %v", tc.latest, Version, got)
		}
	}
}
func TestVersionFileMatchesApplication(t *testing.T) {
	b, err := os.ReadFile("../../VERSION")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != Version {
		t.Fatal("VERSION and app version differ")
	}
}

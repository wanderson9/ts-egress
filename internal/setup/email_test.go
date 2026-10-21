package setup

import "testing"

func TestInitialRegistrationEmailLocksAtConnection(t *testing.T) {
	_, s, _ := fixture(t)
	if err := s.Claim("a long test password"); err != nil {
		t.Fatal(err)
	}
	v := s.Get()
	v.Email = "initial@example.com"
	if err := s.Update(v); err != nil {
		t.Fatal(err)
	}
	v.Email = "corrected@example.com"
	v.Connect = true
	if err := s.Update(v); err != nil {
		t.Fatal(err)
	}
	v.Email = "ignored@example.com"
	if err := s.Update(v); err == nil {
		t.Fatal("accepted ineffective email change")
	}
	if s.Get().Email != "corrected@example.com" {
		t.Fatal("contact changed")
	}
}

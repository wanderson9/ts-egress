package setup

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

type Settings struct {
	Hostname     string   `json:"hostname"`
	Admins       []string `json:"admins"`
	Email        string   `json:"email"`
	AcceptTerms  bool     `json:"acceptTerms"`
	Connect      bool     `json:"connect"`
	TailnetOnly  bool     `json:"tailnetOnly"`
	PasswordHash string   `json:"-"`
}
type diskSettings struct {
	Settings
	PasswordHash string `json:"passwordHash"`
}
type Store struct {
	mu       sync.RWMutex
	path     string
	value    Settings
	revision uint64
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "settings.json"), value: Settings{Hostname: "ts-egress", Admins: []string{}}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var d diskSettings
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	s.value = d.Settings
	s.value.PasswordHash = d.PasswordHash
	s.revision = 1
	if s.value.PasswordHash == "" {
		return nil, errors.New("existing settings contain no administrator password")
	}
	return s, nil
}
func (s *Store) Revision() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.revision
}
func (s *Store) Snapshot() (Settings, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v := s.value
	v.Admins = append([]string{}, v.Admins...)
	return v, s.revision
}
func (s *Store) Get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v := s.value
	v.Admins = append([]string{}, v.Admins...)
	return v
}
func (s *Store) write(v Settings) error {
	b, err := json.MarshalIndent(diskSettings{Settings: v, PasswordHash: v.PasswordHash}, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	if err := dir.Close(); err != nil {
		return err
	}
	s.value = v
	s.revision++
	return nil
}
func (s *Store) Claim(password string) error {
	if len(password) < 12 || len(password) > 72 {
		return errors.New("password must be 12–72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.value.PasswordHash != "" {
		return errors.New("administrator already configured")
	}
	v := s.value
	v.PasswordHash = string(hash)
	return s.write(v)
}
func (s *Store) ChangePassword(current, next string) error {
	if len(next) < 12 || len(next) > 72 {
		return errors.New("password must be 12–72 bytes")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if bcrypt.CompareHashAndPassword([]byte(s.value.PasswordHash), []byte(current)) != nil {
		return errors.New("current password is incorrect")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	v := s.value
	v.PasswordHash = string(hash)
	return s.write(v)
}
func (s *Store) RecoverAdmin(next string, admins []string) error {
	if len(next) < 12 || len(next) > 72 {
		return errors.New("password must be 12–72 bytes")
	}
	if len(admins) > 50 {
		return errors.New("too many administrators")
	}
	for i := range admins {
		admins[i] = strings.TrimSpace(admins[i])
		if admins[i] == "" || len(admins[i]) > 254 {
			return errors.New("invalid administrator login")
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.value
	v.PasswordHash = string(hash)
	if admins != nil {
		v.Admins = admins
	}
	return s.write(v)
}
func (s *Store) Update(v Settings) error {
	return s.UpdateAtRevision(v, 0, false)
}
func (s *Store) UpdateAtRevision(v Settings, revision uint64, check bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if check && revision != s.revision {
		return errors.New("settings changed; reload before saving")
	}
	if s.value.PasswordHash == "" {
		return errors.New("claim the administrator account first")
	}
	if !validHostname(v.Hostname) {
		return errors.New("hostname must be a lowercase DNS label (1–63 characters)")
	}
	if s.value.Connect && v.Hostname != s.value.Hostname {
		return errors.New("hostname cannot change after connection has been requested")
	}
	if s.value.Connect && !v.Connect {
		return errors.New("disconnect/reset is not supported; connection must remain enabled")
	}
	if len(v.Admins) > 50 {
		return errors.New("too many administrators")
	}
	// An empty allowlist is allowed: it admits any member of the tailnet.
	for i, a := range v.Admins {
		v.Admins[i] = strings.TrimSpace(a)
		if v.Admins[i] == "" || len(a) > 254 {
			return errors.New("invalid administrator login")
		}
	}
	if v.Email != "" && (!strings.Contains(v.Email, "@") || strings.ContainsAny(v.Email, "\r\n") || len(v.Email) > 254) {
		return errors.New("invalid certificate contact email")
	}
	if v.AcceptTerms && v.Email == "" {
		return errors.New("certificate contact email is required before accepting terms")
	}
	v.PasswordHash = s.value.PasswordHash
	v.TailnetOnly = s.value.TailnetOnly
	return s.write(v)
}
func (s *Store) Seal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.value.TailnetOnly {
		return nil
	}
	v := s.value
	v.TailnetOnly = true
	return s.write(v)
}
func validHostname(s string) bool {
	if len(s) < 1 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}
func token() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

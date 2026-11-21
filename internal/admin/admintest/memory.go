// Package admintest holds an in-memory admin.Repository for tests of the code above it. The database tests of the admin package prove
// the real repository, so this one only has to follow the same rules.
package admintest

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/admin"
)

// Memory is an admin.Repository that keeps everything in memory.
type Memory struct {
	mu       sync.Mutex
	accounts map[string]*admin.Account
	codes    map[string]map[string]bool

	// OnSetPassword runs inside SetPassword, where the real repository ends the other sessions of the admin.
	OnSetPassword func(id string, keep []byte)
	// Err makes every call fail with this error when it is set.
	Err error
}

// NewMemory returns an empty repository.
func NewMemory() *Memory {
	return &Memory{accounts: map[string]*admin.Account{}, codes: map[string]map[string]bool{}}
}

func (m *Memory) find(match func(*admin.Account) bool) (*admin.Account, error) {
	if m.Err != nil {
		return nil, m.Err
	}

	for _, a := range m.accounts {
		if match(a) {
			return a, nil
		}
	}

	return nil, admin.ErrNotFound
}

func copyOf(a *admin.Account) admin.Account {
	c := *a

	return c
}

// ByEmail returns a copy of the account.
func (m *Memory) ByEmail(_ context.Context, email string) (admin.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, err := m.find(func(a *admin.Account) bool { return a.Email == email })
	if err != nil {
		return admin.Account{}, err
	}

	return copyOf(a), nil
}

// ByID returns a copy of the account.
func (m *Memory) ByID(_ context.Context, id string) (admin.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, err := m.find(func(a *admin.Account) bool { return a.ID == id })
	if err != nil {
		return admin.Account{}, err
	}

	return copyOf(a), nil
}

// Insert stores the account under a new id.
func (m *Memory) Insert(_ context.Context, account admin.Account) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return "", m.Err
	}

	for _, a := range m.accounts {
		if a.Email == account.Email {
			return "", admin.ErrDuplicate
		}
	}

	account.ID = "00000000-0000-4000-8000-" + strconv.FormatInt(int64(100000000000+len(m.accounts)), 10)
	m.accounts[account.ID] = &account
	m.codes[account.ID] = map[string]bool{}

	return account.ID, nil
}

// Put stores an account as it is, with its id, for a test that needs a state that Insert cannot make.
func (m *Memory) Put(account admin.Account) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.accounts[account.ID] = &account

	if m.codes[account.ID] == nil {
		m.codes[account.ID] = map[string]bool{}
	}
}

// AcceptTOTPStep records the step when it is above the stored one.
func (m *Memory) AcceptTOTPStep(_ context.Context, id string, step int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, err := m.find(func(a *admin.Account) bool { return a.ID == id })
	if err != nil {
		return false, err
	}

	if a.TOTPLastStep != nil && *a.TOTPLastStep >= step {
		return false, nil
	}

	a.TOTPLastStep = &step

	return true, nil
}

// EnableTOTP stores the secret and the codes when the enrollment time is the expected one.
func (m *Memory) EnableTOTP(_ context.Context, id string, expected *time.Time, sealed string, at time.Time, step int64, hashes []string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, err := m.find(func(a *admin.Account) bool { return a.ID == id })
	if err != nil {
		return false, err
	}

	if (a.TOTPEnabledAt == nil) != (expected == nil) || (expected != nil && !a.TOTPEnabledAt.Equal(*expected)) {
		return false, nil
	}

	// the table keeps microseconds
	at = at.Truncate(time.Microsecond)
	a.TOTPSecret, a.TOTPEnabledAt, a.TOTPLastStep = sealed, &at, &step
	m.setCodes(id, hashes)

	return true, nil
}

func (m *Memory) setCodes(id string, hashes []string) {
	m.codes[id] = map[string]bool{}
	for _, hash := range hashes {
		m.codes[id][hash] = false
	}
}

// SetPassword stores the hash and calls OnSetPassword.
func (m *Memory) SetPassword(_ context.Context, id, hash string, keep []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, err := m.find(func(a *admin.Account) bool { return a.ID == id })
	if err != nil {
		return err
	}

	a.PasswordHash, a.PasswordExpiresAt = hash, nil

	if m.OnSetPassword != nil {
		m.OnSetPassword(id, keep)
	}

	return nil
}

// UnusedRecoveryHashes lists the hashes of the codes that are not used.
func (m *Memory) UnusedRecoveryHashes(_ context.Context, id string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	var out []string

	for hash, used := range m.codes[id] {
		if !used {
			out = append(out, hash)
		}
	}

	slices.Sort(out)

	return out, nil
}

// UseRecoveryCode marks a code as used when it is not used yet.
func (m *Memory) UseRecoveryCode(_ context.Context, id, hash string, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return false, m.Err
	}

	used, known := m.codes[id][hash]
	if !known || used {
		return false, nil
	}

	m.codes[id][hash] = true

	return true, nil
}

// ReplaceRecoveryCodes stores new codes in place of the old ones.
func (m *Memory) ReplaceRecoveryCodes(_ context.Context, id string, hashes []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, err := m.find(func(a *admin.Account) bool { return a.ID == id }); err != nil {
		return err
	}

	m.setCodes(id, hashes)

	return nil
}

// ResetTOTP clears the factor and the recovery codes of the admin.
func (m *Memory) ResetTOTP(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, err := m.find(func(a *admin.Account) bool { return a.ID == id })
	if err != nil {
		return err
	}

	a.TOTPSecret, a.TOTPEnabledAt, a.TOTPLastStep = "", nil, nil
	m.setCodes(id, nil)

	return nil
}

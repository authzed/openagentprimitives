package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// Account is a registration bound to one verified subject and workspace.
// Tokens must never be serialized into a browser response or log.
type Account struct {
	ClientID     string    `json:"client_id"`
	Subject      string    `json:"subject"`
	Email        string    `json:"email"`
	IDToken      string    `json:"id_token,omitempty"`
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type state struct {
	HostID   string             `json:"host_id"`
	Accounts map[string]Account `json:"accounts"`
}

// Store serializes refreshes across desktop processes with an OS file lock.
type Store struct{ Dir string }

func (s Store) transaction(ctx context.Context, fn func(*state) error) (err error) {
	if err = os.MkdirAll(s.Dir, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	// Closing f releases the lock even when a process exits during a refresh.
	path := filepath.Join(s.Dir, "accounts.json")
	b, err := os.ReadFile(path)
	st := state{HostID: "urn:uuid:" + uuid.NewString(), Accounts: map[string]Account{}}
	if err == nil {
		if err = json.Unmarshal(b, &st); err != nil {
			return fmt.Errorf("chatgpt: invalid credential store")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if st.HostID == "" || st.Accounts == nil {
		return fmt.Errorf("chatgpt: invalid credential store")
	}
	if err = fn(&st); err != nil {
		return err
	}
	encoded, err := json.Marshal(st)
	if err != nil {
		return err
	}
	// Reads do not rewrite unchanged credentials.
	if string(encoded) == string(b) {
		return nil
	}
	tmp, err := os.CreateTemp(s.Dir, ".accounts-*")
	if err != nil {
		return err
	}
	defer func() {
		if e := os.Remove(tmp.Name()); e != nil && !errors.Is(e, os.ErrNotExist) {
			err = errors.Join(err, e)
		}
	}()
	if _, err = tmp.Write(encoded); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err = tmp.Sync(); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s Store) Accounts(ctx context.Context) ([]Account, error) {
	var accounts []Account
	err := s.transaction(ctx, func(st *state) error {
		for _, a := range st.Accounts {
			accounts = append(accounts, a)
		}
		return nil
	})
	return accounts, err
}

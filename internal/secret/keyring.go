package secret

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
	"github.com/zalando/go-keyring"
)

// keyringResolver reads from the operating system's credential store: the
// Windows Credential Manager, the macOS keychain, or a Secret Service daemon.
// References look like "keyring:service/account".
type keyringResolver struct{}

func (keyringResolver) Scheme() string { return "keyring" }

func (keyringResolver) Resolve(_ context.Context, value string) (Secret, error) {
	service, account, ok := strings.Cut(value, "/")
	if !ok {
		return Secret{}, errors.New("keyring reference should be keyring:service/account")
	}
	v, err := keyring.Get(service, account)
	if err != nil {
		return Secret{}, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return Secret{Kind: KindPassword, Bytes: []byte(v)}, nil
}

// StoreKeyring writes material into the operating system credential store.
// This is the only write path the tool offers, and it writes somewhere the
// operating system already protects rather than into the inventory database.
func StoreKeyring(service, account, value string) error {
	return keyring.Set(service, account, value)
}

// DeleteKeyring removes material from the operating system credential store.
func DeleteKeyring(service, account string) error {
	return keyring.Delete(service, account)
}

// KeyringEntry is one entry in the OS credential store, by name. Name is
// what the store calls it: on Windows, service:account.
type KeyringEntry struct {
	Name    string
	Service string
	Account string
}

// Ref is the credential reference that reads this entry.
func (e KeyringEntry) Ref() string { return "keyring:" + e.Service + "/" + e.Account }

// Mistaken reports whether the entry cannot be what it was meant to be: a
// service holding a colon or a slash was almost always a whole reference,
// such as keyring:kryostate/ops, given where service/account was wanted.
func (e KeyringEntry) Mistaken() bool { return strings.ContainsAny(e.Service, ":/") }

// ErrListUnsupported means this operating system's credential store cannot
// be listed.
var ErrListUnsupported = errors.New("this operating system's credential store cannot be listed")

// EntryFromName splits a stored entry's name, as ListKeyring shows it, back
// into service and account. The account is after the last colon, because an
// account never holds one and a mistaken service may.
func EntryFromName(name string) (KeyringEntry, bool) { return entryFromName(name) }

func entryFromName(name string) (KeyringEntry, bool) {
	i := strings.LastIndex(name, ":")
	if i <= 0 || i == len(name)-1 {
		return KeyringEntry{}, false
	}
	return KeyringEntry{Name: name, Service: name[:i], Account: name[i+1:]}, true
}

// identityEnv names the environment variable holding the path to the age
// identity used to open local encrypted files. Keeping the identity outside
// the data directory is the point: an identity file sitting beside the
// ciphertext it opens is decoration, not encryption.
const identityEnv = "KRYOSTATE_AGE_IDENTITY"

// ageFile decrypts a local age file. age is used rather than anything written
// here because credential storage is the wrong place to be original.
func ageFile(_ context.Context, path string) (Secret, error) {
	idPath := os.Getenv(identityEnv)
	if idPath == "" {
		return Secret{}, fmt.Errorf("%s is not set, so %s cannot be opened", identityEnv, path)
	}
	idFile, err := os.Open(idPath)
	if err != nil {
		return Secret{}, fmt.Errorf("open age identity: %w", err)
	}
	defer idFile.Close()
	ids, err := age.ParseIdentities(idFile)
	if err != nil {
		return Secret{}, fmt.Errorf("parse age identity: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return Secret{}, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	defer f.Close()
	r, err := age.Decrypt(f, ids...)
	if err != nil {
		return Secret{}, fmt.Errorf("decrypt %s: %w", path, err)
	}
	// Bounded: a private key is kilobytes. An unbounded read here would let a
	// file on disk decide how much memory this process allocates.
	b, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return Secret{}, err
	}
	return Secret{Kind: KindPrivateKey, Bytes: b}, nil
}

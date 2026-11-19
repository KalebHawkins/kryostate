//go:build !windows

package secret

// ListKeyring is not available here: the macOS keychain and the Secret
// Service offer no dependable way to list entries by name.
func ListKeyring(func(KeyringEntry) bool) ([]KeyringEntry, error) {
	return nil, ErrListUnsupported
}

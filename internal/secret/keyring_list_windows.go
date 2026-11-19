//go:build windows

package secret

import (
	"strings"

	"github.com/danieljoos/wincred"
)

// ListKeyring returns the generic entries in the Windows Credential Manager
// whose name is accepted by keep. The values are never read out: each entry
// is reported by name only, and the material Windows hands back with it is
// overwritten at once.
func ListKeyring(keep func(KeyringEntry) bool) ([]KeyringEntry, error) {
	creds, err := wincred.List()
	if err != nil {
		return nil, err
	}
	var out []KeyringEntry
	for _, c := range creds {
		for i := range c.CredentialBlob {
			c.CredentialBlob[i] = 0
		}
		name, ok := strings.CutPrefix(c.TargetName, "LegacyGeneric:target=")
		if !ok {
			name = c.TargetName
		}
		e, ok := entryFromName(name)
		if ok && keep(e) {
			out = append(out, e)
		}
	}
	return out, nil
}

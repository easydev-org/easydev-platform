package ops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/easylab-platform/easyvcs/store"
)

// VCSTokenMinter mints a scoped credential for a CI run from the VCS store.
//
// easyvcs owns the only token authority (one identity for the whole platform),
// and its tokens have two levels: read and write. `permissions: contents: write`
// therefore mints a write token; anything less gets a read token; nothing gets
// no token at all.
//
// Tokens are stored hashed, so the plaintext exists only here, immediately after
// minting — which is why the caller puts it straight into the run's Secret.
type VCSTokenMinter struct {
	CS *store.CentralStore
	// UserID is the account the token is minted for. 0 = the run's owner is
	// looked up by name (see UserOf).
	UserID int64
}

// MintToken creates a token for the run.
//
// The label is recorded as a separate token per run, so revoking one run's
// credential cannot disturb another's (and a run's token is revocable by
// deleting it).
func (m *VCSTokenMinter) MintToken(ctx context.Context, tenant, label string, write bool) (string, error) {
	if m.CS == nil {
		return "", fmt.Errorf("ops: token minter has no store")
	}
	if m.UserID == 0 {
		return "", fmt.Errorf("ops: token minter has no user (set UserID)")
	}
	level := "read"
	if write {
		level = "write"
	}
	// A random 32-byte token, like every other VCS token.
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b[:])
	if _, err := m.CS.CreateToken(tok, m.UserID, level); err != nil {
		return "", fmt.Errorf("ops: mint %s token: %w", level, err)
	}
	return tok, nil
}

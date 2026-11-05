package ops

import (
	"context"
	"fmt"

	"github.com/easy-platform/easyops/k8s/servicesmgr"
)

// SecretsService adapts the servicesmgr secret backend to the two interfaces the
// CI layer needs.
//
// It is a thin adapter on purpose: the CI orchestrator must not import
// servicesmgr (its OpsClient seam keeps the backends swappable), so the wiring
// lives here in the ops package that already owns the client.
type SecretsService struct {
	Client *servicesmgr.Client
}

// SecretValues reads a tenant secret's key/value pairs.
//
// An empty or missing secret is an ERROR: a workflow that references a secret the
// tenant does not have must fail, not run with an empty credential.
func (s *SecretsService) SecretValues(ctx context.Context, tenant, name string) (map[string]string, error) {
	if s.Client == nil {
		return nil, fmt.Errorf("ops: secrets backend is not configured")
	}
	sec, err := s.Client.GetSecret(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(sec.Data) == 0 {
		return nil, fmt.Errorf("secret %q is empty", name)
	}
	return sec.Data, nil
}

// PutRunSecret creates or replaces the per-run Secret the CI jobs reference.
func (s *SecretsService) PutRunSecret(ctx context.Context, tenant, name string, values map[string]string) error {
	if s.Client == nil {
		return fmt.Errorf("ops: secrets backend is not configured")
	}
	// The creator is the tenant, so only that tenant can overwrite it (A1).
	_, err := s.Client.PutSecret(ctx, name, values, "ci", tenant)
	return err
}

// DeleteRunSecret removes the per-run Secret.
func (s *SecretsService) DeleteRunSecret(ctx context.Context, tenant, name string) error {
	if s.Client == nil {
		return nil
	}
	_, err := s.Client.DeleteSecret(ctx, name)
	return err
}

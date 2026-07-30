package gateway

import (
	"context"
)

type sandboxRuntimeDeletePolicy int

const (
	sandboxRuntimeDeleteRequired sandboxRuntimeDeletePolicy = iota
	sandboxRuntimeDeleteBestEffort
)

type sandboxDeleteResult struct {
	Deleted      bool
	RuntimeError error
}

// deleteSandbox is called while the caller holds the sandbox's lifecycle lock.
func (a *App) deleteSandbox(
	ctx context.Context,
	record SandboxRecord,
	policy sandboxRuntimeDeletePolicy,
) (sandboxDeleteResult, error) {
	result := sandboxDeleteResult{}

	if a.runtime != nil {
		result.RuntimeError = a.runtime.DeleteSandbox(ctx, record.RuntimeInfo)
	}
	if result.RuntimeError != nil && policy == sandboxRuntimeDeleteRequired {
		return result, result.RuntimeError
	}

	deleted, err := a.store.Delete(record.ID)
	if err != nil {
		return result, err
	}
	result.Deleted = deleted
	if deleted {
		a.deadlines.cancel(record.ID)
	}
	return result, nil
}

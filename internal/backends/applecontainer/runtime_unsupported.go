//go:build !darwin || !cgo

package applecontainer

import (
	"fmt"
	"log"

	gateway "e2b-local/internal/gateway"
)

func init() {
	gateway.RegisterSandboxRuntimeFactory("applecontainer", func(cfg gateway.Config, logger *log.Logger) (gateway.SandboxRuntime, error) {
		return nil, fmt.Errorf("applecontainer runtime requires macOS with CGO_ENABLED=1")
	})
}

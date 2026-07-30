package gateway

import "time"

// DefaultSandboxTimeoutSeconds mirrors the E2B REST API default for an
// omitted create/resume timeout. E2B SDKs apply their 300-second default
// client-side and send that value explicitly.
const DefaultSandboxTimeoutSeconds = 15

func requestTimeout(timeout *int32) int32 {
	if timeout == nil {
		return DefaultSandboxTimeoutSeconds
	}
	return *timeout
}

func defaultSandboxEndAt(start time.Time) time.Time {
	return start.Add(time.Duration(DefaultSandboxTimeoutSeconds) * time.Second)
}

package gateway

import "testing"

func TestSandboxTimeoutActionNormalization(t *testing.T) {
	tests := []struct {
		name    string
		action  SandboxTimeoutAction
		want    SandboxTimeoutAction
		wantErr bool
		retains bool
	}{
		{name: "legacy default", action: SandboxTimeoutActionUnspecified, want: SandboxTimeoutActionKill},
		{name: "kill", action: SandboxTimeoutActionKill, want: SandboxTimeoutActionKill},
		{name: "pause", action: SandboxTimeoutActionPause, want: SandboxTimeoutActionPause, retains: true},
		{name: "unknown", action: "hibernate", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.action.Normalize()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Normalize() error = %v, wantErr %t", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("Normalize() = %q, want %q", got, tt.want)
			}
			if got := tt.action.RetainsSandboxAfterTimeout(); got != tt.retains {
				t.Fatalf("RetainsSandboxAfterTimeout() = %t, want %t", got, tt.retains)
			}
		})
	}
}

func TestInternetAccessPolicyBoolConversion(t *testing.T) {
	allowed := true
	denied := false

	tests := []struct {
		name   string
		input  *bool
		policy InternetAccessPolicy
	}{
		{name: "unspecified", policy: InternetAccessUnspecified},
		{name: "allowed", input: &allowed, policy: InternetAccessAllowed},
		{name: "denied", input: &denied, policy: InternetAccessDenied},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := InternetAccessPolicyFromBoolPtr(tt.input)
			if policy != tt.policy {
				t.Fatalf("expected policy %q, got %q", tt.policy, policy)
			}

			value := policy.BoolPtr()
			if tt.input == nil {
				if value != nil {
					t.Fatalf("expected nil, got %v", *value)
				}
				return
			}
			if value == nil || *value != *tt.input {
				t.Fatalf("expected %v, got %#v", *tt.input, value)
			}
		})
	}
}

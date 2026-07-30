package gateway

import "testing"

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

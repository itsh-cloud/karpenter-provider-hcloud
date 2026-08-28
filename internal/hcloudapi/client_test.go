package hcloudapi

import (
	"strings"
	"testing"
)

const goodToken = "abcdefghij0123456789ABCDEFGHIJabcdefghij0123456789ABCDEFGHIJ0123"

// TestNewClientFromEnvValidatesTheToken.
//
// Both checks exist so a malformed secret is named at STARTUP. Without them the
// failure arrives as an hcloud-go error carrying no Hetzner error code, which
// Classify treats as transient, so the provider retries forever with nothing on
// any NodeClass saying why. The trailing-newline case is the common one.
func TestNewClientFromEnvValidatesTheToken(t *testing.T) {
	for _, tc := range []struct {
		name    string
		token   string
		wantErr bool
	}{
		{"valid", goodToken, false},
		{"unset", "", true},
		{"truncated", goodToken[:32], true},
		{"trailingNewline", goodToken + "\n", true},
		{"trailingSpace", goodToken + " ", true},
		{"rightLengthWithQuote", `"` + goodToken[:62] + `"`, true},
		{"rightLengthWithNewlineInside", goodToken[:32] + "\n" + goodToken[33:], true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(TokenEnvVar, tc.token)

			c, err := NewClientFromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				// The token is a credential: it must not reach an error string,
				// which ends up in logs and on NodeClass conditions.
				if tc.token != "" && strings.Contains(err.Error(), strings.TrimSpace(tc.token)) {
					t.Error("the error message contains the token")
				}
				return
			}
			if err != nil {
				t.Fatalf("NewClientFromEnv: %v", err)
			}
			if c == nil {
				t.Fatal("NewClientFromEnv returned no client and no error")
			}
		})
	}
}

// TestTokenLengthMatchesHetzner guards the constant against being "fixed" to
// whatever a malformed secret happened to contain.
func TestTokenLengthMatchesHetzner(t *testing.T) {
	if tokenLength != 64 {
		t.Errorf("tokenLength = %d, want 64, which is what the Hetzner Cloud console issues", tokenLength)
	}
	if len(goodToken) != tokenLength {
		t.Fatalf("test fixture is %d characters, not %d", len(goodToken), tokenLength)
	}
}

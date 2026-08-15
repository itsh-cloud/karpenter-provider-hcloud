package hcloudapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		code hcloud.ErrorCode
		want Class
	}{
		// The founding failure. On 2026-08-11 an entire server-type line went
		// out of stock in one location and the fallback that followed was
		// never undone. Getting this wrong is the whole ballgame.
		{"resource_unavailable", hcloud.ErrorCodeResourceUnavailable, ClassCapacity},
		// Missing from at least one third-party provider. A spread placement
		// group that is full, or a location with no host that fits, both
		// report this; treating it as generic means retrying the same doomed
		// combination until something else changes.
		{"placement_error", hcloud.ErrorCodePlacementError, ClassCapacity},
		{"no_space_left_in_location", hcloud.ErrorCodeNoSpaceLeftInLocation, ClassCapacity},
		{"maintenance", hcloud.ErrorCodeMaintenance, ClassCapacity},

		// Not stock: another server type will not help.
		{"resource_limit_exceeded", hcloud.ErrorCodeResourceLimitExceeded, ClassQuota},

		// Must never be capacity. Rate limiting is caused by our own request
		// volume, so suppressing offerings for it would convert a slowdown
		// into a self-inflicted capacity outage.
		{"rate_limit_exceeded", hcloud.ErrorCodeRateLimitExceeded, ClassTransient},
		{"service_error", hcloud.ErrorCodeServiceError, ClassTransient},
		{"server_error", hcloud.ErrorCodeServerError, ClassTransient},
		{"bad_gateway", hcloud.ErrorCodeBadGateway, ClassTransient},
		{"timeout", hcloud.ErrorCodeTimeout, ClassTransient},
		{"conflict", hcloud.ErrorCodeConflict, ClassTransient},
		{"locked", hcloud.ErrorCodeLocked, ClassTransient},
		{"resource_locked", hcloud.ErrorCodeResourceLocked, ClassTransient},
		{"protected", hcloud.ErrorCodeProtected, ClassTransient},
		{"json_error", hcloud.ErrorCodeJSONError, ClassTransient},
		{"robot_unavailable", hcloud.ErrorCodeRobotUnavailable, ClassTransient},
		{"unknown_error", hcloud.ErrorCodeUnknownError, ClassTransient},

		{"invalid_input", hcloud.ErrorCodeInvalidInput, ClassConfig},
		{"not_found", hcloud.ErrorCodeNotFound, ClassConfig},
		{"unsupported_error", hcloud.ErrorUnsupportedError, ClassConfig},
		{"invalid_server_type", hcloud.ErrorCodeInvalidServerType, ClassConfig},
		{"uniqueness_error", hcloud.ErrorCodeUniquenessError, ClassConfig},

		// Retrying cannot fix a wrong credential, and each attempt burns
		// rate limit that provisioning needs.
		{"forbidden", hcloud.ErrorCodeForbidden, ClassFatal},
		{"unauthorized", hcloud.ErrorCodeUnauthorized, ClassFatal},
		{"token_readonly", hcloud.ErrorCodeTokenReadonly, ClassFatal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := hcloud.Error{Code: tt.code, Message: "test"}
			if got := Classify(err); got != tt.want {
				t.Errorf("Classify(%s) = %s, want %s", tt.code, got, tt.want)
			}
			if !IsKnownCode(err) {
				t.Errorf("%s should be a known code", tt.code)
			}
		})
	}
}

// TestClassifyActionError is the regression test for the subtle half of the
// capacity-handling bug.
//
// Server creation is asynchronous. The API returns an Action, and the
// placement decision fails later, arriving as hcloud.ActionError. That is a
// DIFFERENT type from hcloud.Error, and its Code field is a plain string
// rather than an hcloud.ErrorCode, so hcloud.IsError cannot see it. If the
// classifier only understands hcloud.Error, every async stockout is
// misclassified and the provider retries an unorderable server type.
func TestClassifyActionError(t *testing.T) {
	for _, code := range []string{
		string(hcloud.ErrorCodeResourceUnavailable),
		string(hcloud.ErrorCodePlacementError),
	} {
		t.Run(code, func(t *testing.T) {
			err := hcloud.ActionError{Code: code, Message: "async failure"}

			if got := Classify(err); got != ClassCapacity {
				t.Errorf("Classify(ActionError{%s}) = %s, want capacity", code, got)
			}

			// Demonstrate why Code cannot be built on hcloud.IsError. If this
			// ever starts returning true, hcloud-go has unified the two error
			// types and the extra branch could be simplified.
			if hcloud.IsError(err, hcloud.ErrorCode(code)) {
				t.Errorf("hcloud.IsError now matches ActionError; revisit Code()")
			}
		})
	}
}

// TestClassifyWrapped: errors travel up through fmt.Errorf, so extraction must
// follow the chain rather than type-asserting the top-level value.
func TestClassifyWrapped(t *testing.T) {
	base := hcloud.Error{Code: hcloud.ErrorCodeResourceUnavailable}
	wrapped := fmt.Errorf("creating server: %w", fmt.Errorf("hcloud: %w", base))

	if got := Classify(wrapped); got != ClassCapacity {
		t.Errorf("Classify(wrapped) = %s, want capacity", got)
	}

	wrappedAction := fmt.Errorf("waiting for action: %w",
		hcloud.ActionError{Code: string(hcloud.ErrorCodePlacementError)})
	if got := Classify(wrappedAction); got != ClassCapacity {
		t.Errorf("Classify(wrapped ActionError) = %s, want capacity", got)
	}
}

// TestClassifyNonHetznerErrors: a dial timeout or a cancelled context is not a
// capacity signal and must not suppress an offering.
func TestClassifyNonHetznerErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"plain", errors.New("boom")},
		{"net timeout", &net.OpError{Op: "dial", Err: errors.New("i/o timeout")}},
		{"context canceled", context.Canceled},
		{"deadline exceeded", context.DeadlineExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := ClassTransient
			if tt.err == nil {
				want = ClassNone
			}
			if got := Classify(tt.err); got != want {
				t.Errorf("Classify(%v) = %s, want %s", tt.err, got, want)
			}
			if IsKnownCode(tt.err) {
				t.Errorf("%v should not be a known Hetzner code", tt.err)
			}
		})
	}
}

// TestUnknownCodeIsTransient: a code Hetzner adds later must not be guessed as
// capacity (which would suppress a healthy offering) or config (which would
// fail a NodeClaim that might have succeeded).
func TestUnknownCodeIsTransient(t *testing.T) {
	err := hcloud.Error{Code: hcloud.ErrorCode("some_future_code")}

	if got := Classify(err); got != ClassTransient {
		t.Errorf("Classify(unknown) = %s, want transient", got)
	}
	if IsKnownCode(err) {
		t.Error("unknown code reported as known; callers rely on this to " +
			"surface new codes rather than silently absorbing them")
	}
}

// TestCodeExtraction covers both concrete shapes.
func TestCodeExtraction(t *testing.T) {
	if got, ok := Code(hcloud.Error{Code: hcloud.ErrorCodeNotFound}); !ok || got != "not_found" {
		t.Errorf("Code(Error) = %q, %v", got, ok)
	}
	if got, ok := Code(hcloud.ActionError{Code: "placement_error"}); !ok || got != "placement_error" {
		t.Errorf("Code(ActionError) = %q, %v", got, ok)
	}
	if _, ok := Code(errors.New("x")); ok {
		t.Error("Code() claimed a plain error carried a Hetzner code")
	}
}

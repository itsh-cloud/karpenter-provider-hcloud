package hcloudapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// testToken's first half is what rateLimitMessage echoes. The characters are
// made up.
const (
	testToken        = "AbCdEf0123456789AbCdEf0123456789ZyXwVu9876543210ZyXwVu9876543210"
	rateLimitMessage = "Limit of '3600' requests per hour for token 'AbCdEf0123456789AbCdEf0123456789...' reached."
)

// useToken makes token the one redaction knows for this test; "" means none.
func useToken(t *testing.T, token string) {
	t.Helper()
	prev := apiToken.Load()
	if token == "" {
		apiToken.Store(nil)
	} else {
		apiToken.Store(&token)
	}
	t.Cleanup(func() { apiToken.Store(prev) })
}

// leaksToken reports whether any 8 consecutive characters of testToken survive.
func leaksToken(s string) bool {
	for i := 0; i+8 <= len(testToken); i++ {
		if strings.Contains(s, testToken[i:i+8]) {
			return true
		}
	}
	return false
}

func rateLimited(msg string) hcloud.Error {
	return hcloud.Error{Code: hcloud.ErrorCodeRateLimitExceeded, Message: msg}
}

// TestRedactRemovesTheToken: the message reaches logs, events and NodeClass
// conditions, and half a token is still a credential leak. Each shape must
// come out clean however Hetzner words the message and however the error is
// assembled, and must still classify, or a rate limit would read as an
// unknown failure.
func TestRedactRemovesTheToken(t *testing.T) {
	useToken(t, testToken)
	notFound := hcloud.Error{Code: hcloud.ErrorCodeNotFound, Message: "image not found"}

	for _, tc := range []struct {
		name string
		err  error
		// The code redact's output must carry; rate_limit_exceeded if empty.
		code hcloud.ErrorCode
	}{
		{"singleQuoted", rateLimited(rateLimitMessage), ""},
		{"doubleQuoted", rateLimited(`Limit of "3600" requests per hour for token "AbCdEf0123456789AbCdEf0123456789..." reached.`), ""},
		{"colon", rateLimited("Limit of 3600 requests per hour for token: AbCdEf0123456789AbCdEf0123456789... reached."), ""},
		{"unquoted", rateLimited("Limit of 3600 requests per hour reached for AbCdEf0123456789AbCdEf0123456789"), ""},
		{"notThePrefix", rateLimited("Limit reached for the token ending ZyXwVu9876543210ZyXwVu98"), ""},
		{"unterminated", rateLimited("for token 'AbCdEf0123456789AbCd"), ""},
		{"wrapped", fmt.Errorf("hcloud: %w", rateLimited(rateLimitMessage)), ""},
		{"pointer", &hcloud.Error{Code: hcloud.ErrorCodeRateLimitExceeded, Message: rateLimitMessage}, ""},
		{"actionError", hcloud.ActionError{Code: string(hcloud.ErrorCodeRateLimitExceeded), Message: rateLimitMessage}, ""},
		// Trees: the first hcloud error has nothing to redact, a later one does.
		{"joined", errors.Join(notFound, rateLimited(rateLimitMessage)), hcloud.ErrorCodeNotFound},
		{"multiWrapped", fmt.Errorf("%w %w", notFound, rateLimited(rateLimitMessage)), hcloud.ErrorCodeNotFound},
		{"joinedActionError", errors.Join(notFound,
			hcloud.ActionError{Code: string(hcloud.ErrorCodePlacementError), Message: rateLimitMessage}), hcloud.ErrorCodeNotFound},
		{"joinedOtherWording", errors.Join(notFound,
			rateLimited("Limit of 3600 requests per hour for token: AbCdEf0123456789AbCdEf0123456789... reached.")), hcloud.ErrorCodeNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := redact(tc.err)
			if leaksToken(got.Error()) {
				t.Errorf("redact left the token in %q", got.Error())
			}
			if !strings.Contains(got.Error(), "[redacted]") {
				t.Errorf("redact(%q) = %q, want the token replaced", tc.err, got)
			}
			want := cmp.Or(tc.code, hcloud.ErrorCodeRateLimitExceeded)
			if code, _ := Code(got); code != string(want) {
				t.Errorf("Code = %q after redaction, want %q", code, want)
			}
		})
	}

	// A lone error is rebuilt rather than flattened, so its text stays as
	// Hetzner wrote it, with the code once.
	if got, want := redact(rateLimited("for token: AbCdEf0123456789AbCdEf0123456789")).Error(),
		"for token: [redacted] (rate_limit_exceeded)"; got != want {
		t.Errorf("redact = %q, want %q", got, want)
	}

	// A pointer comes back as the value hcloud.IsError looks for.
	if got := redact(&hcloud.Error{Code: hcloud.ErrorCodeRateLimitExceeded, Message: rateLimitMessage}); !hcloud.IsError(got, hcloud.ErrorCodeRateLimitExceeded) {
		t.Errorf("hcloud.IsError does not match redact's output %#v", got)
	}
}

// TestRedactWithoutTheToken: a client this package did not build still has
// Hetzner's known wording covered.
func TestRedactWithoutTheToken(t *testing.T) {
	useToken(t, "")
	got := redact(rateLimited(rateLimitMessage))
	if leaksToken(got.Error()) || !strings.Contains(got.Error(), "[redacted]") {
		t.Errorf("redact = %q, want the quoted token replaced", got)
	}
}

// TestRedactLeavesCleanErrorsReadable: nothing to remove must not cost the
// message, the code, or errors.Is on a non-Hetzner error.
func TestRedactLeavesCleanErrorsReadable(t *testing.T) {
	useToken(t, testToken)

	if redact(nil) != nil {
		t.Error("redact(nil) is not nil")
	}
	deadline := fmt.Errorf("waiting for action: %w", context.DeadlineExceeded)
	if got := redact(deadline); !errors.Is(got, context.DeadlineExceeded) || got.Error() != deadline.Error() {
		t.Errorf("redact(%q) = %q, want it unchanged", deadline, got)
	}
	notFound := redact(hcloud.Error{Code: hcloud.ErrorCodeNotFound, Message: "image not found"})
	if notFound.Error() != "image not found (not_found)" || !hcloud.IsError(notFound, hcloud.ErrorCodeNotFound) {
		t.Errorf("redact = %q, want the not_found error intact", notFound)
	}
}

// TestRedactScrubsOnlyHetznerText: a selector name is NodeClass input and can
// reach an error through a request URL. Scrubbed by value, a name holding
// guessed token characters would come back "[redacted]" exactly when the guess
// is right, telling a NodeClass author the token without reading the Secret.
func TestRedactScrubsOnlyHetznerText(t *testing.T) {
	useToken(t, testToken)
	guess := testToken[:12]

	transport := fmt.Errorf("resolving image %q: %w", guess, &url.Error{
		Op:  "Get",
		URL: "https://api.hetzner.cloud/v1/images?architecture=x86&name=" + guess,
		Err: errors.New("connection reset by peer"),
	})
	if got := redact(transport); got.Error() != transport.Error() {
		t.Errorf("redact(%q) = %q; the selector name was scrubbed, which answers the guess", transport, got)
	}

	hetzner := hcloud.Error{Code: hcloud.ErrorCodeInvalidInput, Message: "image " + guess + " not found"}
	if got := redact(hetzner); strings.Contains(got.Error(), guess) {
		t.Errorf("redact(%q) = %q, want the token run in Hetzner's message removed", hetzner, got)
	}

	// Both in one tree: the Hetzner message is cleaned, the name is not.
	tree := errors.Join(fmt.Errorf("resolving image %q", guess), hetzner)
	got := redact(tree).Error()
	if !strings.Contains(got, `resolving image "`+guess+`"`) || strings.Count(got, guess) != 1 {
		t.Errorf("redact(%q) = %q, want the name kept and only Hetzner's text scrubbed", tree, got)
	}
}

// assertDetached fails if err still carries an hcloud response or action. The
// response's request holds the whole token in its Authorization header, and
// an action carries the unredacted message.
func assertDetached(t *testing.T, err error) {
	t.Helper()
	var apiErr hcloud.Error
	if errors.As(err, &apiErr) && apiErr.Response() != nil {
		t.Error("the hcloud.Error still carries its response")
	}
	var actionErr hcloud.ActionError
	if errors.As(err, &actionErr) && actionErr.Action() != nil {
		t.Error("the hcloud.ActionError still carries its action")
	}
}

func TestRedactDetaches(t *testing.T) {
	useToken(t, testToken)
	failed := &hcloud.Action{ID: 42, Status: hcloud.ActionStatusError, ErrorCode: "placement_error", ErrorMessage: "no host"}

	t.Run("actionError", func(t *testing.T) {
		got := redact(failed.Error())
		assertDetached(t, got)
		var actionErr hcloud.ActionError
		if !errors.As(got, &actionErr) {
			t.Fatalf("redact = %#v, want a lone ActionError kept as one", got)
		}
		if !strings.Contains(got.Error(), "action 42") {
			t.Errorf("redact = %q, want the action id kept for diagnosis", got)
		}
	})

	// Nothing to scrub, but an attached error in a tree is still flattened.
	t.Run("joined", func(t *testing.T) {
		got := redact(errors.Join(errors.New("creating server"), failed.Error()))
		assertDetached(t, got)
		if code, _ := Code(got); code != "placement_error" {
			t.Errorf("Code = %q after flattening, want placement_error", code)
		}
	})

	t.Run("response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Correlation-Id", "corr-1234")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"code":"service_error","message":"service unavailable","details":{}}}`)
		}))
		defer srv.Close()

		client := hcloud.NewClient(hcloud.WithToken(testToken), hcloud.WithEndpoint(srv.URL),
			hcloud.WithRetryOpts(hcloud.RetryOpts{MaxRetries: 0}))
		_, err := NewServers(client).Get(context.Background(), 1)
		if err == nil {
			t.Fatal("Get succeeded against a 503")
		}
		assertDetached(t, err)
		if want := "getting server 1: service unavailable (correlation id corr-1234) (service_error)"; err.Error() != want {
			t.Errorf("err = %q, want %q", err, want)
		}
	})
}

// TestNewClientFromEnvCapturesTheToken: scrubbing by value only works if
// redaction knows the value.
func TestNewClientFromEnvCapturesTheToken(t *testing.T) {
	useToken(t, "")
	t.Setenv(TokenEnvVar, goodToken)
	if _, err := NewClientFromEnv(); err != nil {
		t.Fatalf("NewClientFromEnv: %v", err)
	}
	if got := apiToken.Load(); got == nil || *got != goodToken {
		t.Error("NewClientFromEnv did not hand the token to redaction")
	}
}

// TestRedactAtTheWrapSite drives a real hcloud-go client into a 429, so the
// test covers what hcloud-go actually builds from the response rather than a
// hand-made error.
func TestRedactAtTheWrapSite(t *testing.T) {
	for _, tc := range []struct {
		name    string
		known   string
		message string
	}{
		{"hetznerWording", "", rateLimitMessage},
		{"otherWording", testToken, `Limit reached for token "AbCdEf0123456789AbCdEf0123456789..."`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useToken(t, tc.known)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// Without it hcloud-go ignores the body and reports a bare status code.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprintf(w, `{"error":{"code":"rate_limit_exceeded","message":%q,"details":{}}}`, tc.message)
			}))
			defer srv.Close()

			client := hcloud.NewClient(
				hcloud.WithToken(testToken),
				hcloud.WithEndpoint(srv.URL),
				hcloud.WithRetryOpts(hcloud.RetryOpts{MaxRetries: 0}),
			)
			servers := NewServers(client)
			for name, call := range map[string]func() error{
				"Get":  func() error { _, err := servers.Get(context.Background(), 1); return err },
				"List": func() error { _, err := servers.List(context.Background(), "a=b"); return err },
			} {
				err := call()
				if err == nil {
					t.Fatalf("%s succeeded against a 429", name)
				}
				if !hcloud.IsError(err, hcloud.ErrorCodeRateLimitExceeded) {
					t.Fatalf("%s: err = %v, want a rate_limit_exceeded hcloud.Error", name, err)
				}
				if !strings.Contains(err.Error(), "[redacted]") {
					t.Errorf("%s: err = %q, want the token redacted", name, err)
				}
				if leaksToken(err.Error()) {
					t.Errorf("%s: the token reached the error: %q", name, err)
				}
				assertDetached(t, err)
			}
		})
	}
}

package hcloudapi

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// apiToken is the token NewClientFromEnv accepted. Kept so Hetzner's message
// text is scrubbed of it by value, whatever the wording.
var apiToken atomic.Pointer[string]

// minEcho is the shortest run of the token scrubbed by value. Twelve random
// alphanumerics do not turn up in a message by chance.
const minEcho = 12

// quotedToken matches the token Hetzner quotes back in some messages, such as
// "Limit of '3600' requests per hour for token '<prefix>...' reached.". It
// covers a client whose token this package never saw. The closing quote is
// optional so a truncated message redacts to its end.
var quotedToken = regexp.MustCompile(`(?i)(token\s*')[^']*'?`)

// redact makes an hcloud-go error safe to leave this package, which every one
// must: from here it reaches logs, events, NodeClaim errors and NodeClass
// conditions.
//
// Two things leak. Hetzner echoes part of the token in some messages, and an
// hcloud.Error carries its response, whose request holds the whole token in
// the Authorization header. So the hcloud error in a plain wrap chain is
// rebuilt detached and scrubbed, its wrappers dropped since their text embeds
// the original. Then the final string is scrubbed: if anything is still
// removed, or the tree holds an error this could not rebuild, the result is
// flattened to a fresh error. Either way the code survives, so Classify and
// hcloud.IsError still match.
//
// Only Hetzner's message text is scrubbed by value. Everything else, such as a
// transport error's URL, can carry NodeClass selector names, and "[redacted]"
// replacing a name would confirm guessed token characters to anyone who can
// write a NodeClass. The token never travels in a URL, so nothing is lost.
func redact(err error) error {
	if err == nil {
		return nil
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		if rebuilt, ok := rebuild(e); ok {
			err = rebuilt
			break
		}
	}
	msg := err.Error()
	clean := scrubQuoted(scrubMessages(err, msg))
	if clean == msg && !attached(err) {
		return err
	}
	code, ok := Code(err)
	if !ok {
		return errors.New(clean)
	}
	return hcloud.Error{Code: hcloud.ErrorCode(code), Message: clean}
}

// rebuild returns a detached, scrubbed copy of err if it is itself an hcloud
// error. The correlation id and action id move into the message, so a support
// request can still name the call.
func rebuild(err error) (error, bool) {
	switch e := hcloudNode(err).(type) {
	case hcloud.Error:
		msg := scrub(e.Message)
		if resp := e.Response(); resp != nil && resp.Response != nil {
			if id := resp.Header.Get("X-Correlation-Id"); id != "" {
				msg = fmt.Sprintf("%s (correlation id %s)", msg, id)
			}
		}
		return hcloud.Error{Code: e.Code, Message: msg}, true
	case hcloud.ActionError:
		msg := scrub(e.Message)
		if a := e.Action(); a != nil {
			msg = fmt.Sprintf("%s (action %d)", msg, a.ID)
		}
		return hcloud.ActionError{Code: e.Code, Message: msg}, true
	}
	return nil, false
}

// hcloudNode returns err itself if it is an hcloud error, dereferencing a
// pointer to one, and nil otherwise. It looks at this one node, never the
// chain behind it; callers walk the tree themselves.
func hcloudNode(err error) any {
	switch e := err.(type) { //nolint:errorlint // this node only: callers walk the tree
	case hcloud.Error, hcloud.ActionError:
		return e
	case *hcloud.Error:
		if e != nil {
			return *e
		}
	case *hcloud.ActionError:
		if e != nil {
			return *e
		}
	}
	return nil
}

// eachNode calls fn on every error in err's tree.
func eachNode(err error, fn func(error)) {
	if err == nil {
		return
	}
	fn(err)
	switch e := err.(type) { //nolint:errorlint // walking the tree by hand is the point
	case interface{ Unwrap() error }:
		eachNode(e.Unwrap(), fn)
	case interface{ Unwrap() []error }:
		for _, w := range e.Unwrap() {
			eachNode(w, fn)
		}
	}
}

// attached reports whether any error in the tree still holds a response or an
// action.
func attached(err error) bool {
	found := false
	eachNode(err, func(e error) {
		switch e := hcloudNode(e).(type) {
		case hcloud.Error:
			found = found || e.Response() != nil
		case hcloud.ActionError:
			found = found || e.Action() != nil
		}
	})
	return found
}

// scrubMessages scrubs by value, within msg, the text of each Hetzner message
// in err's tree, and nothing else.
func scrubMessages(err error, msg string) string {
	eachNode(err, func(e error) {
		var m string
		switch e := hcloudNode(e).(type) {
		case hcloud.Error:
			m = e.Message
		case hcloud.ActionError:
			m = e.Message
		}
		if s := scrubToken(m); s != m {
			msg = strings.ReplaceAll(msg, m, s)
		}
	})
	return msg
}

// scrub cleans Hetzner message text, by value and by wording.
func scrub(msg string) string {
	return scrubQuoted(scrubToken(msg))
}

func scrubQuoted(msg string) string {
	return quotedToken.ReplaceAllString(msg, "${1}[redacted]'")
}

// scrubToken replaces every run of minEcho or more consecutive characters of
// the token, not only its prefix, so an echo of any part of it is caught.
func scrubToken(msg string) string {
	tokenPtr := apiToken.Load()
	if tokenPtr == nil {
		return msg
	}
	token := *tokenPtr
	var b strings.Builder
	changed := false
	for i := 0; i < len(msg); {
		if n := longestRun(msg[i:], token); n >= minEcho {
			b.WriteString("[redacted]")
			i += n
			changed = true
			continue
		}
		b.WriteByte(msg[i])
		i++
	}
	if !changed {
		return msg
	}
	return b.String()
}

// longestRun is the length of the longest prefix of s found anywhere in token.
func longestRun(s, token string) int {
	best := 0
	for j := range len(token) {
		n := 0
		for n < len(s) && j+n < len(token) && s[n] == token[j+n] {
			n++
		}
		best = max(best, n)
	}
	return best
}

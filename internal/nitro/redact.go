package nitro

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Endpoint URLs are credentials: providers put the API key in the userinfo, in a path segment or in a
// query parameter. They must never reach a log line, an error stored as networks.last_error, or /status.
// Three routes carry them out of this package: net/url reports transport failures as *url.Error, whose
// message is the whole URL; a provider can echo the request URL back in an error body or JSON-RPC
// message; and a URL can be built into a message by hand. scrubber closes all three, replacing every
// part that could carry a secret with the endpoint's index.

// minSecret is the shortest URL fragment worth replacing. Anything shorter is not a credential and
// replacing it would mangle unrelated text.
const minSecret = 6

// scrubber rewrites the URLs of one endpoint out of text and errors, replacing them with its name.
type scrubber struct {
	name string
	// secrets are the fragments to replace, longest first, so a whole URL is replaced before its parts.
	secrets []string
}

// newScrubber builds the scrubber of the endpoint at index over every URL it is configured with.
func newScrubber(index int, urls ...string) *scrubber {
	s := &scrubber{name: fmt.Sprintf("endpoint %d", index)}
	seen := map[string]bool{}
	add := func(v string) {
		v = strings.TrimSpace(v)
		if len(v) < minSecret || seen[v] {
			return
		}
		seen[v] = true
		s.secrets = append(s.secrets, v)
	}
	for _, raw := range urls {
		if raw == "" {
			continue
		}
		add(raw)
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		// The whole URL without its scheme, which is how a proxy or provider often echoes it back.
		add(strings.TrimPrefix(raw, u.Scheme+"://"))
		if u.User != nil {
			add(u.User.String())
			if pw, ok := u.User.Password(); ok {
				add(pw)
			}
			add(u.User.Username())
		}
		for _, seg := range strings.Split(u.EscapedPath(), "/") {
			add(seg)
		}
		add(u.RawQuery)
		for _, values := range u.Query() {
			for _, v := range values {
				add(v)
			}
		}
	}
	// Longest first: replacing the whole URL before its parts keeps one occurrence from turning into
	// several endpoint names.
	sort.SliceStable(s.secrets, func(i, j int) bool { return len(s.secrets[i]) > len(s.secrets[j]) })
	return s
}

// text replaces every known secret in v with the endpoint's name.
func (s *scrubber) text(v string) string {
	if s == nil {
		return v
	}
	for _, secret := range s.secrets {
		v = strings.ReplaceAll(v, secret, s.name)
	}
	return v
}

// errorf builds a sanitized error: the format is trusted, the arguments are not.
func (s *scrubber) errorf(format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	return &scrubbedError{msg: s.text(err.Error()), err: errors.Unwrap(err)}
}

// wrap sanitizes an error from outside this package. A *url.Error is rebuilt without its URL and
// unwrapped past, so nothing can reach the value that would print it again; every other error keeps its
// chain, so errors.Is and errors.As still see through it.
func (s *scrubber) wrap(err error) error {
	if err == nil {
		return nil
	}
	if s == nil {
		return err
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		inner := ue.Err
		if inner == nil {
			inner = errors.New("unknown error")
		}
		return &scrubbedError{msg: fmt.Sprintf("%s %s: %s", ue.Op, s.name, s.text(inner.Error())), err: inner}
	}
	msg := s.text(err.Error())
	if msg == err.Error() {
		return err
	}
	return &scrubbedError{msg: msg, err: err}
}

// rpcError returns a copy of a JSON-RPC error whose message and data carry no URL.
func (s *scrubber) rpcError(e *RPCError) *RPCError {
	if e == nil || s == nil {
		return e
	}
	out := &RPCError{Code: e.Code, Message: s.text(e.Message)}
	if len(e.Data) > 0 {
		out.Data = []byte(s.text(string(e.Data)))
	}
	return out
}

// scrubbedError is an error whose message has had an endpoint's URL taken out. Unwrap keeps errors.Is
// and errors.As working on the cause.
type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }

func (e *scrubbedError) Unwrap() error { return e.err }

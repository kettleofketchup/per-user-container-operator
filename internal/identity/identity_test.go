package identity

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

func TestExtractRejections(t *testing.T) {
	const H = "X-User-Id"
	cases := []struct {
		name   string
		h      http.Header
		want   Reason
		status int
	}{
		{"missing", hdr(), ReasonMissing, 401},
		{"empty", hdr(H, ""), ReasonEmpty, 401},
		{"whitespace only", hdr(H, "   "), ReasonEmpty, 401},
		{"too long", hdr(H, strings.Repeat("a", 257)), ReasonTooLong, 401},
		{"duplicate", hdr(H, "alice", H, "bob"), ReasonDuplicate, 400},
		// The OTHER merge shape, and the one Task 15 Step 2's blocking question
		// actually describes. http.Header.Values returns ONE element for
		// "X-User-Id: alice, bob" — Go's server does not split comma-joined
		// values — so the len != 1 check never fires, the value is printable
		// ASCII under maxLength, and Extract mints a well-formed userKey for
		// "alice, bob". A client that merges rather than appends therefore
		// lands the real user in a fresh empty workspace on every request
		// carrying an attacker-supplied copy, with the duplicate rejection
		// never reached — and Task 17 Step 4's duplicated-header variant then
		// passes for the wrong reason, because the merged key resolves to a
		// third workspace that is neither the attacker's nor the victim's.
		{"comma joined", hdr(H, "alice, bob"), ReasonDuplicate, 400},
		{"non printable", hdr(H, "al\x00ice"), ReasonInvalid, 400},
		{"high byte", hdr(H, "ali\xffce"), ReasonInvalid, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Extract(c.h, H, 256)
			if err == nil {
				t.Fatalf("expected rejection, got identity %q", got)
			}
			var rej *Rejection
			if !errors.As(err, &rej) {
				t.Fatalf("expected *Rejection, got %T", err)
			}
			if rej.Reason != c.want {
				t.Fatalf("reason = %q, want %q", rej.Reason, c.want)
			}
			if rej.Status != c.status {
				t.Fatalf("status = %d, want %d", rej.Status, c.status)
			}
			if got != "" {
				t.Fatalf("rejected path returned identity %q; must be empty", got)
			}
		})
	}
}

func TestExtractAccepts(t *testing.T) {
	got, err := Extract(hdr("X-User-Id", "alice@corp.example"), "X-User-Id", 256)
	if err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
	if got != "alice@corp.example" {
		t.Fatalf("identity = %q", got)
	}
}

// Never truncate: two identities sharing a prefix must not collapse.
func TestExtractNeverTruncates(t *testing.T) {
	a := strings.Repeat("a", 300)
	if _, err := Extract(hdr("X-User-Id", a), "X-User-Id", 256); err == nil {
		t.Fatal("over-length identity was accepted; truncation would collide users")
	}
}

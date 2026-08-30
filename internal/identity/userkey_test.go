package identity

import "testing"

// Naive sanitizing collides: a.b and a-b both become a-b, which hands one user
// another's workspace.
func TestUserKeyDistinguishesSanitizerCollisions(t *testing.T) {
	if UserKey("ns", "app", "a.b") == UserKey("ns", "app", "a-b") {
		t.Fatal("a.b and a-b produced the same userKey")
	}
}

// Components must not be concatenation-ambiguous: ("ns","ab","c") and
// ("ns","a","bc") must differ.
func TestUserKeySeparatorIsUnambiguous(t *testing.T) {
	if UserKey("ns", "ab", "c") == UserKey("ns", "a", "bc") {
		t.Fatal("component boundaries are ambiguous")
	}
}

func TestUserKeyIsStableAndScoped(t *testing.T) {
	a := UserKey("ns", "app", "alice")
	if a != UserKey("ns", "app", "alice") {
		t.Fatal("userKey is not deterministic")
	}
	if a == UserKey("other", "app", "alice") {
		t.Fatal("userKey is not namespace-scoped")
	}
	if a == UserKey("ns", "other", "alice") {
		t.Fatal("userKey is not app-scoped")
	}
}

func TestUserKeyShape(t *testing.T) {
	k := UserKey("ns", "app", "alice")
	if len(k) != 18 {
		t.Fatalf("len(userKey) = %d, want 18", len(k))
	}
	if k[:2] != "u-" {
		t.Fatalf("userKey = %q, want u- prefix", k)
	}
	for _, c := range k[2:] {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !isHex {
			t.Fatalf("userKey has non-hex byte %q", c)
		}
	}
}

// The pod-name budget: app(27) + 1 + userKey(18) + 1 + hash(10) + 1 + rand(5) = 63.
func TestChildNameFitsPodBudget(t *testing.T) {
	app := "a123456789012345678901234567"[:27]
	name := ChildName(app, UserKey("ns", app, "alice"))
	if len(name)+1+10+1+5 > 63 {
		t.Fatalf("pod name budget exceeded: deployment name %d chars", len(name))
	}
}

// Frozen vectors. These are LITERALS, deliberately: every other test in this
// file is relational (a != b, deterministic, ns/app-scoped) and survives
// swapping sha256 for sha512, changing the \x00 separator, hexing a different
// slice of the digest, or reordering the three components. A shape check
// (len 18, "u-" prefix) survives all of that too. This is the only test in the
// plan that goes red when the frozen derivation changes.
//
// A diff here is a FLEET-WIDE RE-KEY: every existing user cold-starts onto a
// fresh empty volume, with no request failing and nothing in any log. It is
// never a test to update to match new behaviour.
func TestUserKeyGoldenVector(t *testing.T) {
	for _, c := range []struct{ ns, app, id, want string }{
		{"open-webui", "workspace-app", "alice", "u-e7f5a178dc8eb805"},
		{"open-webui", "workspace-app", "bob", "u-52b58b98108c550e"},
		{"ns", "app", "a.b", "u-d3b9d38a56427d5d"},
		{"ns", "app", "a-b", "u-d817b0928a973b26"},
	} {
		if got := UserKey(c.ns, c.app, c.id); got != c.want {
			t.Fatalf("UserKey(%q,%q,%q) = %q, want %q — the frozen derivation changed; this re-keys every user onto an empty volume", c.ns, c.app, c.id, got, c.want)
		}
	}
}

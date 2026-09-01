// Package identity extracts a caller's identity from an HTTP header and
// enforces that the extraction is fail-closed: every rejection path returns
// an empty identity and an error, and there is no success path that yields a
// shared or default identity. This package has no Kubernetes dependency so
// its safety properties can be unit-tested in isolation from cluster state.
package identity

import (
	"fmt"
	"net/http"
	"strings"
)

// Reason identifies why an identity header was rejected.
type Reason string

// Rejection reasons returned by Extract.
const (
	// ReasonMissing means the header was not present at all.
	ReasonMissing Reason = "missing"
	// ReasonEmpty means the header was present but empty or whitespace-only.
	ReasonEmpty Reason = "empty"
	// ReasonTooLong means the header value exceeded the configured maximum length.
	ReasonTooLong Reason = "too_long"
	// ReasonDuplicate means the header appeared more than once, or a single
	// value looked like a comma-joined merge of more than one value.
	ReasonDuplicate Reason = "duplicate"
	// ReasonInvalid means the header value contained non-printable-ASCII bytes.
	ReasonInvalid Reason = "invalid"
)

// Rejection is returned by Extract whenever the identity header cannot be
// trusted. It carries the HTTP status the caller should respond with.
type Rejection struct {
	Reason Reason
	Status int
}

func (r *Rejection) Error() string { return fmt.Sprintf("identity rejected: %s", r.Reason) }

func reject(reason Reason, status int) (string, error) {
	return "", &Rejection{Reason: reason, Status: status}
}

// Extract returns the raw identity or a *Rejection. It never truncates,
// never returns a value for an absent header, and never has a success path
// that yields a shared or default identity.
func Extract(h http.Header, header string, maxLength int) (string, error) {
	values := h.Values(header)
	switch {
	case len(values) == 0:
		return reject(ReasonMissing, http.StatusUnauthorized)
	case len(values) > 1:
		// Intermediaries append rather than replace. "First" and "last" are
		// both silent impersonation primitives, so refuse instead of choosing.
		return reject(ReasonDuplicate, http.StatusBadRequest)
	}
	raw := values[0]
	if strings.Contains(raw, ",") {
		// The merge shape len(values) cannot see: an intermediary that joins
		// two copies into one header line yields a single printable-ASCII
		// value that hashes to a userKey belonging to neither party. Neither
		// identity header this operator serves ever contains a comma, so
		// refusing one costs nothing and closes the rejection under both
		// merge shapes.
		return reject(ReasonDuplicate, http.StatusBadRequest)
	}
	if strings.TrimSpace(raw) == "" {
		return reject(ReasonEmpty, http.StatusUnauthorized)
	}
	if len(raw) > maxLength {
		// Never truncate: a shared prefix would collide two users.
		return reject(ReasonTooLong, http.StatusUnauthorized)
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x20 || raw[i] > 0x7e {
			return reject(ReasonInvalid, http.StatusBadRequest)
		}
	}
	return raw, nil
}

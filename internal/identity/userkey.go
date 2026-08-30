package identity

import (
	"crypto/sha256"
	"encoding/hex"
)

// UserKey derives a stable, collision-resistant Kubernetes-safe key.
//
// PerUserApp.metadata.uid is deliberately NOT an input: it is the one candidate
// that is not reproducible across the prune-and-recreate cycles a GitOps
// controller performs routinely, and a UID-seeded key would re-key every user
// onto a fresh empty volume. The \x00 separator makes component boundaries
// unambiguous — this derivation is frozen; changing it re-keys every existing
// user onto a fresh empty volume.
func UserKey(namespace, appName, rawIdentity string) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + appName + "\x00" + rawIdentity))
	return "u-" + hex.EncodeToString(sum[:])[:16]
}

// ChildName joins an app name and a userKey into the name of that user's
// per-app child resources.
func ChildName(appName, userKey string) string { return appName + "-" + userKey }

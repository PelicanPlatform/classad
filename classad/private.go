package classad

import (
	"strings"

	"github.com/PelicanPlatform/classad/ast"
)

// Private (secret) attributes. HTCondor treats a fixed set of attribute names --
// claim capabilities and transfer keys -- plus any name with a reserved prefix as
// secret: they must never be serialized to a client by default. Redaction is
// enforced at the serialization boundary (String/MarshalOld/MarshalJSON here, and
// CEDAR PutClassAd / the collector's raw fast path in the daemon layers), so a
// secret cannot leak just because a call site forgot to filter. Emitting a secret
// requires an explicit opt-in (the WithPrivate serializers, or an authorized
// private-ad channel like the collector's StartdPvt table).
//
// This mirrors ClassAdPrivateAttrs and ClassAdAttributeIsPrivate* in HTCondor's
// compat_classad.cpp. It is the single source of truth for what is secret;
// higher layers (cedar, the collector) share this predicate rather than keeping
// their own list, so they cannot disagree.

// privateAttrsV1 is HTCondor's fixed private-attribute set (ClassAdPrivateAttrs),
// keyed lower-case for case-insensitive matching.
var privateAttrsV1 = map[string]struct{}{
	"capability":    {},
	"childclaimids": {},
	"claimid":       {},
	"claimidlist":   {},
	"claimids":      {},
	"transferkey":   {},
}

// privateV2Prefix marks the "V2" private attributes: any name beginning with
// "_condor_priv" (case-insensitive) is secret.
const privateV2Prefix = "_condor_priv"

// IsPrivateAttributeV1 reports whether name is one of HTCondor's fixed private
// attributes (a claim capability or transfer key).
func IsPrivateAttributeV1(name string) bool {
	_, ok := privateAttrsV1[strings.ToLower(name)]
	return ok
}

// IsPrivateAttributeV2 reports whether name is a "_condor_priv"-prefixed private
// attribute (matched case-insensitively).
func IsPrivateAttributeV2(name string) bool {
	return len(name) >= len(privateV2Prefix) &&
		strings.EqualFold(name[:len(privateV2Prefix)], privateV2Prefix)
}

// IsPrivateAttribute reports whether name is a private (secret) attribute of
// either kind, and so must not be serialized to a client by default. This is the
// predicate every serialization layer shares.
func IsPrivateAttribute(name string) bool {
	return IsPrivateAttributeV1(name) || IsPrivateAttributeV2(name)
}

// Redacted returns a copy of the ClassAd with every private attribute removed
// (see IsPrivateAttribute), leaving the original -- and its stored secrets --
// untouched. It is the explicit form to hand to a client where a boundary does
// not already redact. A nil ad (or one with no attributes) is returned as-is; the
// returned copy shares attribute values with the original and must be treated as
// read-only.
func (c *ClassAd) Redacted() *ClassAd {
	if c == nil || c.ad == nil {
		return c
	}
	kept := make([]*ast.AttributeAssignment, 0, len(c.ad.Attributes))
	for _, attr := range c.ad.Attributes {
		if IsPrivateAttribute(attr.Name) {
			continue
		}
		kept = append(kept, attr)
	}
	out := &ClassAd{ad: &ast.ClassAd{Attributes: kept}, attrsDirty: true}
	out.rebuildIndex()
	return out
}

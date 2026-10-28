package dbrpc

import (
	"sort"
	"strconv"
	"strings"
)

// Protocol versioning: how a client learns what a server can do without asking
// it to fail first.
//
// Every capability added to dbrpc before this was discovered by probing. The
// client issued the new request; a server too old answered stBadReq; the client
// mapped that to an Err*Unsupported sentinel and reissued the old request. It
// works, and it has three costs that got worse with each addition.
//
// It puts a fallback path behind every feature -- eleven of them by now -- and
// each is reachable only against a server old enough to refuse. A deployment
// running one version exercises none of them, so the code that exists purely
// for mixed-version sites is the code no test touches.
//
// It conflates failures. stBadReq means "malformed request", and a server that
// does not implement an opcode answers the same way, so four of the sentinels
// cannot tell "too old" from "you sent nonsense". A genuinely bad constraint
// comes back as ErrProjectRefsUnsupported and silently drops the caller onto a
// slower path that then fails for the real reason.
//
// And it costs a wasted round trip per connection per feature, on exactly the
// link where that hurts most: a mixed-version site.
//
// A server now states its protocol version and feature list once, in the CEDAR
// post-authentication ad, which both ends keep with the cached session. The
// client reads them before issuing anything. See the Supports contract below
// for what absence means -- it is the part that is easy to get wrong.

// ProtocolVersion is the dbrpc protocol this build speaks.
//
// It is advertised for coarse checks and for operators reading a log; what a
// caller should branch on is a Feature, because a server may be built without
// one. Bump it when an opcode is added or an existing opcode's reply changes
// shape, and add a Feature for anything a client would otherwise probe for.
const ProtocolVersion = 1

// Session attribute names carried in the CEDAR post-authentication ad. The
// application wires these through: a server publishes ServerSessionAttrs() as
// its cedar SecurityConfig.SessionAttrs, and a client asks for
// ClientSessionAttrNames() in WantSessionAttrs, then hands what it captured to
// NewClient via WithPeerSessionAttrs.
//
// They are strings rather than integers because the handshake ad is read by
// peers of several vintages, and a string reads back identically everywhere.
const (
	SessionAttrProtocol = "DBProtocolVersion"
	SessionAttrFeatures = "DBProtocolFeatures"
)

// A Feature is one capability a client would otherwise have to probe for. The
// string values are protocol, not display: they go on the wire and a released
// name must never change meaning.
type Feature string

const (
	// FeatProjectRefs is the reference-chasing projection (opQueryRawProjRefs).
	FeatProjectRefs Feature = "projrefs"
	// FeatRawWire is the wire-form row stream (opQueryRawWire).
	FeatRawWire Feature = "rawwire"
	// FeatRawWireRefs is the reference-chasing wire-form row stream.
	FeatRawWireRefs Feature = "rawwirerefs"
	// FeatPagination is cursor-resumed projected reads (opQueryRawProjSeq).
	FeatPagination Feature = "pagination"
	// FeatTopK is the server-side ordered top-K read (opTopK).
	FeatTopK Feature = "topk"
	// FeatArchiveAggregate is GROUP BY over an archive table.
	FeatArchiveAggregate Feature = "archiveagg"
	// FeatBucketedAggregate is GROUP BY with server-side bucketing.
	FeatBucketedAggregate Feature = "bucketagg"
	// FeatExtendedAggregate is the extended aggregate frame (bucket widths,
	// filtered aggregates).
	FeatExtendedAggregate Feature = "extagg"
	// FeatWatchWire is the wire-form change feed.
	FeatWatchWire Feature = "watchwire"
	// FeatTxnRead is a transaction-scoped read.
	FeatTxnRead Feature = "txnread"
)

// serverFeatures is what this build implements. Adding an opcode means adding
// its Feature here, or clients keep probing for it.
var serverFeatures = []Feature{
	FeatProjectRefs,
	FeatRawWire,
	FeatRawWireRefs,
	FeatPagination,
	FeatTopK,
	FeatArchiveAggregate,
	FeatBucketedAggregate,
	FeatExtendedAggregate,
	FeatWatchWire,
	FeatTxnRead,
}

// ServerSessionAttrs is what a dbrpc server advertises about itself. Pass it as
// the cedar SecurityConfig.SessionAttrs of the listener serving dbrpc.
func ServerSessionAttrs() map[string]string {
	names := make([]string, len(serverFeatures))
	for i, f := range serverFeatures {
		names[i] = string(f)
	}
	sort.Strings(names)
	return map[string]string{
		SessionAttrProtocol: strconv.Itoa(ProtocolVersion),
		SessionAttrFeatures: strings.Join(names, ","),
	}
}

// ClientSessionAttrNames is what a dbrpc client should capture from the peer.
// Pass it as the cedar SecurityConfig.WantSessionAttrs of the dialer.
func ClientSessionAttrNames() []string {
	return []string{SessionAttrProtocol, SessionAttrFeatures}
}

// Support is a peer's answer about one feature. It has three values, not two,
// because "the peer did not say" is a real and common answer that must not be
// collapsed into either of the others.
type Support int

const (
	// SupportUnknown means the peer advertised nothing: it predates
	// advertisement, or it is reached over a transport that carries none. It
	// says nothing about whether the feature works -- every feature named here
	// existed before advertisement did, so a silent peer may well implement it.
	// Probe, or emulate, or refuse with a message that says which.
	SupportUnknown Support = iota
	// SupportNo means the peer advertised its features and this was not among
	// them. This is a definite no, so a caller must not issue the request to
	// find out.
	SupportNo
	// SupportYes means the peer named the feature. Issue the request directly;
	// a fallback here is unreachable code.
	SupportYes
)

func (s Support) String() string {
	switch s {
	case SupportYes:
		return "yes"
	case SupportNo:
		return "no"
	default:
		return "unknown"
	}
}

// A ClientOption configures a Client at construction.
type ClientOption func(*Client)

// WithPeerSessionAttrs gives the client what the peer advertised, as captured
// from the CEDAR handshake under ClientSessionAttrNames. Attributes that are
// absent or unparseable leave the peer unknown, which is the safe reading.
func WithPeerSessionAttrs(attrs map[string]string) ClientOption {
	return func(c *Client) {
		if len(attrs) == 0 {
			return
		}
		raw, ok := attrs[SessionAttrFeatures]
		if !ok {
			// A version without a feature list tells us the peer advertises,
			// but not what it has. Record the version; leave features unknown.
			c.peerProtocol = parseProtocol(attrs[SessionAttrProtocol])
			return
		}
		c.peerProtocol = parseProtocol(attrs[SessionAttrProtocol])
		c.peerFeatures = make(map[Feature]bool)
		for _, name := range strings.Split(raw, ",") {
			if name = strings.TrimSpace(name); name != "" {
				c.peerFeatures[Feature(name)] = true
			}
		}
	}
}

// parseProtocol reads an advertised version, returning 0 for anything that is
// not a positive integer -- an unreadable claim is no claim.
func parseProtocol(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// PeerProtocol is the protocol version the server advertised, or 0 if it
// advertised none. Prefer Supports for deciding what to send.
func (c *Client) PeerProtocol() int { return c.peerProtocol }

// Supports reports whether the server can serve f.
//
// The three-valued answer is the point: SupportUnknown is not a no. Treating it
// as one would refuse work that an un-advertising server does perfectly well,
// which is every server built before this existed.
func (c *Client) Supports(f Feature) Support {
	if c.peerFeatures == nil {
		return SupportUnknown
	}
	if c.peerFeatures[f] {
		return SupportYes
	}
	return SupportNo
}

package dbrpc

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// AttrTypeReport is one table's measured attribute type profile: what each attribute held
// across a sample, and how large that sample was.
//
// Sampled is reported separately because every count in a Profile is relative to it --
// Present against Sampled is an absence rate, which is a different question from which classes
// of value appeared, and a caller that has only the profiles cannot recover it.
type AttrTypeReport struct {
	Profiles []db.AttrTypeProfile `json:"profiles"`
	Sampled  int                  `json:"sampled"`
}

// filterPrivateProfiles drops the profiles of private attributes unless the connection may see
// them.
//
// A profile carries no values, which is why it is tempting to serve it to anyone. But it
// carries numeric BOUNDS, and the min and max of a secret are not a secret kept -- this package
// already refuses an unprivileged constraint that merely REFERENCES a private attribute
// (refusePrivateConstraint), because answering such a query is an oracle; serving its bounds
// outright would be a larger disclosure through a quieter door.
//
// They are omitted rather than reported with the bounds stripped. Which attributes are private
// is configured, and so already known, so omitting them tells a caller nothing it could not
// look up -- while a reported-but-elided entry invites a caller to treat the attribute as
// typeable and then be surprised by an empty column.
func filterPrivateProfiles(profs []db.AttrTypeProfile, includePrivate bool) []db.AttrTypeProfile {
	if includePrivate {
		return profs
	}
	out := make([]db.AttrTypeProfile, 0, len(profs))
	for _, p := range profs {
		if classad.IsPrivateAttribute(p.Name) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// attrFlagEvaluate asks for the evaluated classes alongside the stored ones. attrFlagsKnown is
// the set this server understands; a request carrying anything else is refused rather than
// served without it, for the same reason the streaming ops refuse an unknown bit -- a reply
// that silently lacks what was asked for is the dangerous shape.
const (
	attrFlagEvaluate uint32 = 1 << 0

	attrFlagsKnown = attrFlagEvaluate
)

// ErrEvalTypesUnsupported reports that the peer cannot measure evaluated classes: it predates
// the option, and would answer the request with stored classes only.
//
// It is returned rather than silently delivering that older answer, because the older answer is
// not a degraded version of this one. Stored classes rule an expression-valued attribute
// untypeable; a caller handed them in place of what it asked for would publish a schema missing
// exactly the columns the option exists to recover.
var ErrEvalTypesUnsupported = errors.New("dbrpc: evaluated attribute type profiles unavailable")

// AttrTypeProfiles returns what each of the table's attributes held across a sample of up to
// sampleMax records: the classes of value, and exact bounds for the numeric ones.
//
// It is the measurement a caller makes before declaring a type per attribute. See
// collections.AttrTypeProfiles for what a profile does and does not answer -- in particular
// that it reports STORED classes, so an attribute held as an expression reads as an expression
// even though evaluating it would yield a scalar. AttrTypeProfilesEvaluated is the measurement
// that answers that one.
//
// Private attributes are absent unless the connection is authorized to see them.
func (c *Client) AttrTypeProfiles(ctx context.Context, table string, sampleMax int) (*AttrTypeReport, error) {
	return c.attrTypeProfiles(ctx, table, sampleMax, 0)
}

// AttrTypeProfilesEvaluated is AttrTypeProfiles with each profile's Evaluated counts filled in:
// the same cells measured by what they EVALUATE to.
//
// That is the measurement a consumer declaring Arrow or SQL types needs, because such a
// consumer evaluates each cell before it encodes it. Reading the stored classes instead is how
// an attribute that is an integer in every ad ends up with no column at all, on the strength of
// being STORED as an expression.
//
// Returns ErrEvalTypesUnsupported when the peer has said it cannot (FeatEvalTypes). The caller's
// fallback is AttrTypeProfiles, whose answer is a different one rather than a lesser one.
func (c *Client) AttrTypeProfilesEvaluated(ctx context.Context, table string, sampleMax int) (*AttrTypeReport, error) {
	if c.Supports(FeatEvalTypes) == SupportNo {
		return nil, ErrEvalTypesUnsupported
	}
	rep, err := c.attrTypeProfiles(ctx, table, sampleMax, attrFlagEvaluate)
	if err != nil {
		return nil, err
	}
	// A peer that advertised nothing cannot be asked in advance, and it answers the request
	// successfully with the older report. Absence of the evaluated counts is how that comes
	// back, so it is turned into the error the caller branches on rather than left to look
	// like a table whose attributes all evaluate to nothing.
	for i := range rep.Profiles {
		if rep.Profiles[i].Evaluated != nil {
			return rep, nil
		}
	}
	if len(rep.Profiles) > 0 {
		return nil, ErrEvalTypesUnsupported
	}
	return rep, nil
}

func (c *Client) attrTypeProfiles(ctx context.Context, table string, sampleMax int, flags uint32) (*AttrTypeReport, error) {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte {
		b := putI32(putStr(req(id, opAttrTypes), table), int32(sampleMax))
		return putI32(b, int32(flags)) //nolint:gosec // a reserved flags word, not a count
	})
	if err != nil {
		return nil, err
	}
	if status != stOK {
		return nil, statusErr(status, body)
	}
	var rep AttrTypeReport
	if err := json.Unmarshal([]byte(body.str()), &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

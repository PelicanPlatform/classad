package dbrpc

import (
	"context"
	"encoding/json"

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

// AttrTypeProfiles returns what each of the table's attributes held across a sample of up to
// sampleMax records: the classes of value, and exact bounds for the numeric ones.
//
// It is the measurement a caller makes before declaring a type per attribute. See
// collections.AttrTypeProfiles for what a profile does and does not answer -- in particular
// that it reports STORED classes, so an attribute held as an expression reads as an expression
// even though evaluating it would yield a scalar.
//
// Private attributes are absent unless the connection is authorized to see them.
func (c *Client) AttrTypeProfiles(ctx context.Context, table string, sampleMax int) (*AttrTypeReport, error) {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte {
		return putI32(putStr(req(id, opAttrTypes), table), int32(sampleMax))
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

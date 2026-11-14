package dbrpc

import (
	"errors"

	"github.com/PelicanPlatform/classad/db"
)

// SENDING A BAND.
//
// db.JoinBand is a conjunct of a step's match: the rows outside its window do not join. That
// is why it has opcodes of its own rather than a few more bytes on the end of the ones that
// predate it. A server built before bands would read appended band bytes as the next part of
// the frame; worse, one that skipped them would answer the same query with the window
// removed, which is a superset of the right rows and looks from the outside exactly like a
// correct answer to a question nobody asked.
//
// So the capability is asked for twice, in the two ways this protocol asks. The client will
// not send a band to a peer that has not advertised FeatJoinBand, and the server will not
// serve one unless its catalog says it honours bands -- neither alone is enough, because
// advertisement can be stale and an interface can be satisfied by a build that predates the
// behaviour.

// ErrJoinBandUnsupported reports a peer that cannot carry a join band.
//
// It is a statement about the PEER. A caller holding one can run the join in process, or
// write the window as an ON condition and accept the nested loop.
var ErrJoinBandUnsupported = errors.New("dbrpc: this server does not support a join band; " +
	"run the join in process, or write the window as an ON condition")

// joinSpecHasBand reports whether any part of a plan is windowed, in either spelling.
func joinSpecHasBand(spec db.JoinSpec) bool {
	if len(spec.Band) > 0 {
		return true
	}
	for i := range spec.Steps {
		if len(spec.Steps[i].Band) > 0 {
			return true
		}
	}
	return false
}

// joinSend picks the opcode and body layout a plan needs, and refuses before sending
// anything when the peer has not advertised everything the plan asks of it.
//
// A banded plan goes to the band opcode in EITHER spelling -- those opcodes carry both, and
// which one is a bit of the layout byte rather than a second opcode. It still asks for the
// feature its unbanded form would have needed, because FeatJoinBand says the catalog honours
// a window and says nothing about whether this funnel can fold in a third table or produce
// wire rows. Asking for both is what keeps each feature a statement about one thing.
func (c *Client) joinSend(jr JoinRequest, pair, chain, band op, pairFeat, chainFeat Feature,
	unsupported error) (op, joinLayout, error) {
	o, feat := joinOpFor(jr, pair, chain, pairFeat, chainFeat)
	if !c.SupportsNewer(feat) {
		return 0, 0, c.featureRefusal(feat, unsupported)
	}
	if !joinSpecHasBand(jr.Spec) {
		return o, joinOpLayout(o), nil
	}
	if !c.SupportsNewer(FeatJoinBand) {
		return 0, 0, c.featureRefusal(FeatJoinBand, ErrJoinBandUnsupported)
	}
	// The chain bit comes from the opcode joinOpFor already picked, so the layout cannot
	// disagree with the feature that was just checked for it.
	return band, layoutBand | joinOpLayout(o), nil
}

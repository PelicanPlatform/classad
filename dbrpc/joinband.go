package dbrpc

import (
	"errors"
	"fmt"

	"github.com/PelicanPlatform/classad/db"
)

// A BAND DOES NOT CROSS THE WIRE YET.
//
// db.JoinBand is a conjunct of a step's match: the rows outside its window do not join. The
// join request's body has no field for one, and a field cannot simply be appended -- a server
// built before it would read the trailing bytes as the next part of the frame. Adding one
// needs an opcode and a Feature of its own, which is how every other addition to this
// protocol has been made.
//
// Until that exists the client REFUSES a plan carrying a band rather than sending the rest of
// it. Encoding what fits would ship a join missing one of its conditions, and the server
// would answer it -- with more rows than the caller asked for, and no indication that the
// question had changed on the way over.

// ErrJoinBandUnsupported reports a plan this protocol cannot carry. It is not a statement
// about the server: no version of this protocol sends a band, so a newer peer does not help.
var ErrJoinBandUnsupported = errors.New("dbrpc: a join band cannot be sent over the wire; " +
	"run the join in process, or write the window as an ON condition")

// refuseJoinBand rejects a spec whose match depends on a band.
func refuseJoinBand(spec db.JoinSpec) error {
	if len(spec.Band) > 0 {
		return fmt.Errorf("%w: %s is windowed on %s", ErrJoinBandUnsupported,
			spec.Right.Alias, spec.Band[0].Attr)
	}
	for i := range spec.Steps {
		if st := &spec.Steps[i]; len(st.Band) > 0 {
			return fmt.Errorf("%w: %s is windowed on %s", ErrJoinBandUnsupported,
				st.Side.Alias, st.Band[0].Attr)
		}
	}
	return nil
}

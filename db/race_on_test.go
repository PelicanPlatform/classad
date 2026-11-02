//go:build race

package db

// raceEnabled reports whether this binary was built with the race detector.
//
// It exists for one test: the fan-out gate's upper bound has to be checked against a fixture at the
// real gate's scale, tens of megabytes, and under the race detector building that fixture takes
// minutes. The race detector has nothing to say about the property in question -- whether a gate
// constant permits a fan-out -- so that test skips here and runs in the ordinary build.
const raceEnabled = true

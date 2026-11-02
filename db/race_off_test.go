//go:build !race

package db

// raceEnabled reports whether this binary was built with the race detector. See race_on_test.go.
const raceEnabled = false

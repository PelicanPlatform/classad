//go:build !libclassad

// Package differ is empty unless built with the `libclassad` build tag. The
// real implementation lives in differ.go (behind `//go:build libclassad`); it
// imports the cgo libclassad oracle and so is only compiled where the library
// is available. This stub keeps the package buildable without the tag. See
// fuzz/README.md ("Build requirements").
package differ

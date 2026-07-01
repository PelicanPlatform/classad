//go:build !libclassad

// Package cgo is empty unless built with the `libclassad` build tag. The real
// in-process libclassad bindings live in cgo.go (behind `//go:build
// libclassad`), which links -lclassad and is therefore only compiled where the
// library is available. This stub keeps the package buildable -- so a plain
// `go build`/`go vet ./...` on a runner without libclassad succeeds by skipping
// it. See fuzz/README.md ("Build requirements").
package cgo

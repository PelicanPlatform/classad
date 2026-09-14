//go:build tools

// Package tools pins the external binaries CI builds. It is never
// imported; the build tag keeps it out of ordinary builds while still
// making the import a real dependency Dependabot can see and bump.
package tools

import (
	// goyacc: generates parser/y.go from parser/classad.y. Every CI job
	// needs it before it can build this module.
	//
	// Pinned here rather than `go install ...@latest` in seven separate
	// workflow steps: @latest resolved to whatever was published before
	// the job started and verified it against nothing, which is a parser
	// generator running with full access to the build.
	_ "golang.org/x/tools/cmd/goyacc"
)

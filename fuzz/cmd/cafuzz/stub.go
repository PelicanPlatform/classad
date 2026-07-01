//go:build !libclassad

// The real cafuzz driver (main.go) links libclassad and is behind
// `//go:build libclassad`. This stub keeps the command buildable without the
// tag -- a plain `go build ./...` on a runner without libclassad succeeds -- and
// tells anyone who runs it how to build the real thing.
package main

import "fmt"

func main() {
	fmt.Println("cafuzz requires libclassad; build with the tag, e.g.:\n" +
		"  CGO_ENABLED=1 go run -tags libclassad ./fuzz/cmd/cafuzz -n 100000 -ignore-parse")
}

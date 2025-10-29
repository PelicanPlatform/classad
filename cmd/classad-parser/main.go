// Package main provides a command-line tool for parsing and evaluating ClassAd expressions.
package main

import (
	"fmt"
	"os"

	"github.com/bbockelm/golang-classads/parser"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: classad-parser <classad-expression>")
		fmt.Println("\nExamples:")
		fmt.Println("  classad-parser '[x = 10; y = x + 5]'")
		fmt.Println("  classad-parser '[Machine = \"test\"; Cpus = 4]'")
		os.Exit(1)
	}

	input := os.Args[1]
	result, err := parser.Parse(input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if result != nil {
		fmt.Println(result.String())
	} else {
		fmt.Println("Successfully parsed (no result to display)")
	}
}

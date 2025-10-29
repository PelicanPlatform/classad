// Package main provides a command-line tool for parsing and evaluating ClassAd expressions.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/bbockelm/golang-classads/parser"
)

func main() {
	// Define command-line flags
	oldFormat := flag.Bool("old", false, "Parse input as old ClassAd format (newline-delimited, no brackets)")
	help := flag.Bool("help", false, "Show usage information")
	flag.BoolVar(help, "h", false, "Show usage information (shorthand)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS] <classad-expression>\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Parse and display ClassAd expressions in new or old format.\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  # New format (default)\n")
		fmt.Fprintf(os.Stderr, "  %s '[x = 10; y = x + 5]'\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s '[Machine = \"test\"; Cpus = 4]'\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  # Old format\n")
		fmt.Fprintf(os.Stderr, "  %s -old 'x = 10\ny = x + 5'\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s --old 'Foo = 3\nBar = \"hello\"'\n", os.Args[0])
	}

	flag.Parse()

	if *help {
		flag.Usage()
		os.Exit(0)
	}

	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "Error: missing ClassAd expression argument\n\n")
		flag.Usage()
		os.Exit(1)
	}

	input := flag.Arg(0)

	var result interface{}
	var err error

	if *oldFormat {
		// Parse as old ClassAd format
		result, err = parser.ParseOldClassAd(input)
	} else {
		// Parse as new ClassAd format (default)
		result, err = parser.Parse(input)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if result != nil {
		// Convert old ClassAd result to string using the ClassAd's String() method
		if classAd, ok := result.(interface{ String() string }); ok {
			fmt.Println(classAd.String())
		} else {
			fmt.Println(result)
		}
	} else {
		fmt.Println("Successfully parsed (no result to display)")
	}
}

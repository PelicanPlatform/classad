package classad

import (
	"bufio"
	"io"
	"iter"
	"strings"
)

// Reader provides an iterator for parsing multiple ClassAds from an io.Reader.
// It supports both new-style (bracketed) and old-style (newline-delimited) formats.
type Reader struct {
	scanner  *bufio.Scanner
	oldStyle bool
	err      error
	current  *ClassAd
}

// NewReader creates a new Reader for parsing new-style ClassAds (with brackets).
// Each ClassAd should be on its own, separated by whitespace or comments.
// Example format:
//
//	[Foo = 1; Bar = 2]
//	[Baz = 3; Qux = 4]
func NewReader(r io.Reader) *Reader {
	return &Reader{
		scanner:  bufio.NewScanner(r),
		oldStyle: false,
	}
}

// NewOldReader creates a new Reader for parsing old-style ClassAds (newline-delimited).
// Each ClassAd is separated by a blank line.
// Example format:
//
//	Foo = 1
//	Bar = 2
//
//	Baz = 3
//	Qux = 4
func NewOldReader(r io.Reader) *Reader {
	return &Reader{
		scanner:  bufio.NewScanner(r),
		oldStyle: true,
	}
}

// Next advances to the next ClassAd and returns true if one was found.
// It returns false when there are no more ClassAds or an error occurred.
// Call Err() after Next returns false to check for errors.
func (r *Reader) Next() bool {
	if r.err != nil {
		return false
	}

	if r.oldStyle {
		return r.nextOld()
	}
	return r.nextNew()
}

// nextNew reads the next new-style ClassAd (with brackets)
func (r *Reader) nextNew() bool {
	var lines []string
	inClassAd := false
	bracketDepth := 0

	for r.scanner.Scan() {
		line := strings.TrimSpace(r.scanner.Text())

		// Skip empty lines and comments outside of ClassAds
		if !inClassAd && (line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "/*")) {
			continue
		}

		// Check if this line starts a ClassAd
		if !inClassAd && strings.HasPrefix(line, "[") {
			inClassAd = true
		}

		if inClassAd {
			lines = append(lines, line)

			// Count brackets to handle nested ClassAds
			for _, ch := range line {
				switch ch {
				case '[':
					bracketDepth++
				case ']':
					bracketDepth--
				}
			}

			// If we've closed all brackets, we have a complete ClassAd
			if bracketDepth == 0 {
				classAdStr := strings.Join(lines, "\n")
				ad, err := Parse(classAdStr)
				if err != nil {
					r.err = err
					return false
				}
				r.current = ad
				return true
			}
		}
	}

	// Check for scanner errors
	if err := r.scanner.Err(); err != nil {
		r.err = err
		return false
	}

	// If we have accumulated lines but hit EOF, try to parse them
	if len(lines) > 0 {
		classAdStr := strings.Join(lines, "\n")
		ad, err := Parse(classAdStr)
		if err != nil {
			r.err = err
			return false
		}
		r.current = ad
		return true
	}

	return false
}

// nextOld reads the next old-style ClassAd (newline-delimited, separated by blank lines)
func (r *Reader) nextOld() bool {
	var lines []string

	for r.scanner.Scan() {
		line := r.scanner.Text()
		trimmedLine := strings.TrimSpace(line)

		// Blank line marks the end of a ClassAd
		if trimmedLine == "" {
			if len(lines) > 0 {
				// We have a complete ClassAd
				classAdStr := strings.Join(lines, "\n")
				ad, err := ParseOld(classAdStr)
				if err != nil {
					r.err = err
					return false
				}
				r.current = ad
				return true
			}
			// Skip consecutive blank lines
			continue
		}

		// Skip comment-only lines
		if strings.HasPrefix(trimmedLine, "//") || strings.HasPrefix(trimmedLine, "#") {
			continue
		}

		lines = append(lines, line)
	}

	// Check for scanner errors
	if err := r.scanner.Err(); err != nil {
		r.err = err
		return false
	}

	// If we have accumulated lines but hit EOF, parse them as the last ClassAd
	if len(lines) > 0 {
		classAdStr := strings.Join(lines, "\n")
		ad, err := ParseOld(classAdStr)
		if err != nil {
			r.err = err
			return false
		}
		r.current = ad
		return true
	}

	return false
}

// ClassAd returns the current ClassAd.
// This should be called after Next() returns true.
func (r *Reader) ClassAd() *ClassAd {
	return r.current
}

// Err returns the error that occurred during iteration, if any.
// This should be called after Next() returns false to distinguish
// between EOF and an actual error.
func (r *Reader) Err() error {
	return r.err
}

// All returns an iterator over all ClassAds from an io.Reader.
// This function is compatible with Go 1.23+ range-over-function syntax.
//
// Example usage (Go 1.23+):
//
//	file, _ := os.Open("jobs.classads")
//	defer file.Close()
//	for ad := range classad.All(file) {
//	    // Process ad...
//	}
//
// For earlier Go versions, use the traditional pattern:
//
//	reader := classad.NewReader(file)
//	for reader.Next() {
//	    ad := reader.ClassAd()
//	    // Process ad...
//	}
func All(r io.Reader) iter.Seq[*ClassAd] {
	return func(yield func(*ClassAd) bool) {
		reader := NewReader(r)
		for reader.Next() {
			if !yield(reader.ClassAd()) {
				return
			}
		}
		// Note: Error is discarded in iterator pattern.
		// For error handling, use AllWithError instead.
	}
}

// AllOld returns an iterator over all old-style ClassAds from an io.Reader.
// This function is compatible with Go 1.23+ range-over-function syntax.
//
// Example usage (Go 1.23+):
//
//	file, _ := os.Open("machines.classads")
//	defer file.Close()
//	for ad := range classad.AllOld(file) {
//	    // Process ad...
//	}
func AllOld(r io.Reader) iter.Seq[*ClassAd] {
	return func(yield func(*ClassAd) bool) {
		reader := NewOldReader(r)
		for reader.Next() {
			if !yield(reader.ClassAd()) {
				return
			}
		}
	}
}

// AllWithIndex returns an iterator over all ClassAds with their index.
// This function is compatible with Go 1.23+ range-over-function syntax.
//
// Example usage (Go 1.23+):
//
//	file, _ := os.Open("jobs.classads")
//	defer file.Close()
//	for i, ad := range classad.AllWithIndex(file) {
//	    fmt.Printf("ClassAd #%d\n", i)
//	    // Process ad...
//	}
func AllWithIndex(r io.Reader) iter.Seq2[int, *ClassAd] {
	return func(yield func(int, *ClassAd) bool) {
		reader := NewReader(r)
		index := 0
		for reader.Next() {
			if !yield(index, reader.ClassAd()) {
				return
			}
			index++
		}
	}
}

// AllOldWithIndex returns an iterator over all old-style ClassAds with their index.
// This function is compatible with Go 1.23+ range-over-function syntax.
func AllOldWithIndex(r io.Reader) iter.Seq2[int, *ClassAd] {
	return func(yield func(int, *ClassAd) bool) {
		reader := NewOldReader(r)
		index := 0
		for reader.Next() {
			if !yield(index, reader.ClassAd()) {
				return
			}
			index++
		}
	}
}

// AllWithError returns an iterator that yields ClassAds and captures any error.
// Use this when you need error handling with the iterator pattern.
//
// Example usage:
//
//	file, _ := os.Open("jobs.classads")
//	defer file.Close()
//	var err error
//	for ad := range classad.AllWithError(file, &err) {
//	    // Process ad...
//	}
//	if err != nil {
//	    log.Fatal(err)
//	}
func AllWithError(r io.Reader, errPtr *error) iter.Seq[*ClassAd] {
	return func(yield func(*ClassAd) bool) {
		reader := NewReader(r)
		for reader.Next() {
			if !yield(reader.ClassAd()) {
				return
			}
		}
		if reader.Err() != nil && errPtr != nil {
			*errPtr = reader.Err()
		}
	}
}

// AllOldWithError returns an iterator for old-style ClassAds that captures any error.
func AllOldWithError(r io.Reader, errPtr *error) iter.Seq[*ClassAd] {
	return func(yield func(*ClassAd) bool) {
		reader := NewOldReader(r)
		for reader.Next() {
			if !yield(reader.ClassAd()) {
				return
			}
		}
		if reader.Err() != nil && errPtr != nil {
			*errPtr = reader.Err()
		}
	}
}

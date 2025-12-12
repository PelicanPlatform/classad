package classad

import (
	"bufio"
	"fmt"
	"io"
	"iter"
	"strings"
	"unicode/utf8"
)

const (
	// maxBufferSize limits the buffer size to prevent unbounded memory growth
	// when processing malformed or very large inputs
	maxBufferSize = 10 * 1024 * 1024 // 10MB
	// readChunkSize is the size of chunks read from the io.Reader
	readChunkSize = 4096 // 4KB
)

// Reader provides an iterator for parsing multiple ClassAds from an io.Reader.
// It supports both new-style (bracketed) and old-style (newline-delimited) formats.
type Reader struct {
	// For new-style (bracketed) ClassAds
	bufReader *bufio.Reader
	buffer    strings.Builder
	oldStyle  bool
	scanner   *bufio.Scanner
	err       error
	current   *ClassAd
}

// NewReader creates a new Reader for parsing new-style ClassAds (with brackets).
// Each ClassAd should be on its own, separated by whitespace or comments.
// This function natively supports concatenated ClassAds (e.g., "][") through
// grammar-level parsing.
// Example format:
//
//	[Foo = 1; Bar = 2]
//	[Baz = 3; Qux = 4]
//
// Also supports concatenated format:
//
//	[Foo = 1; Bar = 2][Baz = 3; Qux = 4]
//
// This implementation streams data from the io.Reader, processing ClassAds
// one at a time without buffering the entire input in memory.
func NewReader(r io.Reader) *Reader {
	return &Reader{
		bufReader: bufio.NewReader(r),
		oldStyle:  false,
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

// nextNew reads the next new-style ClassAd by streaming from the reader.
// It tracks bracket depth to detect complete ClassAds, handling strings
// and comments properly.
func (r *Reader) nextNew() bool {
	// Read data incrementally until we have a complete ClassAd
	for {
		// Check buffer size limit before expensive scan to prevent unbounded growth
		if r.buffer.Len() > maxBufferSize {
			r.err = fmt.Errorf("buffer exceeded maximum size (%d bytes): input may be malformed or too large", maxBufferSize)
			return false
		}

		// Check if we already have a complete ClassAd in the buffer
		adStr, remaining, found := r.findCompleteClassAd()
		if found {
			// Parse the complete ClassAd
			ad, err := Parse(adStr)
			if err != nil {
				r.err = err
				return false
			}
			r.current = ad
			// Update buffer with remaining data
			r.buffer.Reset()
			r.buffer.WriteString(remaining)
			return true
		}

		// Need more data - read a chunk
		chunk := make([]byte, readChunkSize)
		n, err := r.bufReader.Read(chunk)
		if n > 0 {
			r.buffer.Write(chunk[:n])
			// Check buffer size after writing to catch cases where a single chunk exceeds limit
			if r.buffer.Len() > maxBufferSize {
				r.err = fmt.Errorf("buffer exceeded maximum size (%d bytes): input may be malformed or too large", maxBufferSize)
				return false
			}
		}
		if err == io.EOF {
			return r.handleEOF()
		}
		if err != nil {
			r.err = err
			return false
		}
	}
}

// handleEOF processes remaining data when EOF is reached.
// It attempts to parse any complete ClassAd or remaining data in the buffer.
func (r *Reader) handleEOF() bool {
	// Check if we have a complete ClassAd in buffer
	adStr, remaining, found := r.findCompleteClassAd()
	if found {
		ad, parseErr := Parse(adStr)
		if parseErr != nil {
			r.err = parseErr
			return false
		}
		r.current = ad
		r.buffer.Reset()
		r.buffer.WriteString(remaining)
		return true
	}
	// Check if there's any remaining data that might be a ClassAd
	remainingStr := strings.TrimSpace(r.buffer.String())
	if remainingStr != "" {
		// Try to parse what's left
		ad, parseErr := Parse(remainingStr)
		if parseErr != nil {
			r.err = parseErr
			return false
		}
		r.current = ad
		r.buffer.Reset()
		return true
	}
	return false
}

// findCompleteClassAd scans the buffer to find a complete ClassAd (balanced brackets).
// It returns the ClassAd string, any remaining data, and whether a complete ClassAd was found.
// This handles strings and comments properly so brackets inside them don't affect depth.
// The function uses byte-level iteration for efficiency, properly handling UTF-8 sequences.
func (r *Reader) findCompleteClassAd() (classAdStr, remaining string, found bool) {
	bufStr := r.buffer.String()
	if bufStr == "" {
		return "", "", false
	}

	// Track bracket depth, handling strings and comments
	depth := 0
	inString := false
	inLineComment := false
	inBlockComment := false
	escapeNext := false
	startPos := -1

	// Skip leading whitespace and comments to find the start of a ClassAd
	skipWhitespace := true

	// Use byte-level iteration for efficiency (brackets are ASCII, single-byte)
	// But properly handle UTF-8 sequences when advancing
	for i := 0; i < len(bufStr); {
		ch := bufStr[i]

		// Skip whitespace before finding the first bracket
		if skipWhitespace {
			if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
				i++
				continue
			}
			// Check for line comment
			if i+1 < len(bufStr) && ch == '/' && bufStr[i+1] == '/' {
				// Skip to end of line
				for i < len(bufStr) && bufStr[i] != '\n' {
					i++
				}
				continue
			}
			// Check for block comment
			if i+1 < len(bufStr) && ch == '/' && bufStr[i+1] == '*' {
				// Skip block comment
				i += 2
				for i+1 < len(bufStr) {
					if bufStr[i] == '*' && bufStr[i+1] == '/' {
						i += 2
						break
					}
					// Advance by rune to handle UTF-8 in comments
					_, size := utf8.DecodeRuneInString(bufStr[i:])
					if size == 0 {
						break
					}
					i += size
				}
				continue
			}
			skipWhitespace = false
		}

		// Handle escape sequences in strings
		if escapeNext {
			escapeNext = false
			// Advance by rune to handle UTF-8 escape sequences properly
			_, size := utf8.DecodeRuneInString(bufStr[i:])
			if size == 0 {
				break
			}
			i += size
			continue
		}

		// Handle escape sequences in strings
		if inString && ch == '\\' {
			escapeNext = true
			i++
			continue
		}

		// Handle strings
		if !inLineComment && !inBlockComment {
			if ch == '"' {
				inString = !inString
				i++
				continue
			}
		}

		// Only process brackets when not in string or comment
		// Brackets are ASCII (single-byte), so byte-level comparison is safe
		if !inString && !inLineComment && !inBlockComment {
			switch ch {
			case '[':
				if depth == 0 {
					startPos = i
				}
				depth++
				i++
			case ']':
				depth--
				if depth == 0 && startPos >= 0 {
					// Found complete ClassAd
					classAdStr = bufStr[startPos : i+1]
					remaining = strings.TrimSpace(bufStr[i+1:])
					return classAdStr, remaining, true
				}
				i++
			default:
				// Not a bracket - advance by rune for UTF-8 handling
				_, size := utf8.DecodeRuneInString(bufStr[i:])
				if size == 0 {
					break
				}
				i += size
			}
			continue
		}

		// Handle comments (only when not in string)
		if !inString {
			if !inBlockComment && i+1 < len(bufStr) && ch == '/' && bufStr[i+1] == '/' {
				inLineComment = true
				i += 2
				continue
			}
			if inLineComment && ch == '\n' {
				inLineComment = false
				i++
				continue
			}
			if !inLineComment && !inBlockComment && i+1 < len(bufStr) && ch == '/' && bufStr[i+1] == '*' {
				inBlockComment = true
				i += 2
				continue
			}
			if inBlockComment && i+1 < len(bufStr) && ch == '*' && bufStr[i+1] == '/' {
				inBlockComment = false
				i += 2
				continue
			}
		}

		// Advance by rune to handle UTF-8 properly in strings and comments
		_, size := utf8.DecodeRuneInString(bufStr[i:])
		if size == 0 {
			break
		}
		i += size
	}

	return "", "", false
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

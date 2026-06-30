// Package fuzz hosts the differential fuzz target comparing the native Go
// ClassAd evaluation engine against the reference C++ libclassad. See README.md
// in this directory for design and usage. The fuzz target itself lives in
// differential_test.go; this file exists so the package has a non-test Go
// source file (some tooling requires one).
package fuzz

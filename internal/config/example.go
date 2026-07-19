package config

import _ "embed"

//go:embed example.ini
var exampleINI string

// Example returns the full commented example tncd.ini text.
func Example() string {
	return exampleINI
}

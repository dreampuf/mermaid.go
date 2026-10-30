package mermaid_go

import (
	"fmt"
	"os"
	"testing"
)

// TestMain loads the mermaid.js bundle the chrome tests need. The root package
// embeds none, and its tests cannot import the mermaidjs subpackage that does
// -- it imports this package -- so they read the same file from disk.
func TestMain(m *testing.M) {
	source, err := os.ReadFile("mermaidjs/mermaid.min.js")
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading mermaid.js: %v\n", err)
		os.Exit(1)
	}
	SourceMermaid = string(source)
	os.Exit(m.Run())
}

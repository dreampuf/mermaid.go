package mermaidjs

import (
	"strings"
	"testing"

	mermaid_go "github.com/dreampuf/mermaid.go"
)

func TestRegistersTheBundle(t *testing.T) {
	if !strings.Contains(Source, "mermaid") {
		t.Fatalf("Source = %.80q..., want the mermaid.js bundle", Source)
	}
	if mermaid_go.SourceMermaid != Source {
		t.Error("importing mermaidjs did not set mermaid_go.SourceMermaid")
	}
}

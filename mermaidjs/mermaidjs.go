// Package mermaidjs embeds the mermaid.js bundle the chrome backend renders
// with. Importing it, even for its side effect alone, sets
// mermaid_go.SourceMermaid, which NewRenderEngine requires:
//
//	import _ "github.com/dreampuf/mermaid.go/mermaidjs"
//
// The bundle lives here rather than in the root package so that a program
// rendering only through merman does not carry its 5 MB.
package mermaidjs

import (
	_ "embed"

	mermaid_go "github.com/dreampuf/mermaid.go"
)

// Source is the embedded mermaid.js bundle.
//
//go:embed mermaid.min.js
var Source string

func init() {
	// Leave a bundle the program assigned itself in place: init order between
	// packages is not the program's to control, and its choice should win.
	if mermaid_go.SourceMermaid == "" {
		mermaid_go.SourceMermaid = Source
	}
}

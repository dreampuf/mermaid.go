package mermaid_go_test

// The root package embeds neither asset, and its internal tests cannot import
// the subpackages that do, since those import it. An external test file can:
// the go tool links the subpackages against the package under test, so their
// init registers the bundle and the module for every test in the binary.
import (
	_ "github.com/dreampuf/mermaid.go/mermaidjs"
	_ "github.com/dreampuf/mermaid.go/mermanwasm"
)

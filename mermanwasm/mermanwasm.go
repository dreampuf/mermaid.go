// Package mermanwasm embeds merman's WebAssembly renderer, the standard
// wasm32-wasip1 build from the merman SDK: every diagram family, SVG and PNG
// output, Cytoscape layouts and the bundled Cousine fonts, without formulas or
// ELK layout. Importing it, even for its side effect alone, sets
// mermaid_go.MermanWasmModule, which NewMermanWasmEngine runs by default:
//
//	import _ "github.com/dreampuf/mermaid.go/mermanwasm"
//
// The module lives here rather than in the root package so that a program
// rendering only through chrome does not carry its 15 MB. For the math or full
// build, pass it to NewMermanWasmEngine with WithMermanWasmModule instead.
//
// The module is distributed under MIT or Apache-2.0, with the third-party
// notices in THIRD_PARTY_NOTICES.md and THIRD_PARTY_LICENSES; a program that
// ships it should ship those too. SOURCE_AVAILABILITY.md names the exact merman
// commit it was built from.
package mermanwasm

import (
	_ "embed"

	mermaid_go "github.com/dreampuf/mermaid.go"
)

// Version is the merman release the module was taken from.
const Version = "0.9.3"

// SHA256 is the module's checksum as merman's SDK catalog records it, so an
// update can be checked against the release it claims to come from.
const SHA256 = "4ebb5838ea82e30338edcb110d580b954a00a9e70eb64dde4f83b42a3aac6dcb"

// Module is the embedded merman_wasi.wasm reactor.
//
//go:embed merman_wasi.wasm
var Module []byte

func init() {
	// Leave a module the program assigned itself in place, as mermaidjs does
	// for its bundle.
	if mermaid_go.MermanWasmModule == nil {
		mermaid_go.MermanWasmModule = Module
	}
}

package mermanwasm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	mermaid_go "github.com/dreampuf/mermaid.go"
)

func TestModuleMatchesTheRelease(t *testing.T) {
	sum := sha256.Sum256(Module)
	if got := hex.EncodeToString(sum[:]); got != SHA256 {
		t.Errorf("sha256(merman_wasi.wasm) = %s, want %s", got, SHA256)
	}
}

func TestRegistersTheModule(t *testing.T) {
	engine, err := mermaid_go.NewMermanWasmEngine(context.Background())
	if err != nil {
		t.Fatalf("NewMermanWasmEngine() with the registered module error = %v", err)
	}
	defer engine.Cancel()
	if engine.Version() != Version {
		t.Errorf("Version() = %q, want %q", engine.Version(), Version)
	}
	svg, err := engine.Render("graph TD; A-->B;")
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.HasPrefix(svg, "<svg") {
		t.Errorf("Render() = %.80q..., want an svg document", svg)
	}
}

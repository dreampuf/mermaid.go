package mermaid_go

import (
	"context"
	"time"
)

// Renderer is the behaviour shared by the render backends, so a program can
// choose between them at startup and pass one around without caring which it
// got. [RenderEngine] drives a headless chrome through chromedp; [MermanEngine]
// shells out to the merman CLI, which parses and lays diagrams out natively and
// therefore needs no browser at all; the Engine of package
// github.com/dreampuf/mermaid.go/mermanwasm runs merman's WebAssembly build in
// process, needing neither a browser nor a binary.
//
// Backend-specific facilities stay off the interface: chrome's crash and console
// reporting ([RenderEngine.CrashError], [RenderEngine.SetTargetCrashedHandler],
// [RenderEngine.SetConsoleHandler]) has no merman equivalent, and merman's
// capability report has no chrome equivalent.
type Renderer interface {
	Render(content string, opts ...RenderOption) (string, error)
	RenderContext(ctx context.Context, content string, opts ...RenderOption) (string, error)
	RenderAsPng(content string, opts ...RenderOption) ([]byte, *BoxModel, error)
	RenderAsPngContext(ctx context.Context, content string, opts ...RenderOption) ([]byte, *BoxModel, error)
	RenderAsScaledPng(content string, scale float64, opts ...RenderOption) ([]byte, *BoxModel, error)
	RenderAsScaledPngContext(ctx context.Context, content string, scale float64, opts ...RenderOption) ([]byte, *BoxModel, error)
	SetRenderTimeout(d time.Duration)
	Cancel()
}

var (
	_ Renderer = (*RenderEngine)(nil)
	_ Renderer = (*MermanEngine)(nil)
)

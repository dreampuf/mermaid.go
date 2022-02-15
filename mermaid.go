package mermaid_go

import (
	"context"
	_ "embed"
	"fmt"
	"github.com/chromedp/chromedp"
)

//go:embed mermaid.min.js
var SOURCE_MERMAID string

var DEFAULT_PAGE string = `data:text/html,<!DOCTYPE html>
<html lang="en">
    <head><meta charset="utf-8"></head>
    <body></body>
</html>`

type RenderEngine struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func NewRenderEngine(ctx context.Context) *RenderEngine {
	ctx, cancel := chromedp.NewContext(ctx)
	return &RenderEngine{
		ctx:    ctx,
		cancel: cancel,
	}
}

func (r *RenderEngine) Init() error {
	// lib_ready may need to expose to user
	var lib_ready bool
	return chromedp.Run(r.ctx,
		chromedp.Navigate(DEFAULT_PAGE),
		chromedp.Evaluate(SOURCE_MERMAID, &lib_ready),
	)
}
func (r *RenderEngine) Render(content string) (string, error) {
	var (
		result string
	)
	err := chromedp.Run(r.ctx,
		chromedp.Evaluate(fmt.Sprintf("mermaid.render('mermaid', `%s`);", content), &result),
	)
	return result, err
}
func (r *RenderEngine) Cancel() {
	r.cancel()
}

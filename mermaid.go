package mermaid_go

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

//go:embed mermaid.min.js
var SourceMermaid string

var DefaultPage = `data:text/html,<!DOCTYPE html>
<html lang="en">
    <head><meta charset="utf-8"></head>
    <body></body>
</html>`

var ErrMermaidNotReady = errors.New("mermaid.js initial failed")

type BoxModel = dom.BoxModel

type RenderEngine struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func NewRenderEngine(ctx context.Context, statements ...string) (*RenderEngine, error) {
	var (
		lib_ready *runtime.RemoteObject
	)

	args := chromedp.DefaultExecAllocatorOptions[:]

	deadline, ok := ctx.Deadline()
	if ok {
		args = append(args, chromedp.WSURLReadTimeout(time.Until(deadline)))
	}
	actx, _ := chromedp.NewExecAllocator(ctx,
		args...)
	ctx, cancel := chromedp.NewContext(actx)
	actions := []chromedp.Action{
		chromedp.Navigate(DefaultPage),
		chromedp.Evaluate(SourceMermaid, &lib_ready),
		chromedp.Evaluate("mermaid.initialize({startOnLoad:true})", &lib_ready),
	}
	for _, stmt := range statements {
		actions = append(actions, chromedp.Evaluate(stmt, nil))
	}
	err := chromedp.Run(ctx, actions...)
	if err == nil && lib_ready.ObjectID != "" {
		err = ErrMermaidNotReady
	}
	return &RenderEngine{
		ctx:    ctx,
		cancel: cancel,
	}, err
}

func (r *RenderEngine) Render(content string) (string, error) {
	var (
		result string
	)
	encodedContent, _ := json.Marshal(content)
	err := chromedp.Run(r.ctx,
		chromedp.Evaluate(fmt.Sprintf("mermaid.render('mermaid', `%s`).then(({ svg }) => { return svg; });", encodedContent), &result, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
			return p.WithAwaitPromise(true)
		}),
	)
	return result, err
}

func (r *RenderEngine) RenderAsScaledPng(content string, scale float64) ([]byte, *BoxModel, error) {
	var (
		result_in_bytes []byte
		model           *dom.BoxModel
	)
	encodedContent, _ := json.Marshal(content)
	err := chromedp.Run(r.ctx,
		chromedp.Evaluate(fmt.Sprintf("mermaid.render('mermaid', `%s`).then(({ svg }) => { document.body.innerHTML = svg; });", encodedContent), nil),
		chromedp.ScreenshotScale("#mermaid", scale, &result_in_bytes, chromedp.ByID),
		chromedp.Dimensions("#mermaid", &model, chromedp.ByID),
	)
	return result_in_bytes, interface{}(model).(*BoxModel), err
}

func (r *RenderEngine) RenderAsPng(content string) ([]byte, *BoxModel, error) {
	return r.RenderAsScaledPng(content, 1.0)
}

func (r *RenderEngine) Cancel() {
	r.cancel()
}

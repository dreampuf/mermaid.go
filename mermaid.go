package mermaid_go

import (
	"context"
	_ "embed"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"

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

type svg struct {
	Width   string `xml:"width,attr"`
	Height  string `xml:"height,attr"`
	ViewBox string `xml:"viewBox,attr"`
}

func NewRenderEngine(ctx context.Context, statements ...string) (*RenderEngine, error) {
	ctx, cancel := chromedp.NewContext(ctx)
	var (
		lib_ready *runtime.RemoteObject
	)
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

func (r *RenderEngine) Render(content string) (string, string, string, error) {
	var (
		result string
	)
	err := chromedp.Run(r.ctx,
		chromedp.Evaluate(fmt.Sprintf("mermaid.render('mermaid', `%s`).then(({ svg }) => { return svg; });", content), &result, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
			return p.WithAwaitPromise(true)
		}),
	)
	if err != nil {
		return "", "", "", err
	}

	parsed := svg{}
	err = xml.Unmarshal([]byte(result), &parsed)
	if err != nil {
		return "", "", "", err
	}
	if parsed.Height == "" || parsed.Height[len(parsed.Height)-1:] == "%" {
		parsed.Height = strings.Split(parsed.ViewBox, " ")[3]
	}

	if parsed.Width == "" || parsed.Width[len(parsed.Width)-1:] == "%" {
		parsed.Width = strings.Split(parsed.ViewBox, " ")[2]
	}

	return result, parsed.Width, parsed.Height, nil
}

func (r *RenderEngine) RenderAsScaledPng(content string, scale float64) ([]byte, *BoxModel, error) {
	var (
		result_in_bytes []byte
		model           *dom.BoxModel
	)
	err := chromedp.Run(r.ctx,
		chromedp.Evaluate(fmt.Sprintf("mermaid.render('mermaid', `%s`).then(({ svg }) => { document.body.innerHTML = svg; });", content), nil),
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

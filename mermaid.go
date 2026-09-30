// Package mermaid_go renders [mermaid.js] diagrams to SVG and PNG from Go.
//
// Two backends render the same diagrams and satisfy the same [Renderer]
// interface. NewRenderEngine drives mermaid.js itself in a headless Chrome, and
// is the reference: whatever mermaid.js can draw, it draws. [NewMermanEngine]
// instead runs the [merman] CLI, a native reimplementation, which removes the
// browser dependency entirely at the cost of being a separate implementation of
// mermaid rather than mermaid itself.
//
// The chrome backend drives Chrome through [chromedp]. NewRenderEngine launches the
// browser once and loads the embedded mermaid.js bundle into a page; each render
// then reuses that page, so the browser is started once rather than per diagram.
// An engine owns operating system resources and must be released with
// [RenderEngine.Cancel].
//
// Renders are serialised on the engine's single page. Prefer the context-aware
// methods ([RenderEngine.RenderContext] and friends) in a server: their context
// bounds the wait for a turn as well as the render itself.
//
// Failures are classified with sentinel errors so callers can branch with
// [errors.Is] rather than on messages, which mainly decides whether a retry is
// worthwhile: [ErrRenderException] means the diagram was rejected and will fail
// identically next time, whereas [ErrTargetCrashed] and [context.DeadlineExceeded]
// may succeed on a retry, and [ErrEngineClosed] means the engine is spent and a
// new one is needed.
//
// [mermaid.js]: https://github.com/mermaid-js/mermaid
// [chromedp]: https://github.com/chromedp/chromedp
// [merman]: https://github.com/Latias94/merman
package mermaid_go

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/inspector"
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

// DefaultRenderTimeout bounds a single Render/RenderAsPng call. Without it a
// hung page (or a chrome target that never reports the node as visible) would
// block forever, because the engine context has no deadline of its own.
const DefaultRenderTimeout = 30 * time.Second

// DefaultStartupTimeout bounds loading mermaid.js and running the caller's
// statements in NewRenderEngine when the supplied context has no deadline of its
// own. WSURLReadTimeout only covers reading the DevTools URL from chrome's
// stderr, so without this the evaluation of the embedded 3.5MB bundle could hang
// indefinitely. Pass a context with a deadline to choose a different bound.
const DefaultStartupTimeout = 60 * time.Second

var (
	ErrMermaidNotReady = errors.New("mermaid.js initial failed")
	ErrFailedEncoding  = errors.New("failed to encode")
	// ErrTargetCrashed reports that chrome sent Inspector.targetCrashed (or
	// detached for a reason other than a normal shutdown). Renders issued
	// against a crashed target fail with this error joined to the underlying
	// chromedp error.
	ErrTargetCrashed = errors.New("chrome target crashed")
	// ErrRenderException reports that the backend rejected the diagram source.
	// It is worth separating from the transport and lifecycle failures: retrying
	// it will fail identically, whereas retrying ErrTargetCrashed or a timeout
	// may well succeed.
	//
	// Both backends use it, and each keeps its own detail reachable with
	// errors.As: chrome raises a JavaScript exception and leaves the
	// *runtime.ExceptionDetails for the script location and stack, while merman
	// exits 1 and leaves a *MermanExitError. Only chrome's detail is a genuine
	// exception, and only merman's status is imperfectly exclusive to the
	// diagram -- see MermanExitError.
	ErrRenderException = errors.New("render rejected the diagram source")
	// ErrUnsupportedOption reports a RenderOption that the called method cannot
	// honour, rather than ignoring it. WithBundle is the only such option: it
	// embeds the source in the SVG's <desc>, which a PNG has nowhere to put.
	ErrUnsupportedOption = errors.New("unsupported render option")
	// ErrEngineClosed reports that the engine's own context is done, because
	// Cancel was called or the context given to NewRenderEngine was cancelled.
	// Both that and a cancelled caller otherwise surface as an indistinguishable
	// context.Canceled, yet they call for opposite responses: a cancelled caller
	// is routine and the engine is still good, whereas a closed engine will fail
	// every subsequent render until a new one is built.
	ErrEngineClosed = errors.New("render engine is closed")
)

type BoxModel = dom.BoxModel

type RenderEngine struct {
	// sem serialises renders. It is a one-slot channel rather than a mutex so
	// that waiting for a turn can be abandoned when the caller's context is
	// cancelled; sync.Mutex offers no such thing.
	sem             chan struct{}
	ctx             context.Context
	cancel          context.CancelFunc
	allocatorCancel context.CancelFunc
	// renderTimeout is atomic so it can be adjusted while a render is running.
	renderTimeout atomic.Int64

	// crashMu guards the crash bookkeeping below. It is deliberately separate
	// from sem: the chromedp event goroutine takes it while a render is in
	// flight, and making that goroutine wait for a render would deadlock.
	crashMu      sync.Mutex
	crashed      bool
	detachReason string
	lastReported string
	crashHandler func(error)
}

var jsonMarshal = json.Marshal

// serializedRender wraps a render expression so it starts only once every
// earlier render in the page has settled. A render abandoned on a timeout keeps
// running in the page, and clearing the body under it breaks it; mermaid then
// reports that failure to whichever render is waiting in its queue, so the next
// render would fail for the previous one's sake.
func serializedRender(expr string) string {
	return fmt.Sprintf(`window.__mermaidGoRender = (window.__mermaidGoRender || Promise.resolve()).catch(() => {}).then(() => {
		document.body.innerHTML = '';
		return %s;
	});`, expr)
}

func NewRenderEngine(ctx context.Context, statements []string, options ...chromedp.ExecAllocatorOption) (*RenderEngine, error) {
	var (
		result string
	)

	args := make([]chromedp.ExecAllocatorOption, 0, len(chromedp.DefaultExecAllocatorOptions)+len(options)+1)
	args = append(args, chromedp.DefaultExecAllocatorOptions[:]...)

	deadline, ok := ctx.Deadline()
	if ok {
		timeout := time.Until(deadline)
		if timeout < 20*time.Second {
			timeout = 20 * time.Second
		}
		args = append(args, chromedp.WSURLReadTimeout(timeout))
	}
	args = append(args, options...)
	actx, allocatorCancel := chromedp.NewExecAllocator(ctx, args...)
	ctx, cancel := chromedp.NewContext(actx)

	engine := &RenderEngine{
		sem:             make(chan struct{}, 1),
		ctx:             ctx,
		cancel:          cancel,
		allocatorCancel: allocatorCancel,
	}
	engine.renderTimeout.Store(int64(DefaultRenderTimeout))
	// chromedp enables the Inspector domain during target setup, so this picks
	// up crashes for the lifetime of the engine. Registering before the first
	// Run is safe: chromedp queues listeners until the target exists.
	chromedp.ListenTarget(ctx, engine.handleTargetEvent)

	actions := []chromedp.Action{
		chromedp.Navigate(DefaultPage),
		// Evaluate asks for results by value unless handed a
		// **runtime.RemoteObject, so it would serialise the bundle's completion
		// value -- some 23KB of object graph -- and ship it over the websocket
		// only for it to be discarded here. Decline it instead. Overriding the
		// option is enough because Evaluate applies opts after its own default,
		// and a nil res is ignored outright.
		chromedp.Evaluate(SourceMermaid, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
			return p.WithReturnByValue(false)
		}),
		chromedp.Evaluate("mermaid.initialize({startOnLoad:false})", nil),
	}
	for _, stmt := range statements {
		actions = append(actions, chromedp.Evaluate(stmt, nil))
	}
	actions = append(actions, chromedp.Evaluate("typeof mermaid", &result))

	// Allocate the browser against the engine context before bounding anything.
	// chromedp starts chrome with exec.CommandContext using whichever context
	// reaches the first Run, so running the initialisation under a derived
	// deadline would tie chrome's lifetime to that deadline and kill it the
	// moment this function returned.
	err := chromedp.Run(ctx)
	if err == nil {
		startCtx := ctx
		if _, ok := ctx.Deadline(); !ok {
			var startCancel context.CancelFunc
			startCtx, startCancel = context.WithTimeout(ctx, DefaultStartupTimeout)
			defer startCancel()
		}
		err = chromedp.Run(startCtx, actions...)
	}
	if err == nil && result != "object" {
		// The value is the whole diagnostic: "undefined" means the bundle never
		// defined mermaid, anything else means it was replaced.
		err = fmt.Errorf("%w: typeof mermaid = %q", ErrMermaidNotReady, result)
	}
	if err != nil {
		cancel()
		if allocatorCancel != nil {
			allocatorCancel()
		}
		return nil, err
	}
	return engine, nil
}

// handleTargetEvent records chrome crash notifications. It runs on chromedp's
// event goroutine and must not block.
func (r *RenderEngine) handleTargetEvent(ev any) {
	switch e := ev.(type) {
	case *inspector.EventTargetCrashed:
		r.noteCrash("")
	case *inspector.EventDetached:
		// A normal Cancel() detaches too; only unexpected reasons such as
		// "Render process gone." indicate a crash.
		switch e.Reason {
		case inspector.DetachReasonTargetClosed, inspector.DetachReasonCanceledByUser:
		default:
			r.noteCrash(e.Reason.String())
		}
	case *inspector.EventTargetReloadedAfterCrash:
		r.crashMu.Lock()
		r.crashed, r.detachReason, r.lastReported = false, "", ""
		r.crashMu.Unlock()
	}
}

func (r *RenderEngine) noteCrash(reason string) {
	r.crashMu.Lock()
	r.crashed = true
	if reason != "" {
		r.detachReason = reason
	}
	err := r.crashErrLocked()
	handler := r.crashHandler
	// Crash and detach arrive as two events, the second one carrying the
	// reason. Report each time the message gains information, not per event.
	report := handler != nil && err != nil && err.Error() != r.lastReported
	if report {
		r.lastReported = err.Error()
	}
	r.crashMu.Unlock()

	if report {
		reportCrash(handler, err)
	}
}

// reportCrash shields chromedp's event goroutine from a panicking handler. The
// handler runs on a goroutine the consumer does not own and cannot wrap in a
// recover of their own, so a panic there would take the process down. There is
// nowhere to report the panic to, so it is swallowed deliberately.
func reportCrash(handler func(error), err error) {
	defer func() { _ = recover() }()
	handler(err)
}

func (r *RenderEngine) crashErrLocked() error {
	if !r.crashed {
		return nil
	}
	if r.detachReason != "" {
		return fmt.Errorf("%w: %s", ErrTargetCrashed, r.detachReason)
	}
	return ErrTargetCrashed
}

// CrashError reports ErrTargetCrashed (wrapped with chrome's detach reason when
// one was given) if the underlying target has crashed, and nil otherwise. The
// state is cleared if chrome reloads the target after the crash.
func (r *RenderEngine) CrashError() error {
	r.crashMu.Lock()
	defer r.crashMu.Unlock()
	return r.crashErrLocked()
}

// SetTargetCrashedHandler installs fn to be called when chrome reports that the
// target crashed, with the same error CrashError returns. fn runs on chromedp's
// event goroutine, so it must return promptly and must not call back into the
// engine; hand the error to a logger or a buffered channel instead. It may be
// called more than once for a single crash as chrome supplies more detail. Pass
// nil to remove a previously installed handler.
func (r *RenderEngine) SetTargetCrashedHandler(fn func(error)) {
	r.crashMu.Lock()
	defer r.crashMu.Unlock()
	r.crashHandler = fn
}

// SetRenderTimeout overrides DefaultRenderTimeout for subsequent renders. A
// value <= 0 disables the per-render deadline, restoring the old behaviour of
// waiting as long as the engine context allows.
func (r *RenderEngine) SetRenderTimeout(d time.Duration) {
	r.renderTimeout.Store(int64(d))
}

type RenderOption func(*renderOptions)

type renderOptions struct {
	bundle     bool
	timeout    time.Duration
	hasTimeout bool
}

func WithBundle() RenderOption {
	return func(o *renderOptions) {
		o.bundle = true
	}
}

// WithTimeout overrides the engine's render timeout for a single call. A value
// <= 0 disables the deadline for that call.
func WithTimeout(d time.Duration) RenderOption {
	return func(o *renderOptions) {
		o.timeout = d
		o.hasTimeout = true
	}
}

// acquire waits for this render's turn, giving up if the caller's context is
// cancelled or the engine is closed. Without it a caller could sit behind an
// unbounded queue of other renders with no way out, because the per-render
// deadline only starts once a render actually begins.
func (r *RenderEngine) acquire(ctx context.Context) (release func(), err error) {
	release, err = waitForSlot(ctx, r.ctx, r.sem)
	if err != nil {
		return nil, r.renderErr(ctx, err)
	}
	return release, nil
}

// renderErr classifies a failed render: the lifecycle labels every backend
// applies, plus the crash annotation only chrome can supply.
func (r *RenderEngine) renderErr(caller context.Context, err error) error {
	return r.annotateCrash(classifyRenderErr(caller, r.ctx, err))
}

// renderContext derives the context for one render, applying whichever of the
// engine's timeout, this call's WithTimeout and the caller's deadline lands
// first. See deriveRenderContext for how the two contexts are combined.
//
// Cancelling a context derived from the engine context only aborts the in-flight
// commands, so the engine stays usable after a timeout. This is not true of the
// very first Run against a fresh engine, which is why NewRenderEngine allocates
// the browser separately -- see the comment there.
func (r *RenderEngine) renderContext(caller context.Context, opts *renderOptions) (context.Context, context.CancelFunc) {
	return deriveRenderContext(r.ctx, caller, r.effectiveTimeout(opts))
}

// effectiveTimeout is the per-render deadline this call asked for, defaulting to
// the engine's. A value <= 0 means no deadline of our own.
func (r *RenderEngine) effectiveTimeout(opts *renderOptions) time.Duration {
	if opts.hasTimeout {
		return opts.timeout
	}
	return time.Duration(r.renderTimeout.Load())
}

// annotateCrash classifies a render failure so callers can act on it without
// matching on error strings: it joins the crash error when the browser has died,
// and tags a JavaScript exception so an invalid diagram is distinguishable from
// an infrastructure failure.
func (r *RenderEngine) annotateCrash(err error) error {
	if err == nil {
		return nil
	}
	// chromedp returns *runtime.ExceptionDetails as the error itself, which is
	// discoverable only if you already know to look for it.
	var exception *runtime.ExceptionDetails
	if errors.As(err, &exception) {
		err = fmt.Errorf("%w: %w", ErrRenderException, err)
	}
	if crashErr := r.CrashError(); crashErr != nil {
		return errors.Join(err, crashErr)
	}
	return err
}

// Render renders content to SVG. It is equivalent to RenderContext with a
// background context, so it cannot be cancelled by the caller and is bounded
// only by the engine's render timeout.
func (r *RenderEngine) Render(content string, opts ...RenderOption) (string, error) {
	return r.RenderContext(context.Background(), content, opts...)
}

// RenderContext renders content to SVG, giving up if ctx is cancelled. The
// context covers the wait for other renders to finish as well as the render
// itself, and its deadline applies if it is sooner than the engine's render
// timeout. Only cancellation and the deadline are taken from ctx; its values are
// not, because the render has to run on chromedp's own context.
func (r *RenderEngine) RenderContext(ctx context.Context, content string, opts ...RenderOption) (string, error) {
	var (
		result string
	)

	renderOpts := &renderOptions{}
	for _, opt := range opts {
		opt(renderOpts)
	}

	encodedContent, err := jsonMarshal(content)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrFailedEncoding, err)
	}

	release, err := r.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()

	var script string
	if renderOpts.bundle {
		script = serializedRender(fmt.Sprintf(`mermaid.render('mermaid', %s).then(({ svg }) => {
			const parser = new DOMParser();
			const doc = parser.parseFromString(svg, 'image/svg+xml');
			const svgElem = doc.querySelector('svg');
			const desc = doc.createElementNS('http://www.w3.org/2000/svg', 'desc');
			desc.textContent = %s;
			svgElem.insertBefore(desc, svgElem.firstChild);
			return new XMLSerializer().serializeToString(doc);
		})`, string(encodedContent), string(encodedContent)))
	} else {
		script = serializedRender(fmt.Sprintf("mermaid.render('mermaid', %s).then(({ svg }) => svg)", string(encodedContent)))
	}

	runCtx, cancel := r.renderContext(ctx, renderOpts)
	defer cancel()

	err = chromedp.Run(runCtx,
		chromedp.Evaluate(script, &result, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
			return p.WithAwaitPromise(true)
		}),
	)
	if err != nil {
		return "", r.renderErr(ctx, err)
	}
	return result, nil
}

// RenderAsScaledPng renders content to a PNG at the given scale. It is
// equivalent to RenderAsScaledPngContext with a background context.
func (r *RenderEngine) RenderAsScaledPng(content string, scale float64, opts ...RenderOption) ([]byte, *BoxModel, error) {
	return r.RenderAsScaledPngContext(context.Background(), content, scale, opts...)
}

// RenderAsScaledPngContext renders content to a PNG at the given scale, giving
// up if ctx is cancelled. See RenderContext for how ctx is applied. On failure
// it returns no image and no box model, so a caller cannot mistake a partial
// screenshot for a complete one.
func (r *RenderEngine) RenderAsScaledPngContext(ctx context.Context, content string, scale float64, opts ...RenderOption) ([]byte, *BoxModel, error) {
	var (
		result_in_bytes []byte
		model           *dom.BoxModel
	)

	renderOpts := &renderOptions{}
	for _, opt := range opts {
		opt(renderOpts)
	}
	if renderOpts.bundle {
		return nil, nil, fmt.Errorf("%w: WithBundle has no effect on a PNG, the source is embedded in the SVG's <desc>", ErrUnsupportedOption)
	}

	encodedContent, err := jsonMarshal(content)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrFailedEncoding, err)
	}
	script := serializedRender(fmt.Sprintf("mermaid.render('mermaid', %s).then(({ svg }) => { document.body.innerHTML = svg; })", string(encodedContent)))

	release, err := r.acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer release()

	runCtx, cancel := r.renderContext(ctx, renderOpts)
	defer cancel()

	err = chromedp.Run(runCtx,
		chromedp.Evaluate(script, nil, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
			return p.WithAwaitPromise(true)
		}),
		chromedp.ScreenshotScale("#mermaid", scale, &result_in_bytes, chromedp.ByID),
		chromedp.Dimensions("#mermaid", &model, chromedp.ByID),
	)
	if err != nil {
		return nil, nil, r.renderErr(ctx, err)
	}
	return result_in_bytes, model, nil
}

// RenderAsPng renders content to a PNG. It is equivalent to
// RenderAsPngContext with a background context.
func (r *RenderEngine) RenderAsPng(content string, opts ...RenderOption) ([]byte, *BoxModel, error) {
	return r.RenderAsScaledPngContext(context.Background(), content, 1.0, opts...)
}

// RenderAsPngContext renders content to a PNG, giving up if ctx is cancelled.
func (r *RenderEngine) RenderAsPngContext(ctx context.Context, content string, opts ...RenderOption) ([]byte, *BoxModel, error) {
	return r.RenderAsScaledPngContext(ctx, content, 1.0, opts...)
}

// Cancel closes the browser and releases every resource held by the engine. It
// deliberately does not wait for an in-flight render: context.CancelFunc is safe
// for concurrent use, so cancelling straight away aborts a render in progress
// instead of queueing behind it. Calling it more than once is harmless.
func (r *RenderEngine) Cancel() {
	r.cancel()
	if r.allocatorCancel != nil {
		r.allocatorCancel()
	}
}

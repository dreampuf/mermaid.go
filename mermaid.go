// Package mermaid_go renders [mermaid.js] diagrams to SVG and PNG from Go.
//
// Three backends render the same diagrams and satisfy the same [Renderer]
// interface. NewRenderEngine drives mermaid.js itself in a headless Chrome, and
// is the reference: whatever mermaid.js can draw, it draws. [NewMermanEngine]
// instead runs the [merman] CLI, a native reimplementation, which removes the
// browser dependency entirely at the cost of being a separate implementation of
// mermaid rather than mermaid itself. The [mermanwasm] package runs merman's
// WebAssembly build in process, needing neither a browser nor a binary; it is a
// separate package so that only programs which use it link its runtime and
// module.
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
// [mermanwasm]: https://pkg.go.dev/github.com/dreampuf/mermaid.go/mermanwasm
package mermaid_go

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/inspector"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/dreampuf/mermaid.go/internal/render"
)

// SourceMermaid is the mermaid.js bundle NewRenderEngine loads into the page,
// the release embedded in this package unless the program assigns another. The
// linker drops it from programs that never call NewRenderEngine.
//
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
// own. Nothing in chromedp bounds that work, so without this the evaluation of
// the embedded 3.5MB bundle could hang indefinitely. Pass a context with a
// deadline to choose a different bound.
const DefaultStartupTimeout = 60 * time.Second

var (
	ErrMermaidNotReady = errors.New("mermaid.js initial failed")
	ErrFailedEncoding  = render.ErrFailedEncoding
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
	// errors.As: chrome raises a JavaScript exception and leaves a
	// *chromedp.ExceptionError, which embeds the *runtime.ExceptionDetails with
	// the script location and stack, while merman
	// exits 1 and leaves a *MermanExitError. Only chrome's detail is a genuine
	// exception, and only merman's status is imperfectly exclusive to the
	// diagram -- see MermanExitError.
	ErrRenderException = render.ErrRenderException
	// ErrUnsupportedOption reports a RenderOption that the called method cannot
	// honour, rather than ignoring it. WithBundle is the only such option: it
	// embeds the source in the SVG's <desc>, which a PNG has nowhere to put.
	ErrUnsupportedOption = render.ErrUnsupportedOption
	// ErrEngineClosed reports that the engine's own context is done, because
	// Cancel was called, the context given to NewRenderEngine was cancelled, or
	// the browser process died. In the last case the error also wraps the
	// process's *exec.ExitError, so errors.As tells an OOM kill from a Cancel.
	// Both that and a cancelled caller otherwise surface as an indistinguishable
	// context.Canceled, yet they call for opposite responses: a cancelled caller
	// is routine and the engine is still good, whereas a closed engine will fail
	// every subsequent render until a new one is built.
	ErrEngineClosed = render.ErrEngineClosed
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
	// from sem: the event goroutines take it while a render is in flight, and
	// a crash must be recorded without waiting for that render to finish.
	crashMu      sync.Mutex
	crashed      bool
	detachReason string
	lastReported string
	crashHandler func(error)

	// consoleHandler is read for every console message, so it is atomic rather
	// than behind crashMu.
	consoleHandler atomic.Pointer[func(chromedp.ConsoleMessage)]
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

// detach reasons chrome gives when the target goes away on purpose.
const (
	detachReasonTargetClosed   = "target_closed"
	detachReasonCanceledByUser = "canceled_by_user"
)

func NewRenderEngine(ctx context.Context, statements []string, options ...chromedp.ExecAllocatorOption) (*RenderEngine, error) {
	var (
		result string
	)

	// Fail before launching a browser that could never render anything.
	if SourceMermaid == "" {
		return nil, fmt.Errorf("%w: SourceMermaid is empty", ErrMermaidNotReady)
	}

	args := make([]chromedp.ExecAllocatorOption, 0, len(chromedp.DefaultExecAllocatorOptions)+len(options)+1)
	args = append(args, chromedp.DefaultExecAllocatorOptions[:]...)

	// WSURLReadTimeout only matters when the caller opts into the websocket
	// transport (remote.WebSocket); the default pipe transport ignores it.
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

	actions := []chromedp.Action[chromedp.Void]{
		chromedp.Navigate(DefaultPage),
		// Evaluate asks for results by value unless T is *runtime.RemoteObject,
		// so it would serialise the bundle's completion value -- some 23KB of
		// object graph -- and ship it to us only for Void to discard it here.
		// Decline it instead. Overriding the option is enough because Evaluate
		// applies opts after its own default.
		chromedp.Evaluate[chromedp.Void](SourceMermaid, func(p *runtime.EvaluateParams) {
			p.ReturnByValue = new(false)
		}),
		chromedp.Evaluate[chromedp.Void]("mermaid.initialize({startOnLoad:false})"),
	}
	for _, stmt := range statements {
		actions = append(actions, chromedp.Evaluate[chromedp.Void](stmt))
	}

	// Allocate the browser against the engine context before bounding anything.
	// chromedp starts chrome with exec.CommandContext using whichever context
	// reaches the first Run, so running the initialisation under a derived
	// deadline would tie chrome's lifetime to that deadline and kill it the
	// moment this function returned.
	err := chromedp.Do(ctx)
	if err == nil {
		// chromedp enables the Inspector domain during target setup, so these
		// pick up crashes for the lifetime of the engine. Each subscription
		// starts when Events returns and ends with the engine context.
		go listen(chromedp.Events(ctx, inspector.TargetCrashed), engine.handleTargetEvent)
		go listen(chromedp.Events(ctx, inspector.Detached), engine.handleTargetEvent)
		go listen(chromedp.Events(ctx, inspector.TargetReloadedAfterCrash), engine.handleTargetEvent)
		// The subscription's queue is unbounded, so it is drained for the
		// lifetime of the engine whether or not a handler is installed.
		go engine.forwardConsole(chromedp.Console(ctx))

		startCtx, startCancel := render.StartupContext(ctx, DefaultStartupTimeout)
		defer startCancel()
		err = chromedp.Do(startCtx, actions...)
		if err == nil {
			result, err = chromedp.Run(startCtx, chromedp.Evaluate[string]("typeof mermaid"))
		}
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

// listen hands each event to handle until the subscription ends, which happens
// when the engine context is done.
func listen[E any](events iter.Seq2[E, error], handle func(any)) {
	for ev, err := range events {
		if err != nil {
			return
		}
		handle(&ev)
	}
}

// handleTargetEvent records chrome crash notifications. It runs on the engine's
// event goroutines, one per subscribed event, and must not block.
func (r *RenderEngine) handleTargetEvent(ev any) {
	switch e := ev.(type) {
	case *inspector.EventTargetCrashed:
		r.noteCrash("")
	case *inspector.EventDetached:
		// A normal Cancel() detaches too; only unexpected reasons such as
		// "Render process gone." indicate a crash.
		switch e.Reason {
		case detachReasonTargetClosed, detachReasonCanceledByUser:
		default:
			r.noteCrash(e.Reason)
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
		callHandler(handler, err)
	}
}

// forwardConsole hands each console message to the installed handler, if any,
// until the subscription ends with the engine context.
func (r *RenderEngine) forwardConsole(messages iter.Seq2[chromedp.ConsoleMessage, error]) {
	for m, err := range messages {
		if err != nil {
			return
		}
		if handler := r.consoleHandler.Load(); handler != nil {
			callHandler(*handler, m)
		}
	}
}

// callHandler shields the engine's event goroutines from a panicking handler.
// The handler runs on a goroutine the consumer does not own and cannot wrap in a
// recover of their own, so a panic there would take the process down. There is
// nowhere to report the panic to, so it is swallowed deliberately.
func callHandler[T any](handler func(T), v T) {
	defer func() { _ = recover() }()
	handler(v)
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
// target crashed, with the same error CrashError returns. fn runs on the
// engine's event goroutine, so it must return promptly and must not call back into the
// engine; hand the error to a logger or a buffered channel instead. It may be
// called more than once for a single crash as chrome supplies more detail. Pass
// nil to remove a previously installed handler.
func (r *RenderEngine) SetTargetCrashedHandler(fn func(error)) {
	r.crashMu.Lock()
	defer r.crashMu.Unlock()
	r.crashHandler = fn
}

// SetConsoleHandler installs fn to be called with every message the page writes
// to the console: mermaid's own log, uncaught exceptions and unhandled promise
// rejections, and chrome's log entries. It helps explain a render that failed or
// came out wrong. mermaid logs only fatal errors by default; raise its logLevel
// with a statement passed to NewRenderEngine, such as
// `mermaid.initialize({logLevel: 'warn'})`, to see more.
//
// Messages written before fn is installed, including those from NewRenderEngine
// itself, are dropped. fn runs on the engine's event goroutine, one message at a
// time in the order the page wrote them, so it must return promptly and must not
// call back into the engine. Renders are serialised, but a render abandoned on a
// timeout keeps running in the page, so a message is not reliably tied to the
// render in flight when it arrives. Pass nil to remove a previously installed
// handler.
func (r *RenderEngine) SetConsoleHandler(fn func(chromedp.ConsoleMessage)) {
	if fn == nil {
		r.consoleHandler.Store(nil)
		return
	}
	r.consoleHandler.Store(&fn)
}

// SetRenderTimeout overrides DefaultRenderTimeout for subsequent renders. A
// value <= 0 disables the per-render deadline, restoring the old behaviour of
// waiting as long as the engine context allows.
func (r *RenderEngine) SetRenderTimeout(d time.Duration) {
	r.renderTimeout.Store(int64(d))
}

type RenderOption func(*render.Options)

func WithBundle() RenderOption {
	return func(o *render.Options) {
		o.Bundle = true
	}
}

// WithTimeout overrides the engine's render timeout for a single call. A value
// <= 0 disables the deadline for that call.
func WithTimeout(d time.Duration) RenderOption {
	return func(o *render.Options) {
		o.Timeout = d
		o.HasTimeout = true
	}
}

// acquire waits for this render's turn, giving up if the caller's context is
// cancelled or the engine is closed. Without it a caller could sit behind an
// unbounded queue of other renders with no way out, because the per-render
// deadline only starts once a render actually begins.
func (r *RenderEngine) acquire(ctx context.Context) (release func(), err error) {
	release, err = render.WaitForSlot(ctx, r.ctx, r.sem)
	if err != nil {
		return nil, r.renderErr(ctx, err)
	}
	return release, nil
}

// renderErr classifies a failed render: the lifecycle labels every backend
// applies, plus the crash annotation only chrome can supply.
func (r *RenderEngine) renderErr(caller context.Context, err error) error {
	return r.annotateCrash(render.ClassifyErr(caller, r.ctx, r.withBrowserExit(err)))
}

// withBrowserExit replaces a bare context.Canceled with the browser's exit
// error when the browser process died on its own. chromedp attaches that error
// only to the render in flight at the time; every later one fails before
// reaching chromedp, in acquire, and would otherwise not say that the browser
// was killed rather than cancelled. chromedp's error wraps context.Canceled
// too, so nothing is lost by the replacement.
func (r *RenderEngine) withBrowserExit(err error) error {
	if r.ctx.Err() == nil || !errors.Is(err, context.Canceled) || errors.As(err, new(*exec.ExitError)) {
		return err
	}
	// Run decorates the error of its action with the exit error, and adds
	// nothing when the engine was closed with Cancel.
	_, exitErr := chromedp.Run(r.ctx, func(ctx context.Context, _ *chromedp.Target) (chromedp.Void, error) {
		return chromedp.Void{}, ctx.Err()
	})
	if errors.As(exitErr, new(*exec.ExitError)) {
		return exitErr
	}
	return err
}

// renderContext derives the context for one render, applying whichever of the
// engine's timeout, this call's WithTimeout and the caller's deadline lands
// first. See deriveRenderContext for how the two contexts are combined.
//
// Cancelling a context derived from the engine context only aborts the in-flight
// commands, so the engine stays usable after a timeout. This is not true of the
// very first Run against a fresh engine, which is why NewRenderEngine allocates
// the browser separately -- see the comment there.
func (r *RenderEngine) renderContext(caller context.Context, opts *render.Options) (context.Context, context.CancelFunc) {
	return render.DeriveContext(r.ctx, caller, opts.TimeoutOr(&r.renderTimeout))
}

// annotateCrash classifies a render failure so callers can act on it without
// matching on error strings: it joins the crash error when the browser has died,
// and tags a JavaScript exception so an invalid diagram is distinguishable from
// an infrastructure failure.
func (r *RenderEngine) annotateCrash(err error) error {
	if err == nil {
		return nil
	}
	// chromedp returns a *chromedp.ExceptionError as the error itself, which is
	// discoverable only if you already know to look for it.
	var exception *chromedp.ExceptionError
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

	renderOpts := render.NewOptions(opts)

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
	if renderOpts.Bundle {
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

	result, err = chromedp.Run(runCtx, chromedp.Evaluate[string](script, chromedp.EvalAwaitPromise))
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
	renderOpts := render.NewOptions(opts)
	if renderOpts.Bundle {
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

	type pngResult struct {
		png   []byte
		model *dom.BoxModel
	}
	res, err := chromedp.Run(runCtx, func(ctx context.Context, t *chromedp.Target) (res pngResult, err error) {
		if _, err = chromedp.Evaluate[chromedp.Void](script, chromedp.EvalAwaitPromise)(ctx, t); err != nil {
			return res, err
		}
		if res.png, err = chromedp.ScreenshotScale(chromedp.ID("mermaid"), scale)(ctx, t); err != nil {
			return res, err
		}
		res.model, err = chromedp.Dimensions(chromedp.ID("mermaid"))(ctx, t)
		return res, err
	})
	if err != nil {
		return nil, nil, r.renderErr(ctx, err)
	}
	return res.png, res.model, nil
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

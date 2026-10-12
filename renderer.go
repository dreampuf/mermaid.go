package mermaid_go

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Renderer is the behaviour shared by the render backends, so a program can
// choose between them at startup and pass one around without caring which it
// got. [RenderEngine] drives a headless chrome through chromedp; [MermanEngine]
// shells out to the merman CLI, which parses and lays diagrams out natively and
// therefore needs no browser at all.
//
// Backend-specific facilities stay off the interface: chrome's crash and console
// reporting ([RenderEngine.CrashError], [RenderEngine.SetTargetCrashedHandler],
// [RenderEngine.SetConsoleHandler]) has no merman equivalent, and merman's capability report has no chrome equivalent.
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

// waitForSlot waits for a render's turn, giving up if the caller's context is
// cancelled or the engine is closed. Without it a caller could sit behind an
// unbounded queue of other renders with no way out, because the per-render
// deadline only starts once a render actually begins. The error is returned
// unclassified; callers pass it through their own classifyRenderErr.
func waitForSlot(ctx, engineCtx context.Context, sem chan struct{}) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-engineCtx.Done():
		return nil, engineCtx.Err()
	}
}

// classifyRenderErr labels a failed render with the lifecycle facts a caller
// cannot recover from the error alone. When the caller's context is what ended
// it, the derived context reports a bare Canceled; the caller's own cause says
// why, so prefer it. And say so when the engine itself is what ended the render,
// since that calls for building a new engine rather than simply retrying.
func classifyRenderErr(caller, engineCtx context.Context, err error) error {
	if errors.Is(err, context.Canceled) {
		if cause := context.Cause(caller); cause != nil {
			err = cause
		}
	}
	if engineCtx.Err() != nil {
		err = fmt.Errorf("%w: %w", ErrEngineClosed, err)
	}
	return err
}

// deriveRenderContext derives the context for one render. It is rooted at the
// engine context, so cancelling the engine aborts renders in flight, while the
// caller's context contributes its deadline and its cancellation but not its
// values.
func deriveRenderContext(engineCtx, caller context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	// Honour whichever of the two deadlines lands first, so a caller asking for
	// less than the engine's timeout gets a truthful DeadlineExceeded rather
	// than the bare Canceled that propagation alone would produce.
	deadline, hasDeadline := caller.Deadline()
	switch {
	case timeout > 0 && hasDeadline && time.Now().Add(timeout).Before(deadline):
		ctx, cancel = context.WithTimeout(engineCtx, timeout)
	case hasDeadline:
		ctx, cancel = context.WithDeadline(engineCtx, deadline)
	case timeout > 0:
		ctx, cancel = context.WithTimeout(engineCtx, timeout)
	default:
		ctx, cancel = context.WithCancel(engineCtx)
	}

	stop := context.AfterFunc(caller, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

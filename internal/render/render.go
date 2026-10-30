// Package render holds what the backends of mermaid.go share: the render
// options, the sentinels failures are classified with, and the handling of
// contexts and output common to them. The sentinels are documented where
// package mermaid_go re-exports them.
package render

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

var (
	ErrFailedEncoding    = errors.New("failed to encode")
	ErrRenderException   = errors.New("render rejected the diagram source")
	ErrUnsupportedOption = errors.New("unsupported render option")
	ErrEngineClosed      = errors.New("render engine is closed")
	ErrMermanUnavailable = errors.New("merman unavailable")
	ErrMermanCapability  = errors.New("merman lacks a required capability")
	ErrMermanFailed      = errors.New("merman render failed")
)

// Options are what a render's RenderOptions ask for.
type Options struct {
	Bundle     bool
	Timeout    time.Duration
	HasTimeout bool
}

// NewOptions applies a render's options.
func NewOptions[O ~func(*Options)](opts []O) *Options {
	o := &Options{}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// TimeoutOr is the per-render deadline this call asked for, defaulting to the
// engine's. A value <= 0 means no deadline of our own.
func (o *Options) TimeoutOr(engine *atomic.Int64) time.Duration {
	if o.HasTimeout {
		return o.Timeout
	}
	return time.Duration(engine.Load())
}

// WaitForSlot waits for a render's turn, giving up if the caller's context is
// cancelled or the engine is closed. Without it a caller could sit behind an
// unbounded queue of other renders with no way out, because the per-render
// deadline only starts once a render actually begins. The error is returned
// unclassified; callers pass it through their own ClassifyErr.
func WaitForSlot(ctx, engineCtx context.Context, sem chan struct{}) (release func(), err error) {
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

// AcquireSlot is WaitForSlot with its error classified, as the merman backends
// report it.
func AcquireSlot(caller, engineCtx context.Context, sem chan struct{}) (release func(), err error) {
	release, err = WaitForSlot(caller, engineCtx, sem)
	if err != nil {
		return nil, ClassifyErr(caller, engineCtx, err)
	}
	return release, nil
}

// ClassifyErr labels a failed render with the lifecycle facts a caller
// cannot recover from the error alone. When the caller's context is what ended
// it, the derived context reports a bare Canceled; the caller's own cause says
// why, so prefer it. And say so when the engine itself is what ended the render,
// since that calls for building a new engine rather than simply retrying.
func ClassifyErr(caller, engineCtx context.Context, err error) error {
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

// StartupContext bounds an engine's start-up by d, unless ctx carries a
// deadline of its own or d is <= 0.
func StartupContext(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok || d <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// DeriveContext derives the context for one render. It is rooted at the
// engine context, so cancelling the engine aborts renders in flight, while the
// caller's context contributes its deadline and its cancellation but not its
// values.
func DeriveContext(engineCtx, caller context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
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

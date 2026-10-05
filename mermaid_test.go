package mermaid_go

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/inspector"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// renderTimeout bounds engine startup in the subtests that launch their own
// browser; chrome plus the 3.5MB mermaid bundle is slow under -race.
var renderTimeout = 60 * time.Second

func TestRenderEngine_Render(t *testing.T) {
	cases := []struct {
		content/*, result */ string
		wantException bool
	}{
		{content: `graph TD;
    A-->B;
    A-->C;
    B-->D;
    C-->D;`},
		{content: `sequenceDiagram
			participant Alice
			participant Bob
			Alice->>John: Hello John, how are you?
			loop Healthcheck
			John->>John: Fight against hypochondria
			end
			Note right of John: Rational thoughts <br/>prevail!
			John-->>Alice: Great!
			John->>Bob: How about you?
			Bob-->>John: Jolly good!`},
		{content: `gantt
dateFormat  YYYY-MM-DD
title Adding GANTT diagram to mermaid
excludes weekdays 2014-01-10

section A section
Completed task            :done,    des1, 2014-01-06,2014-01-08
Active task               :active,  des2, 2014-01-09, 3d
Future task               :         des3, after des2, 5d
Future task2               :         des4, after des3, 5d`},
		{content: `classDiagram
Class01 <|-- AveryLongClass : Cool
Class03 *-- Class04
Class05 o-- Class06
Class07 .. Class08
Class09 --> C2 : Where am i?
Class09 --* C3
Class09 --|> Class07
Class07 : equals()
Class07 : Object[] elementData
Class01 : size()
Class01 : int chimp
Class01 : int gorilla
Class08 <--> C2: Cool label`},
		{content: `gitGraph
       commit
       commit
       branch develop
       commit
       commit
       commit
       checkout main
       commit
       commit
       merge develop
       commit
       commit`},
		{content: `erDiagram
    CUSTOMER ||--o{ ORDER : places
    ORDER ||--|{ LINE-ITEM : contains
    CUSTOMER }|..|{ DELIVERY-ADDRESS : uses
`},
		{content: `journey
    title My working day
    section Go to work
      Make tea: 5: Me
      Go upstairs: 3: Me
      Do work: 1: Me, Cat
    section Go home
      Go downstairs: 5: Me
      Sit down: 5: Me`},
		{content: `graph TD;
    A-->B['name'];
    A-->C["pic"];
    B-->D;
    C-->D;`},
		{content: `graph TD;
    A-->B['name'];
    A-->;`, wantException: true},
		{content: `graph TD;
	A-->B["` + "`Hello World`" + `"];
	B-->C;`},
	}

	// The engine outlives the whole suite, so it must not be tied to a
	// per-render deadline; each render is bounded by DefaultRenderTimeout.
	re1, err := NewRenderEngine(context.Background(),
		[]string{`mermaid.initialize({'theme': 'base', 'themeVariables': { 'primaryColor': '#1473e6'}});`})
	if err != nil {
		t.Fatalf("NewRenderEngine() error = %v", err)
	}

	defer re1.Cancel()

	t.Run("BundleDiagram", func(t *testing.T) {
		content := "graph TD; A-->B;"
		got, err := re1.Render(content, WithBundle())
		if err != nil {
			t.Errorf("Render() error = %v", err)
		}
		expected := "graph TD; A--&gt;B;"
		if !strings.Contains(got, "<desc>"+expected+"</desc>") {
			t.Errorf("Render() expected to contain <desc>%s</desc>, but got %s", expected, got)
		}
	})

	t.Run("InvalidSyntax", func(t *testing.T) {
		content := "graph TD; A---;" // Invalid syntax
		_, err := re1.Render(content)
		if err == nil {
			t.Error("Render() expected error for invalid syntax, but got nil")
		}
	})

	t.Run("Scaling", func(t *testing.T) {
		content := "graph TD; A-->B;"
		img1, _, err := re1.RenderAsScaledPng(content, 1.0)
		if err != nil {
			t.Fatalf("RenderAsScaledPng(1.0) error = %v", err)
		}
		img2, _, err := re1.RenderAsScaledPng(content, 2.0)
		if err != nil {
			t.Fatalf("RenderAsScaledPng(2.0) error = %v", err)
		}

		if len(img2) <= len(img1) {
			t.Errorf("RenderAsScaledPng() expected larger image data for 2.0 scale than 1.0, got %v bytes vs %v bytes", len(img2), len(img1))
		}
	})

	t.Run("SequentialDifferentPngs", func(t *testing.T) {
		content1 := "graph TD; A-->B;"
		content2 := "sequenceDiagram; Alice->>Bob: Hello John, how are you?; Bob-->>Alice: Fine!"
		img1, box1, err := re1.RenderAsPng(content1)
		if err != nil {
			t.Fatalf("RenderAsPng(content1) error = %v", err)
		}
		img2, box2, err := re1.RenderAsPng(content2)
		if err != nil {
			t.Fatalf("RenderAsPng(content2) error = %v", err)
		}
		// Render content2 again
		img3, box3, err := re1.RenderAsPng(content2)
		if err != nil {
			t.Fatalf("RenderAsPng(content2 second time) error = %v", err)
		}
		t.Logf("box1: %#v, box2: %#v, box3: %#v", box1, box2, box3)
		if string(img2) == string(img1) {
			t.Errorf("img2 (sequence diagram) was identical to img1 (flowchart) because screenshot was taken before promise resolved!")
		}
		if string(img2) != string(img3) {
			t.Errorf("img2 and img3 should both be sequence diagrams, but img2 was stale!")
		}
	})

	t.Run("InvalidSyntaxPng", func(t *testing.T) {
		content := "graph TD; A---;" // Invalid syntax
		_, _, err := re1.RenderAsPng(content)
		if err == nil {
			t.Error("RenderAsPng() expected error for invalid syntax, but got nil")
		}
	})

	t.Run("ConcurrentRenders", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				content := "graph TD; A-->B;"
				svg, err := re1.Render(content)
				if err != nil {
					t.Errorf("Concurrent Render() error = %v", err)
				}
				if !strings.HasPrefix(svg, "<svg") {
					t.Errorf("Concurrent Render() invalid svg")
				}
			}(i)
		}
		wg.Wait()
	})

	t.Run("CancelledContextStartup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		engine, err := NewRenderEngine(ctx, nil)
		if err == nil {
			t.Error("NewRenderEngine() expected error with cancelled context, got nil")
		}
		if engine != nil {
			t.Error("NewRenderEngine() expected nil engine on error, got non-nil")
		}
	})

	t.Run("Cancel", func(t *testing.T) {
		ctx := context.Background()
		re, err := NewRenderEngine(ctx, nil)
		if err != nil {
			t.Fatalf("NewRenderEngine() error = %v", err)
		}
		re.Cancel()
		_, err = re.Render("graph TD; A-->B;")
		if err == nil {
			t.Error("Render() expected error after Cancel(), but got nil")
		}
	})

	t.Run("SequentialRenders", func(t *testing.T) {
		contents := []string{
			"graph TD; A-->B;",
			"sequenceDiagram; Alice->>Bob: Hello;",
			"pie title Rats; \"Cats\" : 45; \"Dogs\" : 55",
		}
		for _, content := range contents {
			got, err := re1.Render(content)
			if err != nil {
				t.Errorf("Render(%s) error = %v", content, err)
			}
			if !strings.HasPrefix(got, "<svg") {
				t.Errorf("Render(%s) got invalid svg", content)
			}
		}
	})

	t.Run("ErrMermaidNotReady", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), renderTimeout)
		defer cancel()
		engine, err := NewRenderEngine(ctx, []string{"delete window.mermaid"})
		if !errors.Is(err, ErrMermaidNotReady) {
			t.Errorf("NewRenderEngine() expected ErrMermaidNotReady, got %v", err)
		}
		if engine != nil {
			t.Error("NewRenderEngine() expected nil engine on error, got non-nil")
		}
	})

	t.Run("ErrFailedEncoding", func(t *testing.T) {
		oldJSONMarshal := jsonMarshal
		defer func() { jsonMarshal = oldJSONMarshal }()
		jsonMarshal = func(v any) ([]byte, error) {
			return nil, errors.New("mock marshal error")
		}

		content := "graph TD; A-->B;"
		_, err := re1.Render(content)
		if !errors.Is(err, ErrFailedEncoding) {
			t.Errorf("Render() expected ErrFailedEncoding, got %v", err)
		}

		_, _, err = re1.RenderAsScaledPng(content, 1.0)
		if !errors.Is(err, ErrFailedEncoding) {
			t.Errorf("RenderAsScaledPng() expected ErrFailedEncoding, got %v", err)
		}

		_, _, err = re1.RenderAsPng(content)
		if !errors.Is(err, ErrFailedEncoding) {
			t.Errorf("RenderAsPng() expected ErrFailedEncoding, got %v", err)
		}
	})

	t.Run("AllocatorOptions", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), renderTimeout)
		defer cancel()
		engine, err := NewRenderEngine(ctx, nil, chromedp.NoSandbox)
		if err != nil {
			t.Fatalf("NewRenderEngine() with custom options error = %v", err)
		}
		defer engine.Cancel()

		svg, err := engine.Render("graph TD; A-->B;")
		if err != nil {
			t.Errorf("Render() error = %v", err)
		}
		if !strings.HasPrefix(svg, "<svg") {
			t.Errorf("Render() invalid svg")
		}
	})

	t.Run("MultipleCancel", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), renderTimeout)
		defer cancel()
		engine, err := NewRenderEngine(ctx, nil)
		if err != nil {
			t.Fatalf("NewRenderEngine() error = %v", err)
		}
		engine.Cancel()
		// Calling Cancel a second time should be safe and non-panicking
		engine.Cancel()
	})

	t.Run("DeadlineContext", func(t *testing.T) {
		deadline := time.Now().Add(renderTimeout)
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		engine, err := NewRenderEngine(ctx, nil)
		if err != nil {
			t.Fatalf("NewRenderEngine() with deadline error = %v", err)
		}
		defer engine.Cancel()

		svg, err := engine.Render("graph TD; A-->B;")
		if err != nil {
			t.Errorf("Render() error = %v", err)
		}
		if !strings.HasPrefix(svg, "<svg") {
			t.Errorf("Render() invalid svg")
		}
	})

	for _, tt := range cases {
		t.Run("", func(t *testing.T) {
			got, err := re1.Render(tt.content)
			t.Logf("got %s, error %s", got, err)
			if tt.wantException {
				// Classified rather than matched on the message, so the
				// assertion survives a reworded chrome exception.
				if !errors.Is(err, ErrRenderException) {
					t.Errorf("Render() error = %v, want ErrRenderException", err)
				}
				if _, _, err := re1.RenderAsPng(tt.content); !errors.Is(err, ErrRenderException) {
					t.Errorf("RenderAsPng() error = %v, want ErrRenderException", err)
				}
				return
			}
			if err != nil {
				t.Errorf("Render() error = %v", err)
			}
			if !strings.HasPrefix(got, "<svg") {
				t.Errorf("Render() got an invalid svg = %v, err = %s", got, err)
			}

			result_in_bytes, box, err := re1.RenderAsPng(tt.content)
			if err != nil {
				t.Fatalf("RenderAsPng() error = %v", err)
			}
			if box == nil {
				t.Errorf("RenderAsPng() returned an empty box")
			} else if box.Width < 1 || box.Height < 1 {
				t.Errorf("RenderAsPng() got empty image = w:%d, h:%d)", box.Width, box.Height)
			}
			content_type := http.DetectContentType(result_in_bytes)
			if content_type != "image/png" {
				t.Errorf("RenderAsPng() return an '%s' rather than 'image/png'", content_type)
			}
		})
	}
}

func BenchmarkRenderEngine_Render(b *testing.B) {
	case1 := `graph TD;
    A-->B;
    A-->C;
    B-->D;
    C-->D;`
	// Both errors used to be discarded, so a browser that would not start
	// panicked on the nil engine, and a failing render was timed as if it had
	// succeeded.
	re1, err := NewRenderEngine(context.Background(), nil)
	if err != nil {
		b.Fatalf("NewRenderEngine() error = %v", err)
	}
	defer re1.Cancel()

	for b.Loop() {
		if _, err := re1.Render(case1); err != nil {
			b.Fatalf("Render() error = %v", err)
		}
	}
}

func TestRenderEngine_RenderTimeout(t *testing.T) {
	// No deadline on the engine: each render carries its own.
	re, err := NewRenderEngine(context.Background(), nil)
	if err != nil {
		t.Fatalf("NewRenderEngine() error = %v", err)
	}
	defer re.Cancel()

	content := "graph TD; A-->B;"

	t.Run("PerCallTimeout", func(t *testing.T) {
		if _, err := re.Render(content, WithTimeout(time.Nanosecond)); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Render() expected context.DeadlineExceeded, got %v", err)
		}
		if _, _, err := re.RenderAsPng(content, WithTimeout(time.Nanosecond)); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("RenderAsPng() expected context.DeadlineExceeded, got %v", err)
		}
		if _, _, err := re.RenderAsScaledPng(content, 2.0, WithTimeout(time.Nanosecond)); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("RenderAsScaledPng() expected context.DeadlineExceeded, got %v", err)
		}
	})

	t.Run("EngineSurvivesTimeout", func(t *testing.T) {
		svg, err := re.Render(content)
		if err != nil {
			t.Fatalf("Render() after a timed out render error = %v", err)
		}
		if !strings.HasPrefix(svg, "<svg") {
			t.Errorf("Render() got an invalid svg = %v", svg)
		}
	})

	t.Run("EngineTimeout", func(t *testing.T) {
		re.SetRenderTimeout(time.Nanosecond)
		if _, err := re.Render(content); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Render() expected context.DeadlineExceeded, got %v", err)
		}
		// A per-call option still wins over the engine default.
		if _, err := re.Render(content, WithTimeout(renderTimeout)); err != nil {
			t.Errorf("Render() with a per-call timeout error = %v", err)
		}
		re.SetRenderTimeout(DefaultRenderTimeout)
	})

	t.Run("DisabledTimeout", func(t *testing.T) {
		re.SetRenderTimeout(0)
		defer re.SetRenderTimeout(DefaultRenderTimeout)
		if _, err := re.Render(content, WithTimeout(0)); err != nil {
			t.Errorf("Render() with the deadline disabled error = %v", err)
		}
	})
}

func TestRenderEngine_TargetCrashed(t *testing.T) {
	// The crash bookkeeping is driven purely by Inspector events, so it can be
	// exercised without a browser.
	re := &RenderEngine{}

	var reported []error
	re.SetTargetCrashedHandler(func(err error) { reported = append(reported, err) })

	if err := re.CrashError(); err != nil {
		t.Errorf("CrashError() on a healthy engine = %v, want nil", err)
	}

	// Chrome's reason when the renderer dies under the target.
	const renderProcessGone = "Render process gone."

	for _, reason := range []string{detachReasonTargetClosed, detachReasonCanceledByUser} {
		re.handleTargetEvent(&inspector.EventDetached{Reason: reason})
		if err := re.CrashError(); err != nil {
			t.Errorf("CrashError() after a %q detach = %v, want nil", reason, err)
		}
	}

	re.handleTargetEvent(&inspector.EventTargetCrashed{})
	err := re.CrashError()
	if !errors.Is(err, ErrTargetCrashed) {
		t.Fatalf("CrashError() after a crash = %v, want ErrTargetCrashed", err)
	}
	if len(reported) != 1 {
		t.Fatalf("handler called %d times, want 1", len(reported))
	}

	// A repeated event carries nothing new, so it must not be reported again.
	re.handleTargetEvent(&inspector.EventTargetCrashed{})
	if len(reported) != 1 {
		t.Errorf("handler called %d times for a duplicate crash, want 1", len(reported))
	}

	// The detach that follows a crash carries the reason chrome gives.
	re.handleTargetEvent(&inspector.EventDetached{Reason: renderProcessGone})
	err = re.CrashError()
	if !errors.Is(err, ErrTargetCrashed) {
		t.Fatalf("CrashError() after a crash detach = %v, want ErrTargetCrashed", err)
	}
	if !strings.Contains(err.Error(), renderProcessGone) {
		t.Errorf("CrashError() = %q, want it to mention %q", err, renderProcessGone)
	}
	if len(reported) != 2 {
		t.Fatalf("handler called %d times, want 2", len(reported))
	}
	if !errors.Is(reported[1], ErrTargetCrashed) || !strings.Contains(reported[1].Error(), renderProcessGone) {
		t.Errorf("handler got %v, want a crash error mentioning the detach reason", reported[1])
	}

	// Render errors are annotated so callers can tell a dead browser apart
	// from a slow diagram.
	annotated := re.annotateCrash(context.DeadlineExceeded)
	if !errors.Is(annotated, context.DeadlineExceeded) || !errors.Is(annotated, ErrTargetCrashed) {
		t.Errorf("annotateCrash() = %v, want both the original and the crash error", annotated)
	}
	if re.annotateCrash(nil) != nil {
		t.Error("annotateCrash(nil) expected nil")
	}

	// Chrome can bring the target back; the engine must not stay poisoned.
	re.handleTargetEvent(&inspector.EventTargetReloadedAfterCrash{})
	if err := re.CrashError(); err != nil {
		t.Errorf("CrashError() after a reload = %v, want nil", err)
	}
	if got := re.annotateCrash(context.DeadlineExceeded); !errors.Is(got, context.DeadlineExceeded) || errors.Is(got, ErrTargetCrashed) {
		t.Errorf("annotateCrash() after a reload = %v, want the original error only", got)
	}

	re.SetTargetCrashedHandler(nil)
	re.handleTargetEvent(&inspector.EventTargetCrashed{})
	if len(reported) != 2 {
		t.Errorf("handler called %d times after being removed, want 2", len(reported))
	}
}

// TestRenderEngine_TargetCrashedLive exercises the listener against a real
// browser. It is opt-in via MERMAID_GO_LIVE_CRASH_TEST because how a renderer
// crash is reported is not portable: Page.crash reliably kills the renderer
// everywhere (the command never replies, because the renderer it is addressed
// to is gone), but the GitHub runner delivers no Inspector.targetCrashed for it
// within 15s, while a desktop chrome does. TestRenderEngine_TargetCrashed
// covers the bookkeeping deterministically; this only adds proof that
// the event subscriptions are wired to real chrome events.
func TestRenderEngine_TargetCrashedLive(t *testing.T) {
	if os.Getenv("MERMAID_GO_LIVE_CRASH_TEST") == "" {
		t.Skip("set MERMAID_GO_LIVE_CRASH_TEST=1 to run the live crash test")
	}

	re, err := NewRenderEngine(context.Background(), nil)
	if err != nil {
		t.Fatalf("NewRenderEngine() error = %v", err)
	}
	defer re.Cancel()

	crashed := make(chan error, 4)
	re.SetTargetCrashedHandler(func(err error) {
		select {
		case crashed <- err:
		default:
		}
	})

	// Page.crash is the documented way to kill the renderer on purpose. It is
	// far more portable than navigating to chrome://crash, whose handling
	// varies between builds. The command itself never gets a reply, since the
	// renderer it is addressed to is gone, so the deadline here is the success
	// path rather than a failure.
	crashCtx, crashCancel := context.WithTimeout(re.ctx, 10*time.Second)
	defer crashCancel()
	if _, err := chromedp.Call(crashCtx, page.Crash, cdp.Empty{}); err != nil {
		t.Logf("Page.crash returned %v (expected)", err)
	}

	select {
	case err := <-crashed:
		if !errors.Is(err, ErrTargetCrashed) {
			t.Errorf("handler got %v, want ErrTargetCrashed", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("chrome did not report a crash within 15s")
	}

	if err := re.CrashError(); !errors.Is(err, ErrTargetCrashed) {
		t.Errorf("CrashError() = %v, want ErrTargetCrashed", err)
	}

	// A render against the dead target must fail promptly and say why.
	_, err = re.Render("graph TD; A-->B;", WithTimeout(10*time.Second))
	if err == nil {
		t.Fatal("Render() on a crashed target expected an error, got nil")
	}
	if !errors.Is(err, ErrTargetCrashed) {
		t.Errorf("Render() error = %v, want it to wrap ErrTargetCrashed", err)
	}
}

func TestRenderEngine_RenderContext(t *testing.T) {
	re, err := NewRenderEngine(context.Background(), nil)
	if err != nil {
		t.Fatalf("NewRenderEngine() error = %v", err)
	}
	defer re.Cancel()

	content := "graph TD; A-->B;"

	t.Run("Succeeds", func(t *testing.T) {
		svg, err := re.RenderContext(context.Background(), content)
		if err != nil {
			t.Fatalf("RenderContext() error = %v", err)
		}
		if !strings.HasPrefix(svg, "<svg") {
			t.Errorf("RenderContext() got an invalid svg = %v", svg)
		}
	})

	t.Run("AlreadyCancelledFailsFast", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := re.RenderContext(ctx, content); !errors.Is(err, context.Canceled) {
			t.Errorf("RenderContext() error = %v, want context.Canceled", err)
		}
		if _, _, err := re.RenderAsPngContext(ctx, content); !errors.Is(err, context.Canceled) {
			t.Errorf("RenderAsPngContext() error = %v, want context.Canceled", err)
		}
	})

	t.Run("CancelledWhileQueued", func(t *testing.T) {
		// Occupy the engine so the call has to queue. This is the case a
		// caller could not escape before: the render timeout only starts once
		// a render begins, so the wait for a turn was unbounded.
		re.sem <- struct{}{}
		defer func() { <-re.sem }()

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		start := time.Now()
		_, err := re.RenderContext(ctx, content)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("RenderContext() error = %v, want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("RenderContext() waited %v for a turn, want it to give up with the context", elapsed)
		}
	})

	t.Run("CancellationCauseIsReported", func(t *testing.T) {
		re.sem <- struct{}{}
		defer func() { <-re.sem }()

		reason := errors.New("caller went away")
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(reason)

		if _, err := re.RenderContext(ctx, content); !errors.Is(err, reason) {
			t.Errorf("RenderContext() error = %v, want it to report the cancellation cause", err)
		}
	})
}

func TestRenderEngine_renderContext(t *testing.T) {
	// The deadline arithmetic is worth checking directly: it decides whether a
	// caller sees a truthful DeadlineExceeded or a bare Canceled.
	re := &RenderEngine{sem: make(chan struct{}, 1), ctx: context.Background()}

	t.Run("CallerDeadlineWinsWhenSooner", func(t *testing.T) {
		re.SetRenderTimeout(time.Hour)
		want := time.Now().Add(2 * time.Second)
		caller, cancel := context.WithDeadline(context.Background(), want)
		defer cancel()

		ctx, done := re.renderContext(caller, &renderOptions{})
		defer done()

		got, ok := ctx.Deadline()
		if !ok {
			t.Fatal("renderContext() produced no deadline")
		}
		if got.Sub(want).Abs() > 50*time.Millisecond {
			t.Errorf("renderContext() deadline = %v, want the caller's %v", got, want)
		}
	})

	t.Run("EngineTimeoutWinsWhenSooner", func(t *testing.T) {
		re.SetRenderTimeout(time.Second)
		caller, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()

		ctx, done := re.renderContext(caller, &renderOptions{})
		defer done()

		got, ok := ctx.Deadline()
		if !ok {
			t.Fatal("renderContext() produced no deadline")
		}
		if until := time.Until(got); until > 5*time.Second {
			t.Errorf("renderContext() deadline is %v away, want the engine's 1s", until)
		}
	})

	t.Run("CallerCancellationPropagates", func(t *testing.T) {
		re.SetRenderTimeout(time.Hour)
		caller, cancel := context.WithCancel(context.Background())

		ctx, done := re.renderContext(caller, &renderOptions{})
		defer done()

		cancel()
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Error("renderContext() did not propagate the caller's cancellation")
		}
	})

	t.Run("NoDeadlineWhenBothDisabled", func(t *testing.T) {
		re.SetRenderTimeout(0)
		ctx, done := re.renderContext(context.Background(), &renderOptions{})
		defer done()
		if _, ok := ctx.Deadline(); ok {
			t.Error("renderContext() set a deadline when both the engine and the caller declined one")
		}
	})
}

func TestRenderEngine_CancelDoesNotWaitForRender(t *testing.T) {
	re, err := NewRenderEngine(context.Background(), nil)
	if err != nil {
		t.Fatalf("NewRenderEngine() error = %v", err)
	}

	// Stand in for a render in flight. Cancel used to take the same lock, so
	// shutdown queued behind the render it was meant to abort.
	re.sem <- struct{}{}
	defer func() { <-re.sem }()

	done := make(chan struct{})
	go func() {
		re.Cancel()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Cancel() blocked behind an in-flight render")
	}
}

func TestRenderEngine_ErrorClassification(t *testing.T) {
	re, err := NewRenderEngine(context.Background(), nil)
	if err != nil {
		t.Fatalf("NewRenderEngine() error = %v", err)
	}
	defer re.Cancel()

	t.Run("InvalidDiagramIsAnException", func(t *testing.T) {
		_, err := re.Render("graph TD; A---;")
		if !errors.Is(err, ErrRenderException) {
			t.Fatalf("Render() error = %v, want ErrRenderException", err)
		}
		// The chrome detail stays reachable for the script location and stack.
		var exception *chromedp.ExceptionError
		if !errors.As(err, &exception) || exception.ExceptionDetails == nil {
			t.Errorf("Render() error = %v, want the *chromedp.ExceptionError to remain reachable", err)
		}
		// An invalid diagram is not a browser failure; retrying it is pointless
		// whereas retrying a crash is not, so the two must not be conflated.
		if errors.Is(err, ErrTargetCrashed) {
			t.Errorf("Render() error = %v, should not report a crash", err)
		}
	})

	t.Run("NoPartialPngOnFailure", func(t *testing.T) {
		png, box, err := re.RenderAsPng("graph TD; A---;")
		if err == nil {
			t.Fatal("RenderAsPng() expected an error for invalid syntax")
		}
		if png != nil || box != nil {
			t.Errorf("RenderAsPng() returned png=%d bytes, box=%v on failure, want neither", len(png), box)
		}
	})

	t.Run("BundleIsRejectedForPng", func(t *testing.T) {
		// Previously accepted and silently ignored, because the PNG methods take
		// RenderOptions but only Render implements bundling.
		if _, _, err := re.RenderAsPng("graph TD; A-->B;", WithBundle()); !errors.Is(err, ErrUnsupportedOption) {
			t.Errorf("RenderAsPng(WithBundle()) error = %v, want ErrUnsupportedOption", err)
		}
		if _, _, err := re.RenderAsScaledPng("graph TD; A-->B;", 2.0, WithBundle()); !errors.Is(err, ErrUnsupportedOption) {
			t.Errorf("RenderAsScaledPng(WithBundle()) error = %v, want ErrUnsupportedOption", err)
		}
	})

	t.Run("EncodingErrorIsWrapped", func(t *testing.T) {
		oldJSONMarshal := jsonMarshal
		defer func() { jsonMarshal = oldJSONMarshal }()
		underlying := errors.New("mock marshal error")
		jsonMarshal = func(v any) ([]byte, error) { return nil, underlying }

		_, err := re.Render("graph TD; A-->B;")
		if !errors.Is(err, ErrFailedEncoding) {
			t.Errorf("Render() error = %v, want ErrFailedEncoding", err)
		}
		if !errors.Is(err, underlying) {
			t.Errorf("Render() error = %v, want the underlying marshal error preserved", err)
		}
	})
}

func TestRenderEngine_StartupDiagnostics(t *testing.T) {
	t.Run("MermaidNotReadyNamesTheValue", func(t *testing.T) {
		engine, err := NewRenderEngine(context.Background(), []string{"delete window.mermaid"})
		if !errors.Is(err, ErrMermaidNotReady) {
			t.Fatalf("NewRenderEngine() error = %v, want ErrMermaidNotReady", err)
		}
		if engine != nil {
			t.Error("NewRenderEngine() expected nil engine on error")
		}
		// Knowing what typeof mermaid actually was is the whole diagnostic.
		if !strings.Contains(err.Error(), `"undefined"`) {
			t.Errorf("NewRenderEngine() error = %q, want it to name the typeof result", err)
		}
	})

	t.Run("StartupDeadlineDoesNotKillChrome", func(t *testing.T) {
		// chromedp binds chrome's process to the context of the first Run, so
		// bounding startup with a derived deadline would kill the browser as
		// soon as NewRenderEngine returned. Renders afterwards prove it did not.
		re, err := NewRenderEngine(context.Background(), nil)
		if err != nil {
			t.Fatalf("NewRenderEngine() error = %v", err)
		}
		defer re.Cancel()

		for i := range 2 {
			svg, err := re.Render("graph TD; A-->B;")
			if err != nil {
				t.Fatalf("Render() %d after a bounded startup error = %v", i, err)
			}
			if !strings.HasPrefix(svg, "<svg") {
				t.Errorf("Render() %d got an invalid svg = %v", i, svg)
			}
		}
	})
}

func TestRenderEngine_ErrEngineClosed(t *testing.T) {
	re, err := NewRenderEngine(context.Background(), nil)
	if err != nil {
		t.Fatalf("NewRenderEngine() error = %v", err)
	}

	// A cancelled caller against a healthy engine is routine: it must not be
	// reported as the engine being spent.
	cancelledCtx, cancelCaller := context.WithCancel(context.Background())
	cancelCaller()
	if _, err := re.RenderContext(cancelledCtx, "graph TD; A-->B;"); errors.Is(err, ErrEngineClosed) {
		t.Errorf("RenderContext() with a cancelled caller = %v, should not report ErrEngineClosed", err)
	}

	re.Cancel()

	// Every entry point must agree once the engine is gone.
	if _, err := re.Render("graph TD; A-->B;"); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("Render() after Cancel() = %v, want ErrEngineClosed", err)
	}
	if _, err := re.RenderContext(context.Background(), "graph TD; A-->B;"); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("RenderContext() after Cancel() = %v, want ErrEngineClosed", err)
	}
	if _, _, err := re.RenderAsPng("graph TD; A-->B;"); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("RenderAsPng() after Cancel() = %v, want ErrEngineClosed", err)
	}

	// The underlying cause stays visible; only the interpretation is added.
	_, err = re.Render("graph TD; A-->B;")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Render() after Cancel() = %v, want the underlying context.Canceled preserved", err)
	}
	// Closing the engine on purpose is not a browser death.
	if errors.As(err, new(*exec.ExitError)) {
		t.Errorf("Render() after Cancel() = %v, should not carry an exit error", err)
	}
}

func TestRenderEngine_BrowserExitIsReported(t *testing.T) {
	re, err := NewRenderEngine(context.Background(), nil)
	if err != nil {
		t.Fatalf("NewRenderEngine() error = %v", err)
	}
	defer re.Cancel()

	process := chromedp.FromContext(re.ctx).Browser.Process()
	if process == nil {
		t.Skip("the allocator started no process")
	}
	if err := process.Kill(); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	select {
	case <-re.ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the engine context outlived the browser")
	}

	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrEngineClosed) {
			t.Errorf("%s after the browser died = %v, want ErrEngineClosed", name, err)
		}
		if !errors.As(err, new(*exec.ExitError)) {
			t.Errorf("%s after the browser died = %v, want the *exec.ExitError", name, err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s after the browser died = %v, want context.Canceled preserved", name, err)
		}
	}

	// With the slot free a render may still reach chromedp, which attaches the
	// exit error itself.
	_, err = re.Render("graph TD; A-->B;")
	check("Render()", err)

	// Holding the slot forces a render to fail in acquire, before chromedp; it
	// must still say that the browser died rather than that it was cancelled.
	re.sem <- struct{}{}
	defer func() { <-re.sem }()
	_, err = re.Render("graph TD; A-->B;")
	check("Render() waiting for its turn", err)
	_, _, err = re.RenderAsPng("graph TD; A-->B;")
	check("RenderAsPng() waiting for its turn", err)
}

func TestRenderEngine_ClosedEngineOutlivesParentContext(t *testing.T) {
	// Cancelling the context handed to NewRenderEngine kills the engine just as
	// Cancel does, and must be reported the same way.
	ctx, cancel := context.WithCancel(context.Background())
	re, err := NewRenderEngine(ctx, nil)
	if err != nil {
		t.Fatalf("NewRenderEngine() error = %v", err)
	}
	defer re.Cancel()

	cancel()
	if _, err := re.Render("graph TD; A-->B;"); !errors.Is(err, ErrEngineClosed) {
		t.Errorf("Render() after the parent context was cancelled = %v, want ErrEngineClosed", err)
	}
}

func TestReportCrash_ContainsPanic(t *testing.T) {
	// The handler runs on the engine's event goroutine, where a panic would take
	// the process down with no way for the consumer to recover it.
	re := &RenderEngine{}
	re.SetTargetCrashedHandler(func(error) { panic("handler blew up") })

	done := make(chan struct{})
	go func() {
		defer close(done)
		re.handleTargetEvent(&inspector.EventTargetCrashed{})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleTargetEvent did not return")
	}

	// The crash is still recorded even though the handler misbehaved.
	if err := re.CrashError(); !errors.Is(err, ErrTargetCrashed) {
		t.Errorf("CrashError() = %v, want the crash recorded despite the panic", err)
	}
}

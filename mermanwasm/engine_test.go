package mermanwasm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"

	mermaid_go "github.com/dreampuf/mermaid.go"
	"github.com/dreampuf/mermaid.go/internal/fixtures"
)

func TestConstructorInputs(t *testing.T) {
	decode := func(t *testing.T, cfg *engineConfig) map[string]any {
		t.Helper()
		config, _, err := constructorInputs(cfg)
		if err != nil {
			t.Fatalf("constructorInputs() error = %v", err)
		}
		envelope := map[string]any{}
		if err := json.Unmarshal(config, &envelope); err != nil {
			t.Fatalf("constructor envelope is not JSON: %v", err)
		}
		if envelope["version"] != 1.0 {
			t.Errorf("envelope version = %v, want 1", envelope["version"])
		}
		options, _ := envelope["options"].(map[string]any)
		if options["version"] != float64(optionsSchema) {
			t.Errorf("options version = %v, want %d", options["version"], optionsSchema)
		}
		return options
	}

	t.Run("SvgIDAndThemeOverSiteConfig", func(t *testing.T) {
		options := decode(t, &engineConfig{
			svgID:      "diagram",
			theme:      "dark",
			siteConfig: []byte(`{"theme":"forest","fontSize":12}`),
		})
		if got := options["svg"]; !mapHas(got, "diagram_id", "diagram") {
			t.Errorf("svg group = %v, want diagram_id diagram", got)
		}
		site := options["site_config"]
		if !mapHas(site, "theme", "dark") || !mapHas(site, "fontSize", 12.0) {
			t.Errorf("site_config = %v, want the theme option over the configured theme and the rest kept", site)
		}
	})

	t.Run("NothingConfigured", func(t *testing.T) {
		options := decode(t, &engineConfig{})
		if len(options) != 1 {
			t.Errorf("options = %v, want only the schema version", options)
		}
	})

	t.Run("RawOptionsReplaceGroups", func(t *testing.T) {
		options := decode(t, &engineConfig{
			svgID:   "diagram",
			options: []byte(`{"svg":{"diagram_id":"raw"},"resources":{"profile":"constrained"},"version":99}`),
		})
		if !mapHas(options["svg"], "diagram_id", "raw") || !mapHas(options["resources"], "profile", "constrained") {
			t.Errorf("options = %v, want the raw groups to win", options)
		}
	})

	t.Run("RawOptionsMergeIntoGroups", func(t *testing.T) {
		options := decode(t, &engineConfig{
			svgID:   "diagram",
			theme:   "dark",
			options: []byte(`{"svg":{},"site_config":{"fontSize":12,"theme":"forest"}}`),
		})
		if !mapHas(options["svg"], "diagram_id", "diagram") {
			t.Errorf("svg group = %v, want the svg id kept", options["svg"])
		}
		site := options["site_config"]
		if !mapHas(site, "theme", "dark") || !mapHas(site, "fontSize", 12.0) {
			t.Errorf("site_config = %v, want the raw keys merged and the theme option over them", site)
		}
	})

	t.Run("RawOptionsReplaceANonObjectGroup", func(t *testing.T) {
		options := decode(t, &engineConfig{svgID: "diagram", options: []byte(`{"svg":null}`)})
		if options["svg"] != nil {
			t.Errorf("svg group = %v, want the raw null", options["svg"])
		}
	})

	t.Run("FontsAreConcatenatedWithRanges", func(t *testing.T) {
		config, fonts, err := constructorInputs(&engineConfig{fonts: [][]byte{[]byte("abc"), []byte("de")}})
		if err != nil {
			t.Fatalf("constructorInputs() error = %v", err)
		}
		if string(fonts) != "abcde" {
			t.Errorf("font blob = %q, want abcde", fonts)
		}
		if !bytes.Contains(config, []byte(`"fonts":[{"offset":0,"length":3},{"offset":3,"length":2}]`)) {
			t.Errorf("constructor envelope = %s, want ranges into the blob", config)
		}
	})

	for name, cfg := range map[string]*engineConfig{
		"ThemeCSS":           {siteConfig: []byte(`{"themeCSS":".node rect{}"}`)},
		"ThemeCSSThroughRaw": {options: []byte(`{"site_config":{"themeCSS":".node rect{}"}}`)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := constructorInputs(cfg); !errors.Is(err, mermaid_go.ErrUnsupportedOption) {
				t.Errorf("constructorInputs() error = %v, want mermaid_go.ErrUnsupportedOption", err)
			}
		})
	}

	for name, cfg := range map[string]*engineConfig{
		"SiteConfigRawNotAnObject": {options: []byte(`{"site_config":"dark"}`)},
		"SiteConfigNotAnObject":    {siteConfig: []byte(`["dark"]`)},
		"SiteConfigNull":           {siteConfig: []byte(`null`)},
		"OptionsNotJSON":           {options: []byte(`{`)},
		"EmptyFont":                {fonts: [][]byte{{}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := constructorInputs(cfg); !errors.Is(err, mermaid_go.ErrMermanFailed) {
				t.Errorf("constructorInputs() error = %v, want mermaid_go.ErrMermanFailed", err)
			}
		})
	}
}

func mapHas(group any, key string, want any) bool {
	m, ok := group.(map[string]any)
	return ok && m[key] == want
}

func TestErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		payload string
		want    error
	}{
		{`{"code":5,"code_name":"MERMAN_PARSE_ERROR","kind":"generic","message":"bad"}`, mermaid_go.ErrRenderException},
		{`{"code":4,"code_name":"MERMAN_NO_DIAGRAM","kind":"generic","message":"none"}`, mermaid_go.ErrRenderException},
		{`{"code":10,"code_name":"MERMAN_RESOURCE_LIMIT_EXCEEDED","kind":"generic","message":"big"}`, mermaid_go.ErrRenderException},
		{`{"code":7,"code_name":"MERMAN_UNSUPPORTED_OPERATION","kind":"missing-capability","capability_id":"math","message":"no math"}`, mermaid_go.ErrMermanCapability},
		{`{"code":3,"code_name":"MERMAN_OPTIONS_JSON_ERROR","kind":"generic","message":"bogus"}`, mermaid_go.ErrMermanFailed},
		{`not json`, mermaid_go.ErrMermanFailed},
	} {
		err := classifyRender(decodeError([]byte(tc.payload)))
		if !errors.Is(err, tc.want) {
			t.Errorf("classifyRender(%s) = %v, want %v", tc.payload, err, tc.want)
		}
	}

	var owned *Error
	err := classifyRender(decodeError([]byte(`{"code":7,"code_name":"MERMAN_UNSUPPORTED_OPERATION","kind":"missing-capability","capability_id":"math","message":"no math","details":{"x":1}}`)))
	if !errors.As(err, &owned) || owned.CapabilityID != "math" || string(owned.Details) != `{"x":1}` {
		t.Errorf("classifyRender() = %#v, want the payload's fields reachable", owned)
	}
}

func TestModuleMatchesTheRelease(t *testing.T) {
	sum := sha256.Sum256(Module)
	if got := hex.EncodeToString(sum[:]); got != SHA256 {
		t.Errorf("sha256(merman_wasi.wasm) = %s, want %s", got, SHA256)
	}
}

func TestNew_EmbeddedModule(t *testing.T) {
	_, cache := liveModule()
	engine, err := New(context.Background(), WithCompilationCache(cache), WithStartupTimeout(10*time.Minute))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer engine.Cancel()
	if engine.Version() != Version {
		t.Errorf("Version() = %q, want the embedded release %q", engine.Version(), Version)
	}
}

func TestNew_MemoryLimitBelowOnePage(t *testing.T) {
	if _, err := New(context.Background(), WithMemoryLimit(1024)); !errors.Is(err, mermaid_go.ErrMermanFailed) {
		t.Errorf("New(WithMemoryLimit(1024)) error = %v, want ErrMermanFailed", err)
	}
}

func TestNew_Unavailable(t *testing.T) {
	for name, opts := range map[string][]Option{
		"EmptyModule":   {WithModule([]byte{})},
		"MissingFile":   {WithModuleFile(t.TempDir() + "/missing.wasm")},
		"NotWasm":       {WithModule([]byte("not a wasm module"))},
		"NotAReactor":   {WithModule([]byte("\x00asm\x01\x00\x00\x00"))},
		"LastModuleWin": {WithModuleFile("unused"), WithModule([]byte("\x00asm\x01\x00\x00\x00"))},
	} {
		t.Run(name, func(t *testing.T) {
			engine, err := New(context.Background(), opts...)
			if !errors.Is(err, mermaid_go.ErrMermanUnavailable) {
				t.Errorf("New() = %v, %v, want mermaid_go.ErrMermanUnavailable", engine, err)
			}
		})
	}
}

// liveModule is the reactor the live tests run: the one MERMAN_WASI_MODULE
// names, as in merman's own SDK examples, or else the embedded standard build. The module is compiled once and shared
// through a cache, since compiling it takes seconds.
var liveModule = sync.OnceValues(func() ([]byte, wazero.CompilationCache) {
	module := Module
	if path := os.Getenv("MERMAN_WASI_MODULE"); path != "" {
		var err error
		if module, err = os.ReadFile(path); err != nil {
			panic(fmt.Sprintf("MERMAN_WASI_MODULE: %v", err))
		}
	}
	return module, wazero.NewCompilationCache()
})

// liveOptions run the live module through the shared cache, with start-up
// bounded generously: under the race detector compiling it takes over a minute.
func liveOptions() []Option {
	module, cache := liveModule()
	return []Option{WithModule(module), WithCompilationCache(cache), WithStartupTimeout(10 * time.Minute)}
}

// liveEngine builds an engine on a real merman reactor.
func liveEngine(t *testing.T, opts ...Option) *Engine {
	t.Helper()
	opts = append(liveOptions(), opts...)
	engine, err := New(context.Background(), opts...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(engine.Cancel)
	return engine
}

func TestEngine_LiveRender(t *testing.T) {
	engine := liveEngine(t)
	t.Logf("merman %s, capabilities %v", engine.Version(), engine.Capabilities())

	for name, content := range fixtures.Diagrams {
		t.Run(name, func(t *testing.T) {
			svg, err := engine.Render(content)
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			if !strings.HasPrefix(svg, `<svg id="mermaid"`) || !strings.HasSuffix(strings.TrimSpace(svg), "</svg>") {
				t.Errorf("Render() = %.120q..., want a complete svg document with the default id", svg)
			}
		})
	}

	t.Run("InvalidSyntax", func(t *testing.T) {
		_, err := engine.Render("graph TD; A---;")
		if !errors.Is(err, mermaid_go.ErrRenderException) {
			t.Fatalf("Render() error = %v, want mermaid_go.ErrRenderException", err)
		}
		var owned *Error
		if !errors.As(err, &owned) || owned.CodeName != "MERMAN_PARSE_ERROR" || len(owned.Details) == 0 {
			t.Errorf("Render() error = %#v, want a MERMAN_PARSE_ERROR with its diagnostic", owned)
		}
		// An ordinary failure leaves the instance usable.
		if _, err := engine.Render("graph TD; A-->B;"); err != nil {
			t.Errorf("Render() after a parse error = %v", err)
		}
	})

	t.Run("Bundle", func(t *testing.T) {
		content := "graph TD; A-->B;"
		svg, err := engine.Render(content, mermaid_go.WithBundle())
		if err != nil {
			t.Fatalf("Render(mermaid_go.WithBundle()) error = %v", err)
		}
		if !strings.Contains(svg, "<desc>graph TD; A--&gt;B;</desc>") {
			t.Errorf("Render(mermaid_go.WithBundle()) = %.200q..., want the source in <desc>", svg)
		}
	})
}

func TestEngine_LiveOptions(t *testing.T) {
	content := "graph TD; A-->B;"

	t.Run("SvgID", func(t *testing.T) {
		svg, err := liveEngine(t, WithSvgID("custom")).Render(content)
		if err != nil {
			t.Fatalf("Render() error = %v", err)
		}
		if !strings.HasPrefix(svg, `<svg id="custom"`) {
			t.Errorf("Render() = %.80q..., want the configured id", svg)
		}
	})

	t.Run("Theme", func(t *testing.T) {
		light, err := liveEngine(t).Render(content)
		if err != nil {
			t.Fatalf("Render() error = %v", err)
		}
		dark, err := liveEngine(t, WithTheme("dark")).Render(content)
		if err != nil {
			t.Fatalf("Render() error = %v", err)
		}
		if light == dark {
			t.Error("Render() with the dark theme matches the default theme")
		}
		siteDark, err := liveEngine(t, WithSiteConfig([]byte(`{"theme":"dark"}`))).Render(content)
		if err != nil {
			t.Fatalf("Render() error = %v", err)
		}
		if siteDark != dark {
			t.Error("Render() with a dark site configuration differs from the dark theme option")
		}
		// The theme is part of the engine, so a request that brings options of
		// its own -- a PNG scale -- must not lose it.
		lightPng, _, err := liveEngine(t).RenderAsScaledPng(content, 2)
		if err != nil {
			t.Fatalf("RenderAsScaledPng() error = %v", err)
		}
		darkPng, _, err := liveEngine(t, WithTheme("dark")).RenderAsScaledPng(content, 2)
		if err != nil {
			t.Fatalf("RenderAsScaledPng() error = %v", err)
		}
		if bytes.Equal(lightPng, darkPng) {
			t.Error("RenderAsScaledPng() with the dark theme matches the default theme")
		}
	})

	t.Run("RejectedOptionsFailConstruction", func(t *testing.T) {
		_, err := New(context.Background(), append(liveOptions(), WithOptions([]byte(`{"bogus":1}`)))...)
		var owned *Error
		if !errors.Is(err, mermaid_go.ErrMermanFailed) || !errors.As(err, &owned) || owned.CodeName != "MERMAN_OPTIONS_JSON_ERROR" {
			t.Errorf("New() error = %v, want mermaid_go.ErrMermanFailed carrying MERMAN_OPTIONS_JSON_ERROR", err)
		}
	})

	t.Run("CallerFont", func(t *testing.T) {
		font := os.Getenv("MERMAN_WASI_FONT")
		if font == "" {
			t.Skip("MERMAN_WASI_FONT does not name a font")
		}
		data, err := os.ReadFile(font)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := liveEngine(t, WithFont(data)).Render(content); err != nil {
			t.Errorf("Render() with a caller font error = %v", err)
		}
	})
}

func TestEngine_LivePng(t *testing.T) {
	engine := liveEngine(t)
	if !slices.Contains(engine.Capabilities(), "png") {
		t.Skip("the module was built without png output")
	}
	content := "graph TD; A-->B;"

	unscaled, box, err := engine.RenderAsPng(content)
	if err != nil {
		t.Fatalf("RenderAsPng() error = %v", err)
	}
	config, err := png.DecodeConfig(bytes.NewReader(unscaled))
	if err != nil {
		t.Fatalf("RenderAsPng() returned an undecodable image: %v", err)
	}
	if int64(config.Width) != box.Width || int64(config.Height) != box.Height {
		t.Errorf("RenderAsPng() image is %dx%d but the box says %dx%d", config.Width, config.Height, box.Width, box.Height)
	}

	scaled, scaledBox, err := engine.RenderAsScaledPng(content, 2.0)
	if err != nil {
		t.Fatalf("RenderAsScaledPng(2.0) error = %v", err)
	}
	scaledConfig, err := png.DecodeConfig(bytes.NewReader(scaled))
	if err != nil {
		t.Fatalf("RenderAsScaledPng(2.0) returned an undecodable image: %v", err)
	}
	if scaledConfig.Width != 2*config.Width || scaledConfig.Height != 2*config.Height {
		t.Errorf("RenderAsScaledPng(2.0) image is %dx%d, want twice %dx%d",
			scaledConfig.Width, scaledConfig.Height, config.Width, config.Height)
	}
	if scaledBox.Width != box.Width || scaledBox.Height != box.Height {
		t.Errorf("RenderAsScaledPng(2.0) box = %dx%d, want the unscaled %dx%d",
			scaledBox.Width, scaledBox.Height, box.Width, box.Height)
	}

	for _, scale := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, _, err := engine.RenderAsScaledPng(content, scale); !errors.Is(err, mermaid_go.ErrUnsupportedOption) {
			t.Errorf("RenderAsScaledPng(%v) error = %v, want mermaid_go.ErrUnsupportedOption", scale, err)
		}
	}
	if _, _, err := engine.RenderAsPng(content, mermaid_go.WithBundle()); !errors.Is(err, mermaid_go.ErrUnsupportedOption) {
		t.Errorf("RenderAsPng(mermaid_go.WithBundle()) error = %v, want mermaid_go.ErrUnsupportedOption", err)
	}
	if data, box, err := engine.RenderAsPng("graph TD; A---;"); !errors.Is(err, mermaid_go.ErrRenderException) || data != nil || box != nil {
		t.Errorf("RenderAsPng(invalid) = %d bytes, %v, %v, want mermaid_go.ErrRenderException and nothing else", len(data), box, err)
	}
}

// slowMermanDiagram is a flowchart large enough that laying it out takes well
// over the deadlines the interruption tests set.
func slowMermanDiagram() string {
	var b strings.Builder
	b.WriteString("graph TD\n")
	for i := range 1500 {
		fmt.Fprintf(&b, "N%d-->N%d\nN%d-->N%d\n", i, i+1, i, (i*7+3)%1500)
	}
	return b.String()
}

func TestEngine_LiveLifecycle(t *testing.T) {
	content := "graph TD; A-->B;"

	t.Run("RendersRunConcurrently", func(t *testing.T) {
		engine := liveEngine(t, WithConcurrency(3))
		var wg sync.WaitGroup
		errs := make(chan error, 12)
		for i := range cap(errs) {
			wg.Go(func() {
				svg, err := engine.Render(fmt.Sprintf("graph TD; A%d-->B%d;", i, i))
				if err == nil && !strings.Contains(svg, fmt.Sprintf("A%d", i)) {
					err = fmt.Errorf("render %d returned another diagram", i)
				}
				errs <- err
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Error(err)
			}
		}
	})

	t.Run("AnInterruptedRenderDoesNotSpoilTheEngine", func(t *testing.T) {
		engine := liveEngine(t, WithConcurrency(1))
		start := time.Now()
		_, err := engine.Render(slowMermanDiagram(), mermaid_go.WithTimeout(300*time.Millisecond))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Render(mermaid_go.WithTimeout()) error = %v, want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("Render(mermaid_go.WithTimeout()) took %v, want the deadline to interrupt it", elapsed)
		}
		// The interrupted instance is gone; the next render builds another.
		if _, err := engine.Render(content); err != nil {
			t.Errorf("Render() after an interrupted render error = %v", err)
		}
	})

	t.Run("AReplacementOutlivesATightDeadline", func(t *testing.T) {
		engine := liveEngine(t, WithConcurrency(1))
		if _, err := engine.Render(slowMermanDiagram(), mermaid_go.WithTimeout(100*time.Millisecond)); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Render(WithTimeout()) error = %v, want context.DeadlineExceeded", err)
		}
		// The only instance is gone. A render whose deadline passes while its
		// replacement is built fails, but the replacement is kept for the next.
		if _, err := engine.Render(content, mermaid_go.WithTimeout(time.Nanosecond)); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Render(WithTimeout(1ns)) error = %v, want context.DeadlineExceeded", err)
		}
		if len(engine.idle) != 1 {
			t.Errorf("idle instances = %d after a render outrun by its build, want 1", len(engine.idle))
		}
	})

	t.Run("CallerCancellationReportsItsCause", func(t *testing.T) {
		engine := liveEngine(t)
		reason := errors.New("caller went away")
		ctx, cancel := context.WithCancelCause(context.Background())
		time.AfterFunc(200*time.Millisecond, func() { cancel(reason) })
		if _, err := engine.RenderContext(ctx, slowMermanDiagram()); !errors.Is(err, reason) {
			t.Errorf("RenderContext() error = %v, want it to report the cancellation cause", err)
		}
	})

	t.Run("CancelAbortsARenderInFlight", func(t *testing.T) {
		engine := liveEngine(t)
		done := make(chan error, 1)
		go func() {
			_, err := engine.Render(slowMermanDiagram())
			done <- err
		}()
		time.Sleep(200 * time.Millisecond)
		engine.Cancel()

		select {
		case err := <-done:
			if !errors.Is(err, mermaid_go.ErrEngineClosed) {
				t.Errorf("Render() error = %v, want mermaid_go.ErrEngineClosed", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Cancel() did not abort the render in flight")
		}
		if _, err := engine.Render(content); !errors.Is(err, mermaid_go.ErrEngineClosed) {
			t.Errorf("Render() after Cancel() error = %v, want mermaid_go.ErrEngineClosed", err)
		}
		if _, _, err := engine.RenderAsPng(content); !errors.Is(err, mermaid_go.ErrEngineClosed) {
			t.Errorf("RenderAsPng() after Cancel() error = %v, want mermaid_go.ErrEngineClosed", err)
		}
	})

	t.Run("WaitingForAnInstanceIsBoundedByTheCaller", func(t *testing.T) {
		engine := liveEngine(t, WithConcurrency(1))
		go func() { _, _ = engine.Render(slowMermanDiagram(), mermaid_go.WithTimeout(10*time.Second)) }()
		for len(engine.sem) == 0 {
			time.Sleep(time.Millisecond)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		if _, err := engine.RenderContext(ctx, content); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("RenderContext() error = %v, want context.DeadlineExceeded", err)
		}
	})
}

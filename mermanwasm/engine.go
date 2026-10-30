package mermanwasm

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"runtime"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	mermaid_go "github.com/dreampuf/mermaid.go"
	"github.com/dreampuf/mermaid.go/internal/render"
)

// ABIVersion is the merman-wasi ABI this package speaks. The reactor reports
// its own from merman_wasi_abi_version, and New refuses any other: the ABI has
// no compatibility aliases between versions.
const ABIVersion = 1

// DefaultMemoryLimit caps the linear memory of each reactor instance,
// matching the limit merman's own wazero example applies.
const DefaultMemoryLimit = 512 << 20

// maxIdleMemory is the most linear memory an instance may hold and still go
// back to the pool. Linear memory never shrinks, so an instance that grew for a
// large diagram is discarded rather than kept at its peak for the engine's
// lifetime; a fresh one costs tens of milliseconds.
const maxIdleMemory = 64 << 20

// maxInput is the ABI's ceiling on a single input or result segment.
const maxInput = 32 << 20

// descriptorSize is the size of a result descriptor: ten
// little-endian u32 fields.
const descriptorSize = 40

// Engine renders diagrams with merman's wasm32-wasip1 reactor, run in process
// by wazero, a pure Go WebAssembly runtime. It needs neither a browser nor a
// merman binary nor cgo. The module is the embedded [Module] unless another
// merman_wasi.wasm is supplied, and the engine compiles it once at
// construction. It satisfies [mermaid_go.Renderer].
//
// A reactor instance is strictly serial, so the engine keeps a pool of them,
// each with its own merman engine, and runs renders concurrently up to the
// limit set by [WithConcurrency]. Instances are created on demand and
// kept for reuse. One that traps, or is interrupted by a deadline or
// cancellation, is discarded rather than reused, as the ABI requires; the next
// render that needs an instance builds a fresh one.
//
// The output is merman's, as with [mermaid_go.MermanEngine], and an instance
// has no filesystem, network, clock or environment of its own: text is measured
// and painted only with the fonts the module bundles and those passed with
// [WithFont].
type Engine struct {
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
	// config and fonts are the constructor inputs every new instance's merman
	// engine is built from.
	config string
	fonts  string

	// sem bounds how many renders run at once; idle holds instances between
	// renders. idle has the same capacity as sem, and an instance is only ever
	// in use by a render holding a slot, so returning one never blocks.
	sem  chan struct{}
	idle chan *instance

	ctx    context.Context
	cancel context.CancelFunc
	// closed is closed once the runtime has been torn down.
	closed chan struct{}

	renderTimeout atomic.Int64
	// startupTimeout also bounds building an instance during a render, which
	// is not charged to that render's deadline.
	startupTimeout time.Duration

	version string
	// capabilities is sorted; operations is for lookups.
	capabilities []string
	operations   map[string]bool
}

var _ mermaid_go.Renderer = (*Engine)(nil)

type engineConfig struct {
	module         []byte
	moduleFile     string
	fonts          [][]byte
	svgID          string
	theme          string
	siteConfig     []byte
	options        []byte
	concurrency    int
	memoryLimit    uint64
	cache          wazero.CompilationCache
	startupTimeout time.Duration
}

// Option configures an Engine at construction.
type Option func(*engineConfig)

// WithModule supplies the merman_wasi.wasm reactor to run instead of the
// embedded standard build, such as the SDK's math or full build embedded in the
// program with go:embed. The engine keeps no reference to the slice once it has
// been compiled.
func WithModule(module []byte) Option {
	return func(c *engineConfig) { c.module, c.moduleFile = module, "" }
}

// WithModuleFile names a merman_wasi.wasm reactor to load from disk instead
// of the embedded standard build.
func WithModuleFile(path string) Option {
	return func(c *engineConfig) { c.module, c.moduleFile = nil, path }
}

// WithFont admits a TrueType or OpenType font for measuring and
// painting text, ahead of the fonts the module bundles, so a family it shares
// with a bundled font takes precedence. Pass it once per font. SVG output does
// not embed fonts: whatever displays the SVG needs matching fonts installed.
func WithFont(font []byte) Option {
	return func(c *engineConfig) { c.fonts = append(c.fonts, font) }
}

// WithSvgID overrides [mermaid_go.DefaultMermanSvgID] as the root <svg>
// element's id and internal marker prefix. An empty id leaves merman's default
// in place.
func WithSvgID(id string) Option {
	return func(c *engineConfig) { c.svgID = id }
}

// WithTheme selects a mermaid theme, the equivalent of passing
// mermaid.initialize({theme: ...}) as a statement to NewRenderEngine. It takes
// precedence over a theme in WithSiteConfig or WithOptions.
func WithTheme(theme string) Option {
	return func(c *engineConfig) { c.theme = theme }
}

// WithSiteConfig supplies a mermaid configuration object as JSON, the
// in-memory counterpart of [mermaid_go.WithMermanConfigFile]. New fails with
// [mermaid_go.ErrMermanFailed] if it is not a JSON object.
func WithSiteConfig(config []byte) Option {
	return func(c *engineConfig) { c.siteConfig = config }
}

// WithOptions merges a JSON object into the merman binding options the
// engine is constructed with, for the parts of the schema this package does not
// model -- resource profiles and limits, layout viewport sizes, presentation
// themes and the like. Each top-level group is merged key by key over the one
// built from the other options, so {"svg":{}} keeps the svg id; a group that is
// not an object replaces it. The engine speaks options schema 2, the schema of
// the merman 0.9 releases; see merman's docs/bindings/OPTIONS_JSON.md.
func WithOptions(options []byte) Option {
	return func(c *engineConfig) { c.options = options }
}

// WithConcurrency bounds how many renders run at once, and so how
// many reactor instances the engine keeps, defaulting to GOMAXPROCS. Each
// instance holds its own linear memory, up to WithMemoryLimit. A value
// <= 0 restores the default.
func WithConcurrency(n int) Option {
	return func(c *engineConfig) { c.concurrency = n }
}

// WithMemoryLimit caps each instance's linear memory, rounded down to
// whole 64 KiB pages, defaulting to DefaultMemoryLimit. A render that needs
// more traps, and its instance is discarded. A value of 0 restores the default.
// A limit below the module's own minimum memory fails New as the module is
// compiled, with an error naming both.
func WithMemoryLimit(bytes uint64) Option {
	return func(c *engineConfig) { c.memoryLimit = bytes }
}

// WithCompilationCache shares compiled code between engines, so only
// the first of them pays for compiling the module. A cache made with
// wazero.NewCompilationCacheWithDir also survives the process, which takes the
// several seconds compilation costs off every later start. The caller owns the
// cache and closes it once no engine uses it.
func WithCompilationCache(cache wazero.CompilationCache) Option {
	return func(c *engineConfig) { c.cache = cache }
}

// WithStartupTimeout bounds compiling the module and building the first
// instance when the supplied context has no deadline of its own, defaulting to
// [mermaid_go.DefaultStartupTimeout]. It also bounds building a replacement
// instance during a render.
func WithStartupTimeout(d time.Duration) Option {
	return func(c *engineConfig) { c.startupTimeout = d }
}

// catalogInfo is the part of the reactor's runtime catalog this package
// reads.
type catalogInfo struct {
	PackageVersion        string `json:"package_version"`
	OptionsSchemaVersions []int  `json:"options_schema_versions"`
	Capabilities          struct {
		CapabilityIDs []string `json:"capability_ids"`
		OperationIDs  []string `json:"operation_ids"`
	} `json:"capabilities"`
}

// optionsSchema is the binding options schema this package writes.
const optionsSchema = 2

// New compiles the merman reactor and builds one instance from it, so a
// missing or incompatible module is reported here, with
// [mermaid_go.ErrMermanUnavailable], as is a misconfiguration -- a font merman
// rejects, options it does not accept -- with [mermaid_go.ErrMermanFailed],
// rather than on the first render. The instance is kept for that render.
//
// ctx governs the engine's whole lifetime, as it does for
// [mermaid_go.NewRenderEngine]: when it is done the engine closes, every later
// render fails with [mermaid_go.ErrEngineClosed] and the runtime's resources
// are released. For a long-lived engine prefer
// context.Background() and let each render carry its own deadline; to bound
// only start-up, pass WithStartupTimeout.
func New(ctx context.Context, opts ...Option) (_ *Engine, err error) {
	cfg := &engineConfig{svgID: mermaid_go.DefaultMermanSvgID, startupTimeout: mermaid_go.DefaultStartupTimeout}
	for _, opt := range opts {
		opt(cfg)
	}
	if cfg.concurrency <= 0 {
		cfg.concurrency = runtime.GOMAXPROCS(0)
	}
	if cfg.memoryLimit == 0 {
		cfg.memoryLimit = DefaultMemoryLimit
	}
	if cfg.memoryLimit < 65536 {
		return nil, fmt.Errorf("%w: memory limit %d is below one 64 KiB page", mermaid_go.ErrMermanFailed, cfg.memoryLimit)
	}

	// The module options clear each other, so at most one of them is set.
	var module []byte
	switch {
	case cfg.moduleFile != "":
		if module, err = os.ReadFile(cfg.moduleFile); err != nil {
			return nil, fmt.Errorf("%w: %w", mermaid_go.ErrMermanUnavailable, err)
		}
	case cfg.module != nil:
		module = cfg.module
	default:
		module = Module
	}
	if len(module) == 0 {
		return nil, fmt.Errorf("%w: empty module", mermaid_go.ErrMermanUnavailable)
	}

	config, fonts, err := constructorInputs(cfg)
	if err != nil {
		return nil, err
	}

	startCtx, cancel := render.StartupContext(ctx, cfg.startupTimeout)
	defer cancel()

	pages := uint32(min(cfg.memoryLimit/65536, 65536))
	rtConfig := wazero.NewRuntimeConfig().
		WithMemoryLimitPages(pages).
		// Without this a deadline or cancellation cannot interrupt a render
		// that is running guest code, which is where nearly all its time goes.
		WithCloseOnContextDone(true)
	if cfg.cache != nil {
		rtConfig = rtConfig.WithCompilationCache(cfg.cache)
	}

	e := &Engine{
		runtime: wazero.NewRuntimeWithConfig(ctx, rtConfig),
		config:  string(config),
		fonts:   string(fonts),
		sem:     make(chan struct{}, cfg.concurrency),
		idle:    make(chan *instance, cfg.concurrency),
		closed:  make(chan struct{}),

		startupTimeout: cfg.startupTimeout,
	}
	e.ctx, e.cancel = context.WithCancel(ctx)
	e.renderTimeout.Store(int64(mermaid_go.DefaultRenderTimeout))
	// Tear the runtime down when the engine closes, however that happens, so a
	// cancelled parent context releases the compiled code too.
	context.AfterFunc(e.ctx, func() {
		_ = e.runtime.Close(context.Background())
		close(e.closed)
	})
	defer func() {
		if err != nil {
			e.Cancel()
			<-e.closed
		}
	}()

	if _, err = wasi_snapshot_preview1.Instantiate(startCtx, e.runtime); err != nil {
		return nil, e.startErr(startCtx, "instantiate wasi", err)
	}
	if e.compiled, err = e.runtime.CompileModule(startCtx, module); err != nil {
		return nil, e.startErr(startCtx, "compile module", err)
	}

	// Read the catalog before constructing a merman engine, so a module that
	// does not take this package's options is reported as incompatible rather
	// than as the options it rejects.
	inst, err := e.instantiate(startCtx)
	if err != nil {
		return nil, e.startErr(startCtx, "instantiate module", err)
	}
	catalog, err := inst.catalog(startCtx)
	if err == nil && !slices.Contains(catalog.OptionsSchemaVersions, optionsSchema) {
		err = fmt.Errorf("%w: module accepts options schemas %v, this package writes schema %d",
			mermaid_go.ErrMermanUnavailable, catalog.OptionsSchemaVersions, optionsSchema)
	}
	if err == nil {
		err = inst.construct(startCtx, e.config, e.fonts)
	}
	if err != nil {
		inst.close()
		if startCtx.Err() != nil {
			return nil, e.startErr(startCtx, "build instance", err)
		}
		return nil, err
	}
	e.idle <- inst

	e.version = catalog.PackageVersion
	e.capabilities = slices.Sorted(slices.Values(catalog.Capabilities.CapabilityIDs))
	e.operations = make(map[string]bool, len(catalog.Capabilities.OperationIDs))
	for _, id := range catalog.Capabilities.OperationIDs {
		e.operations[id] = true
	}
	if !e.operations["svg"] {
		return nil, fmt.Errorf("%w: module cannot render SVG", mermaid_go.ErrMermanCapability)
	}
	return e, nil
}

// startErr reports a failure building the engine as ErrMermanUnavailable,
// preferring the context's error when the start-up deadline is what ended it.
func (m *Engine) startErr(ctx context.Context, step string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	if errors.Is(err, mermaid_go.ErrMermanUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %s: %w", mermaid_go.ErrMermanUnavailable, step, err)
}

// constructorInputs builds the configuration envelope every instance's
// merman engine is constructed from, and the font blob its ranges point into.
func constructorInputs(cfg *engineConfig) (config, fonts []byte, err error) {
	// Marshalling string maps and raw JSON that has just been decoded cannot
	// fail, so those errors are not checked.
	options := map[string]json.RawMessage{}
	if cfg.svgID != "" {
		options["svg"], _ = json.Marshal(map[string]string{"diagram_id": cfg.svgID})
	}
	if len(cfg.siteConfig) > 0 {
		options["site_config"] = cfg.siteConfig
	}
	if len(cfg.options) > 0 {
		var extra map[string]json.RawMessage
		if err := json.Unmarshal(cfg.options, &extra); err != nil || extra == nil {
			return nil, nil, fmt.Errorf("%w: binding options are not a JSON object", mermaid_go.ErrMermanFailed)
		}
		for name, value := range extra {
			options[name] = mergeGroup(options[name], value)
		}
	}
	if site, err := siteConfig(options["site_config"], cfg.theme); err != nil {
		return nil, nil, err
	} else if site != nil {
		options["site_config"] = site
	}
	options["version"] = json.RawMessage(fmt.Appendf(nil, "%d", optionsSchema))

	type fontRange struct {
		Offset uint32 `json:"offset"`
		Length uint32 `json:"length"`
	}
	ranges := make([]fontRange, 0, len(cfg.fonts))
	total := 0
	for i, font := range cfg.fonts {
		if len(font) == 0 {
			return nil, nil, fmt.Errorf("%w: font %d is empty", mermaid_go.ErrMermanFailed, i)
		}
		if len(font) > maxInput-total {
			return nil, nil, fmt.Errorf("%w: fonts exceed the %d MiB the module accepts", mermaid_go.ErrMermanFailed, maxInput>>20)
		}
		ranges = append(ranges, fontRange{uint32(total), uint32(len(font))})
		total += len(font)
	}
	fonts = make([]byte, 0, total)
	for _, font := range cfg.fonts {
		fonts = append(fonts, font...)
	}

	config, err = json.Marshal(struct {
		Version uint32                     `json:"version"`
		Options map[string]json.RawMessage `json:"options"`
		Fonts   []fontRange                `json:"fonts"`
	}{1, options, ranges})
	if err != nil {
		return nil, nil, fmt.Errorf("%w: constructor options: %w", mermaid_go.ErrMermanFailed, err)
	}
	return config, fonts, nil
}

// mergeGroup merges an options group over another key by key when both are
// JSON objects, and otherwise lets over replace base.
func mergeGroup(base, over json.RawMessage) json.RawMessage {
	var baseKeys, overKeys map[string]json.RawMessage
	if json.Unmarshal(base, &baseKeys) != nil || baseKeys == nil ||
		json.Unmarshal(over, &overKeys) != nil || overKeys == nil {
		return over
	}
	maps.Copy(baseKeys, overKeys)
	merged, _ := json.Marshal(baseKeys)
	return merged
}

// siteConfig applies the theme to the site configuration and checks it,
// returning nil when there is neither.
//
// It rejects what the module accepts at construction only to fail every render
// with. The reactor always measures text with real fonts, and that measurement
// cannot honour CSS applied after layout, so a themeCSS turns each render into
// an opaque internal error. Unlike the CLI, the transport offers no
// deterministic measurement to fall back on.
func siteConfig(raw json.RawMessage, theme string) (json.RawMessage, error) {
	site := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &site); err != nil || site == nil {
			return nil, fmt.Errorf("%w: site configuration is not a JSON object", mermaid_go.ErrMermanFailed)
		}
	}
	if _, ok := site["themeCSS"]; ok {
		return nil, fmt.Errorf("%w: themeCSS is not supported by merman's font measurement; style through themeVariables instead", mermaid_go.ErrUnsupportedOption)
	}
	if theme != "" {
		site["theme"], _ = json.Marshal(theme)
	}
	if len(site) == 0 {
		return nil, nil
	}
	merged, _ := json.Marshal(site)
	return merged, nil
}

// Version reports the merman release the module was built from.
func (m *Engine) Version() string { return m.version }

// Capabilities reports the merman features the module was compiled with,
// sorted, using merman's own ids: "svg", "png", "math", "layout-elk" and so on.
// They are what distinguishes the SDK's standard, math and full modules.
func (m *Engine) Capabilities() []string { return slices.Clone(m.capabilities) }

// SetRenderTimeout overrides [mermaid_go.DefaultRenderTimeout] for subsequent
// renders. A value <= 0 disables the per-render deadline, leaving renders
// bounded only by the engine context and the caller's own.
func (m *Engine) SetRenderTimeout(d time.Duration) {
	m.renderTimeout.Store(int64(d))
}

// Cancel closes the engine: renders in flight are interrupted, every later
// render fails with [mermaid_go.ErrEngineClosed], and the runtime and its
// compiled code are released. Calling it more than once is harmless.
func (m *Engine) Cancel() { m.cancel() }

// Render renders content to SVG. It is equivalent to RenderContext with a
// background context.
func (m *Engine) Render(content string, opts ...mermaid_go.RenderOption) (string, error) {
	return m.RenderContext(context.Background(), content, opts...)
}

// RenderContext renders content to SVG, giving up if ctx is cancelled. The
// context covers the wait for a free instance as well as the render itself,
// and its deadline applies if it is sooner than the engine's render timeout.
// Only cancellation and the deadline are taken from ctx; its values are not.
func (m *Engine) RenderContext(ctx context.Context, content string, opts ...mermaid_go.RenderOption) (string, error) {
	renderOpts := render.NewOptions(opts)
	out, err := m.run(ctx, renderOpts, "svg", nil, content)
	if err != nil {
		return "", err
	}
	return render.MermanSVG(out, content, renderOpts)
}

// RenderAsPng renders content to a PNG. It is equivalent to RenderAsPngContext
// with a background context.
func (m *Engine) RenderAsPng(content string, opts ...mermaid_go.RenderOption) ([]byte, *mermaid_go.BoxModel, error) {
	return m.RenderAsScaledPngContext(context.Background(), content, 1.0, opts...)
}

// RenderAsPngContext renders content to a PNG, giving up if ctx is cancelled.
func (m *Engine) RenderAsPngContext(ctx context.Context, content string, opts ...mermaid_go.RenderOption) ([]byte, *mermaid_go.BoxModel, error) {
	return m.RenderAsScaledPngContext(ctx, content, 1.0, opts...)
}

// RenderAsScaledPng renders content to a PNG at the given scale. It is
// equivalent to RenderAsScaledPngContext with a background context.
func (m *Engine) RenderAsScaledPng(content string, scale float64, opts ...mermaid_go.RenderOption) ([]byte, *mermaid_go.BoxModel, error) {
	return m.RenderAsScaledPngContext(context.Background(), content, scale, opts...)
}

// RenderAsScaledPngContext renders content to a PNG at the given scale, giving
// up if ctx is cancelled. The box model is derived from the image as it is for
// [mermaid_go.MermanEngine.RenderAsScaledPngContext].
func (m *Engine) RenderAsScaledPngContext(ctx context.Context, content string, scale float64, opts ...mermaid_go.RenderOption) ([]byte, *mermaid_go.BoxModel, error) {
	renderOpts, err := render.MermanPngOptions(opts, scale)
	if err != nil {
		return nil, nil, err
	}
	if !m.operations["png"] {
		return nil, nil, fmt.Errorf("%w: module was built without png output", mermaid_go.ErrMermanCapability)
	}

	var options json.RawMessage
	if scale != 1 {
		// scale is finite and positive, so it formats as a JSON number.
		options = fmt.Appendf(nil, `{"version":%d,"raster":{"scale":%s}}`,
			optionsSchema, strconv.FormatFloat(scale, 'g', -1, 64))
	}
	out, err := m.run(ctx, renderOpts, "png", options, content)
	if err != nil {
		return nil, nil, err
	}
	return render.MermanPng(out, scale, "module")
}

// run executes one operation on a pooled instance and returns its output.
func (m *Engine) run(caller context.Context, opts *render.Options, operation string, options []byte, content string) ([]byte, error) {
	if len(content) > maxInput {
		return nil, fmt.Errorf("%w: diagram source exceeds the %d MiB the module accepts", mermaid_go.ErrRenderException, maxInput>>20)
	}
	release, err := render.AcquireSlot(caller, m.ctx, m.sem)
	if err != nil {
		return nil, err
	}
	defer release()

	runCtx, cancel := render.DeriveContext(m.ctx, caller, opts.TimeoutOr(&m.renderTimeout))
	defer cancel()

	var inst *instance
	select {
	case inst = <-m.idle:
	default:
		// Build the replacement under the engine's lifetime rather than this
		// render's deadline: charged to a tight deadline it could be
		// interrupted every time, and the pool would never refill.
		buildCtx, cancelBuild := render.StartupContext(m.ctx, m.startupTimeout)
		inst, err = m.newInstance(buildCtx)
		cancelBuild()
		if err != nil {
			return nil, m.runErr(caller, runCtx, err)
		}
	}
	// A render whose context ended while it waited for or built an instance
	// leaves the instance untouched for the next.
	if err := runCtx.Err(); err != nil {
		m.idle <- inst
		return nil, m.runErr(caller, runCtx, err)
	}

	out, err := inst.execute(runCtx, operation, options, content)
	m.recycle(inst, runCtx)
	if err != nil {
		return nil, m.runErr(caller, runCtx, err)
	}
	return out, nil
}

// recycle returns an instance to the pool if it is worth keeping, and closes it
// otherwise. It is worth keeping when it finished the call cleanly or with an
// ordinary failure: one that trapped or broke the ABI's contract is marked
// broken and in a state the ABI makes no promises about. One whose render's
// context ended may have been closed by the runtime's watcher even after its
// last call returned, and one that grew past maxIdleMemory would hold that
// memory for good.
func (m *Engine) recycle(inst *instance, runCtx context.Context) {
	if !inst.broken && runCtx.Err() == nil && m.ctx.Err() == nil &&
		!inst.mod.IsClosed() && inst.mod.Memory().Size() <= maxIdleMemory {
		m.idle <- inst
		return
	}
	inst.close()
}

// runErr classifies a failed render, consulting the context first: an
// interrupted call reports only that its module was closed, and the context
// knows whether the deadline passed, the caller gave up or the engine closed.
func (m *Engine) runErr(caller, runCtx context.Context, err error) error {
	var owned *Error
	if !errors.As(err, &owned) {
		if ctxErr := runCtx.Err(); ctxErr != nil {
			return render.ClassifyErr(caller, m.ctx, ctxErr)
		}
	}
	return render.ClassifyErr(caller, m.ctx, err)
}

// Error is an ordinary failure the merman reactor reported, an error it owns
// rather than a trap: an invalid diagram, options it rejected, a capability the
// module lacks, a resource limit. It is wrapped by the sentinel that classifies
// it -- [mermaid_go.ErrRenderException], [mermaid_go.ErrMermanCapability] or
// [mermaid_go.ErrMermanFailed] -- and stays reachable with errors.As.
type Error struct {
	// Code and CodeName identify the failure, for instance 5 and
	// "MERMAN_PARSE_ERROR".
	Code     int32  `json:"code"`
	CodeName string `json:"code_name"`
	// Kind is "generic" unless the failure is of a more specific kind, such as
	// "missing-capability".
	Kind string `json:"kind"`
	// CapabilityID names the missing capability when Kind says one is missing.
	CapabilityID string `json:"capability_id"`
	Message      string `json:"message"`
	// Details carries the structured detail merman attached, such as a parse
	// diagnostic's source span, as raw JSON.
	Details json.RawMessage `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.CodeName + ": " + e.Message }

// decodeError decodes an owned failure's payload.
func decodeError(payload []byte) error {
	owned := &Error{}
	if err := json.Unmarshal(payload, owned); err != nil {
		return fmt.Errorf("%w: unreadable error payload: %w", mermaid_go.ErrMermanFailed, err)
	}
	return owned
}

// classifyRender wraps an owned failure of a render in its sentinel. The
// statuses attributable to the diagram -- it is not UTF-8, names no diagram,
// does not parse or render, or exceeds a resource limit -- become the same
// non-retryable [mermaid_go.ErrRenderException] an invalid diagram raises under
// the other backends. Other errors pass through.
func classifyRender(err error) error {
	var owned *Error
	if !errors.As(err, &owned) {
		return err
	}
	sentinel := mermaid_go.ErrMermanFailed
	switch {
	case owned.Kind == "missing-capability":
		sentinel = mermaid_go.ErrMermanCapability
	case owned.CodeName == "MERMAN_UTF8_ERROR", owned.CodeName == "MERMAN_NO_DIAGRAM",
		owned.CodeName == "MERMAN_PARSE_ERROR", owned.CodeName == "MERMAN_RENDER_ERROR",
		owned.CodeName == "MERMAN_RESOURCE_LIMIT_EXCEEDED":
		sentinel = mermaid_go.ErrRenderException
	}
	return fmt.Errorf("%w: %w", sentinel, err)
}

// instance is one reactor instance and the merman engine built in it.
// It is used by one render at a time.
type instance struct {
	mod    api.Module
	engine uint32
	// broken records that the instance must not be reused: it trapped, was
	// interrupted, or answered outside the ABI's contract.
	broken bool

	alloc, free, engineNew, executeFn, resultInfo, resultFree, engineClose, runtimeCatalog api.Function
}

// newInstance instantiates the reactor and constructs its merman engine.
func (m *Engine) newInstance(ctx context.Context) (*instance, error) {
	inst, err := m.instantiate(ctx)
	if err != nil {
		return nil, err
	}
	if err := inst.construct(ctx, m.config, m.fonts); err != nil {
		inst.close()
		return nil, err
	}
	return inst, nil
}

// instantiate instantiates and initialises the reactor and checks it speaks
// this package's ABI, without constructing a merman engine in it yet.
func (m *Engine) instantiate(ctx context.Context) (inst *instance, err error) {
	// An empty name lets any number of instances of the module coexist in the
	// runtime; no start functions, because a reactor is initialised explicitly.
	mod, err := m.runtime.InstantiateModule(ctx, m.compiled, wazero.NewModuleConfig().WithName("").WithStartFunctions())
	if err != nil {
		return nil, fmt.Errorf("%w: instantiate module: %w", mermaid_go.ErrMermanUnavailable, err)
	}
	inst = &instance{mod: mod}
	defer func(i *instance) {
		if err != nil {
			i.close()
		}
	}(inst)

	if mod.Memory() == nil {
		return nil, fmt.Errorf("%w: module exports no memory", mermaid_go.ErrMermanUnavailable)
	}
	exports := map[string]*api.Function{
		"merman_wasi_alloc":           &inst.alloc,
		"merman_wasi_free":            &inst.free,
		"merman_wasi_execute":         &inst.executeFn,
		"merman_wasi_result_info":     &inst.resultInfo,
		"merman_wasi_result_free":     &inst.resultFree,
		"merman_wasi_engine_close":    &inst.engineClose,
		"merman_wasi_runtime_catalog": &inst.runtimeCatalog,
	}
	for name, fn := range exports {
		if *fn = mod.ExportedFunction(name); *fn == nil {
			return nil, fmt.Errorf("%w: module does not export %s; is it merman_wasi.wasm?", mermaid_go.ErrMermanUnavailable, name)
		}
	}
	initialize := mod.ExportedFunction("_initialize")
	abiVersion := mod.ExportedFunction("merman_wasi_abi_version")
	inst.engineNew = mod.ExportedFunction("merman_wasi_engine_new")
	if initialize == nil || abiVersion == nil || inst.engineNew == nil {
		return nil, fmt.Errorf("%w: module is not a merman-wasi reactor", mermaid_go.ErrMermanUnavailable)
	}

	if _, err := inst.call(ctx, initialize); err != nil {
		return nil, err
	}
	version, err := inst.call(ctx, abiVersion)
	if err != nil {
		return nil, err
	}
	if version != ABIVersion {
		return nil, fmt.Errorf("%w: module speaks merman-wasi ABI %d, this package speaks %d",
			mermaid_go.ErrMermanUnavailable, version, ABIVersion)
	}
	return inst, nil
}

// construct builds the instance's merman engine from the constructor inputs.
func (i *instance) construct(ctx context.Context, config, fonts string) error {
	result, err := i.invoke(ctx, i.engineNew, nil, config, fonts)
	if err != nil {
		var owned *Error
		if errors.As(err, &owned) {
			// A constructor failure is the configuration's fault, never the
			// diagram's, whatever status the payload carries.
			return fmt.Errorf("%w: constructing the merman engine: %w", mermaid_go.ErrMermanFailed, owned)
		}
		return err
	}
	if result.engine == 0 {
		i.broken = true
		return fmt.Errorf("%w: constructor returned no engine", mermaid_go.ErrMermanFailed)
	}
	i.engine = result.engine
	return nil
}

// close releases the instance. A broken instance gets no further guest calls:
// closing the module reclaims everything it held.
func (i *instance) close() {
	if !i.broken && i.engine != 0 {
		_, _ = i.call(context.Background(), i.engineClose, uint64(i.engine))
	}
	_ = i.mod.Close(context.Background())
}

// call invokes a guest function returning at most one scalar. Any error from
// the runtime -- a trap, an interruption, a closed module -- leaves the
// instance unusable.
func (i *instance) call(ctx context.Context, fn api.Function, args ...uint64) (uint32, error) {
	values, err := fn.Call(ctx, args...)
	if err != nil {
		i.broken = true
		return 0, fmt.Errorf("%w: %s: %w", mermaid_go.ErrMermanFailed, fn.Definition().ExportNames()[0], err)
	}
	if len(values) == 0 {
		return 0, nil
	}
	return uint32(values[0]), nil
}

// ownedResult is a host-side copy of an owned result.
type ownedResult struct {
	engine uint32
	data   []byte
}

// catalog reads the instance's runtime catalog.
func (i *instance) catalog(ctx context.Context) (*catalogInfo, error) {
	handle, err := i.call(ctx, i.runtimeCatalog)
	if err != nil {
		return nil, err
	}
	result, err := i.result(ctx, handle)
	if err != nil {
		return nil, fmt.Errorf("%w: runtime catalog: %w", mermaid_go.ErrMermanUnavailable, err)
	}
	catalog := &catalogInfo{}
	if err := json.Unmarshal(result.data, catalog); err != nil {
		return nil, fmt.Errorf("%w: unreadable runtime catalog: %w", mermaid_go.ErrMermanUnavailable, err)
	}
	return catalog, nil
}

// execute runs one operation on the instance's merman engine and returns its
// raw output, with an ordinary failure classified for the caller.
func (i *instance) execute(ctx context.Context, operation string, options json.RawMessage, source string) ([]byte, error) {
	request, err := json.Marshal(struct {
		Version   uint32          `json:"version"`
		Operation string          `json:"operation_id"`
		Options   json.RawMessage `json:"options,omitempty"`
	}{1, operation, options})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", mermaid_go.ErrFailedEncoding, err)
	}
	result, err := i.invoke(ctx, i.executeFn, []uint64{uint64(i.engine)}, string(request), source)
	if err != nil {
		return nil, classifyRender(err)
	}
	return result.data, nil
}

// invoke copies each input into a fresh guest allocation, calls fn with args
// followed by each input's offset and length, frees the inputs again and
// returns the owned result fn published. An empty input is passed as the
// canonical empty range, offset 0. Inputs are strings so the diagram source is
// copied once, straight into the guest.
func (i *instance) invoke(ctx context.Context, fn api.Function, args []uint64, inputs ...string) (_ *ownedResult, err error) {
	ptrs := make([]uint32, 0, len(inputs))
	defer func() {
		for _, p := range ptrs {
			if p != 0 && !i.broken {
				err = errors.Join(err, i.release(i.free, p))
			}
		}
	}()
	for _, input := range inputs {
		var ptr uint32
		if len(input) > 0 {
			if ptr, err = i.call(ctx, i.alloc, uint64(len(input))); err != nil {
				return nil, err
			}
			if ptr == 0 {
				i.broken = true
				return nil, fmt.Errorf("%w: guest could not allocate %d bytes", mermaid_go.ErrMermanFailed, len(input))
			}
			ptrs = append(ptrs, ptr)
			if !i.mod.Memory().WriteString(ptr, input) {
				i.broken = true
				return nil, fmt.Errorf("%w: guest allocation out of bounds", mermaid_go.ErrMermanFailed)
			}
		}
		args = append(args, uint64(ptr), uint64(len(input)))
	}
	handle, err := i.call(ctx, fn, args...)
	if err != nil {
		return nil, err
	}
	return i.result(ctx, handle)
}

// release hands a guest allocation or result back through fn. It runs with a
// live context, since the render's may have expired between guest calls and the
// instance is worth keeping tidy; a nonzero status breaks the ABI's contract.
func (i *instance) release(fn api.Function, handle uint32) error {
	status, err := i.call(context.Background(), fn, uint64(handle))
	if err == nil && int32(status) != 0 {
		i.broken = true
		err = fmt.Errorf("%w: %s: status %d", mermaid_go.ErrMermanFailed, fn.Definition().ExportNames()[0], int32(status))
	}
	return err
}

// result decodes and frees an owned result, returning an ordinary failure as
// a *Error.
func (i *instance) result(ctx context.Context, handle uint32) (_ *ownedResult, err error) {
	if handle == 0 {
		i.broken = true
		return nil, fmt.Errorf("%w: guest could not publish a result", mermaid_go.ErrMermanFailed)
	}
	defer func() {
		if !i.broken {
			err = errors.Join(err, i.release(i.resultFree, handle))
		}
	}()

	ptr, err := i.call(ctx, i.resultInfo, uint64(handle))
	if err != nil {
		return nil, err
	}
	descriptor, ok := i.mod.Memory().Read(ptr, descriptorSize)
	if ptr == 0 || !ok {
		i.broken = true
		return nil, fmt.Errorf("%w: invalid result descriptor", mermaid_go.ErrMermanFailed)
	}
	var fields [descriptorSize / 4]uint32
	for n := range fields {
		fields[n] = binary.LittleEndian.Uint32(descriptor[4*n:])
	}
	if fields[0] != 1 || fields[1] != descriptorSize || fields[2] > 1 {
		i.broken = true
		return nil, fmt.Errorf("%w: unsupported result descriptor (version %d, size %d, status %d)",
			mermaid_go.ErrMermanFailed, fields[0], fields[1], fields[2])
	}

	// The media type, fields 4 and 5, is implied by the operation. The
	// metadata, fields 8 and 9, carries the payload of an ordinary failure.
	if fields[2] == 1 {
		metadata, err := i.segment(fields[8], fields[9])
		if err != nil {
			return nil, err
		}
		return nil, decodeError(metadata)
	}
	data, err := i.segment(fields[6], fields[7])
	if err != nil {
		return nil, err
	}
	return &ownedResult{engine: fields[3], data: data}, nil
}

// segment copies a result segment out of guest memory, which the next call may
// reuse or grow.
func (i *instance) segment(ptr, length uint32) ([]byte, error) {
	if (ptr == 0) != (length == 0) {
		i.broken = true
		return nil, fmt.Errorf("%w: noncanonical result segment", mermaid_go.ErrMermanFailed)
	}
	if length == 0 {
		return nil, nil
	}
	data, ok := i.mod.Memory().Read(ptr, length)
	if !ok {
		i.broken = true
		return nil, fmt.Errorf("%w: result segment outside guest memory", mermaid_go.ErrMermanFailed)
	}
	return bytes.Clone(data), nil
}

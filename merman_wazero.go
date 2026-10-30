package mermaid_go

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// MermanWasmABIVersion is the merman-wasi ABI this package speaks. The reactor
// reports its own from merman_wasi_abi_version, and NewMermanWasmEngine refuses
// any other: the ABI has no compatibility aliases between versions.
const MermanWasmABIVersion = 1

// MermanWasmModule is the merman_wasi.wasm reactor NewMermanWasmEngine runs
// when it is given neither WithMermanWasmModule nor WithMermanWasmModuleFile.
// This package embeds none, so that a program which only renders through chrome
// does not carry it: import the mermanwasm subpackage, which sets it to the
// module it embeds,
//
//	import _ "github.com/dreampuf/mermaid.go/mermanwasm"
//
// or assign a module of your own.
var MermanWasmModule []byte

// DefaultMermanWasmMemoryLimit caps the linear memory of each reactor instance,
// matching the limit merman's own wazero example applies.
const DefaultMermanWasmMemoryLimit = 512 << 20

// mermanWasmMaxInput is the ABI's ceiling on a single input or result segment.
const mermanWasmMaxInput = 32 << 20

// mermanWasmDescriptorSize is the size of a result descriptor: ten
// little-endian u32 fields.
const mermanWasmDescriptorSize = 40

// MermanWasmEngine renders diagrams with merman's wasm32-wasip1 reactor, run in
// process by wazero, a pure Go WebAssembly runtime. It needs neither a browser
// nor a merman binary nor cgo: the module is the merman SDK's merman_wasi.wasm,
// supplied as bytes or as a file, and the engine compiles it once at
// construction.
//
// A reactor instance is strictly serial, so the engine keeps a pool of them,
// each with its own merman engine, and runs renders concurrently up to the
// limit set by [WithMermanWasmConcurrency]. Instances are created on demand and
// kept for reuse. One that traps, or is interrupted by a deadline or
// cancellation, is discarded rather than reused, as the ABI requires; the next
// render that needs an instance builds a fresh one.
//
// The output is merman's, as with [MermanEngine], and an instance has no
// filesystem, network, clock or environment of its own: text is measured and
// painted only with the fonts the module bundles and those passed with
// [WithMermanWasmFont].
type MermanWasmEngine struct {
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
	idle chan *mermanWasmInstance

	ctx    context.Context
	cancel context.CancelFunc
	// closed is closed once the runtime has been torn down.
	closed chan struct{}

	renderTimeout atomic.Int64

	version string
	// capabilities is sorted; operations is for lookups.
	capabilities []string
	operations   map[string]bool
}

type mermanWasmConfig struct {
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

// MermanWasmOption configures a MermanWasmEngine at construction.
type MermanWasmOption func(*mermanWasmConfig)

// WithMermanWasmModule supplies the merman_wasi.wasm reactor as bytes, which
// suits a module embedded in the program with go:embed. The engine keeps no
// reference to the slice once it has been compiled.
func WithMermanWasmModule(module []byte) MermanWasmOption {
	return func(c *mermanWasmConfig) { c.module, c.moduleFile = module, "" }
}

// WithMermanWasmModuleFile names the merman_wasi.wasm reactor to load from disk.
func WithMermanWasmModuleFile(path string) MermanWasmOption {
	return func(c *mermanWasmConfig) { c.module, c.moduleFile = nil, path }
}

// WithMermanWasmFont admits a TrueType or OpenType font for measuring and
// painting text, ahead of the fonts the module bundles, so a family it shares
// with a bundled font takes precedence. Pass it once per font. SVG output does
// not embed fonts: whatever displays the SVG needs matching fonts installed.
func WithMermanWasmFont(font []byte) MermanWasmOption {
	return func(c *mermanWasmConfig) { c.fonts = append(c.fonts, font) }
}

// WithMermanWasmSvgID overrides DefaultMermanSvgID as the root <svg> element's
// id and internal marker prefix. An empty id leaves merman's default in place.
func WithMermanWasmSvgID(id string) MermanWasmOption {
	return func(c *mermanWasmConfig) { c.svgID = id }
}

// WithMermanWasmTheme selects a mermaid theme, the equivalent of passing
// mermaid.initialize({theme: ...}) as a statement to NewRenderEngine. It takes
// precedence over a theme in WithMermanWasmSiteConfig.
func WithMermanWasmTheme(theme string) MermanWasmOption {
	return func(c *mermanWasmConfig) { c.theme = theme }
}

// WithMermanWasmSiteConfig supplies a mermaid configuration object as JSON, the
// in-memory counterpart of WithMermanConfigFile. NewMermanWasmEngine fails with
// ErrMermanFailed if it is not a JSON object.
func WithMermanWasmSiteConfig(config []byte) MermanWasmOption {
	return func(c *mermanWasmConfig) { c.siteConfig = config }
}

// WithMermanWasmOptions merges a JSON object into the merman binding options the
// engine is constructed with, for the parts of the schema this package does not
// model -- resource profiles and limits, layout viewport sizes, presentation
// themes and the like. Its top-level groups replace the ones built from the
// other options. The engine speaks options schema 2, the schema of the merman
// 0.9 releases; see merman's docs/bindings/OPTIONS_JSON.md.
func WithMermanWasmOptions(options []byte) MermanWasmOption {
	return func(c *mermanWasmConfig) { c.options = options }
}

// WithMermanWasmConcurrency bounds how many renders run at once, and so how
// many reactor instances the engine keeps, defaulting to GOMAXPROCS. Each
// instance holds its own linear memory, up to WithMermanWasmMemoryLimit. A value
// <= 0 restores the default.
func WithMermanWasmConcurrency(n int) MermanWasmOption {
	return func(c *mermanWasmConfig) { c.concurrency = n }
}

// WithMermanWasmMemoryLimit caps each instance's linear memory, rounded down to
// whole 64 KiB pages, defaulting to DefaultMermanWasmMemoryLimit. A render that
// needs more traps, and its instance is discarded. A value of 0 restores the
// default.
func WithMermanWasmMemoryLimit(bytes uint64) MermanWasmOption {
	return func(c *mermanWasmConfig) { c.memoryLimit = bytes }
}

// WithMermanWasmCompilationCache shares compiled code between engines, so only
// the first of them pays for compiling the module. A cache made with
// wazero.NewCompilationCacheWithDir also survives the process, which takes the
// several seconds compilation costs off every later start. The caller owns the
// cache and closes it once no engine uses it.
func WithMermanWasmCompilationCache(cache wazero.CompilationCache) MermanWasmOption {
	return func(c *mermanWasmConfig) { c.cache = cache }
}

// WithMermanWasmStartupTimeout bounds compiling the module and building the
// first instance when the supplied context has no deadline of its own,
// defaulting to DefaultStartupTimeout.
func WithMermanWasmStartupTimeout(d time.Duration) MermanWasmOption {
	return func(c *mermanWasmConfig) { c.startupTimeout = d }
}

// mermanWasmCatalog is the part of the reactor's runtime catalog this package
// reads.
type mermanWasmCatalog struct {
	PackageVersion        string `json:"package_version"`
	OptionsSchemaVersions []int  `json:"options_schema_versions"`
	Capabilities          struct {
		CapabilityIDs []string `json:"capability_ids"`
		OperationIDs  []string `json:"operation_ids"`
	} `json:"capabilities"`
}

// mermanWasmOptionsSchema is the binding options schema this package writes.
const mermanWasmOptionsSchema = 2

// NewMermanWasmEngine compiles the merman reactor and builds one instance from
// it, so a missing, incompatible or misconfigured module -- a font merman
// rejects, options it does not accept -- is reported here rather than on the
// first render. The instance is kept for that render.
//
// ctx governs the engine's whole lifetime, as it does for NewRenderEngine: when
// it is done the engine closes, every later render fails with ErrEngineClosed
// and the runtime's resources are released. For a long-lived engine prefer
// context.Background() and let each render carry its own deadline; to bound
// only start-up, pass WithMermanWasmStartupTimeout.
func NewMermanWasmEngine(ctx context.Context, opts ...MermanWasmOption) (_ *MermanWasmEngine, err error) {
	cfg := &mermanWasmConfig{svgID: DefaultMermanSvgID, startupTimeout: DefaultStartupTimeout}
	for _, opt := range opts {
		opt(cfg)
	}
	if cfg.concurrency <= 0 {
		cfg.concurrency = runtime.GOMAXPROCS(0)
	}
	if cfg.memoryLimit == 0 {
		cfg.memoryLimit = DefaultMermanWasmMemoryLimit
	}

	// The module options clear each other, so at most one of them is set.
	var module []byte
	switch {
	case cfg.moduleFile != "":
		if module, err = os.ReadFile(cfg.moduleFile); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrMermanUnavailable, err)
		}
	case cfg.module != nil:
		module = cfg.module
	default:
		module = MermanWasmModule
	}
	if len(module) == 0 {
		return nil, fmt.Errorf(`%w: no module supplied; import _ "github.com/dreampuf/mermaid.go/mermanwasm", or pass WithMermanWasmModule or WithMermanWasmModuleFile`, ErrMermanUnavailable)
	}

	config, fonts, err := mermanWasmConstructorInputs(cfg)
	if err != nil {
		return nil, err
	}

	startCtx, cancel := startupContext(ctx, cfg.startupTimeout)
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

	e := &MermanWasmEngine{
		runtime: wazero.NewRuntimeWithConfig(ctx, rtConfig),
		config:  string(config),
		fonts:   string(fonts),
		sem:     make(chan struct{}, cfg.concurrency),
		idle:    make(chan *mermanWasmInstance, cfg.concurrency),
		closed:  make(chan struct{}),
	}
	e.ctx, e.cancel = context.WithCancel(ctx)
	e.renderTimeout.Store(int64(DefaultRenderTimeout))
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

	inst, err := e.newInstance(startCtx)
	if err != nil {
		return nil, err
	}
	catalog, err := inst.catalog(startCtx)
	if err != nil {
		inst.close()
		return nil, err
	}
	e.idle <- inst

	if !slices.Contains(catalog.OptionsSchemaVersions, mermanWasmOptionsSchema) {
		return nil, fmt.Errorf("%w: module accepts options schemas %v, this package writes schema %d",
			ErrMermanUnavailable, catalog.OptionsSchemaVersions, mermanWasmOptionsSchema)
	}
	e.version = catalog.PackageVersion
	e.capabilities = slices.Sorted(slices.Values(catalog.Capabilities.CapabilityIDs))
	e.operations = make(map[string]bool, len(catalog.Capabilities.OperationIDs))
	for _, id := range catalog.Capabilities.OperationIDs {
		e.operations[id] = true
	}
	if !e.operations["svg"] {
		return nil, fmt.Errorf("%w: module cannot render SVG", ErrMermanCapability)
	}
	return e, nil
}

// startErr reports a failure building the engine, preferring the context's
// error when the start-up deadline is what ended it.
func (m *MermanWasmEngine) startErr(ctx context.Context, step string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%w: %s: %w", ErrMermanUnavailable, step, ctxErr)
	}
	return fmt.Errorf("%w: %s: %w", ErrMermanUnavailable, step, err)
}

// mermanWasmConstructorInputs builds the configuration envelope every instance's
// merman engine is constructed from, and the font blob its ranges point into.
func mermanWasmConstructorInputs(cfg *mermanWasmConfig) (config, fonts []byte, err error) {
	// Marshalling string maps and raw JSON that has just been decoded cannot
	// fail, so those errors are not checked.
	options := map[string]json.RawMessage{}
	if cfg.svgID != "" {
		options["svg"], _ = json.Marshal(map[string]string{"diagram_id": cfg.svgID})
	}
	site := map[string]json.RawMessage{}
	if len(cfg.siteConfig) > 0 {
		if err := json.Unmarshal(cfg.siteConfig, &site); err != nil || site == nil {
			return nil, nil, fmt.Errorf("%w: site configuration is not a JSON object", ErrMermanFailed)
		}
	}
	if cfg.theme != "" {
		raw, _ := json.Marshal(cfg.theme)
		site["theme"] = raw
	}
	if len(site) > 0 {
		options["site_config"], _ = json.Marshal(site)
	}
	if len(cfg.options) > 0 {
		var extra map[string]json.RawMessage
		if err := json.Unmarshal(cfg.options, &extra); err != nil || extra == nil {
			return nil, nil, fmt.Errorf("%w: binding options are not a JSON object", ErrMermanFailed)
		}
		for name, value := range extra {
			options[name] = value
		}
	}
	options["version"] = json.RawMessage(fmt.Appendf(nil, "%d", mermanWasmOptionsSchema))
	if err := checkMermanWasmSiteConfig(options["site_config"]); err != nil {
		return nil, nil, err
	}

	type fontRange struct {
		Offset uint32 `json:"offset"`
		Length uint32 `json:"length"`
	}
	ranges := make([]fontRange, 0, len(cfg.fonts))
	total := 0
	for i, font := range cfg.fonts {
		if len(font) == 0 {
			return nil, nil, fmt.Errorf("%w: font %d is empty", ErrMermanFailed, i)
		}
		if len(font) > mermanWasmMaxInput-total {
			return nil, nil, fmt.Errorf("%w: fonts exceed the %d MiB the module accepts", ErrMermanFailed, mermanWasmMaxInput>>20)
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
		return nil, nil, fmt.Errorf("%w: constructor options: %w", ErrMermanFailed, err)
	}
	return config, fonts, nil
}

// checkMermanWasmSiteConfig rejects what the module accepts at construction
// only to fail every render with. The reactor always measures text with real
// fonts, and that measurement cannot honour CSS applied after layout, so a
// themeCSS turns each render into an opaque internal error. Unlike the CLI,
// the transport offers no deterministic measurement to fall back on.
func checkMermanWasmSiteConfig(site json.RawMessage) error {
	if len(site) == 0 {
		return nil
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(site, &config); err != nil || config == nil {
		return fmt.Errorf("%w: site configuration is not a JSON object", ErrMermanFailed)
	}
	if _, ok := config["themeCSS"]; ok {
		return fmt.Errorf("%w: themeCSS is not supported by merman's font measurement; style through themeVariables instead", ErrUnsupportedOption)
	}
	return nil
}

// Version reports the merman release the module was built from.
func (m *MermanWasmEngine) Version() string { return m.version }

// Capabilities reports the merman features the module was compiled with,
// sorted, using merman's own ids: "svg", "png", "math", "layout-elk" and so on.
// They are what distinguishes the SDK's standard, math and full modules.
func (m *MermanWasmEngine) Capabilities() []string { return slices.Clone(m.capabilities) }

// SetRenderTimeout overrides DefaultRenderTimeout for subsequent renders. A
// value <= 0 disables the per-render deadline, leaving renders bounded only by
// the engine context and the caller's own.
func (m *MermanWasmEngine) SetRenderTimeout(d time.Duration) {
	m.renderTimeout.Store(int64(d))
}

// Cancel closes the engine: renders in flight are interrupted, every later
// render fails with ErrEngineClosed, and the runtime and its compiled code are
// released. Calling it more than once is harmless.
func (m *MermanWasmEngine) Cancel() { m.cancel() }

// Render renders content to SVG. It is equivalent to RenderContext with a
// background context.
func (m *MermanWasmEngine) Render(content string, opts ...RenderOption) (string, error) {
	return m.RenderContext(context.Background(), content, opts...)
}

// RenderContext renders content to SVG, giving up if ctx is cancelled. The
// context covers the wait for a free instance as well as the render itself,
// and its deadline applies if it is sooner than the engine's render timeout.
// Only cancellation and the deadline are taken from ctx; its values are not.
func (m *MermanWasmEngine) RenderContext(ctx context.Context, content string, opts ...RenderOption) (string, error) {
	renderOpts := newRenderOptions(opts)
	out, err := m.run(ctx, renderOpts, "svg", nil, content)
	if err != nil {
		return "", err
	}
	return mermanSVG(out, content, renderOpts)
}

// RenderAsPng renders content to a PNG. It is equivalent to RenderAsPngContext
// with a background context.
func (m *MermanWasmEngine) RenderAsPng(content string, opts ...RenderOption) ([]byte, *BoxModel, error) {
	return m.RenderAsScaledPngContext(context.Background(), content, 1.0, opts...)
}

// RenderAsPngContext renders content to a PNG, giving up if ctx is cancelled.
func (m *MermanWasmEngine) RenderAsPngContext(ctx context.Context, content string, opts ...RenderOption) ([]byte, *BoxModel, error) {
	return m.RenderAsScaledPngContext(ctx, content, 1.0, opts...)
}

// RenderAsScaledPng renders content to a PNG at the given scale. It is
// equivalent to RenderAsScaledPngContext with a background context.
func (m *MermanWasmEngine) RenderAsScaledPng(content string, scale float64, opts ...RenderOption) ([]byte, *BoxModel, error) {
	return m.RenderAsScaledPngContext(context.Background(), content, scale, opts...)
}

// RenderAsScaledPngContext renders content to a PNG at the given scale, giving
// up if ctx is cancelled. The box model is derived from the image as it is for
// [MermanEngine.RenderAsScaledPngContext].
func (m *MermanWasmEngine) RenderAsScaledPngContext(ctx context.Context, content string, scale float64, opts ...RenderOption) ([]byte, *BoxModel, error) {
	renderOpts, err := mermanPngOptions(opts, scale)
	if err != nil {
		return nil, nil, err
	}
	if !m.operations["png"] {
		return nil, nil, fmt.Errorf("%w: module was built without png output", ErrMermanCapability)
	}

	var options json.RawMessage
	if scale != 1 {
		// scale is finite and positive, so it formats as a JSON number.
		options = fmt.Appendf(nil, `{"version":%d,"raster":{"scale":%s}}`,
			mermanWasmOptionsSchema, strconv.FormatFloat(scale, 'g', -1, 64))
	}
	out, err := m.run(ctx, renderOpts, "png", options, content)
	if err != nil {
		return nil, nil, err
	}
	return mermanPng(out, scale, "module")
}

// run executes one operation on a pooled instance and returns its output.
func (m *MermanWasmEngine) run(caller context.Context, opts *renderOptions, operation string, options []byte, content string) ([]byte, error) {
	if len(content) > mermanWasmMaxInput {
		return nil, fmt.Errorf("%w: diagram source exceeds the %d MiB the module accepts", ErrRenderException, mermanWasmMaxInput>>20)
	}
	release, err := waitForSlot(caller, m.ctx, m.sem)
	if err != nil {
		return nil, classifyRenderErr(caller, m.ctx, err)
	}
	defer release()

	runCtx, cancel := deriveRenderContext(m.ctx, caller, opts.timeoutOr(&m.renderTimeout))
	defer cancel()

	var inst *mermanWasmInstance
	select {
	case inst = <-m.idle:
	default:
		if inst, err = m.newInstance(runCtx); err != nil {
			return nil, m.runErr(caller, runCtx, err)
		}
	}

	out, err := inst.execute(runCtx, operation, options, content)
	// An instance is only worth keeping when it finished the call cleanly or
	// with an ordinary failure: one interrupted mid-call is closed already, and
	// one that trapped or broke the ABI's contract is in a state the ABI makes
	// no promises about. Every such path marks it broken.
	if !inst.broken && m.ctx.Err() == nil {
		m.idle <- inst
	} else {
		inst.close()
	}
	if err != nil {
		return nil, m.runErr(caller, runCtx, err)
	}
	return out, nil
}

// runErr classifies a failed render, consulting the context first: an
// interrupted call reports only that its module was closed, and the context
// knows whether the deadline passed, the caller gave up or the engine closed.
func (m *MermanWasmEngine) runErr(caller, runCtx context.Context, err error) error {
	var owned *MermanWasmError
	if !errors.As(err, &owned) {
		if ctxErr := runCtx.Err(); ctxErr != nil {
			return classifyRenderErr(caller, m.ctx, ctxErr)
		}
	}
	return classifyRenderErr(caller, m.ctx, err)
}

// MermanWasmError is an ordinary failure the merman reactor reported, an
// error it owns rather than a trap: an invalid diagram, options it rejected, a
// capability the module lacks, a resource limit. It is wrapped by the sentinel
// that classifies it -- ErrRenderException, ErrMermanCapability or
// ErrMermanFailed -- and stays reachable with errors.As.
type MermanWasmError struct {
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

func (e *MermanWasmError) Error() string { return e.CodeName + ": " + e.Message }

// mermanWasmError decodes an owned failure's payload.
func mermanWasmError(payload []byte) error {
	owned := &MermanWasmError{}
	if err := json.Unmarshal(payload, owned); err != nil {
		return fmt.Errorf("%w: unreadable error payload: %w", ErrMermanFailed, err)
	}
	return owned
}

// classifyMermanWasmRender wraps an owned failure of a render in its sentinel.
// The statuses attributable to the diagram -- it is not UTF-8, names no
// diagram, does not parse or render, or exceeds a resource limit -- become the
// same non-retryable ErrRenderException an invalid diagram raises under the
// other backends. Other errors pass through.
func classifyMermanWasmRender(err error) error {
	var owned *MermanWasmError
	if !errors.As(err, &owned) {
		return err
	}
	sentinel := ErrMermanFailed
	switch {
	case owned.Kind == "missing-capability":
		sentinel = ErrMermanCapability
	case owned.CodeName == "MERMAN_UTF8_ERROR", owned.CodeName == "MERMAN_NO_DIAGRAM",
		owned.CodeName == "MERMAN_PARSE_ERROR", owned.CodeName == "MERMAN_RENDER_ERROR",
		owned.CodeName == "MERMAN_RESOURCE_LIMIT_EXCEEDED":
		sentinel = ErrRenderException
	}
	return fmt.Errorf("%w: %w", sentinel, err)
}

// mermanWasmInstance is one reactor instance and the merman engine built in it.
// It is used by one render at a time.
type mermanWasmInstance struct {
	mod    api.Module
	engine uint32
	// broken records that the instance must not be reused: it trapped, was
	// interrupted, or answered outside the ABI's contract.
	broken bool

	alloc, free, executeFn, resultInfo, resultFree, engineClose, runtimeCatalog api.Function
}

// newInstance instantiates the reactor and constructs its merman engine.
func (m *MermanWasmEngine) newInstance(ctx context.Context) (inst *mermanWasmInstance, err error) {
	// An empty name lets any number of instances of the module coexist in the
	// runtime; no start functions, because a reactor is initialised explicitly.
	mod, err := m.runtime.InstantiateModule(ctx, m.compiled, wazero.NewModuleConfig().WithName("").WithStartFunctions())
	if err != nil {
		return nil, fmt.Errorf("%w: instantiate module: %w", ErrMermanFailed, err)
	}
	inst = &mermanWasmInstance{mod: mod}
	defer func(i *mermanWasmInstance) {
		if err != nil {
			i.close()
		}
	}(inst)

	if mod.Memory() == nil {
		return nil, fmt.Errorf("%w: module exports no memory", ErrMermanUnavailable)
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
			return nil, fmt.Errorf("%w: module does not export %s; is it merman_wasi.wasm?", ErrMermanUnavailable, name)
		}
	}
	initialize := mod.ExportedFunction("_initialize")
	abiVersion := mod.ExportedFunction("merman_wasi_abi_version")
	engineNew := mod.ExportedFunction("merman_wasi_engine_new")
	if initialize == nil || abiVersion == nil || engineNew == nil {
		return nil, fmt.Errorf("%w: module is not a merman-wasi reactor", ErrMermanUnavailable)
	}

	if _, err := inst.call(ctx, initialize); err != nil {
		return nil, err
	}
	version, err := inst.call(ctx, abiVersion)
	if err != nil {
		return nil, err
	}
	if version != MermanWasmABIVersion {
		return nil, fmt.Errorf("%w: module speaks merman-wasi ABI %d, this package speaks %d",
			ErrMermanUnavailable, version, MermanWasmABIVersion)
	}

	result, err := inst.invoke(ctx, engineNew, nil, m.config, m.fonts)
	if err != nil {
		var owned *MermanWasmError
		if errors.As(err, &owned) {
			// A constructor failure is the configuration's fault, never the
			// diagram's, whatever status the payload carries.
			return nil, fmt.Errorf("%w: constructing the merman engine: %w", ErrMermanFailed, owned)
		}
		return nil, err
	}
	if result.engine == 0 {
		inst.broken = true
		return nil, fmt.Errorf("%w: constructor returned no engine", ErrMermanFailed)
	}
	inst.engine = result.engine
	return inst, nil
}

// close releases the instance. A broken instance gets no further guest calls:
// closing the module reclaims everything it held.
func (i *mermanWasmInstance) close() {
	if !i.broken && i.engine != 0 {
		_, _ = i.call(context.Background(), i.engineClose, uint64(i.engine))
	}
	_ = i.mod.Close(context.Background())
}

// call invokes a guest function returning at most one scalar. Any error from
// the runtime -- a trap, an interruption, a closed module -- leaves the
// instance unusable.
func (i *mermanWasmInstance) call(ctx context.Context, fn api.Function, args ...uint64) (uint32, error) {
	values, err := fn.Call(ctx, args...)
	if err != nil {
		i.broken = true
		return 0, fmt.Errorf("%w: %s: %w", ErrMermanFailed, fn.Definition().ExportNames()[0], err)
	}
	if len(values) == 0 {
		return 0, nil
	}
	return uint32(values[0]), nil
}

// mermanWasmResult is a host-side copy of an owned result.
type mermanWasmResult struct {
	engine uint32
	data   []byte
}

// catalog reads the instance's runtime catalog.
func (i *mermanWasmInstance) catalog(ctx context.Context) (*mermanWasmCatalog, error) {
	handle, err := i.call(ctx, i.runtimeCatalog)
	if err != nil {
		return nil, err
	}
	result, err := i.result(ctx, handle)
	if err != nil {
		return nil, fmt.Errorf("%w: runtime catalog: %w", ErrMermanUnavailable, err)
	}
	catalog := &mermanWasmCatalog{}
	if err := json.Unmarshal(result.data, catalog); err != nil {
		return nil, fmt.Errorf("%w: unreadable runtime catalog: %w", ErrMermanUnavailable, err)
	}
	return catalog, nil
}

// execute runs one operation on the instance's merman engine and returns its
// raw output, with an ordinary failure classified for the caller.
func (i *mermanWasmInstance) execute(ctx context.Context, operation string, options json.RawMessage, source string) ([]byte, error) {
	request, err := json.Marshal(struct {
		Version   uint32          `json:"version"`
		Operation string          `json:"operation_id"`
		Options   json.RawMessage `json:"options,omitempty"`
	}{1, operation, options})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFailedEncoding, err)
	}
	result, err := i.invoke(ctx, i.executeFn, []uint64{uint64(i.engine)}, string(request), source)
	if err != nil {
		return nil, classifyMermanWasmRender(err)
	}
	return result.data, nil
}

// invoke copies each input into a fresh guest allocation, calls fn with args
// followed by each input's offset and length, frees the inputs again and
// returns the owned result fn published. An empty input is passed as the
// canonical empty range, offset 0. Inputs are strings so the diagram source is
// copied once, straight into the guest.
func (i *mermanWasmInstance) invoke(ctx context.Context, fn api.Function, args []uint64, inputs ...string) (_ *mermanWasmResult, err error) {
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
				return nil, fmt.Errorf("%w: guest could not allocate %d bytes", ErrMermanFailed, len(input))
			}
			ptrs = append(ptrs, ptr)
			if !i.mod.Memory().WriteString(ptr, input) {
				i.broken = true
				return nil, fmt.Errorf("%w: guest allocation out of bounds", ErrMermanFailed)
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
func (i *mermanWasmInstance) release(fn api.Function, handle uint32) error {
	status, err := i.call(context.Background(), fn, uint64(handle))
	if err == nil && int32(status) != 0 {
		i.broken = true
		err = fmt.Errorf("%w: %s: status %d", ErrMermanFailed, fn.Definition().ExportNames()[0], int32(status))
	}
	return err
}

// result decodes and frees an owned result, returning an ordinary failure as
// a *MermanWasmError.
func (i *mermanWasmInstance) result(ctx context.Context, handle uint32) (_ *mermanWasmResult, err error) {
	if handle == 0 {
		i.broken = true
		return nil, fmt.Errorf("%w: guest could not publish a result", ErrMermanFailed)
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
	descriptor, ok := i.mod.Memory().Read(ptr, mermanWasmDescriptorSize)
	if ptr == 0 || !ok {
		i.broken = true
		return nil, fmt.Errorf("%w: invalid result descriptor", ErrMermanFailed)
	}
	var fields [mermanWasmDescriptorSize / 4]uint32
	for n := range fields {
		fields[n] = binary.LittleEndian.Uint32(descriptor[4*n:])
	}
	if fields[0] != 1 || fields[1] != mermanWasmDescriptorSize || fields[2] > 1 {
		i.broken = true
		return nil, fmt.Errorf("%w: unsupported result descriptor (version %d, size %d, status %d)",
			ErrMermanFailed, fields[0], fields[1], fields[2])
	}

	// The media type, fields 4 and 5, is implied by the operation. The
	// metadata, fields 8 and 9, carries the payload of an ordinary failure.
	if fields[2] == 1 {
		metadata, err := i.segment(fields[8], fields[9])
		if err != nil {
			return nil, err
		}
		return nil, mermanWasmError(metadata)
	}
	data, err := i.segment(fields[6], fields[7])
	if err != nil {
		return nil, err
	}
	return &mermanWasmResult{engine: fields[3], data: data}, nil
}

// segment copies a result segment out of guest memory, which the next call may
// reuse or grow.
func (i *mermanWasmInstance) segment(ptr, length uint32) ([]byte, error) {
	if (ptr == 0) != (length == 0) {
		i.broken = true
		return nil, fmt.Errorf("%w: noncanonical result segment", ErrMermanFailed)
	}
	if length == 0 {
		return nil, nil
	}
	data, ok := i.mod.Memory().Read(ptr, length)
	if !ok {
		i.broken = true
		return nil, fmt.Errorf("%w: result segment outside guest memory", ErrMermanFailed)
	}
	return bytes.Clone(data), nil
}

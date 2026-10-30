package mermaid_go

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dreampuf/mermaid.go/internal/render"
)

// MermanBinaries are the names NewMermanEngine looks for on PATH, in order,
// when no binary is named with WithMermanBinary. merman ships the renderer as
// merman-cli; the shorter name is accepted for a locally built or renamed copy.
var MermanBinaries = []string{"merman-cli", "merman"}

// DefaultMermanSvgID is the id given to the root <svg> element, matching the id
// mermaid.js gives it under the chrome backend so a diagram styled or scripted
// against one backend keeps working under the other.
const DefaultMermanSvgID = "mermaid"

var (
	// ErrMermanUnavailable reports that the merman binary could not be found or
	// could not describe itself, or that the merman WebAssembly module was
	// missing, would not compile or is not a reactor this package speaks to. The
	// engine refuses to start rather than deferring the failure to the first
	// render.
	ErrMermanUnavailable = render.ErrMermanUnavailable
	// ErrMermanCapability reports that the merman binary or module was built
	// without a feature the call needs -- most often PNG output or formulas,
	// which are compile-time features rather than something every build has. It
	// is separated from ErrMermanFailed because the fix is to install a
	// different build, not to change the diagram or retry.
	ErrMermanCapability = render.ErrMermanCapability
	// ErrMermanFailed reports a merman invocation that failed for a reason other
	// than an invalid diagram: a rejected command line, an unusable configuration
	// file, an operational failure, or output this package could not use. The
	// binary's stderr is included, and a *MermanExitError carrying the exit status
	// is reachable with errors.As when a process is what failed.
	ErrMermanFailed = render.ErrMermanFailed
)

// MermanEngine renders diagrams by running the merman CLI, which parses, lays
// out and renders mermaid natively. It is an alternative to [RenderEngine]:
// there is no browser and no JavaScript, so nothing has to be installed beyond
// the merman binary itself, and start-up costs nothing but one capability probe.
//
// Unlike [RenderEngine], which serialises everything onto one browser tab, each
// render is its own short-lived process, so renders run concurrently up to the
// limit set by [WithMermanConcurrency]. An engine holds no operating system
// resources between renders, but [MermanEngine.Cancel] still exists to abort
// renders in flight and to make every later render fail with [ErrEngineClosed],
// so both backends can be shut down the same way.
//
// Output is merman's own, not mermaid.js's. It aims to match mermaid but is a
// separate implementation, so the two backends do not produce byte-identical
// SVG; see the merman project for the compatibility it claims.
type MermanEngine struct {
	bin  string
	args []string
	// sem bounds how many merman processes run at once. As in RenderEngine it is
	// a channel rather than a semaphore type so that waiting for a turn can be
	// abandoned when the caller's context is cancelled.
	sem    chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	// renderTimeout is atomic so it can be adjusted while a render is running.
	renderTimeout atomic.Int64

	version string
	mermaid string
	// capabilities is the catalogue the binary reported, or nil when it is an
	// older release with no `capabilities` command to ask. Nil means unknown,
	// not empty: the engine must not conclude a format is missing from it.
	capabilities map[string]bool
}

type mermanConfig struct {
	bin            string
	svgID          string
	theme          string
	configFile     string
	args           []string
	concurrency    int
	startupTimeout time.Duration
}

// MermanOption configures a MermanEngine at construction.
type MermanOption func(*mermanConfig)

// WithMermanBinary names the merman executable to run, as a path or as a name
// to look up on PATH. Without it NewMermanEngine tries each of MermanBinaries.
func WithMermanBinary(path string) MermanOption {
	return func(c *mermanConfig) { c.bin = path }
}

// WithMermanSvgID overrides DefaultMermanSvgID as the root <svg> element's id
// and internal marker prefix. An empty id leaves merman's default in place.
func WithMermanSvgID(id string) MermanOption {
	return func(c *mermanConfig) { c.svgID = id }
}

// WithMermanTheme selects a mermaid theme, the equivalent of passing
// mermaid.initialize({theme: ...}) as a statement to NewRenderEngine.
func WithMermanTheme(theme string) MermanOption {
	return func(c *mermanConfig) { c.theme = theme }
}

// WithMermanConfigFile points merman at a JSON mermaid configuration file. It is
// the closest equivalent to NewRenderEngine's statements, which cannot carry
// over because there is no JavaScript context to run them in. The file is read
// and checked for well-formed JSON by NewMermanEngine, which fails with
// ErrMermanFailed rather than leaving every render to fail as a bad diagram.
func WithMermanConfigFile(path string) MermanOption {
	return func(c *mermanConfig) { c.configFile = path }
}

// WithMermanArgs appends arguments to every render invocation, for the parts of
// merman's command line this package does not model -- resource profiles, icon
// packs, layout viewport sizes and the like. They are appended last, so they
// override the arguments built from the other options.
func WithMermanArgs(args ...string) MermanOption {
	return func(c *mermanConfig) { c.args = append(c.args, args...) }
}

// WithMermanConcurrency bounds how many merman processes may run at once,
// defaulting to GOMAXPROCS. Renders beyond the limit wait for a turn, and that
// wait is covered by the caller's context just as the render itself is. A value
// <= 0 restores the default.
func WithMermanConcurrency(n int) MermanOption {
	return func(c *mermanConfig) { c.concurrency = n }
}

// WithMermanStartupTimeout bounds the capability probe NewMermanEngine runs when
// the supplied context has no deadline of its own, defaulting to
// DefaultStartupTimeout.
func WithMermanStartupTimeout(d time.Duration) MermanOption {
	return func(c *mermanConfig) { c.startupTimeout = d }
}

// mermanCapabilityDocument is the part of `merman-cli capabilities --json` this
// package reads. The CLI compiles its output formats in as features, so a build
// without PNG support is a real possibility that is better reported at start-up
// than as a puzzling exit status on the first RenderAsPng.
//
// The command arrived after 0.7.0, so its absence is not a failure; see
// probeMerman.
type mermanCapabilityDocument struct {
	Package struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"package"`
	Compatibility struct {
		Mermaid string `json:"mermaid"`
	} `json:"compatibility"`
	Commands     []string `json:"commands"`
	Capabilities []struct {
		ID string `json:"id"`
	} `json:"capabilities"`
}

// NewMermanEngine builds an engine backed by the merman CLI. It locates the
// binary and asks it what it is, so a missing or unusable binary is reported
// here rather than on the first render.
//
// ctx governs the engine's whole lifetime, as it does for NewRenderEngine: the
// engine context is derived from it, so a deadline on ctx does far more than
// bound start-up -- when it passes, the engine closes and every later render
// fails with ErrEngineClosed. For a long-lived engine prefer context.Background()
// and let each render carry its own deadline; to bound only the probe, pass
// WithMermanStartupTimeout.
func NewMermanEngine(ctx context.Context, opts ...MermanOption) (*MermanEngine, error) {
	cfg := &mermanConfig{
		svgID:          DefaultMermanSvgID,
		concurrency:    runtime.GOMAXPROCS(0),
		startupTimeout: DefaultStartupTimeout,
	}
	for _, opt := range opts {
		opt(cfg)
	}
	if cfg.concurrency <= 0 {
		cfg.concurrency = runtime.GOMAXPROCS(0)
	}

	if cfg.configFile != "" {
		if err := checkMermanConfigFile(cfg.configFile); err != nil {
			return nil, err
		}
	}

	bin, err := lookMerman(cfg.bin)
	if err != nil {
		return nil, err
	}

	probeCtx, cancel := render.StartupContext(ctx, cfg.startupTimeout)
	defer cancel()
	probe, err := probeMerman(probeCtx, bin)
	if err != nil {
		return nil, err
	}
	// Only refuse on a catalogue the binary actually reported. A release too old
	// to have `capabilities` renders perfectly well, and rejecting it for saying
	// nothing would be worse than the puzzle the probe exists to prevent.
	if probe.capabilities != nil && (!probe.commands["render"] || !probe.capabilities["svg"]) {
		return nil, fmt.Errorf("%w: %s cannot render SVG", ErrMermanCapability, bin)
	}

	engine := &MermanEngine{
		bin:          bin,
		args:         mermanRenderArgs(cfg),
		sem:          make(chan struct{}, cfg.concurrency),
		version:      probe.version,
		mermaid:      probe.mermaid,
		capabilities: probe.capabilities,
	}
	engine.ctx, engine.cancel = context.WithCancel(ctx)
	engine.renderTimeout.Store(int64(DefaultRenderTimeout))
	return engine, nil
}

// checkMermanConfigFile reads the configuration merman is to be pointed at. A
// missing or malformed file makes merman exit 1 -- the status it also uses for an
// invalid diagram -- so without this every render would fail as
// ErrRenderException and blame the diagram for a configuration mistake. Reading
// it once at construction turns that into a single honest error instead.
func checkMermanConfigFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: configuration file: %w", ErrMermanFailed, err)
	}
	if !json.Valid(data) {
		return fmt.Errorf("%w: configuration file %s is not valid JSON", ErrMermanFailed, path)
	}
	return nil
}

// lookMerman resolves the executable, naming everything it tried so a PATH
// problem is obvious from the error alone.
func lookMerman(bin string) (string, error) {
	if bin != "" {
		path, err := exec.LookPath(bin)
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrMermanUnavailable, err)
		}
		return path, nil
	}
	var errs []error
	for _, name := range MermanBinaries {
		path, err := exec.LookPath(name)
		if err == nil {
			return path, nil
		}
		errs = append(errs, err)
	}
	return "", fmt.Errorf("%w: none of %s found on PATH: %w",
		ErrMermanUnavailable, strings.Join(MermanBinaries, ", "), errors.Join(errs...))
}

// mermanProbe is what NewMermanEngine could learn about the binary before
// rendering anything with it.
type mermanProbe struct {
	version string
	mermaid string
	// commands and capabilities are nil unless the binary reported a catalogue.
	commands     map[string]bool
	capabilities map[string]bool
}

// probeMerman identifies the binary. It prefers `capabilities --json`, which is
// authoritative about the compiled feature set, and falls back to --version for
// releases that predate that command -- 0.7.0 among them.
// Those releases accept every argument this package builds and render exactly
// the same, so all the fallback gives up is the catalogue, and the engine then
// declines to guess rather than refusing to start.
//
// The fallback is deliberately narrow: it applies when the CLI rejects the
// command as a usage error, not when it accepts and fails it. A binary that
// knows `capabilities` and cannot answer it is broken, and that is worth
// failing on here rather than at the first render.
func probeMerman(ctx context.Context, bin string) (*mermanProbe, error) {
	stdout, capErr := runMermanProbe(ctx, bin, "capabilities", "--json")
	if capErr == nil {
		doc := &mermanCapabilityDocument{}
		if err := json.Unmarshal(stdout, doc); err != nil {
			return nil, fmt.Errorf("%w: %s reported unreadable capabilities: %w", ErrMermanUnavailable, bin, err)
		}
		probe := &mermanProbe{
			version:      doc.Package.Version,
			mermaid:      doc.Compatibility.Mermaid,
			commands:     make(map[string]bool, len(doc.Commands)),
			capabilities: make(map[string]bool, len(doc.Capabilities)),
		}
		for _, command := range doc.Commands {
			probe.commands[command] = true
		}
		for _, capability := range doc.Capabilities {
			probe.capabilities[capability.ID] = true
		}
		return probe, nil
	}
	if ctx.Err() != nil {
		return nil, capErr
	}
	// Only a usage error means "this release has no such command". Anything else
	// -- the command understood and failed, or the process refusing to start --
	// is a broken or incompatible binary, and starting it blind would hide that
	// until the first render.
	if !mermanRejectedTheCommand(capErr) {
		return nil, capErr
	}

	stdout, err := runMermanProbe(ctx, bin, "--version")
	if err != nil {
		// Report the --version failure: an old merman rejects `capabilities`
		// too, so that error says nothing about whether the binary is usable.
		return nil, err
	}
	return &mermanProbe{version: mermanVersion(stdout)}, nil
}

// mermanRejectedTheCommand reports whether the CLI answered with a usage error,
// exit status 2, which is how a release that predates a subcommand says it does
// not know it -- 0.7.0 answers `capabilities --json` exactly that way.
func mermanRejectedTheCommand(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 2
}

// runMermanProbe runs one identification command, folding the binary's stderr
// into the error so a wrong or broken executable explains itself.
func runMermanProbe(ctx context.Context, bin string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("%w: %s %s: %w", ErrMermanUnavailable, bin, strings.Join(args, " "), ctxErr)
		}
		return nil, fmt.Errorf("%w: %s %s: %w: %s",
			ErrMermanUnavailable, bin, strings.Join(args, " "), err, mermanDiagnostic(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

// mermanVersion picks the version out of `merman-cli 0.7.0`.
func mermanVersion(stdout []byte) string {
	line, _, _ := strings.Cut(string(stdout), "\n")
	if fields := strings.Fields(line); len(fields) > 0 {
		return fields[len(fields)-1]
	}
	return ""
}

// mermanRenderArgs builds the arguments shared by every render: the ones fixed
// by this package's contract (stdin in, stdout out, diagnostics on stderr only)
// followed by the ones the caller configured.
func mermanRenderArgs(cfg *mermanConfig) []string {
	args := []string{"render", "-", "--output", "-", "--quiet"}
	if cfg.svgID != "" {
		args = append(args, "--svg-id", cfg.svgID)
	}
	if cfg.theme != "" {
		args = append(args, "--theme", cfg.theme)
	}
	if cfg.configFile != "" {
		args = append(args, "--config-file", cfg.configFile)
	}
	return append(args, cfg.args...)
}

// Version reports the version of the merman binary behind the engine.
func (m *MermanEngine) Version() string { return m.version }

// MermaidVersion reports the mermaid release merman's output is pinned to, the
// counterpart of the embedded SourceMermaid bundle's version. It is empty when
// the binary is too old to report one.
func (m *MermanEngine) MermaidVersion() string { return m.mermaid }

// Binary reports the resolved path of the merman executable.
func (m *MermanEngine) Binary() string { return m.bin }

// Capabilities reports the merman features the binary was compiled with, sorted
// -- "svg", "png", "math" and so on, using merman's own ids. It is nil, rather
// than empty, when the binary predates the `capabilities` command and so never
// said: an engine on such a binary still renders, it just cannot promise a
// format in advance.
func (m *MermanEngine) Capabilities() []string {
	if m.capabilities == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(m.capabilities))
}

// SetRenderTimeout overrides DefaultRenderTimeout for subsequent renders. A
// value <= 0 disables the per-render deadline, leaving renders bounded only by
// the engine context and the caller's own.
func (m *MermanEngine) SetRenderTimeout(d time.Duration) {
	m.renderTimeout.Store(int64(d))
}

// Cancel closes the engine: renders in flight are killed and every later render
// fails with ErrEngineClosed. Calling it more than once is harmless.
func (m *MermanEngine) Cancel() { m.cancel() }

// Render renders content to SVG. It is equivalent to RenderContext with a
// background context, so it cannot be cancelled by the caller and is bounded
// only by the engine's render timeout.
func (m *MermanEngine) Render(content string, opts ...RenderOption) (string, error) {
	return m.RenderContext(context.Background(), content, opts...)
}

// RenderContext renders content to SVG, giving up if ctx is cancelled. The
// context covers the wait for a concurrency slot as well as the render itself,
// and its deadline applies if it is sooner than the engine's render timeout.
// Only cancellation and the deadline are taken from ctx; its values are not.
func (m *MermanEngine) RenderContext(ctx context.Context, content string, opts ...RenderOption) (string, error) {
	renderOpts := render.NewOptions(opts)
	out, err := m.run(ctx, renderOpts, content, nil)
	if err != nil {
		return "", err
	}
	return render.MermanSVG(out, content, renderOpts)
}

// RenderAsPng renders content to a PNG. It is equivalent to RenderAsPngContext
// with a background context.
func (m *MermanEngine) RenderAsPng(content string, opts ...RenderOption) ([]byte, *BoxModel, error) {
	return m.RenderAsScaledPngContext(context.Background(), content, 1.0, opts...)
}

// RenderAsPngContext renders content to a PNG, giving up if ctx is cancelled.
func (m *MermanEngine) RenderAsPngContext(ctx context.Context, content string, opts ...RenderOption) ([]byte, *BoxModel, error) {
	return m.RenderAsScaledPngContext(ctx, content, 1.0, opts...)
}

// RenderAsScaledPng renders content to a PNG at the given scale. It is
// equivalent to RenderAsScaledPngContext with a background context.
func (m *MermanEngine) RenderAsScaledPng(content string, scale float64, opts ...RenderOption) ([]byte, *BoxModel, error) {
	return m.RenderAsScaledPngContext(context.Background(), content, scale, opts...)
}

// RenderAsScaledPngContext renders content to a PNG at the given scale, giving
// up if ctx is cancelled. See RenderContext for how ctx is applied. On failure
// it returns no image and no box model, so a caller cannot mistake a partial
// image for a complete one.
//
// The box model is derived from the image, since merman reports no layout box:
// only the width and height are populated, in CSS pixels as chrome reports them
// -- that is, the image's pixel size divided by scale -- and the quads describe
// that box at the origin.
func (m *MermanEngine) RenderAsScaledPngContext(ctx context.Context, content string, scale float64, opts ...RenderOption) ([]byte, *BoxModel, error) {
	renderOpts, err := render.MermanPngOptions(opts, scale)
	if err != nil {
		return nil, nil, err
	}
	// Only pre-empt the render when the binary said it cannot do this.
	if m.capabilities != nil && !m.capabilities["png"] {
		return nil, nil, fmt.Errorf("%w: %s was built without png output", ErrMermanCapability, m.bin)
	}

	args := []string{"--format", "png"}
	if scale != 1 {
		args = append(args, "--scale", strconv.FormatFloat(scale, 'f', -1, 64))
	}
	out, err := m.run(ctx, renderOpts, content, args)
	if err != nil {
		return nil, nil, err
	}
	return render.MermanPng(out, scale, m.bin)
}

// run executes one merman invocation, with extra taking the arguments specific
// to this render. It returns stdout, which carries the rendered payload and
// nothing else -- merman keeps diagnostics on stderr.
func (m *MermanEngine) run(caller context.Context, opts *render.Options, content string, extra []string) ([]byte, error) {
	release, err := render.AcquireSlot(caller, m.ctx, m.sem)
	if err != nil {
		return nil, err
	}
	defer release()

	runCtx, cancel := render.DeriveContext(m.ctx, caller, opts.TimeoutOr(&m.renderTimeout))
	defer cancel()

	args := make([]string, 0, len(m.args)+len(extra))
	args = append(args, m.args...)
	args = append(args, extra...)

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(runCtx, m.bin, args...)
	cmd.Stdin = strings.NewReader(content)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// Killing the process on a deadline is not enough on its own: Wait also
	// waits for the output pipes to close, which anything the render left behind
	// would hold open. Give that a bound so a timed out render cannot outlive
	// its deadline indefinitely.
	cmd.WaitDelay = mermanWaitDelay
	if err := cmd.Run(); err != nil {
		return nil, m.runErr(caller, runCtx, err, stderr.Bytes())
	}
	return stdout.Bytes(), nil
}

// MermanExitError reports how a merman invocation failed: the status it exited
// with and the diagnostics it wrote. It is wrapped by whichever sentinel runErr
// chose and stays reachable with errors.As, because merman's exit codes do not
// partition failures perfectly -- releases up to 0.7.0 exit 1 for an unreadable
// configuration file as well as for an invalid diagram -- so a caller who must be
// certain can read the status and the message instead of trusting the sentinel.
type MermanExitError struct {
	// Bin is the executable that ran.
	Bin string
	// Code is its exit status: 1 for invalid content, 2 for a rejected command
	// line, 3 for an operational failure.
	Code int
	// Stderr is what merman reported, trimmed and truncated.
	Stderr string
}

func (e *MermanExitError) Error() string {
	return fmt.Sprintf("%s exited %d: %s", e.Bin, e.Code, e.Stderr)
}

// runErr classifies a failed invocation. A killed process reports only "signal:
// killed", so the context is consulted first: it knows whether the deadline
// passed, the caller gave up or the engine was closed.
//
// Otherwise the exit status classifies it. Exit 1 is merman's status for invalid
// mermaid or content, so it becomes the same non-retryable ErrRenderException an
// invalid diagram raises under chrome. It is not exclusively that -- 0.7.0 also
// exits 1 for a missing or malformed configuration file -- which is why
// NewMermanEngine validates that file up front, leaving the diagram as the
// explanation this package can still produce, and why MermanExitError keeps the
// raw status reachable for callers that pass their own arguments.
func (m *MermanEngine) runErr(caller, runCtx context.Context, err error, stderr []byte) error {
	if ctxErr := runCtx.Err(); ctxErr != nil {
		return render.ClassifyErr(caller, m.ctx, ctxErr)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		sentinel := ErrMermanFailed
		if exit.ExitCode() == 1 {
			sentinel = ErrRenderException
		}
		return render.ClassifyErr(caller, m.ctx, fmt.Errorf("%w: %w", sentinel,
			&MermanExitError{Bin: m.bin, Code: exit.ExitCode(), Stderr: mermanDiagnostic(stderr)}))
	}
	return render.ClassifyErr(caller, m.ctx, fmt.Errorf("%w: %s: %w", ErrMermanFailed, m.bin, err))
}

// mermanWaitDelay bounds how long a killed render may keep this call waiting on
// its output pipes. See the use in run.
const mermanWaitDelay = 5 * time.Second

// maxDiagnosticBytes caps how much of merman's stderr is carried in an error.
// A diagram with hundreds of diagnostics would otherwise produce an error string
// too long to log.
const maxDiagnosticBytes = 4096

func mermanDiagnostic(stderr []byte) string {
	text := strings.TrimSpace(string(stderr))
	if text == "" {
		return "no diagnostics on stderr"
	}
	if len(text) > maxDiagnosticBytes {
		text = text[:maxDiagnosticBytes] + "... (truncated)"
	}
	return text
}

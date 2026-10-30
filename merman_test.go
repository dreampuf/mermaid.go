package mermaid_go

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// defaultFakeCapabilities mirrors the shape of `merman-cli capabilities --json`
// for a fully featured build.
const defaultFakeCapabilities = `{
  "schema_version": 1,
  "cli_contract_version": 1,
  "package": {"name": "merman-cli", "version": "0.7.0"},
  "compatibility": {"mermaid": "11.16.0", "mmdc": "11.16.0"},
  "commands": ["render", "batch", "capabilities"],
  "capabilities": [{"id": "svg"}, {"id": "png"}, {"id": "pdf"}]
}`

// fakeMerman is a stand-in for the merman binary: a script that answers the
// capability probe from a canned document and runs the given shell body for a
// render, recording what it was invoked with. It keeps the tests honest about
// the command line and the stdin/stdout contract without requiring a Rust
// toolchain to build the real thing; TestMermanEngine_Live covers a real binary
// when one is installed.
type fakeMerman struct {
	path      string
	argsFile  string
	stdinFile string
}

// legacyMerman, as the capability document, makes the fake behave like merman
// 0.7.0: no `capabilities` command at all, only --version.
const legacyMerman = ""

func newFakeMerman(t *testing.T, capabilities, render string) *fakeMerman {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake merman binary is a shell script")
	}
	dir := t.TempDir()
	fake := &fakeMerman{
		path:      filepath.Join(dir, "merman-cli"),
		argsFile:  filepath.Join(dir, "args"),
		stdinFile: filepath.Join(dir, "stdin"),
	}
	capabilityBranch := fmt.Sprintf("cat <<'MERMAN_CAPABILITIES'\n%s\nMERMAN_CAPABILITIES\nexit 0", capabilities)
	if capabilities == legacyMerman {
		capabilityBranch = "echo \"error: unrecognized subcommand 'capabilities'\" >&2\nexit 2"
	}
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
capabilities)
%s
;;
--version)
echo "merman-cli 0.7.0"
exit 0
;;
esac
printf '%%s\n' "$@" > %q
cat > %q
%s
`, capabilityBranch, fake.argsFile, fake.stdinFile, render)
	if err := os.WriteFile(fake.path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the fake merman binary: %v", err)
	}
	return fake
}

// engine builds an engine on the fake binary, failing the test if it will not
// start.
func (f *fakeMerman) engine(t *testing.T, opts ...MermanOption) *MermanEngine {
	t.Helper()
	opts = append([]MermanOption{WithMermanBinary(f.path)}, opts...)
	engine, err := NewMermanEngine(context.Background(), opts...)
	if err != nil {
		t.Fatalf("NewMermanEngine() error = %v", err)
	}
	t.Cleanup(engine.Cancel)
	return engine
}

func (f *fakeMerman) args(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.argsFile)
	if err != nil {
		t.Fatalf("reading the recorded arguments: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func (f *fakeMerman) stdin(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.stdinFile)
	if err != nil {
		t.Fatalf("reading the recorded stdin: %v", err)
	}
	return string(data)
}

// containsArgs reports whether want appears as a run of consecutive arguments,
// so a test asserts a flag and its value arrived together rather than merely
// both being somewhere on the command line.
func containsArgs(args []string, want ...string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		if slices.Equal(args[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

// writePNG renders a w x h image to a file, so the fake binary has something
// genuinely decodable to hand back.
func writePNG(t *testing.T, w, h int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "diagram.png")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating the png: %v", err)
	}
	defer func() { _ = file.Close() }()
	if err := png.Encode(file, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatalf("encoding the png: %v", err)
	}
	return path
}

// writeConfig writes a mermaid configuration file for WithMermanConfigFile,
// which NewMermanEngine now reads rather than trusting.
func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mermaid.json")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("writing the config file: %v", err)
	}
	return path
}

const fakeSVG = `<svg id="mermaid" width="100" height="50"><g/></svg>`

func TestNewMermanEngine(t *testing.T) {
	t.Run("ReportsAMissingBinary", func(t *testing.T) {
		_, err := NewMermanEngine(context.Background(), WithMermanBinary(filepath.Join(t.TempDir(), "absent")))
		if !errors.Is(err, ErrMermanUnavailable) {
			t.Fatalf("NewMermanEngine() error = %v, want ErrMermanUnavailable", err)
		}
	})

	t.Run("ReportsAFailingProbe", func(t *testing.T) {
		// The probe is the only thing standing between a broken install and a
		// baffling failure on the first render, so its diagnostics must survive.
		fake := newFakeMerman(t, "", "")
		script := "#!/bin/sh\necho 'not a merman binary' >&2\nexit 127\n"
		if err := os.WriteFile(fake.path, []byte(script), 0o755); err != nil {
			t.Fatalf("rewriting the fake binary: %v", err)
		}
		_, err := NewMermanEngine(context.Background(), WithMermanBinary(fake.path))
		if !errors.Is(err, ErrMermanUnavailable) {
			t.Fatalf("NewMermanEngine() error = %v, want ErrMermanUnavailable", err)
		}
		if !strings.Contains(err.Error(), "not a merman binary") {
			t.Errorf("NewMermanEngine() error = %q, want it to carry the binary's stderr", err)
		}
	})

	t.Run("RejectsABinaryThatCannotRenderSvg", func(t *testing.T) {
		fake := newFakeMerman(t, `{"package":{"version":"0.7.0"},"commands":["lint"],"capabilities":[{"id":"analysis"}]}`, "")
		_, err := NewMermanEngine(context.Background(), WithMermanBinary(fake.path))
		if !errors.Is(err, ErrMermanCapability) {
			t.Fatalf("NewMermanEngine() error = %v, want ErrMermanCapability", err)
		}
	})

	t.Run("ReportsWhatTheBinaryIs", func(t *testing.T) {
		fake := newFakeMerman(t, defaultFakeCapabilities, "")
		engine := fake.engine(t)
		if engine.Version() != "0.7.0" {
			t.Errorf("Version() = %q, want 0.7.0", engine.Version())
		}
		if engine.MermaidVersion() != "11.16.0" {
			t.Errorf("MermaidVersion() = %q, want 11.16.0", engine.MermaidVersion())
		}
		if engine.Binary() != fake.path {
			t.Errorf("Binary() = %q, want %q", engine.Binary(), fake.path)
		}
		if got := engine.Capabilities(); !slices.Equal(got, []string{"pdf", "png", "svg"}) {
			t.Errorf("Capabilities() = %v, want the probed document's ids, sorted", got)
		}
	})

	t.Run("AcceptsAReleaseWithoutTheCapabilitiesCommand", func(t *testing.T) {
		// merman 0.7.0, the current stable, has no `capabilities` command. It
		// accepts every argument this package builds, so refusing to start on it
		// -- or concluding from its silence that it cannot render PNG -- would
		// lock users out of the only release they can install.
		fake := newFakeMerman(t, legacyMerman, "cat "+fmt.Sprintf("%q", writePNG(t, 20, 10)))
		engine := fake.engine(t)

		if engine.Version() != "0.7.0" {
			t.Errorf("Version() = %q, want the version parsed from --version", engine.Version())
		}
		if engine.Capabilities() != nil {
			t.Errorf("Capabilities() = %v, want nil for a binary that reported none", engine.Capabilities())
		}
		if engine.MermaidVersion() != "" {
			t.Errorf("MermaidVersion() = %q, want empty when the binary does not report one", engine.MermaidVersion())
		}
		// PNG must be attempted rather than pre-empted by an absent catalogue.
		if _, _, err := engine.RenderAsPng("graph TD; A-->B;"); err != nil {
			t.Fatalf("RenderAsPng() error = %v", err)
		}
		if !containsArgs(fake.args(t), "--format", "png") {
			t.Errorf("arguments %v did not reach merman", fake.args(t))
		}
	})

	t.Run("RejectsAnUnusableConfigFile", func(t *testing.T) {
		// merman exits 1 for a bad config file, the same status an invalid diagram
		// uses, so without this check every render would fail as
		// ErrRenderException and blame the diagram for a configuration mistake.
		fake := newFakeMerman(t, defaultFakeCapabilities, "printf '<svg/>'")

		_, err := NewMermanEngine(context.Background(),
			WithMermanBinary(fake.path), WithMermanConfigFile(filepath.Join(t.TempDir(), "absent.json")))
		if !errors.Is(err, ErrMermanFailed) {
			t.Fatalf("NewMermanEngine() with a missing config error = %v, want ErrMermanFailed", err)
		}
		if errors.Is(err, ErrRenderException) {
			t.Errorf("NewMermanEngine() error = %v, should not blame a diagram", err)
		}

		_, err = NewMermanEngine(context.Background(),
			WithMermanBinary(fake.path), WithMermanConfigFile(writeConfig(t, "not json")))
		if !errors.Is(err, ErrMermanFailed) {
			t.Fatalf("NewMermanEngine() with malformed config error = %v, want ErrMermanFailed", err)
		}
	})

	t.Run("RejectsABinaryThatCannotAnswerItsOwnCapabilities", func(t *testing.T) {
		// Falling back to --version is for releases too old to know the command,
		// which say so with a usage error. A binary that knows it and fails it is
		// broken, and starting blind would hide that until the first render.
		fake := newFakeMerman(t, defaultFakeCapabilities, "printf '<svg/>'")
		script := "#!/bin/sh\n" +
			"case \"$1\" in\n" +
			"capabilities) echo 'capability catalogue unreadable' >&2; exit 3 ;;\n" +
			"--version) echo 'merman-cli 0.9.0'; exit 0 ;;\n" +
			"esac\nprintf '<svg/>'\n"
		if err := os.WriteFile(fake.path, []byte(script), 0o755); err != nil {
			t.Fatalf("rewriting the fake binary: %v", err)
		}

		_, err := NewMermanEngine(context.Background(), WithMermanBinary(fake.path))
		if !errors.Is(err, ErrMermanUnavailable) {
			t.Fatalf("NewMermanEngine() error = %v, want ErrMermanUnavailable", err)
		}
		if !strings.Contains(err.Error(), "capabilities") || !strings.Contains(err.Error(), "unreadable") {
			t.Errorf("NewMermanEngine() error = %q, want the capabilities failure, not the --version fallback", err)
		}
	})

	t.Run("ReportsAFailedFallback", func(t *testing.T) {
		// The other side of the legacy path: the binary rejects `capabilities`
		// like an old release would, and then cannot answer --version either. The
		// fallback's own failure is the diagnostic, since the rejection above it
		// says nothing about whether the binary works.
		fake := newFakeMerman(t, defaultFakeCapabilities, "")
		script := "#!/bin/sh\n" +
			"case \"$1\" in\n" +
			"capabilities) echo \"error: unrecognized subcommand 'capabilities'\" >&2; exit 2 ;;\n" +
			"esac\necho 'segmentation fault' >&2\nexit 139\n"
		if err := os.WriteFile(fake.path, []byte(script), 0o755); err != nil {
			t.Fatalf("rewriting the fake binary: %v", err)
		}
		_, err := NewMermanEngine(context.Background(), WithMermanBinary(fake.path))
		if !errors.Is(err, ErrMermanUnavailable) {
			t.Fatalf("NewMermanEngine() error = %v, want ErrMermanUnavailable", err)
		}
		if !strings.Contains(err.Error(), "--version") || !strings.Contains(err.Error(), "segmentation fault") {
			t.Errorf("NewMermanEngine() error = %q, want the --version failure and its diagnostics", err)
		}
	})
}

func TestMermanEngine_Render(t *testing.T) {
	content := "graph TD; A-->B;"

	t.Run("PipesTheDiagramThroughTheCLI", func(t *testing.T) {
		fake := newFakeMerman(t, defaultFakeCapabilities, "printf '%s' "+fmt.Sprintf("%q", fakeSVG))
		engine := fake.engine(t)

		svg, err := engine.Render(content)
		if err != nil {
			t.Fatalf("Render() error = %v", err)
		}
		if svg != fakeSVG {
			t.Errorf("Render() = %q, want the binary's stdout verbatim", svg)
		}
		if got := fake.stdin(t); got != content {
			t.Errorf("the binary read %q on stdin, want the diagram source", got)
		}

		args := fake.args(t)
		// The payload has to arrive on stdout and the source on stdin, or the
		// engine would be reading a file merman never wrote.
		for _, want := range [][]string{{"render", "-"}, {"--output", "-"}, {"--svg-id", DefaultMermanSvgID}} {
			if !containsArgs(args, want...) {
				t.Errorf("arguments %v are missing %v", args, want)
			}
		}
	})

	t.Run("PassesTheConfiguredOptions", func(t *testing.T) {
		config := writeConfig(t, `{"theme":"dark"}`)
		fake := newFakeMerman(t, defaultFakeCapabilities, "printf '<svg/>'")
		engine := fake.engine(t,
			WithMermanTheme("dark"),
			WithMermanConfigFile(config),
			WithMermanSvgID("diagram"),
			WithMermanArgs("--resource-profile", "constrained"),
		)
		if _, err := engine.Render(content); err != nil {
			t.Fatalf("Render() error = %v", err)
		}
		args := fake.args(t)
		for _, want := range [][]string{
			{"--theme", "dark"},
			{"--config-file", config},
			{"--svg-id", "diagram"},
			{"--resource-profile", "constrained"},
		} {
			if !containsArgs(args, want...) {
				t.Errorf("arguments %v are missing %v", args, want)
			}
		}
	})

	t.Run("BundlesTheSourceIntoTheSvg", func(t *testing.T) {
		fake := newFakeMerman(t, defaultFakeCapabilities, "printf '%s' "+fmt.Sprintf("%q", fakeSVG))
		engine := fake.engine(t)

		svg, err := engine.Render("graph TD; A-->B & C;", WithBundle())
		if err != nil {
			t.Fatalf("Render(WithBundle()) error = %v", err)
		}
		if !strings.Contains(svg, "<desc>graph TD; A--&gt;B &amp; C;</desc>") {
			t.Errorf("Render(WithBundle()) = %q, want the escaped source in a <desc>", svg)
		}
	})
}

func TestMermanEngine_RenderAsScaledPng(t *testing.T) {
	image := writePNG(t, 200, 100)

	t.Run("ReturnsTheImageAndItsBox", func(t *testing.T) {
		fake := newFakeMerman(t, defaultFakeCapabilities, "cat "+fmt.Sprintf("%q", image))
		engine := fake.engine(t)

		data, box, err := engine.RenderAsScaledPng("graph TD; A-->B;", 2.0)
		if err != nil {
			t.Fatalf("RenderAsScaledPng() error = %v", err)
		}
		if _, err := png.DecodeConfig(bytes.NewReader(data)); err != nil {
			t.Fatalf("RenderAsScaledPng() returned an undecodable image: %v", err)
		}
		// The box model is in CSS pixels, as chrome reports it, so a 2x image of
		// a 100x50 diagram still measures 100x50.
		if box.Width != 100 || box.Height != 50 {
			t.Errorf("RenderAsScaledPng() box = %dx%d, want 100x50", box.Width, box.Height)
		}
		if len(box.Content) != 8 || box.Content[4] != 100 || box.Content[5] != 50 {
			t.Errorf("RenderAsScaledPng() content quad = %v, want the box at the origin", box.Content)
		}
		if !containsArgs(fake.args(t), "--format", "png") || !containsArgs(fake.args(t), "--scale", "2") {
			t.Errorf("arguments %v do not ask for a 2x png", fake.args(t))
		}
	})

	t.Run("OmitsTheDefaultScale", func(t *testing.T) {
		fake := newFakeMerman(t, defaultFakeCapabilities, "cat "+fmt.Sprintf("%q", image))
		engine := fake.engine(t)
		if _, _, err := engine.RenderAsPng("graph TD; A-->B;"); err != nil {
			t.Fatalf("RenderAsPng() error = %v", err)
		}
		if containsArgs(fake.args(t), "--scale") {
			t.Errorf("arguments %v pass a scale merman already defaults to", fake.args(t))
		}
	})

	t.Run("RejectsWhatItCannotHonour", func(t *testing.T) {
		fake := newFakeMerman(t, defaultFakeCapabilities, "cat "+fmt.Sprintf("%q", image))
		engine := fake.engine(t)

		if _, _, err := engine.RenderAsPng("graph TD; A-->B;", WithBundle()); !errors.Is(err, ErrUnsupportedOption) {
			t.Errorf("RenderAsPng(WithBundle()) error = %v, want ErrUnsupportedOption", err)
		}
		// NaN and +Inf slip past a bare scale <= 0 check and would reach merman
		// as "NaN"/"+Inf", coming back as a rejected command line instead.
		for _, scale := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
			if _, _, err := engine.RenderAsScaledPng("graph TD; A-->B;", scale); !errors.Is(err, ErrUnsupportedOption) {
				t.Errorf("RenderAsScaledPng(%v) error = %v, want ErrUnsupportedOption", scale, err)
			}
		}
	})

	t.Run("ReportsABinaryWithoutPng", func(t *testing.T) {
		// PNG support is a compile-time feature of the CLI, so this is worth
		// saying plainly rather than letting merman reject the command line.
		fake := newFakeMerman(t, `{"package":{"version":"0.7.0"},"commands":["render"],"capabilities":[{"id":"svg"}]}`, "printf '<svg/>'")
		engine := fake.engine(t)
		_, _, err := engine.RenderAsPng("graph TD; A-->B;")
		if !errors.Is(err, ErrMermanCapability) {
			t.Errorf("RenderAsPng() error = %v, want ErrMermanCapability", err)
		}
	})

	t.Run("RejectsAnUndecodableImage", func(t *testing.T) {
		fake := newFakeMerman(t, defaultFakeCapabilities, "printf 'not a png'")
		engine := fake.engine(t)
		data, box, err := engine.RenderAsPng("graph TD; A-->B;")
		if !errors.Is(err, ErrMermanFailed) {
			t.Fatalf("RenderAsPng() error = %v, want ErrMermanFailed", err)
		}
		if data != nil || box != nil {
			t.Errorf("RenderAsPng() returned data=%d bytes, box=%v on failure, want neither", len(data), box)
		}
	})
}

func TestMermanEngine_ErrorClassification(t *testing.T) {
	t.Run("InvalidDiagramIsAnException", func(t *testing.T) {
		// merman exits 1 for invalid mermaid, which is the same non-retryable
		// rejection of the diagram source the chrome backend reports.
		fake := newFakeMerman(t, defaultFakeCapabilities, "echo 'error: unexpected token' >&2\nexit 1")
		engine := fake.engine(t)

		_, err := engine.Render("graph TD; A---;")
		if !errors.Is(err, ErrRenderException) {
			t.Fatalf("Render() error = %v, want ErrRenderException", err)
		}
		if !strings.Contains(err.Error(), "unexpected token") {
			t.Errorf("Render() error = %q, want merman's diagnostics", err)
		}
	})

	t.Run("ARejectedCommandLineIsNotADiagramError", func(t *testing.T) {
		fake := newFakeMerman(t, defaultFakeCapabilities, "echo 'unexpected argument' >&2\nexit 2")
		engine := fake.engine(t)

		_, err := engine.Render("graph TD; A-->B;")
		if !errors.Is(err, ErrMermanFailed) {
			t.Fatalf("Render() error = %v, want ErrMermanFailed", err)
		}
		if errors.Is(err, ErrRenderException) {
			t.Errorf("Render() error = %v, should not blame the diagram", err)
		}
	})

	t.Run("TheExitStatusStaysReachable", func(t *testing.T) {
		// Exit 1 is not exclusively an invalid diagram on every release, so a
		// caller that needs certainty must be able to read the status itself.
		fake := newFakeMerman(t, defaultFakeCapabilities, "echo 'configuration file does not exist' >&2\nexit 1")
		engine := fake.engine(t)

		_, err := engine.Render("graph TD; A-->B;")
		var exit *MermanExitError
		if !errors.As(err, &exit) {
			t.Fatalf("Render() error = %v, want a *MermanExitError to be reachable", err)
		}
		if exit.Code != 1 || exit.Bin != fake.path {
			t.Errorf("MermanExitError = %+v, want exit 1 from %s", exit, fake.path)
		}
		if !strings.Contains(exit.Stderr, "configuration file") {
			t.Errorf("MermanExitError.Stderr = %q, want merman's diagnostics", exit.Stderr)
		}
	})

	t.Run("SilentFailuresStillSayWhichBinaryFailed", func(t *testing.T) {
		fake := newFakeMerman(t, defaultFakeCapabilities, "exit 3")
		engine := fake.engine(t)

		_, err := engine.Render("graph TD; A-->B;")
		if !errors.Is(err, ErrMermanFailed) {
			t.Fatalf("Render() error = %v, want ErrMermanFailed", err)
		}
		if !strings.Contains(err.Error(), fake.path) {
			t.Errorf("Render() error = %q, want it to name the binary", err)
		}
	})
}

func TestMermanEngine_Timeout(t *testing.T) {
	// exec, not a plain sleep: the fake is a shell, and a descendant would keep
	// the output pipes open past the kill.
	sleeping := func(t *testing.T) *fakeMerman {
		return newFakeMerman(t, defaultFakeCapabilities, "exec sleep 30")
	}

	t.Run("EngineTimeout", func(t *testing.T) {
		engine := sleeping(t).engine(t)
		engine.SetRenderTimeout(200 * time.Millisecond)
		if _, err := engine.Render("graph TD; A-->B;"); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Render() error = %v, want context.DeadlineExceeded", err)
		}
	})

	t.Run("PerCallTimeout", func(t *testing.T) {
		engine := sleeping(t).engine(t)
		if _, err := engine.Render("graph TD; A-->B;", WithTimeout(200*time.Millisecond)); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Render(WithTimeout()) error = %v, want context.DeadlineExceeded", err)
		}
	})

	t.Run("CallerCancellationReportsItsCause", func(t *testing.T) {
		engine := sleeping(t).engine(t)
		reason := errors.New("caller went away")
		ctx, cancel := context.WithCancelCause(context.Background())
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel(reason)
		}()
		if _, err := engine.RenderContext(ctx, "graph TD; A-->B;"); !errors.Is(err, reason) {
			t.Errorf("RenderContext() error = %v, want it to report the cancellation cause", err)
		}
	})

	t.Run("ATimedOutRenderDoesNotSpoilTheEngine", func(t *testing.T) {
		fake := newFakeMerman(t, defaultFakeCapabilities, "printf '<svg/>'")
		engine := fake.engine(t)
		if _, err := engine.Render("graph TD; A-->B;", WithTimeout(time.Nanosecond)); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Render(WithTimeout()) error = %v, want context.DeadlineExceeded", err)
		}
		if _, err := engine.Render("graph TD; A-->B;"); err != nil {
			t.Errorf("Render() after a timeout error = %v", err)
		}
	})
}

func TestMermanEngine_Lifecycle(t *testing.T) {
	t.Run("CancelClosesTheEngine", func(t *testing.T) {
		fake := newFakeMerman(t, defaultFakeCapabilities, "printf '<svg/>'")
		engine := fake.engine(t)
		engine.Cancel()

		if _, err := engine.Render("graph TD; A-->B;"); !errors.Is(err, ErrEngineClosed) {
			t.Errorf("Render() after Cancel() error = %v, want ErrEngineClosed", err)
		}
		if _, _, err := engine.RenderAsPng("graph TD; A-->B;"); !errors.Is(err, ErrEngineClosed) {
			t.Errorf("RenderAsPng() after Cancel() error = %v, want ErrEngineClosed", err)
		}
	})

	t.Run("CancelAbortsARenderInFlight", func(t *testing.T) {
		fake := newFakeMerman(t, defaultFakeCapabilities, "exec sleep 30")
		engine := fake.engine(t)

		done := make(chan error, 1)
		go func() {
			_, err := engine.Render("graph TD; A-->B;")
			done <- err
		}()
		time.Sleep(200 * time.Millisecond)
		engine.Cancel()

		select {
		case err := <-done:
			if !errors.Is(err, ErrEngineClosed) {
				t.Errorf("Render() error = %v, want ErrEngineClosed", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Cancel() did not abort the render in flight")
		}
	})

	t.Run("WaitingForASlotIsBoundedByTheCaller", func(t *testing.T) {
		// The queueing wait is where a server sheds load, so it has to honour
		// the caller's context even though the render has not started.
		fake := newFakeMerman(t, defaultFakeCapabilities, "exec sleep 30")
		engine := fake.engine(t, WithMermanConcurrency(1))

		go func() { _, _ = engine.Render("graph TD; A-->B;") }()
		time.Sleep(200 * time.Millisecond)

		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		if _, err := engine.RenderContext(ctx, "graph TD; A-->B;"); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("RenderContext() error = %v, want context.DeadlineExceeded", err)
		}
		if waited := time.Since(start); waited > 5*time.Second {
			t.Errorf("RenderContext() waited %v for a slot, want it to give up with its context", waited)
		}
	})
}

func TestBundleSource(t *testing.T) {
	cases := []struct {
		name, svg, want string
		wantErr         bool
	}{
		{
			name: "PlainRoot",
			svg:  `<svg id="mermaid"><g/></svg>`,
			want: `<svg id="mermaid"><desc>a-&gt;b</desc><g/></svg>`,
		},
		{
			name: "AttributeContainingAngleBrackets",
			// A '>' inside an attribute value must not be mistaken for the end
			// of the start tag, or the <desc> lands inside the attribute.
			svg:  `<svg data-note="a > b"><g/></svg>`,
			want: `<svg data-note="a > b"><desc>a-&gt;b</desc><g/></svg>`,
		},
		{
			name: "XMLPrologue",
			svg:  `<?xml version="1.0"?><svg><g/></svg>`,
			want: `<?xml version="1.0"?><svg><desc>a-&gt;b</desc><g/></svg>`,
		},
		{
			name: "SelfClosingRoot",
			svg:  `<svg width="10"/>`,
			want: `<svg width="10"><desc>a-&gt;b</desc></svg>`,
		},
		{
			// XML allows space before the '/' but not between '/' and '>'
			// (EmptyElemTag ::= '<' Name (S Attribute)* S? '/>'), so looking at
			// the character before '>' is enough to recognise every valid form.
			name: "SelfClosingRootWithSpace",
			svg:  `<svg width="10" />`,
			want: `<svg width="10" ><desc>a-&gt;b</desc></svg>`,
		},
		{
			name: "SelfClosingRootAcrossLines",
			svg:  "<svg width=\"10\"\n/>",
			want: "<svg width=\"10\"\n><desc>a-&gt;b</desc></svg>",
		},
		{name: "NotAnSvg", svg: "kaboom", wantErr: true},
		{name: "UnterminatedStartTag", svg: `<svg id="mermaid"`, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bundleSource(tc.svg, "a->b")
			if tc.wantErr {
				if !errors.Is(err, ErrMermanFailed) {
					t.Fatalf("bundleSource() error = %v, want ErrMermanFailed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("bundleSource() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("bundleSource() = %q, want %q", got, tc.want)
			}
		})
	}
}

// liveMermanEngine builds an engine on a real merman binary, skipping when none
// is installed. The fake binary keeps this package honest about its own side of
// the contract; only a real binary proves the command line it builds is one
// merman accepts, which is why CI installs it.
func liveMermanEngine(t *testing.T, opts ...MermanOption) *MermanEngine {
	t.Helper()
	if _, err := lookMerman(""); err != nil {
		t.Skipf("no merman binary on PATH: %v", err)
	}
	engine, err := NewMermanEngine(context.Background(), opts...)
	if err != nil {
		t.Fatalf("NewMermanEngine() error = %v", err)
	}
	t.Cleanup(engine.Cancel)
	return engine
}

// liveDiagrams are the families the chrome backend is tested against, so the
// merman backends are held to the same diagrams rather than to a merman-shaped
// subset.
var liveDiagrams = map[string]string{
	"Flowchart": `graph TD;
    A-->B;
    A-->C;
    B-->D;
    C-->D;`,
	"Sequence": `sequenceDiagram
    participant Alice
    participant Bob
    Alice->>John: Hello John, how are you?
    loop Healthcheck
    John->>John: Fight against hypochondria
    end
    Note right of John: Rational thoughts <br/>prevail!
    John-->>Alice: Great!`,
	"Gantt": `gantt
dateFormat  YYYY-MM-DD
title Adding GANTT diagram to mermaid
section A section
Completed task            :done,    des1, 2014-01-06,2014-01-08
Active task               :active,  des2, 2014-01-09, 3d`,
	"Class": `classDiagram
Class01 <|-- AveryLongClass : Cool
Class03 *-- Class04
Class07 : equals()
Class01 : size()`,
	"GitGraph": `gitGraph
       commit
       branch develop
       commit
       checkout main
       merge develop`,
	"Entity": `erDiagram
    CUSTOMER ||--o{ ORDER : places
    ORDER ||--|{ LINE-ITEM : contains`,
	"Journey": `journey
    title My working day
    section Go to work
      Make tea: 5: Me
      Do work: 1: Me, Cat`,
	"State": `stateDiagram-v2
    [*] --> Still
    Still --> Moving
    Moving --> [*]`,
	"Pie": `pie title Pets
    "Dogs" : 386
    "Cats" : 85`,
	"QuotedLabels": `graph TD;
    A-->B['name'];
    A-->C["pic"];`,
	"MarkdownLabel": "graph TD;\n\tA-->B[\"`Hello World`\"];\n\tB-->C;",
}

func TestMermanEngine_LiveRender(t *testing.T) {
	engine := liveMermanEngine(t)
	t.Logf("merman %s (mermaid %q, capabilities %v) at %s",
		engine.Version(), engine.MermaidVersion(), engine.Capabilities(), engine.Binary())

	for name, content := range liveDiagrams {
		t.Run(name, func(t *testing.T) {
			svg, err := engine.Render(content)
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(strings.TrimSpace(svg), "</svg>") {
				t.Errorf("Render() = %.120q..., want a complete svg document", svg)
			}
		})
	}

	t.Run("RootIDMatchesTheChromeBackend", func(t *testing.T) {
		// merman's own default is id="merman"; the engine asks for mermaid.js's
		// id so a diagram styled against one backend works under the other.
		svg, err := engine.Render("graph TD; A-->B;")
		if err != nil {
			t.Fatalf("Render() error = %v", err)
		}
		if !strings.Contains(svg, `id="`+DefaultMermanSvgID+`"`) {
			t.Errorf("Render() = %.120q..., want the root svg to carry id=%q", svg, DefaultMermanSvgID)
		}
	})

	t.Run("BundleDiagram", func(t *testing.T) {
		svg, err := engine.Render("graph TD; A-->B;", WithBundle())
		if err != nil {
			t.Fatalf("Render(WithBundle()) error = %v", err)
		}
		// The same assertion the chrome backend's BundleDiagram subtest makes.
		if !strings.Contains(svg, "<desc>graph TD; A--&gt;B;</desc>") {
			t.Errorf("Render(WithBundle()) = %.200q..., want the escaped source in a <desc>", svg)
		}
	})

	t.Run("InvalidSyntax", func(t *testing.T) {
		// merman exits 1 for a parse error, which must arrive as the same
		// non-retryable error the chrome backend reports for a bad diagram.
		_, err := engine.Render("graph TD; A---;")
		if !errors.Is(err, ErrRenderException) {
			t.Fatalf("Render() error = %v, want ErrRenderException", err)
		}
		if errors.Is(err, ErrMermanFailed) {
			t.Errorf("Render() error = %v, should blame the diagram rather than the binary", err)
		}
		if !strings.Contains(err.Error(), "parse error") {
			t.Errorf("Render() error = %v, want merman's own diagnostic", err)
		}
	})

	t.Run("Options", func(t *testing.T) {
		themed := liveMermanEngine(t, WithMermanTheme("dark"), WithMermanSvgID("diagram"))
		svg, err := themed.Render("graph TD; A-->B;")
		if err != nil {
			t.Fatalf("Render() error = %v", err)
		}
		if !strings.Contains(svg, `id="diagram"`) {
			t.Errorf("Render() = %.120q..., want WithMermanSvgID honoured", svg)
		}
	})

	t.Run("OperationalFailureStaysInspectable", func(t *testing.T) {
		// WithMermanArgs can reach a failure the diagram is not to blame for -- a
		// missing configuration file is one, which is why NewMermanEngine
		// validates the file it owns. Which status merman picks for it is its own
		// business and has changed across releases: 0.7.0 exits 1, the same as an
		// invalid diagram, while 0.8 exits 2. That is precisely why the status has
		// to stay readable rather than be inferred, so assert it is there and
		// explains itself, not what it is.
		rogue := liveMermanEngine(t, WithMermanArgs("--config-file", filepath.Join(t.TempDir(), "absent.json")))
		_, err := rogue.Render("graph TD; A-->B;")
		var exit *MermanExitError
		if !errors.As(err, &exit) {
			t.Fatalf("Render() error = %v, want a *MermanExitError to be reachable", err)
		}
		if exit.Code == 0 || exit.Stderr == "" {
			t.Errorf("MermanExitError = %+v, want a non-zero status with merman's diagnostics", exit)
		}
	})

	t.Run("ConcurrentRenders", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := range 5 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				svg, err := engine.Render("graph TD; A-->B;")
				if err != nil {
					t.Errorf("concurrent Render() %d error = %v", i, err)
					return
				}
				if !strings.HasPrefix(svg, "<svg") {
					t.Errorf("concurrent Render() %d returned an invalid svg", i)
				}
			}()
		}
		wg.Wait()
	})

	t.Run("ATimedOutRenderDoesNotSpoilTheEngine", func(t *testing.T) {
		if _, err := engine.Render("graph TD; A-->B;", WithTimeout(time.Nanosecond)); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Render(WithTimeout()) error = %v, want context.DeadlineExceeded", err)
		}
		if _, err := engine.Render("graph TD; A-->B;"); err != nil {
			t.Errorf("Render() after a timeout error = %v", err)
		}
	})
}

func TestMermanEngine_LivePng(t *testing.T) {
	engine := liveMermanEngine(t)
	if caps := engine.Capabilities(); caps != nil && !slices.Contains(caps, "png") {
		t.Skip("the installed merman was built without png output")
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

	t.Run("ScaleGrowsTheImageNotTheBox", func(t *testing.T) {
		// The box model is in CSS pixels, as chrome reports it, so it must not
		// move with the scale even though the image does.
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
	})

	t.Run("InvalidSyntaxPng", func(t *testing.T) {
		data, box, err := engine.RenderAsPng("graph TD; A---;")
		if !errors.Is(err, ErrRenderException) {
			t.Fatalf("RenderAsPng() error = %v, want ErrRenderException", err)
		}
		if data != nil || box != nil {
			t.Errorf("RenderAsPng() returned data=%d bytes, box=%v on failure, want neither", len(data), box)
		}
	})
}

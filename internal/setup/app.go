package setup

import (
	"bytes"
	_ "embed"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

// Why the daemon runs as an app bundle instead of a bare binary:
//
// Since macOS 15, "Local Network" privacy applies to LAN destinations. A
// command-line tool started from Terminal borrows Terminal's permission, but
// a bare binary started by launchd has no identity macOS can attach a
// permission to, so its connections to e.g. 192.168.x.x fail with
// "no route to host" and no dialog ever appears. Reminders access (TCC)
// likewise needs a bundle with usage descriptions to be requested reliably.
// Wrapping the binary in a minimal .app and launching it through
// LaunchServices (`open -a`) gives it an identity — and the system a place
// to show the permission prompts.

//go:embed app_info.plist.tmpl
var appInfoPlistTemplateStr string

// AppName is the display name of the daemon's app bundle (ReminderRelay.app).
const AppName = "ReminderRelay"

// Version is stamped into the bundle's Info.plist. main sets it from the
// version injected at build time.
var Version = "dev"

type appInfoData struct {
	BundleID   string
	Name       string
	Executable string
	Version    string
}

// AppBundlePath returns the daemon's app bundle location. It lives under the
// user's own ~/Applications so installing it needs no sudo.
func AppBundlePath(homeDir string) string {
	return filepath.Join(homeDir, "Applications", AppName+".app")
}

// AppExecutablePath returns the executable inside the app bundle.
func AppExecutablePath(homeDir string) string {
	return filepath.Join(AppBundlePath(homeDir), "Contents", "MacOS", BinaryName)
}

// bundleVersion turns Version into the dotted-numeric form Info.plist expects
// ("v1.2.3-rc1" → "1.2.3"; anything unparseable, such as "dev" → "0.0.0").
func bundleVersion() string {
	v := strings.TrimPrefix(Version, "v")
	v, _, _ = strings.Cut(v, "-")
	if regexp.MustCompile(`^\d+(\.\d+){0,2}$`).MatchString(v) {
		return v
	}
	return "0.0.0"
}

// codesignBundle ad-hoc signs the bundle and verifies the result. It is a
// variable so tests can stub it out. The signature seals Info.plist into the
// code identity — that is what lets macOS tie a permission grant to this app.
var codesignBundle = func(bundlePath string) error {
	if out, err := exec.Command("codesign", "--force", "--sign", "-", bundlePath).CombinedOutput(); err != nil {
		return fmt.Errorf("codesign %s: %s: %w", bundlePath, strings.TrimSpace(string(out)), err)
	}
	if out, err := exec.Command("codesign", "--verify", "--strict", bundlePath).CombinedOutput(); err != nil {
		return fmt.Errorf("verifying signature of %s: %s: %w", bundlePath, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// InstallAppBundle (re)creates ~/Applications/ReminderRelay.app from the
// currently running binary.
func InstallAppBundle(homeDir string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving current executable path: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return fmt.Errorf("resolving executable symlinks: %w", err)
	}
	return installAppBundleFrom(homeDir, self)
}

func installAppBundleFrom(homeDir, srcExe string) error {
	bundle := AppBundlePath(homeDir)

	// The bundle is rebuilt from scratch below; if the source binary lives
	// inside it, that would delete the very file being copied.
	if rel, err := filepath.Rel(bundle, srcExe); err == nil && !strings.HasPrefix(rel, "..") {
		return fmt.Errorf("refusing to rebuild %s from a binary inside it (%s) — run the new build from outside the bundle", bundle, srcExe)
	}

	// Start clean so files from an older layout can't linger inside the
	// signed bundle.
	if err := os.RemoveAll(bundle); err != nil {
		return fmt.Errorf("removing old app bundle %s: %w", bundle, err)
	}
	macOSDir := filepath.Dir(AppExecutablePath(homeDir))
	if err := os.MkdirAll(macOSDir, 0o755); err != nil {
		return fmt.Errorf("creating app bundle directories: %w", err)
	}

	tmpl, err := template.New("info").Parse(appInfoPlistTemplateStr)
	if err != nil {
		return fmt.Errorf("parsing Info.plist template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, appInfoData{
		BundleID:   PlistLabel,
		Name:       AppName,
		Executable: BinaryName,
		Version:    bundleVersion(),
	}); err != nil {
		return fmt.Errorf("executing Info.plist template: %w", err)
	}
	infoPath := filepath.Join(bundle, "Contents", "Info.plist")
	if err := os.WriteFile(infoPath, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", infoPath, err)
	}

	if err := copyFile(srcExe, AppExecutablePath(homeDir), 0o755); err != nil {
		return fmt.Errorf("copying binary into app bundle: %w", err)
	}

	return codesignBundle(bundle)
}

// RemoveAppBundle deletes the app bundle, if present.
func RemoveAppBundle(homeDir string) error {
	bundle := AppBundlePath(homeDir)
	if err := os.RemoveAll(bundle); err != nil {
		return fmt.Errorf("removing app bundle %s: %w", bundle, err)
	}
	return nil
}

// StopApp asks a running daemon app to quit. The launchd job's child is
// `open`, which does not forward signals to the app it launched — so
// unloading the job alone would leave the app (and its old code) running.
func StopApp(homeDir string) {
	pattern := regexp.QuoteMeta(AppExecutablePath(homeDir))
	// pkill exits 1 when nothing matched, which is fine: nothing to stop.
	_ = exec.Command("pkill", "-TERM", "-f", pattern).Run()
}

// escapeXML escapes s for use as plist <string> content.
func escapeXML(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s)) // writing to a bytes.Buffer cannot fail
	return b.String()
}

// PermissionNote tells the user about the one-time macOS prompts.
func PermissionNote(w io.Writer) {
	_, _ = fmt.Fprintf(w, "\n  On first start macOS asks for \"Local Network\" and \"Reminders\" access for\n")
	_, _ = fmt.Fprintf(w, "  %s — click Allow in the Mac's desktop session (on a VM: its console\n", AppName)
	_, _ = fmt.Fprintf(w, "  window; a terminal or SSH session can't answer them). Rebuilding the app\n")
	_, _ = fmt.Fprintf(w, "  changes its signature, so the prompts can reappear after an update.\n")
}

// InstallDaemon installs everything the background daemon needs — the CLI
// binary, the app bundle, the launchd plist — and (re)loads the job. It is the
// single implementation behind both the setup wizard and `install-daemon`.
func InstallDaemon(w io.Writer, homeDir string) error {
	// Stop any running instance first, so nothing being replaced below is in
	// use and launchd doesn't respawn it halfway through.
	if err := UnloadDaemon(homeDir); err != nil {
		_, _ = fmt.Fprintf(w, "  ⚠ stopping existing daemon: %v\n", err)
	}

	_, _ = fmt.Fprintf(w, "  Installing binary to %s...\n", BinaryInstallPath())
	if err := InstallBinary(); err != nil {
		return fmt.Errorf("installing binary: %w", err)
	}
	_, _ = fmt.Fprintf(w, "  ✓ Binary installed\n")

	_, _ = fmt.Fprintf(w, "  Building app bundle %s...\n", AppBundlePath(homeDir))
	if err := InstallAppBundle(homeDir); err != nil {
		return fmt.Errorf("installing app bundle: %w", err)
	}
	_, _ = fmt.Fprintf(w, "  ✓ App bundle installed and signed\n")

	if err := CreateLogDir(homeDir); err != nil {
		return fmt.Errorf("creating log directory: %w", err)
	}
	_, _ = fmt.Fprintf(w, "  ✓ Log directory created\n")

	if err := WritePlist(homeDir); err != nil {
		return fmt.Errorf("writing plist: %w", err)
	}
	_, _ = fmt.Fprintf(w, "  ✓ LaunchAgent plist written\n")

	if err := LoadDaemon(homeDir); err != nil {
		return fmt.Errorf("loading daemon: %w", err)
	}
	_, _ = fmt.Fprintf(w, "  ✓ Daemon loaded — running now\n")
	return nil
}

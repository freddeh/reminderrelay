package setup

import (
	"bytes"
	"encoding/xml"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubCodesign replaces codesignBundle for the duration of the test and
// returns a pointer to the list of bundle paths it was asked to sign.
func stubCodesign(t *testing.T) *[]string {
	t.Helper()
	var signed []string
	orig := codesignBundle
	codesignBundle = func(bundlePath string) error {
		signed = append(signed, bundlePath)
		return nil
	}
	t.Cleanup(func() { codesignBundle = orig })
	return &signed
}

func writeFakeBinary(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reminderrelay")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("writing fake binary: %v", err)
	}
	return path
}

func TestInstallAppBundle_LayoutAndInfoPlist(t *testing.T) {
	home := t.TempDir()
	signed := stubCodesign(t)
	src := writeFakeBinary(t, "fake-binary")

	if err := installAppBundleFrom(home, src); err != nil {
		t.Fatalf("installAppBundleFrom: %v", err)
	}

	exe := AppExecutablePath(home)
	data, err := os.ReadFile(exe)
	if err != nil || string(data) != "fake-binary" {
		t.Fatalf("executable in bundle = %q, %v; want the copied binary", data, err)
	}
	if info, _ := os.Stat(exe); info.Mode()&0o111 == 0 {
		t.Errorf("bundle executable mode = %v, want executable", info.Mode())
	}

	infoBytes, err := os.ReadFile(filepath.Join(AppBundlePath(home), "Contents", "Info.plist"))
	if err != nil {
		t.Fatalf("reading Info.plist: %v", err)
	}
	info := string(infoBytes)
	for _, want := range []string{
		"<string>" + PlistLabel + "</string>", // CFBundleIdentifier
		"<key>CFBundleExecutable</key>\n    <string>" + BinaryName + "</string>",
		"<key>NSLocalNetworkUsageDescription</key>",
		"<key>NSRemindersUsageDescription</key>",
		"<key>NSRemindersFullAccessUsageDescription</key>",
		"<key>LSUIElement</key>",
	} {
		if !strings.Contains(info, want) {
			t.Errorf("Info.plist is missing %q", want)
		}
	}

	if len(*signed) != 1 || (*signed)[0] != AppBundlePath(home) {
		t.Errorf("codesign called for %v, want exactly the bundle %s", *signed, AppBundlePath(home))
	}
}

func TestInstallAppBundle_ReinstallDropsStaleFiles(t *testing.T) {
	home := t.TempDir()
	stubCodesign(t)

	if err := installAppBundleFrom(home, writeFakeBinary(t, "v1")); err != nil {
		t.Fatalf("first install: %v", err)
	}
	stale := filepath.Join(AppBundlePath(home), "Contents", "Resources", "old.txt")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := installAppBundleFrom(home, writeFakeBinary(t, "v2")); err != nil {
		t.Fatalf("reinstall: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a file from the previous bundle survived the reinstall — it would sit unsealed inside the new signature")
	}
	if data, _ := os.ReadFile(AppExecutablePath(home)); string(data) != "v2" {
		t.Errorf("executable = %q, want the new binary", data)
	}
}

func TestInstallAppBundle_RefusesSourceInsideBundle(t *testing.T) {
	home := t.TempDir()
	stubCodesign(t)
	if err := installAppBundleFrom(home, writeFakeBinary(t, "v1")); err != nil {
		t.Fatalf("first install: %v", err)
	}

	// Rebuilding from the bundle's own executable would delete the source
	// before copying it.
	err := installAppBundleFrom(home, AppExecutablePath(home))
	if err == nil {
		t.Fatal("expected an error when the source binary is inside the bundle being rebuilt")
	}
	if _, statErr := os.Stat(AppExecutablePath(home)); statErr != nil {
		t.Errorf("the existing bundle must be left intact on refusal: %v", statErr)
	}
}

func TestRemoveAppBundle(t *testing.T) {
	home := t.TempDir()
	stubCodesign(t)
	if err := installAppBundleFrom(home, writeFakeBinary(t, "v1")); err != nil {
		t.Fatal(err)
	}

	if err := RemoveAppBundle(home); err != nil {
		t.Fatalf("RemoveAppBundle: %v", err)
	}
	if _, err := os.Stat(AppBundlePath(home)); !os.IsNotExist(err) {
		t.Error("bundle still exists after RemoveAppBundle")
	}
	if err := RemoveAppBundle(home); err != nil {
		t.Errorf("removing an already-absent bundle should not fail: %v", err)
	}
}

// The real thing: an ad-hoc signed bundle that macOS' own tools accept. Only
// meaningful on macOS, where codesign and plutil exist.
func TestInstallAppBundle_RealCodesignAndPlutil(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	for _, tool := range []string{"codesign", "plutil"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}

	home := t.TempDir()
	// codesign needs a real Mach-O to sign; the test binary itself is one.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := installAppBundleFrom(home, self); err != nil {
		t.Fatalf("installAppBundleFrom with real codesign: %v", err)
	}

	bundle := AppBundlePath(home)
	if out, err := exec.Command("codesign", "--verify", "--strict", bundle).CombinedOutput(); err != nil {
		t.Errorf("codesign --verify failed: %s: %v", out, err)
	}
	if out, err := exec.Command("plutil", "-lint", filepath.Join(bundle, "Contents", "Info.plist")).CombinedOutput(); err != nil {
		t.Errorf("Info.plist is not a valid plist: %s: %v", out, err)
	}
}

func TestRenderPlist_LaunchesAppViaOpen(t *testing.T) {
	home := "/Users/someone"
	out, err := RenderPlist(home)
	if err != nil {
		t.Fatalf("RenderPlist: %v", err)
	}
	plist := string(out)

	for _, want := range []string{
		"<string>/usr/bin/open</string>",
		"<string>-W</string>", // keep `open` alive as long as the app runs, so KeepAlive works
		"<string>-a</string>",
		"<string>" + AppBundlePath(home) + "</string>",
		"<string>--stdout</string>",
		"<string>" + home + "/Library/Logs/reminderrelay/output.log</string>",
		"<string>--stderr</string>",
		"<string>" + home + "/Library/Logs/reminderrelay/errors.log</string>",
		"<string>--args</string>",
		"<string>daemon</string>",
		"<key>KeepAlive</key>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist is missing %q", want)
		}
	}
	// The bare binary must not be what launchd execs — that is precisely what
	// macOS refuses Local Network access to.
	if strings.Contains(plist, BinaryInstallPath()) {
		t.Errorf("plist still references the bare binary %s", BinaryInstallPath())
	}
}

func TestRenderPlist_EscapesXMLInPaths(t *testing.T) {
	out, err := RenderPlist("/Users/a&b")
	if err != nil {
		t.Fatalf("RenderPlist: %v", err)
	}
	if !strings.Contains(string(out), "a&amp;b") {
		t.Errorf("a home directory containing & must be XML-escaped, got:\n%s", out)
	}

	dec := xml.NewDecoder(bytes.NewReader(out))
	for {
		if _, err := dec.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("rendered plist is not well-formed XML: %v", err)
		}
	}
}

func TestRenderPlist_PassesPlutilLint(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("plutil not available")
	}
	out, err := RenderPlist(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "test.plist")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	if lint, err := exec.Command("plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Errorf("plutil rejects the rendered launchd plist: %s: %v", lint, err)
	}
}

func TestBundleVersion(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })

	for in, want := range map[string]string{
		"1.2.3":      "1.2.3",
		"v1.2.3":     "1.2.3",
		"v1.2.3-rc1": "1.2.3",
		"2":          "2",
		"dev":        "0.0.0",
		"":           "0.0.0",
		"1.2.3.4.5":  "0.0.0",
	} {
		Version = in
		if got := bundleVersion(); got != want {
			t.Errorf("bundleVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

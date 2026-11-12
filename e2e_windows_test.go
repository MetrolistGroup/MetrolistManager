//go:build windows

package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MetrolistGroup/MetrolistManager/internal/githubrelease"
	"github.com/MetrolistGroup/MetrolistManager/internal/installer"
	"github.com/MetrolistGroup/MetrolistManager/internal/operation"
	"github.com/MetrolistGroup/MetrolistManager/internal/winplatform"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// TestEndToEndInstallUpdateAndUninstall installs into the real per-user registry and Start Menu, so it only runs
// when METROLIST_MANAGER_E2E_BINARY names a built manager. METROLIST_MANAGER_E2E_PAYLOAD and
// METROLIST_MANAGER_E2E_VERSION optionally replace the synthetic payload with a real portable ZIP.
func TestEndToEndInstallUpdateAndUninstall(t *testing.T) {
	binary := os.Getenv("METROLIST_MANAGER_E2E_BINARY")
	if binary == "" {
		t.Skip("set METROLIST_MANAGER_E2E_BINARY to run against the real registry and Start Menu")
	}
	if _, _, err := winplatform.LoadInstallation(); !errors.Is(err, winplatform.ErrNotInstalled) {
		t.Fatalf("this machine already has a Metrolist Desktop registration: %v", err)
	}
	type step struct{ payload, version string }
	steps := []step{
		{writeE2EPayload(t, "14.0.0"), "14.0.0"},
		{writeE2EPayload(t, "14.0.1"), "14.0.1"},
	}
	if payload := os.Getenv("METROLIST_MANAGER_E2E_PAYLOAD"); payload != "" {
		version := os.Getenv("METROLIST_MANAGER_E2E_VERSION")
		// Installing the same release twice covers the repair path over an existing installation.
		steps = []step{{payload, version}, {payload, version}}
	}
	root := filepath.Join(t.TempDir(), "Metrolist")
	t.Cleanup(func() { _ = winplatform.Unregister() })

	for _, step := range steps {
		installE2EPayload(t, binary, root, step.payload, step.version)
		assertDesktopUpdaterContract(t, root, step.version)
	}

	// Apps & Features runs the registered QuietUninstallString.
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Uninstall\Metrolist`, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("open uninstall registration: %v", err)
	}
	quiet, _, err := key.GetStringValue("QuietUninstallString")
	key.Close()
	if err != nil {
		t.Fatalf("read QuietUninstallString: %v", err)
	}
	manager := filepath.Join(root, installer.ManagerDirectoryName, installer.ManagerExecutable)
	if want := `"` + manager + `" uninstall --silent`; quiet != want {
		t.Fatalf("QuietUninstallString = %q, want %q", quiet, want)
	}
	if output, err := exec.Command(manager, "uninstall", "--silent").CombinedOutput(); err != nil {
		t.Fatalf("uninstall: %v: %s", err, output)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		_, statErr := os.Stat(root)
		_, _, loadErr := winplatform.LoadInstallation()
		if errors.Is(statErr, os.ErrNotExist) && errors.Is(loadErr, winplatform.ErrNotInstalled) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("uninstall did not finish: stat=%v registration=%v", statErr, loadErr)
		}
		time.Sleep(250 * time.Millisecond)
	}
	programs, err := windows.KnownFolderPath(windows.FOLDERID_Programs, windows.KF_FLAG_DEFAULT)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(programs, "Metrolist.lnk")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Start Menu shortcut remains after uninstall: %v", err)
	}
}

func installE2EPayload(t *testing.T, binary, root, payload, version string) {
	t.Helper()
	content, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(content) }))
	defer server.Close()
	client := githubrelease.NewClient("MetrolistManager/e2e")
	client.HTTPClient = server.Client()
	asset := githubrelease.Asset{
		Name:   "Metrolist-Desktop-windows.zip",
		URL:    server.URL + "/Metrolist-Desktop-windows.zip",
		Digest: "sha256:" + hex.EncodeToString(digest[:]),
		Size:   int64(len(content)),
	}
	desktopShortcut := false
	err = operation.Run(context.Background(), operation.Request{
		Version:         version,
		InstallRoot:     root,
		ManagerSource:   binary,
		DesktopShortcut: &desktopShortcut,
	}, operation.Dependencies{
		ResolveAsset: func(context.Context) (githubrelease.Asset, error) { return asset, nil },
		Downloader:   client,
		Registrar:    winplatform.Registrar{},
	}, nil)
	if err != nil {
		t.Fatalf("install %s: %v", version, err)
	}
}

// assertDesktopUpdaterContract checks what Metrolist Desktop's DesktopUpdateManager reads before it offers an in-app update.
func assertDesktopUpdaterContract(t *testing.T, root, version string) {
	t.Helper()
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Metrolist\Desktop`, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("open app registration: %v", err)
	}
	defer key.Close()
	location, _, err := key.GetStringValue("InstallLocation")
	if err != nil || !strings.EqualFold(filepath.Clean(location), filepath.Clean(root)) {
		t.Fatalf("InstallLocation = %q (%v), want %q", location, err, root)
	}
	registered, _, err := key.GetStringValue("Version")
	if err != nil || registered != version {
		t.Fatalf("Version = %q (%v), want %q", registered, err, version)
	}
	data, err := os.ReadFile(filepath.Join(root, installer.ManagerDirectoryName, "install.json"))
	if err != nil {
		t.Fatal(err)
	}
	var marker struct {
		Schema  int    `json:"schema"`
		AppID   string `json:"appId"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &marker); err != nil {
		t.Fatal(err)
	}
	if marker.Schema != 1 || marker.AppID != "com.metrolist.desktop" || marker.Version != version {
		t.Fatalf("install marker = %+v", marker)
	}
	for _, file := range []string{
		filepath.Join(root, installer.AppDirectoryName, "Metrolist.exe"),
		filepath.Join(root, installer.ManagerDirectoryName, installer.ManagerExecutable),
	} {
		if info, err := os.Stat(file); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("%s is missing: %v", file, err)
		}
	}
}

func writeE2EPayload(t *testing.T, version string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Metrolist-Desktop-windows-"+version+".zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for name, content := range map[string]string{
		"install-manifest.json":  fmt.Sprintf(`{"schema":1,"appId":"com.metrolist.desktop","version":%q,"platform":"windows","architecture":"x86_64","launcher":"Metrolist.exe"}`, version),
		"Metrolist.exe":          "launcher",
		"app/Metrolist.cfg":      "[Application]",
		"runtime/bin/server.dll": version,
	} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

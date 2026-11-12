# Metrolist Manager

Metrolist Manager for windows 10/11, used to install, update, repair, and uninstall Metrolist Desktop. Written in Go.

## Installation layout

```text
%LOCALAPPDATA%\Programs\Metrolist\
  app\
    Metrolist.exe
    install-manifest.json
    ...
  manager\
    MetrolistManager.exe
    install.json
```

> [!NOTE]
> The manager owns `app` and replaces that directory during repair or update. It retains `manager`, registers the installation under HKCU, creates a Start Menu shortcut, and optionally creates a desktop shortcut. Downloads and ZIP metadata are fully validated before `app` is removed. Replacement is intentionally direct: an interruption after removal is recovered by rerunning the manager rather than by automatic rollback.

## Commands

```text
MetrolistManager.exe install
MetrolistManager.exe update --pid <desktop-pid>
MetrolistManager.exe repair
MetrolistManager.exe uninstall
```

Every command supports `--silent`. Run `go test ./...`. Validate a release archive, including every entry's expanded size and CRC, with:

```bash
go run ./cmd/validate-payload /path/to/Metrolist-Desktop-windows.zip 14.0.0
```

An equivalent cross-build from a Metrolist checkout can be done with the following commands:

```bash
go run github.com/akavel/rsrc@v0.10.2 \
  -arch amd64 \
  -manifest MetrolistManager.exe.manifest \
  -ico ../Metrolist/desktopApp/src/jvmMain/resources/logo.ico \
  -o rsrc_windows_amd64.syso
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags "-s -w -H=windowsgui" -o MetrolistManager.exe .
rm rsrc_windows_amd64.syso
```

> [!WARNING]
> This configuration is intentionally unbound and cannot install a release. Production binaries are bound to a stable GitHub release by the Metrolist release workflow, which also embeds the Windows application manifest and icon.

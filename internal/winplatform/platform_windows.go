//go:build windows

package winplatform

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"github.com/MetrolistGroup/MetrolistManager/internal/buildconfig"
	"github.com/MetrolistGroup/MetrolistManager/internal/installer"
	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	appRegistryPath       = `Software\Metrolist\Desktop`
	uninstallRegistryPath = `Software\Microsoft\Windows\CurrentVersion\Uninstall\Metrolist`
)

var ErrNotInstalled = errors.New("Metrolist Desktop is not installed")

type Registrar struct{}

func AcquireManagerLock() (func(), error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return nil, fmt.Errorf("open current user token: %w", err)
	}
	user, err := token.GetTokenUser()
	token.Close()
	if err != nil {
		return nil, fmt.Errorf("read current user SID: %w", err)
	}
	name, err := windows.UTF16PtrFromString(`Global\MetrolistManager-` + user.User.Sid.String())
	if err != nil {
		return nil, err
	}
	handle, createErr := windows.CreateMutex(nil, false, name)
	if errors.Is(createErr, windows.ERROR_ALREADY_EXISTS) {
		windows.CloseHandle(handle)
		return nil, errors.New("another Metrolist Manager operation is already running")
	}
	if createErr != nil {
		if handle != 0 {
			windows.CloseHandle(handle)
		}
		return nil, createErr
	}
	return func() { windows.CloseHandle(handle) }, nil
}

func DefaultInstallRoot() (string, error) {
	localAppData, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return "", err
	}
	return filepath.Join(localAppData, "Programs", "Metrolist"), nil
}

func Launch(path string) error {
	command := exec.Command(path)
	command.Dir = filepath.Dir(path)
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func LoadInstallation() (root, version string, err error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, appRegistryPath, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", "", ErrNotInstalled
	}
	if err != nil {
		return "", "", fmt.Errorf("open Metrolist registry key: %w", err)
	}
	root, _, err = key.GetStringValue("InstallLocation")
	if err != nil {
		key.Close()
		return "", "", fmt.Errorf("read installation location: %w", err)
	}
	if err := key.Close(); err != nil {
		return "", "", fmt.Errorf("close Metrolist registry key: %w", err)
	}
	if !filepath.IsAbs(root) {
		return "", "", errors.New("registered installation location is not an absolute path")
	}
	root = filepath.Clean(root)
	marker, err := installer.ReadMarker(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			entries, readErr := os.ReadDir(root)
			if errors.Is(readErr, os.ErrNotExist) || (readErr == nil && len(entries) == 0) {
				if cleanupErr := Unregister(); cleanupErr != nil {
					return "", "", fmt.Errorf("clean stale Metrolist registration: %w", cleanupErr)
				}
				return "", "", ErrNotInstalled
			}
			if readErr != nil {
				return "", "", fmt.Errorf("inspect registered installation: %w", readErr)
			}
		}
		return "", "", fmt.Errorf("validate installation marker: %w", err)
	}
	return root, marker.Version, nil
}

func (Registrar) Register(root, version string, desktopShortcut bool) error {
	launcher := filepath.Join(root, installer.AppDirectoryName, "Metrolist.exe")
	manager := filepath.Join(root, installer.ManagerDirectoryName, installer.ManagerExecutable)

	// Keep the installation discoverable if a later shortcut or uninstall-key
	// operation fails, so the installed manager can retry registration.
	appKey, appKeyExisted, err := registry.CreateKey(registry.CURRENT_USER, appRegistryPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("create application registry key: %w", err)
	}
	if err := appKey.SetStringValue("InstallLocation", root); err != nil {
		appKey.Close()
		if !appKeyExisted {
			_ = registry.DeleteKey(registry.CURRENT_USER, appRegistryPath)
		}
		return fmt.Errorf("set registry value InstallLocation: %w", err)
	}
	if err := appKey.SetStringValue("Version", version); err != nil {
		appKey.Close()
		return fmt.Errorf("set registry value Version: %w", err)
	}
	if err := appKey.Close(); err != nil {
		return fmt.Errorf("close application registry key: %w", err)
	}

	programs, err := windows.KnownFolderPath(windows.FOLDERID_Programs, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return fmt.Errorf("locate Start Menu: %w", err)
	}
	if err := createShortcut(filepath.Join(programs, "Metrolist.lnk"), launcher, filepath.Dir(launcher)); err != nil {
		return fmt.Errorf("create Start Menu shortcut: %w", err)
	}
	desktop, err := windows.KnownFolderPath(windows.FOLDERID_Desktop, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return fmt.Errorf("locate desktop: %w", err)
	}
	desktopLink := filepath.Join(desktop, "Metrolist.lnk")
	if desktopShortcut {
		if err := createShortcut(desktopLink, launcher, filepath.Dir(launcher)); err != nil {
			return fmt.Errorf("create desktop shortcut: %w", err)
		}
	} else if err := os.Remove(desktopLink); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove desktop shortcut: %w", err)
	}

	uninstallKey, _, err := registry.CreateKey(registry.CURRENT_USER, uninstallRegistryPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("create uninstall registry key: %w", err)
	}
	if err := setStrings(uninstallKey, map[string]string{
		"DisplayName":          "Metrolist Desktop",
		"DisplayVersion":       version,
		"Publisher":            "MetrolistGroup",
		"InstallLocation":      root,
		"DisplayIcon":          launcher,
		"UninstallString":      quote(manager) + " uninstall",
		"QuietUninstallString": quote(manager) + " uninstall --silent",
		"URLInfoAbout":         "https://github.com/" + buildconfig.RepositoryOwner + "/" + buildconfig.RepositoryName,
	}); err != nil {
		uninstallKey.Close()
		return err
	}
	if err := uninstallKey.SetDWordValue("NoModify", 1); err != nil {
		uninstallKey.Close()
		return err
	}
	if size, err := directorySize(root); err == nil {
		_ = uninstallKey.SetDWordValue("EstimatedSize", uint32(min(size/1024, int64(^uint32(0)))))
	}
	if err := uninstallKey.Close(); err != nil {
		return fmt.Errorf("close uninstall registry key: %w", err)
	}

	return nil
}

func Unregister() error {
	var errs []error
	programs, err := windows.KnownFolderPath(windows.FOLDERID_Programs, windows.KF_FLAG_DEFAULT)
	if err == nil {
		if removeErr := os.Remove(filepath.Join(programs, "Metrolist.lnk")); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			errs = append(errs, removeErr)
		}
	} else {
		errs = append(errs, err)
	}
	desktop, err := windows.KnownFolderPath(windows.FOLDERID_Desktop, windows.KF_FLAG_DEFAULT)
	if err == nil {
		if removeErr := os.Remove(filepath.Join(desktop, "Metrolist.lnk")); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			errs = append(errs, removeErr)
		}
	} else {
		errs = append(errs, err)
	}
	if len(errs) != 0 {
		return errors.Join(errs...)
	}
	if err := registry.DeleteKey(registry.CURRENT_USER, uninstallRegistryPath); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("remove uninstall registry key: %w", err)
	}
	if err := registry.DeleteKey(registry.CURRENT_USER, appRegistryPath); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("remove application registry key: %w", err)
	}
	return nil
}

func WaitForProcess(pid uint32, timeout time.Duration) (bool, error) {
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("open Metrolist process: %w", err)
	}
	defer windows.CloseHandle(process)
	result, err := windows.WaitForSingleObject(process, uint32(timeout/time.Millisecond))
	if err != nil {
		return false, err
	}
	return result == windows.WAIT_OBJECT_0, nil
}

func FindRunningApps(root string) ([]uint32, error) {
	processes, err := openRunningApps(root, windows.PROCESS_QUERY_LIMITED_INFORMATION)
	if err != nil {
		return nil, err
	}
	defer closeProcesses(processes)
	ids := make([]uint32, len(processes))
	for i, process := range processes {
		ids[i] = process.pid
	}
	return ids, nil
}

func ValidateAppProcess(root string, pid uint32) (exists, matches bool, err error) {
	if pid == 0 {
		return false, false, nil
	}
	wanted, err := os.Stat(filepath.Join(root, installer.AppDirectoryName, "Metrolist.exe"))
	if err != nil {
		return false, false, fmt.Errorf("inspect installed launcher: %w", err)
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(process)
	if processExited(process) {
		return false, false, nil
	}
	matches, err = processMatchesFile(process, wanted)
	if err != nil && processExited(process) {
		return false, false, nil
	}
	return true, matches, err
}

func WaitForApps(root string, timeout time.Duration) (bool, error) {
	processes, err := openRunningApps(root, windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION)
	if err != nil {
		return false, err
	}
	defer closeProcesses(processes)
	deadline := time.Now().Add(timeout)
	for _, process := range processes {
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		result, err := windows.WaitForSingleObject(process.handle, uint32(remaining/time.Millisecond))
		if err != nil {
			return false, fmt.Errorf("wait for Metrolist process %d: %w", process.pid, err)
		}
		if result == uint32(windows.WAIT_TIMEOUT) {
			return false, nil
		}
		if result != windows.WAIT_OBJECT_0 {
			return false, fmt.Errorf("wait for Metrolist process %d returned %d", process.pid, result)
		}
	}
	return true, nil
}

func TerminateRunningApps(root string) error {
	processes, err := openRunningApps(root, windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION)
	if err != nil {
		return err
	}
	defer closeProcesses(processes)
	for _, process := range processes {
		if err := windows.TerminateProcess(process.handle, 1); err != nil {
			return fmt.Errorf("terminate Metrolist process %d: %w", process.pid, err)
		}
		result, err := windows.WaitForSingleObject(process.handle, 10_000)
		if err != nil {
			return fmt.Errorf("wait for terminated Metrolist process %d: %w", process.pid, err)
		}
		if result != windows.WAIT_OBJECT_0 {
			return fmt.Errorf("Metrolist process %d did not exit after it was terminated", process.pid)
		}
	}
	return nil
}

type runningProcess struct {
	pid    uint32
	handle windows.Handle
}

func openRunningApps(root string, access uint32) ([]runningProcess, error) {
	wanted, err := os.Stat(filepath.Join(root, installer.AppDirectoryName, "Metrolist.exe"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect installed launcher: %w", err)
	}
	var currentSession uint32
	if err := windows.ProcessIdToSessionId(uint32(os.Getpid()), &currentSession); err != nil {
		return nil, fmt.Errorf("identify current Windows session: %w", err)
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, err
	}
	var processes []runningProcess
	for {
		if strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), "Metrolist.exe") {
			var session uint32
			sessionErr := windows.ProcessIdToSessionId(entry.ProcessID, &session)
			if sessionErr != nil {
				if !errors.Is(sessionErr, windows.ERROR_INVALID_PARAMETER) {
					closeProcesses(processes)
					return nil, fmt.Errorf("identify Windows session for process %d: %w", entry.ProcessID, sessionErr)
				}
			} else if session == currentSession {
				process, matches, openErr := openMatchingProcess(entry.ProcessID, access, wanted)
				if openErr != nil {
					closeProcesses(processes)
					return nil, fmt.Errorf("inspect Metrolist.exe process %d: %w", entry.ProcessID, openErr)
				}
				if matches {
					processes = append(processes, process)
				}
			}
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return processes, nil
			}
			closeProcesses(processes)
			return nil, err
		}
	}
}

func openMatchingProcess(pid, access uint32, wanted os.FileInfo) (runningProcess, bool, error) {
	handle, err := windows.OpenProcess(access|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return runningProcess{}, false, nil
	}
	if err != nil {
		return runningProcess{}, false, err
	}
	if processExited(handle) {
		windows.CloseHandle(handle)
		return runningProcess{}, false, nil
	}
	matches, err := processMatchesFile(handle, wanted)
	if err != nil {
		if processExited(handle) {
			windows.CloseHandle(handle)
			return runningProcess{}, false, nil
		}
		windows.CloseHandle(handle)
		return runningProcess{}, false, err
	}
	if !matches {
		windows.CloseHandle(handle)
		return runningProcess{}, false, nil
	}
	return runningProcess{pid: pid, handle: handle}, true, nil
}

func processExited(process windows.Handle) bool {
	result, err := windows.WaitForSingleObject(process, 0)
	return err == nil && result == windows.WAIT_OBJECT_0
}

func processMatchesFile(process windows.Handle, wanted os.FileInfo) (bool, error) {
	buffer := make([]uint16, 32_768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(process, 0, &buffer[0], &size); err != nil {
		return false, err
	}
	actual, err := os.Stat(windows.UTF16ToString(buffer[:size]))
	if err != nil {
		return false, err
	}
	return os.SameFile(actual, wanted), nil
}

func closeProcesses(processes []runningProcess) {
	for _, process := range processes {
		windows.CloseHandle(process.handle)
	}
}

func RemoveUserData() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	paths := []string{
		filepath.Join(envOrDefault("XDG_DATA_HOME", filepath.Join(home, ".local", "share")), "metrolist"),
		filepath.Join(envOrDefault("XDG_STATE_HOME", filepath.Join(home, ".local", "state")), "metrolist"),
		filepath.Join(envOrDefault("XDG_CACHE_HOME", filepath.Join(home, ".cache")), "metrolist"),
		filepath.Join(envOrDefault("XDG_CONFIG_HOME", filepath.Join(home, ".config")), "metrolist"),
		filepath.Join(home, ".metrolist"),
	}
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		paths = append(paths,
			filepath.Join(localAppData, "Metrolist", "WebView2"),
			filepath.Join(localAppData, "Metrolist", "Manager"),
		)
	}
	var errs []error
	for _, path := range paths {
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func createShortcut(path, target, workingDirectory string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		if oleError, ok := err.(*ole.OleError); !ok || oleError.Code() != 1 {
			return err
		}
	}
	defer ole.CoUninitialize()
	unknown, err := oleutil.CreateObject("WScript.Shell")
	if err != nil {
		return err
	}
	defer unknown.Release()
	shell, err := unknown.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return err
	}
	defer shell.Release()
	value, err := oleutil.CallMethod(shell, "CreateShortcut", path)
	if err != nil {
		return err
	}
	shortcut := value.ToIDispatch()
	defer shortcut.Release()
	properties := map[string]string{
		"TargetPath":       target,
		"WorkingDirectory": workingDirectory,
		"Description":      "Metrolist Desktop",
		"IconLocation":     target + ",0",
	}
	for name, propertyValue := range properties {
		result, err := oleutil.PutProperty(shortcut, name, propertyValue)
		if result != nil {
			_ = result.Clear()
		}
		if err != nil {
			return err
		}
	}
	result, err := oleutil.CallMethod(shortcut, "Save")
	if result != nil {
		_ = result.Clear()
	}
	return err
}

func setStrings(key registry.Key, values map[string]string) error {
	for name, value := range values {
		if err := key.SetStringValue(name, value); err != nil {
			return fmt.Errorf("set registry value %s: %w", name, err)
		}
	}
	return nil
}

func directorySize(root string) (int64, error) {
	var size int64
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func quote(value string) string {
	return `"` + value + `"`
}

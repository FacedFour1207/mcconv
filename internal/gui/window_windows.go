package gui

import (
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed ui/index.html
var indexHTML string

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	dwmapi   = windows.NewLazySystemDLL("dwmapi.dll")
	ole32    = windows.NewLazySystemDLL("ole32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procSetDpiCtx       = user32.NewProc("SetProcessDpiAwarenessContext")
	procGetDpiForSys    = user32.NewProc("GetDpiForSystem")
	procMessageBox      = user32.NewProc("MessageBoxW")
	procLoadImage       = user32.NewProc("LoadImageW")
	procSendMessage     = user32.NewProc("SendMessageW")
	procDwmSetAttr      = dwmapi.NewProc("DwmSetWindowAttribute")
	procCoCreate        = ole32.NewProc("CoCreateInstance")
	procCoInit          = ole32.NewProc("CoInitializeEx")
	procCoTaskMemFree   = ole32.NewProc("CoTaskMemFree")
	procSHCreateItem    = shell32.NewProc("SHCreateItemFromParsingName")
	procGetModuleHandl  = kernel32.NewProc("GetModuleHandleW")
	procGlobalMemStatus = kernel32.NewProc("GlobalMemoryStatusEx")
)

// Run opens the main window and returns when it is closed.
func Run() error {
	procSetDpiCtx.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
	scale := 1.0
	if dpi, _, _ := procGetDpiForSys.Call(); dpi > 0 {
		scale = float64(dpi) / 96
	}
	dataPath := filepath.Join(os.Getenv("LOCALAPPDATA"), "mcconv", "webview")
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		DataPath:  dataPath,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "mcconv — Java ⇄ 統合版 ワールド変換",
			Width:  uint(1200 * scale),
			Height: uint(820 * scale),
			Center: true,
		},
	})
	if w == nil {
		msg := "WebView2 ランタイムを起動できませんでした。\nMicrosoft Edge WebView2 ランタイムをインストールしてください。\n\nコマンドラインでは mcconv <入力> <出力> で変換できます。"
		t, _ := syscall.UTF16PtrFromString(msg)
		c, _ := syscall.UTF16PtrFromString("mcconv")
		procMessageBox.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), 0x10)
		return errors.New("WebView2 is not available")
	}
	hwnd := uintptr(w.Window())
	setTitleBarTheme(hwnd)
	setWindowIcon(hwnd)
	w.SetSize(int(920*scale), int(640*scale), webview2.HintMin)

	app := newApp(func(ev map[string]any) {
		b, err := json.Marshal(ev)
		if err != nil {
			return
		}
		js := "window.__mc && window.__mc(" + string(b) + ")"
		w.Dispatch(func() { w.Eval(js) })
	})
	bind := func(name string, f any) {
		if err := w.Bind(name, f); err != nil {
			panic(err)
		}
	}
	bind("mcState", app.State)
	bind("mcWorlds", app.Worlds)
	bind("mcInspect", app.Inspect)
	bind("mcDestinations", app.Destinations)
	bind("mcStart", app.Start)
	bind("mcCancel", app.Cancel)
	bind("mcUpdateData", app.UpdateData)
	bind("mcSaveVersions", app.SaveVersions)
	bind("mcPickFolder", func(title, initial string) string { return pickFolder(hwnd, title, initial) })
	bind("mcOpen", openInExplorer)
	bind("mcSetDark", func(dark bool) { setDarkTitleBar(hwnd, dark) })
	w.SetHtml(indexHTML)
	w.Run()
	app.shutdown()
	return nil
}

// setTitleBarTheme makes the title bar dark when Windows uses dark mode.
func setTitleBarTheme(hwnd uintptr) {
	dark := false
	if k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE); err == nil {
		if v, _, err := k.GetIntegerValue("AppsUseLightTheme"); err == nil && v == 0 {
			dark = true
		}
		k.Close()
	}
	setDarkTitleBar(hwnd, dark)
	backdrop := uint32(2) // DWMSBT_MAINWINDOW (Mica) where supported
	procDwmSetAttr.Call(hwnd, 38, uintptr(unsafe.Pointer(&backdrop)), 4)
}

// setDarkTitleBar switches the title bar between light and dark.
func setDarkTitleBar(hwnd uintptr, dark bool) {
	v := uint32(0)
	if dark {
		v = 1
	}
	procDwmSetAttr.Call(hwnd, 20, uintptr(unsafe.Pointer(&v)), 4) // DWMWA_USE_IMMERSIVE_DARK_MODE
}

// totalMemoryMB returns the installed RAM.
func totalMemoryMB() int {
	type memStatus struct {
		Length, MemoryLoad                     uint32
		TotalPhys, AvailPhys, TotalPageFile    uint64
		AvailPageFile, TotalVirtual, AvailVirt uint64
		AvailExtendedVirtual                   uint64
	}
	m := memStatus{Length: uint32(unsafe.Sizeof(memStatus{}))}
	if r, _, _ := procGlobalMemStatus.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0
	}
	return int(m.TotalPhys >> 20)
}

// setWindowIcon uses the executable's icon resource (#1) for the window.
func setWindowIcon(hwnd uintptr) {
	inst, _, _ := procGetModuleHandl.Call(0)
	for _, size := range []struct{ kind, px uintptr }{{1, 32}, {0, 16}} { // ICON_BIG, ICON_SMALL
		h, _, _ := procLoadImage.Call(inst, 1, 1 /*IMAGE_ICON*/, size.px, size.px, 0)
		if h != 0 {
			procSendMessage.Call(hwnd, 0x0080 /*WM_SETICON*/, size.kind, h)
		}
	}
}

func openInExplorer(path string) error {
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		return exec.Command("explorer.exe", "/select,", path).Start()
	}
	return exec.Command("explorer.exe", path).Start()
}

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

// pickFolder shows the system folder picker (IFileOpenDialog) and returns
// the chosen path, or "" if canceled.
func pickFolder(owner uintptr, title, initial string) string {
	procCoInit.Call(0, 2) // COINIT_APARTMENTTHREADED (already initialised by WebView2)
	clsid := windows.GUID{Data1: 0xDC1C5A9C, Data2: 0xE88A, Data3: 0x4DDE, Data4: [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iid := windows.GUID{Data1: 0xD57C7288, Data2: 0xD4AD, Data3: 0x4768, Data4: [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60}}
	var dlg uintptr
	if hr, _, _ := procCoCreate.Call(uintptr(unsafe.Pointer(&clsid)), 0, 1 /*CLSCTX_INPROC_SERVER*/, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&dlg))); hr != 0 {
		return ""
	}
	defer comCall(dlg, 2) // Release
	var opts uintptr
	comCall(dlg, 10, uintptr(unsafe.Pointer(&opts))) // GetOptions
	comCall(dlg, 9, opts|0x20|0x40|0x800)            // SetOptions: PICKFOLDERS | FORCEFILESYSTEM | PATHMUSTEXIST
	if t, err := syscall.UTF16PtrFromString(title); err == nil && title != "" {
		comCall(dlg, 17, uintptr(unsafe.Pointer(t))) // SetTitle
	}
	if initial != "" {
		itemIID := windows.GUID{Data1: 0x43826D1E, Data2: 0xE718, Data3: 0x42EE, Data4: [8]byte{0xBC, 0x55, 0xA1, 0xE2, 0x61, 0xC3, 0x7B, 0xFE}}
		p, _ := syscall.UTF16PtrFromString(initial)
		var item uintptr
		if hr, _, _ := procSHCreateItem.Call(uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&itemIID)), uintptr(unsafe.Pointer(&item))); hr == 0 {
			comCall(dlg, 12, item) // SetFolder
			comCall(item, 2)
		}
	}
	if hr := comCall(dlg, 3, owner); hr != 0 { // Show (canceled = ERROR_CANCELLED)
		return ""
	}
	var item uintptr
	if hr := comCall(dlg, 20, uintptr(unsafe.Pointer(&item))); hr != 0 { // GetResult
		return ""
	}
	defer comCall(item, 2)
	var name *uint16
	if hr := comCall(item, 5, 0x80058000, uintptr(unsafe.Pointer(&name))); hr != 0 { // GetDisplayName(SIGDN_FILESYSPATH)
		return ""
	}
	defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(name)))
	return windows.UTF16PtrToString(name)
}

// comCall calls method i of a COM object's vtable.
func comCall(obj uintptr, i int, args ...uintptr) uintptr {
	objp := *(**uintptr)(unsafe.Pointer(&obj))     // the object
	vtbl := *(**[64]uintptr)(unsafe.Pointer(objp)) // its first field: the vtable
	fn := vtbl[i]
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{obj}, args...)...)
	return r
}

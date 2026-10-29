// Package install hooks agbridge into an Antigravity desktop installation by
// replacing its language server with the shim, and restores it again.
package install

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agbridge/internal/config"
	"agbridge/internal/hook"
)

type Status struct {
	Dir             string `json:"dir"`
	Found           bool   `json:"found"`
	Version         string `json:"version,omitempty"`
	Hooked          bool   `json:"hooked"`
	ShimCurrent     bool   `json:"shimCurrent"`
	OriginalPresent bool   `json:"originalPresent"`
	PatchedPresent  bool   `json:"patchedPresent"`
	Running         bool   `json:"running"`
	// RestartNeeded is set by the host app: the hook went in while an
	// unhooked language server was running, which keeps bypassing agbridge
	// until Antigravity restarts it.
	RestartNeeded bool   `json:"restartNeeded"`
	Message       string `json:"message,omitempty"`
}

// mu serialises Install and Uninstall (console and auto-repair may race).
var mu sync.Mutex

// Candidates lists likely install roots (the folder holding Antigravity.exe).
func Candidates() []string {
	var out []string
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		out = append(out, filepath.Join(d, "Programs", "antigravity"), filepath.Join(d, "Programs", "Antigravity"))
	}
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
		if d := os.Getenv(env); d != "" {
			out = append(out, filepath.Join(d, "Antigravity"))
		}
	}
	return out
}

// Resolve returns the install root, from config or auto-detection.
func Resolve(cfg *config.Config) string {
	if cfg.AntigravityDir != "" {
		return normalizeRoot(cfg.AntigravityDir)
	}
	for _, c := range Candidates() {
		if exists(filepath.Join(c, "resources", "bin", hook.LSName)) || exists(filepath.Join(c, "resources", "bin", hook.OrigName)) {
			return c
		}
	}
	return ""
}

// normalizeRoot accepts the root, its resources dir or its bin dir.
func normalizeRoot(p string) string {
	p = filepath.Clean(p)
	switch strings.ToLower(filepath.Base(p)) {
	case "bin":
		return filepath.Dir(filepath.Dir(p))
	case "resources":
		return filepath.Dir(p)
	}
	return p
}

func binDir(root string) string { return filepath.Join(root, "resources", "bin") }

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// IsShim reports whether the file at p is an agbridge shim.
func IsShim(p string) bool {
	st, err := os.Stat(p)
	if err != nil || st.Size() > 64<<20 {
		return false
	}
	b, err := os.ReadFile(p)
	return err == nil && bytes.Contains(b, []byte(hook.ShimMarker))
}

func Detect(cfg *config.Config, shim []byte) Status {
	root := Resolve(cfg)
	st := Status{Dir: root}
	if root == "" {
		st.Message = "没有找到 Antigravity 安装目录，请在设置里手动指定"
		return st
	}
	bin := binDir(root)
	ls := filepath.Join(bin, hook.LSName)
	st.Found = exists(ls) || exists(filepath.Join(bin, hook.OrigName))
	if !st.Found {
		st.Message = "目录里没有 language_server.exe：" + bin
		return st
	}
	st.Version = asarVersion(filepath.Join(root, "resources", "app.asar"))
	st.Hooked = IsShim(ls)
	st.OriginalPresent = exists(filepath.Join(bin, hook.OrigName))
	st.PatchedPresent = exists(filepath.Join(bin, hook.PatchedName))
	if st.Hooked && shim != nil {
		cur, err := os.ReadFile(ls)
		st.ShimCurrent = err == nil && bytes.Equal(cur, shim)
	}
	st.Running = Running(root)
	switch {
	case st.Hooked && !st.OriginalPresent:
		st.Message = "接入文件不完整（缺少原版语言服务器），请重新安装 Antigravity 后再接入"
	case !st.Hooked && st.OriginalPresent:
		st.Message = "Antigravity 已更新，接入被覆盖，需要重新接入"
	}
	return st
}

// Install replaces the language server with the shim. agbridgeExe is the
// absolute path of the running agbridge executable.
//
// It works while Antigravity is running: Windows will not overwrite or delete
// a running executable but lets it be renamed, so files in use are moved aside
// and the running language server keeps going until Antigravity restarts it.
func Install(cfg *config.Config, shim []byte, agbridgeExe, logFile string) error {
	mu.Lock()
	defer mu.Unlock()
	root := Resolve(cfg)
	if root == "" {
		return errors.New("没有找到 Antigravity 安装目录")
	}
	if len(shim) == 0 {
		return errors.New("程序内没有内置 shim，请使用正式构建")
	}
	bin := binDir(root)
	ls := filepath.Join(bin, hook.LSName)
	orig := filepath.Join(bin, hook.OrigName)
	patched := filepath.Join(bin, hook.PatchedName)
	cleanupStale(bin)

	// fresh: language_server.exe is a vendor build (first install, or an
	// Antigravity update replaced the shim) and becomes the new original.
	fresh := !IsShim(ls)
	src := orig
	if fresh {
		if !exists(ls) {
			return fmt.Errorf("找不到 %s", ls)
		}
		src = ls
	} else if !exists(orig) {
		return errors.New("缺少原版语言服务器备份，请重新安装 Antigravity 后再接入")
	}
	vendor, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("读取原版语言服务器失败: %w", err)
	}
	if fresh {
		if err := completePE(vendor); err != nil {
			return fmt.Errorf("language_server.exe 还没写完整（可能正在更新），请稍后再试: %w", err)
		}
	}

	sc := hook.Shim{
		Proxy:       fmt.Sprintf("http://127.0.0.1:%d", cfg.Port),
		AgbridgeExe: agbridgeExe,
		Original:    hook.OrigName,
		LogFile:     logFile,
	}
	if cfg.PatchEnabled() {
		hits, err := writePatched(vendor, patched, cfg.Port)
		if err != nil {
			return fmt.Errorf("生成补丁版语言服务器失败: %w", err)
		}
		if hits > 0 {
			sc.Patched = hook.PatchedName
		}
	} else if err := discard(patched); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(sc, "", "  ")
	if err := replaceFile(filepath.Join(bin, hook.ShimConfig), b); err != nil {
		return err
	}

	if !fresh {
		if cur, err := os.ReadFile(ls); err == nil && bytes.Equal(cur, shim) {
			return nil
		}
		if err := replaceFile(ls, shim); err != nil {
			return fmt.Errorf("写入 shim 失败: %w", err)
		}
		return nil
	}
	// Swap the shim in last with two renames, so a language server started
	// in the meantime always finds a complete set of files.
	stage := ls + ".agb-tmp"
	if err := os.WriteFile(stage, shim, 0o755); err != nil {
		return fmt.Errorf("写入 shim 失败: %w", err)
	}
	if err := discard(orig); err != nil {
		_ = os.Remove(stage)
		return fmt.Errorf("清理旧备份失败: %w", err)
	}
	if err := os.Rename(ls, orig); err != nil {
		_ = os.Remove(stage)
		return fmt.Errorf("备份原版语言服务器失败: %w", err)
	}
	if err := os.Rename(stage, ls); err != nil {
		_ = os.Rename(orig, ls)
		_ = os.Remove(stage)
		return fmt.Errorf("写入 shim 失败: %w", err)
	}
	return nil
}

// Uninstall restores the vendor language server and removes agbridge files.
// Like Install it works while Antigravity is running.
func Uninstall(cfg *config.Config) error {
	mu.Lock()
	defer mu.Unlock()
	root := Resolve(cfg)
	if root == "" {
		return errors.New("没有找到 Antigravity 安装目录")
	}
	bin := binDir(root)
	ls := filepath.Join(bin, hook.LSName)
	orig := filepath.Join(bin, hook.OrigName)
	cleanupStale(bin)
	if IsShim(ls) {
		if !exists(orig) {
			return errors.New("缺少原版语言服务器备份，无法还原；请重新安装 Antigravity")
		}
		old, err := moveAside(ls)
		if err != nil {
			return fmt.Errorf("移除 shim 失败: %w", err)
		}
		if err := os.Rename(orig, ls); err != nil {
			_ = os.Rename(old, ls)
			return fmt.Errorf("还原原版语言服务器失败: %w", err)
		}
		_ = os.Remove(old)
	} else if err := discard(orig); err != nil { // stale backup left by an Antigravity update
		return err
	}
	if err := discard(filepath.Join(bin, hook.PatchedName)); err != nil {
		return err
	}
	return discard(filepath.Join(bin, hook.ShimConfig))
}

// Fingerprint is a cheap summary of the hook's files; it changes whenever
// Antigravity or agbridge rewrites one of them.
func Fingerprint(cfg *config.Config) string {
	root := Resolve(cfg)
	if root == "" {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(root)
	for _, name := range []string{hook.LSName, hook.OrigName, hook.PatchedName, hook.ShimConfig} {
		if st, err := os.Stat(filepath.Join(binDir(root), name)); err == nil {
			fmt.Fprintf(&sb, "|%d@%d", st.Size(), st.ModTime().UnixNano())
		} else {
			sb.WriteString("|-")
		}
	}
	return sb.String()
}

// completePE rejects an executable whose sections or signature extend past
// the end of the data, i.e. one the updater has not finished writing.
func completePE(b []byte) error {
	f, err := pe.NewFile(bytes.NewReader(b))
	if err != nil {
		return err
	}
	var end int64
	for _, s := range f.Sections {
		end = max(end, int64(s.Offset)+int64(s.Size))
	}
	if oh, ok := f.OptionalHeader.(*pe.OptionalHeader64); ok && len(oh.DataDirectory) > pe.IMAGE_DIRECTORY_ENTRY_SECURITY {
		// The certificate table's "address" is a file offset.
		if d := oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_SECURITY]; d.Size > 0 {
			end = max(end, int64(d.VirtualAddress)+int64(d.Size))
		}
	}
	if end > int64(len(b)) {
		return fmt.Errorf("只有 %d / %d 字节", len(b), end)
	}
	return nil
}

// writePatched writes the vendor binary to dst with the hardcoded endpoints
// replaced; it returns the number of replacements (0 means no patched copy).
// An identical existing copy is left alone, since it may be running.
func writePatched(vendor []byte, dst string, port int) (int, error) {
	b := vendor
	hits := 0
	for _, r := range hook.Rules {
		repl, err := r.PatchedURL(port)
		if err != nil {
			return 0, err
		}
		n := bytes.Count(b, []byte(r.Original))
		if n > 0 {
			b = bytes.ReplaceAll(b, []byte(r.Original), []byte(repl))
			hits += n
		}
	}
	if hits == 0 {
		return 0, discard(dst)
	}
	if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, b) {
		return hits, nil
	}
	return hits, replaceFile(dst, b)
}

// replaceFile atomically writes data to path. Windows refuses to replace a
// running executable but lets it be renamed, so such a target is moved aside.
func replaceFile(path string, data []byte) error {
	tmp := path + ".agb-tmp"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	err := os.Rename(tmp, path)
	if err != nil {
		var old string
		if old, err = moveAside(path); err == nil {
			if err = os.Rename(tmp, path); err != nil {
				_ = os.Rename(old, path)
			} else {
				_ = os.Remove(old)
			}
		}
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// discard removes path, or moves it aside when it is a running executable.
func discard(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	_, err = moveAside(path)
	return err
}

// moveAside renames path to a name cleanupStale deletes once it is unused.
func moveAside(path string) (string, error) {
	old := fmt.Sprintf("%s.agb-old-%d", path, time.Now().UnixNano())
	return old, os.Rename(path, old)
}

// cleanupStale deletes leftovers of earlier installs that were still running
// at the time, plus temporary files of interrupted writes.
func cleanupStale(bin string) {
	for _, pat := range []string{"*.agb-old-*", "*.agb-tmp"} {
		matches, _ := filepath.Glob(filepath.Join(bin, pat))
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
}

// asarVersion reads "version" from package.json inside app.asar.
func asarVersion(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var head [16]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return ""
	}
	headerSize := binary.LittleEndian.Uint32(head[4:8])
	jsonLen := binary.LittleEndian.Uint32(head[12:16])
	if jsonLen > 32<<20 {
		return ""
	}
	hdr := make([]byte, jsonLen)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return ""
	}
	var tree struct {
		Files map[string]struct {
			Offset string `json:"offset"`
			Size   int64  `json:"size"`
		} `json:"files"`
	}
	if json.Unmarshal(hdr, &tree) != nil {
		return ""
	}
	pkg, ok := tree.Files["package.json"]
	if !ok || pkg.Size > 1<<20 {
		return ""
	}
	var off int64
	fmt.Sscan(pkg.Offset, &off)
	buf := make([]byte, pkg.Size)
	if _, err := f.ReadAt(buf, 8+int64(headerSize)+off); err != nil {
		return ""
	}
	var v struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(buf, &v)
	return v.Version
}

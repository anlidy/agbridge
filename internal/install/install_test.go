package install

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"agbridge/internal/config"
	"agbridge/internal/hook"
)

func TestInstallPatchUninstall(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "resources", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("...https://daily-cloudcode-pa.googleapis.comfailed...https://cloudcode-pa.googleapis.comnil")
	vendor := fakePE(payload)
	ls := filepath.Join(bin, hook.LSName)
	shim := []byte("shim binary " + hook.ShimMarker)
	cfg := &config.Config{Port: 50999, AntigravityDir: bin} // bin dir is accepted too

	// A half-written binary (updater still copying) is left alone.
	if err := os.WriteFile(ls, vendor[:len(vendor)-10], 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Install(cfg, shim, `C:\agb\agbridge.exe`, "shim.log"); err == nil || IsShim(ls) {
		t.Fatalf("installed over a truncated binary: %v", err)
	}

	if err := os.WriteFile(ls, vendor, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(bin, hook.PatchedName+".agb-old-1")
	os.WriteFile(stale, []byte("x"), 0o755)
	if err := Install(cfg, shim, `C:\agb\agbridge.exe`, "shim.log"); err != nil {
		t.Fatal(err)
	}
	if exists(stale) {
		t.Fatal("stale file not cleaned up")
	}
	st := Detect(cfg, shim)
	if !st.Hooked || !st.ShimCurrent || !st.OriginalPresent || !st.PatchedPresent || st.Dir != root {
		t.Fatalf("status after install = %+v", st)
	}
	orig, _ := os.ReadFile(filepath.Join(bin, hook.OrigName))
	if !bytes.Equal(orig, vendor) {
		t.Fatal("original not preserved")
	}
	patched, _ := os.ReadFile(filepath.Join(bin, hook.PatchedName))
	if len(patched) != len(vendor) || !bytes.Contains(patched, []byte("http://127.0.0.1:50999/_agbd/xxxxxxxxxxxxfailed")) ||
		!bytes.Contains(patched, []byte("https://cloudcode-pa.googleapis.comnil")) {
		t.Fatalf("bad patch: %s", patched)
	}
	sc, err := hook.ReadShim(filepath.Join(bin, hook.ShimConfig))
	if err != nil || sc.Patched != hook.PatchedName || sc.Proxy != "http://127.0.0.1:50999" {
		t.Fatalf("shim config = %+v, %v", sc, err)
	}

	// Re-running install (repair) keeps the original backup intact.
	if err := Install(cfg, shim, `C:\agb\agbridge.exe`, "shim.log"); err != nil {
		t.Fatal(err)
	}
	if orig, _ := os.ReadFile(filepath.Join(bin, hook.OrigName)); !bytes.Equal(orig, vendor) {
		t.Fatal("repair clobbered the original")
	}

	// Simulate an Antigravity update overwriting the shim with a new vendor build.
	vendor2 := fakePE(append([]byte("v2 "), payload...))
	os.WriteFile(ls, vendor2, 0o755)
	if st := Detect(cfg, shim); st.Hooked || st.Message == "" {
		t.Fatalf("update not detected: %+v", st)
	}
	if err := Install(cfg, shim, `C:\agb\agbridge.exe`, "shim.log"); err != nil {
		t.Fatal(err)
	}
	if orig, _ := os.ReadFile(filepath.Join(bin, hook.OrigName)); !bytes.Equal(orig, vendor2) {
		t.Fatal("updated vendor binary should become the new original")
	}

	if err := Uninstall(cfg); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(ls); !bytes.Equal(got, vendor2) {
		t.Fatal("uninstall did not restore the vendor binary")
	}
	for _, f := range []string{hook.OrigName, hook.PatchedName, hook.ShimConfig} {
		if _, err := os.Stat(filepath.Join(bin, f)); err == nil {
			t.Fatalf("%s left behind", f)
		}
	}
}

// fakePE wraps payload in a minimal, complete PE image with one section.
func fakePE(payload []byte) []byte {
	var b bytes.Buffer
	dos := make([]byte, 0x40)
	copy(dos, "MZ")
	binary.LittleEndian.PutUint32(dos[0x3c:], 0x40)
	b.Write(dos)
	b.WriteString("PE\x00\x00")
	binary.Write(&b, binary.LittleEndian, pe.FileHeader{
		Machine: pe.IMAGE_FILE_MACHINE_AMD64, NumberOfSections: 1,
		SizeOfOptionalHeader: uint16(binary.Size(pe.OptionalHeader64{})),
	})
	binary.Write(&b, binary.LittleEndian, pe.OptionalHeader64{Magic: 0x20b, NumberOfRvaAndSizes: 16})
	off := b.Len() + binary.Size(pe.SectionHeader32{})
	binary.Write(&b, binary.LittleEndian, pe.SectionHeader32{
		Name: [8]uint8{'.', 'd', 'a', 't', 'a'}, VirtualSize: uint32(len(payload)), VirtualAddress: 0x1000,
		SizeOfRawData: uint32(len(payload)), PointerToRawData: uint32(off),
	})
	b.Write(payload)
	return b.Bytes()
}

func TestStripMarker(t *testing.T) {
	for in, want := range map[string][2]string{
		"/_agbd/xxxxxxxxxxxx/v1internal:fetchAvailableModels": {"/v1internal:fetchAvailableModels", "https://daily-cloudcode-pa.googleapis.com"},
		"/_agbp/v1internal:loadCodeAssist":                    {"/v1internal:loadCodeAssist", "https://cloudcode-pa.googleapis.com"},
		"/_agbd/xxxxxxxxxxxx":                                 {"/", "https://daily-cloudcode-pa.googleapis.com"},
		"/v1internal:streamGenerateContent":                   {"/v1internal:streamGenerateContent", ""},
	} {
		rest, origin := hook.StripMarker(in)
		if rest != want[0] || origin != want[1] {
			t.Errorf("StripMarker(%q) = %q, %q", in, rest, origin)
		}
	}
	for _, port := range []int{1024, 50999, 65535} {
		u, err := hook.Rules[0].PatchedURL(port)
		if err != nil || len(u) != len(hook.Rules[0].Original) {
			t.Errorf("port %d: %q %v", port, u, err)
		}
	}
}

func TestAssignPlaceholders(t *testing.T) {
	five := 5
	c := &config.Config{Models: []config.Model{{ID: "a", Placeholder: &five}, {ID: "b"}, {ID: "c"}}}
	config.AssignPlaceholders(c, []int{1650, 5})
	got := []int{*c.Models[0].Placeholder, *c.Models[1].Placeholder, *c.Models[2].Placeholder}
	// "a" loses 5 (now used by Google) and everyone gets distinct free slots from the top.
	if got[0] != 1649 || got[1] != 1648 || got[2] != 1647 {
		t.Fatalf("got %v", got)
	}
	if config.AssignPlaceholders(c, []int{1650, 5}) {
		t.Fatal("stable assignment reported a change")
	}
}

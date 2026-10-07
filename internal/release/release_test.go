package release

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLibrary returns a minimal header that passes CheckBinaryFormat.
func fakeLibrary(p Platform) []byte {
	lib := make([]byte, 256)
	switch p.GOOS {
	case "linux":
		copy(lib, []byte{0x7f, 'E', 'L', 'F'})
		m := map[string]uint16{"amd64": 62, "arm64": 183}[p.GOARCH]
		lib[18], lib[19] = byte(m), byte(m>>8)
	case "darwin":
		copy(lib, []byte{0xcf, 0xfa, 0xed, 0xfe})
		c := map[string]uint32{"amd64": 0x01000007, "arm64": 0x0100000c}[p.GOARCH]
		lib[4], lib[5], lib[6], lib[7] = byte(c), byte(c>>8), byte(c>>16), byte(c>>24)
	case "windows":
		copy(lib, "MZ")
		lib[0x3c] = 0x80
		copy(lib[0x80:], "PE\x00\x00")
		m := map[string]uint16{"amd64": 0x8664, "arm64": 0xaa64}[p.GOARCH]
		lib[0x84], lib[0x85] = byte(m), byte(m>>8)
	}
	return lib
}

func buildRelease(t *testing.T, version string) string {
	t.Helper()
	libs := t.TempDir()
	out := t.TempDir()
	for _, p := range StorePlatforms {
		lib := filepath.Join(libs, p.GOOS+"-"+p.GOARCH+LibraryExtension(p.GOOS))
		if err := os.WriteFile(lib, fakeLibrary(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Package(lib, out, PluginID, version, p); err != nil {
			t.Fatalf("package %s: %v", p, err)
		}
	}
	if _, err := WriteChecksums(out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPackageAndVerifyStoreRelease(t *testing.T) {
	dir := buildRelease(t, "0.1.1")
	if err := Verify(dir, PluginID, "0.1.1", StorePlatforms); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"cliproxyapi-ollama_0.1.1_darwin_amd64.zip",
		"cliproxyapi-ollama_0.1.1_darwin_arm64.zip",
		"cliproxyapi-ollama_0.1.1_linux_amd64.zip",
		"cliproxyapi-ollama_0.1.1_linux_arm64.zip",
		"cliproxyapi-ollama_0.1.1_windows_amd64.zip",
		"checksums.txt",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing asset %s", name)
		}
	}
	data, _ := os.ReadFile(filepath.Join(dir, "cliproxyapi-ollama_0.1.1_windows_amd64.zip"))
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 1 || zr.File[0].Name != "cliproxyapi-ollama.dll" {
		t.Fatalf("zip entries = %v", zr.File)
	}
	if zr.File[0].Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v", zr.File[0].Mode())
	}
}

func TestPackageIsReproducible(t *testing.T) {
	a := buildRelease(t, "0.1.1")
	b := buildRelease(t, "0.1.1")
	ca, _ := os.ReadFile(filepath.Join(a, ChecksumsFile))
	cb, _ := os.ReadFile(filepath.Join(b, ChecksumsFile))
	if !bytes.Equal(ca, cb) {
		t.Fatalf("checksums differ:\n%s\n%s", ca, cb)
	}
}

func TestVerifyDetectsProblems(t *testing.T) {
	dir := buildRelease(t, "0.1.1")
	if err := os.Remove(filepath.Join(dir, "cliproxyapi-ollama_0.1.1_linux_arm64.zip")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cliproxyapi-ollama_0.1.1_darwin_amd64.zip"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Verify(dir, PluginID, "0.1.1", StorePlatforms)
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, want := range []string{"linux/arm64: missing archive", "darwin/amd64: checksum mismatch", "darwin/amd64", "open zip"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func zipWith(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestVerifyArchiveLayoutRules(t *testing.T) {
	linux := Platform{"linux", "amd64"}
	lib := fakeLibrary(linux)
	cases := map[string]struct {
		entries map[string][]byte
		want    string
	}{
		"nested":          {map[string][]byte{"dist/cliproxyapi-ollama.so": lib}, "zip root"},
		"wrong name":      {map[string][]byte{"plugin.so": lib}, "must be cliproxyapi-ollama.so"},
		"two libraries":   {map[string][]byte{"cliproxyapi-ollama.so": lib, "cliproxyapi-ollama-v0.1.1.so": lib}, "multiple libraries"},
		"zip slip":        {map[string][]byte{"../cliproxyapi-ollama.so": lib}, "escapes archive root"},
		"missing":         {map[string][]byte{"README.md": []byte("hi")}, "does not contain"},
		"wrong arch":      {map[string][]byte{"cliproxyapi-ollama.so": fakeLibrary(Platform{"linux", "arm64"})}, "does not match amd64"},
		"wrong os format": {map[string][]byte{"cliproxyapi-ollama.so": fakeLibrary(Platform{"windows", "amd64"})}, "not an ELF"},
	}
	for name, c := range cases {
		err := VerifyArchive(zipWith(t, c.entries), PluginID, "0.1.1", linux)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	ok := zipWith(t, map[string][]byte{"cliproxyapi-ollama-v0.1.1.so": lib, "LICENSE": []byte("MIT")})
	if err := VerifyArchive(ok, PluginID, "0.1.1", linux); err != nil {
		t.Fatalf("versioned library name and extra files must be accepted: %v", err)
	}
}

func TestVersionRules(t *testing.T) {
	for _, v := range []string{"0.1.1", "1.0", "10.20.30"} {
		if !ValidVersion(v) {
			t.Errorf("%q rejected", v)
		}
	}
	for _, v := range []string{"", "v0.1.1", "1", "0.1.1-rc1", "0.1.x"} {
		if ValidVersion(v) {
			t.Errorf("%q accepted", v)
		}
	}
}

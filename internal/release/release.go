// Package release packages plugin libraries in the CLIProxyAPI plugin store
// release format and verifies a release directory against the same rules the
// v8.0.11 store installer (internal/pluginstore) enforces.
package release

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// PluginID is the plugin ID; it is also the library base name.
const PluginID = "cliproxyapi-ollama"

// ChecksumsFile is the checksum asset name the store installer requires.
const ChecksumsFile = "checksums.txt"

// Platform is one GOOS/GOARCH target.
type Platform struct {
	GOOS   string
	GOARCH string
}

func (p Platform) String() string { return p.GOOS + "/" + p.GOARCH }

// StorePlatforms is the matrix the official store's review requires.
var StorePlatforms = []Platform{
	{"darwin", "amd64"},
	{"darwin", "arm64"},
	{"linux", "amd64"},
	{"linux", "arm64"},
	{"windows", "amd64"},
}

// Same patterns as CLIProxyAPI internal/pluginstore/registry.go.
var (
	versionPattern = regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]*$`)
	idPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	// The store README requires a dotted numeric release tag, e.g. v0.1.0.
	dottedNumeric = regexp.MustCompile(`^[0-9]+(\.[0-9]+)+$`)
)

// ValidVersion reports whether version (without a leading "v") is accepted.
func ValidVersion(version string) bool {
	return version != "" && !strings.HasPrefix(version, "v") && versionPattern.MatchString(version) && dottedNumeric.MatchString(version)
}

// ValidID reports whether id matches CLIProxyAPI's plugin ID rules.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// LibraryExtension returns the dynamic library extension for goos.
func LibraryExtension(goos string) string {
	switch goos {
	case "darwin":
		return ".dylib"
	case "windows":
		return ".dll"
	default:
		return ".so"
	}
}

// LibraryName is the file name the installer expects at the zip root.
func LibraryName(id, goos string) string { return id + LibraryExtension(goos) }

// ArchiveName is the release asset name the installer looks up.
func ArchiveName(id, version, goos, goarch string) string {
	return fmt.Sprintf("%s_%s_%s_%s.zip", id, version, goos, goarch)
}

// zipModTime keeps archives reproducible for identical libraries.
var zipModTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Package writes <outDir>/<ArchiveName> containing the library at the zip root
// under its canonical name, and returns the archive path and its SHA-256.
func Package(libraryPath, outDir, id, version string, p Platform) (string, string, error) {
	if !ValidID(id) {
		return "", "", fmt.Errorf("invalid plugin id %q", id)
	}
	if !ValidVersion(version) {
		return "", "", fmt.Errorf("invalid version %q: use dotted numeric without a leading v", version)
	}
	lib, err := os.ReadFile(libraryPath)
	if err != nil {
		return "", "", err
	}
	if err := CheckBinaryFormat(lib, p); err != nil {
		return "", "", fmt.Errorf("%s: %w", libraryPath, err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	header := &zip.FileHeader{Name: LibraryName(id, p.GOOS), Method: zip.Deflate, Modified: zipModTime}
	header.SetMode(0o755)
	w, err := zw.CreateHeader(header)
	if err != nil {
		return "", "", err
	}
	if _, err := w.Write(lib); err != nil {
		return "", "", err
	}
	if err := zw.Close(); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", "", err
	}
	archivePath := filepath.Join(outDir, ArchiveName(id, version, p.GOOS, p.GOARCH))
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	return archivePath, hex.EncodeToString(sum[:]), nil
}

// WriteChecksums writes checksums.txt in sha256sum format for every zip in dir.
func WriteChecksums(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".zip") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no zip archives in %s", dir)
	}
	sort.Strings(names)
	var out strings.Builder
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(&out, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	target := filepath.Join(dir, ChecksumsFile)
	return target, os.WriteFile(target, []byte(out.String()), 0o644)
}

// ParseChecksums mirrors the installer's sha256sum parser.
func ParseChecksums(data []byte) (map[string]string, error) {
	out := map[string]string{}
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("line %d: invalid checksum entry", i+1)
		}
		hash := strings.ToLower(fields[0])
		if len(hash) != sha256.Size*2 {
			return nil, fmt.Errorf("line %d: invalid sha256 length", i+1)
		}
		if _, err := hex.DecodeString(hash); err != nil {
			return nil, fmt.Errorf("line %d: invalid sha256: %w", i+1, err)
		}
		out[strings.TrimPrefix(fields[1], "*")] = hash
	}
	return out, nil
}

// Verify checks a release directory: one archive per platform with the exact
// asset name, a checksums.txt that covers every archive, and a zip layout the
// installer accepts with a binary built for the right platform.
func Verify(dir, id, version string, platforms []Platform) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid plugin id %q", id)
	}
	if !ValidVersion(version) {
		return fmt.Errorf("invalid version %q", version)
	}
	checksumData, err := os.ReadFile(filepath.Join(dir, ChecksumsFile))
	if err != nil {
		return fmt.Errorf("read %s: %w", ChecksumsFile, err)
	}
	sums, err := ParseChecksums(checksumData)
	if err != nil {
		return fmt.Errorf("%s: %w", ChecksumsFile, err)
	}
	var problems []string
	for _, p := range platforms {
		name := ArchiveName(id, version, p.GOOS, p.GOARCH)
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: missing archive %s", p, name))
			continue
		}
		sum := sha256.Sum256(data)
		want, ok := sums[name]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s: %s has no entry in %s", p, name, ChecksumsFile))
		case want != hex.EncodeToString(sum[:]):
			problems = append(problems, fmt.Sprintf("%s: checksum mismatch for %s", p, name))
		}
		if err := VerifyArchive(data, id, version, p); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %s: %v", p, name, err))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("release verification failed:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// VerifyArchive applies the installer's zip rules (readTargetLibrary) and
// checks the binary format for the platform.
func VerifyArchive(data []byte, id, version string, p Platform) error {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	target := LibraryName(id, p.GOOS)
	versioned := id + "-v" + version + LibraryExtension(p.GOOS)
	var found *zip.File
	for _, f := range reader.File {
		name := f.Name
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("zip entry has empty name")
		}
		if strings.Contains(name, `\`) {
			return fmt.Errorf("zip entry %s uses backslash separators", name)
		}
		if path.IsAbs(name) {
			return fmt.Errorf("zip entry %s is absolute", name)
		}
		cleaned := path.Clean(name)
		if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return fmt.Errorf("zip entry %s escapes archive root", name)
		}
		if f.FileInfo().IsDir() {
			continue
		}
		mode := f.FileInfo().Mode()
		if !(mode.IsRegular() || mode.Type() == 0) {
			return fmt.Errorf("zip entry %s is not a regular file", name)
		}
		lower := strings.ToLower(cleaned)
		if !(strings.HasSuffix(lower, ".so") || strings.HasSuffix(lower, ".dylib") || strings.HasSuffix(lower, ".dll")) {
			continue
		}
		if cleaned != target && cleaned != versioned {
			if path.Base(cleaned) == target || path.Base(cleaned) == versioned {
				return fmt.Errorf("library must be at the zip root")
			}
			return fmt.Errorf("library file name must be %s, got %s", target, cleaned)
		}
		if found != nil {
			return fmt.Errorf("zip contains multiple libraries")
		}
		found = f
	}
	if found == nil {
		return fmt.Errorf("zip does not contain %s", target)
	}
	rc, err := found.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	lib, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	return CheckBinaryFormat(lib, p)
}

// CheckBinaryFormat checks the executable format and CPU of a library.
func CheckBinaryFormat(lib []byte, p Platform) error {
	if len(lib) < 64 {
		return fmt.Errorf("library is too small (%d bytes)", len(lib))
	}
	switch p.GOOS {
	case "linux":
		if !bytes.HasPrefix(lib, []byte{0x7f, 'E', 'L', 'F'}) {
			return fmt.Errorf("not an ELF library")
		}
		machine := uint16(lib[18]) | uint16(lib[19])<<8
		want := map[string]uint16{"amd64": 62, "arm64": 183}[p.GOARCH]
		if want == 0 || machine != want {
			return fmt.Errorf("ELF machine %d does not match %s", machine, p.GOARCH)
		}
	case "darwin":
		if !bytes.HasPrefix(lib, []byte{0xcf, 0xfa, 0xed, 0xfe}) {
			return fmt.Errorf("not a 64-bit Mach-O library")
		}
		cpu := uint32(lib[4]) | uint32(lib[5])<<8 | uint32(lib[6])<<16 | uint32(lib[7])<<24
		want := map[string]uint32{"amd64": 0x01000007, "arm64": 0x0100000c}[p.GOARCH]
		if want == 0 || cpu != want {
			return fmt.Errorf("Mach-O CPU type %#x does not match %s", cpu, p.GOARCH)
		}
	case "windows":
		if !bytes.HasPrefix(lib, []byte("MZ")) {
			return fmt.Errorf("not a PE library")
		}
		off := int(uint32(lib[0x3c]) | uint32(lib[0x3d])<<8 | uint32(lib[0x3e])<<16 | uint32(lib[0x3f])<<24)
		if off < 0 || off+6 > len(lib) || !bytes.Equal(lib[off:off+4], []byte("PE\x00\x00")) {
			return fmt.Errorf("invalid PE header")
		}
		machine := uint16(lib[off+4]) | uint16(lib[off+5])<<8
		want := map[string]uint16{"amd64": 0x8664, "arm64": 0xaa64}[p.GOARCH]
		if want == 0 || machine != want {
			return fmt.Errorf("PE machine %#x does not match %s", machine, p.GOARCH)
		}
	default:
		return fmt.Errorf("unsupported GOOS %q", p.GOOS)
	}
	return nil
}

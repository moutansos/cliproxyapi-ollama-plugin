// Command releasetool packages and verifies CLIProxyAPI plugin store releases.
//
//	releasetool package   -lib <path> -out <dir> -version <x.y.z> -goos <os> -goarch <arch>
//	releasetool checksums -dir <dir>
//	releasetool verify    -dir <dir> -version <x.y.z> [-platforms supported|store|linux/amd64,...]
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/release"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "package":
		err = packageCmd(os.Args[2:])
	case "checksums":
		err = checksumsCmd(os.Args[2:])
	case "verify":
		err = verifyCmd(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: releasetool package|checksums|verify [flags]")
	os.Exit(2)
}

func packageCmd(args []string) error {
	fs := flag.NewFlagSet("package", flag.ExitOnError)
	lib := fs.String("lib", "", "built library")
	out := fs.String("out", "dist", "output directory")
	version := fs.String("version", "", "version without a leading v")
	goos := fs.String("goos", "", "target GOOS")
	goarch := fs.String("goarch", "", "target GOARCH")
	_ = fs.Parse(args)
	archive, sum, err := release.Package(*lib, *out, release.PluginID, strings.TrimPrefix(*version, "v"), release.Platform{GOOS: *goos, GOARCH: *goarch})
	if err != nil {
		return err
	}
	fmt.Printf("%s  %s\n", sum, archive)
	return nil
}

func checksumsCmd(args []string) error {
	fs := flag.NewFlagSet("checksums", flag.ExitOnError)
	dir := fs.String("dir", "dist", "directory containing the release zips")
	_ = fs.Parse(args)
	path, err := release.WriteChecksums(*dir)
	if err != nil {
		return err
	}
	data, _ := os.ReadFile(path)
	fmt.Print(string(data))
	return nil
}

func verifyCmd(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := fs.String("dir", "dist", "release directory")
	version := fs.String("version", "", "version without a leading v")
	platforms := fs.String("platforms", "supported", "supported, store, or a comma-separated list like linux/amd64")
	_ = fs.Parse(args)
	var selected []release.Platform
	switch *platforms {
	case "supported":
		selected = release.SupportedPlatforms
	case "store":
		selected = release.StorePlatforms
	default:
		for _, item := range strings.Split(*platforms, ",") {
			goos, goarch, ok := strings.Cut(strings.TrimSpace(item), "/")
			if !ok {
				return fmt.Errorf("invalid platform %q", item)
			}
			selected = append(selected, release.Platform{GOOS: goos, GOARCH: goarch})
		}
	}
	v := strings.TrimPrefix(*version, "v")
	if err := release.Verify(*dir, release.PluginID, v, selected); err != nil {
		return err
	}
	fmt.Printf("release %s verified for %d platform(s)\n", v, len(selected))
	return nil
}

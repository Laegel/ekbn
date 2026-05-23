package opencode

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func EnsureInstalled() error {
	if _, err := exec.LookPath("opencode"); err == nil {
		return nil
	}
	return install()
}

func install() error {
	version, err := latestVersion()
	if err != nil {
		return fmt.Errorf("fetching latest opencode version: %w", err)
	}
	pkg := packageName()
	url := fmt.Sprintf("https://registry.npmjs.org/%s/-/%s-%s.tgz", pkg, pkg, version)

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: HTTP %d", url, resp.StatusCode)
	}

	gzr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("decompressing tarball: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	binName := "opencode"
	if runtime.GOOS == "windows" {
		binName = "opencode.exe"
	}

	var binaryData []byte
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tarball: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeSymlink {
			continue
		}
		if filepath.Base(hdr.Name) == binName {
			binaryData, err = io.ReadAll(tr)
			if err != nil {
				return fmt.Errorf("reading binary from tarball: %w", err)
			}
			_ = hdr.Linkname
			break
		}
	}

	if binaryData == nil {
		return fmt.Errorf("binary %q not found in package %s", binName, pkg)
	}

	dest := "/usr/local/bin/opencode"
	if err := os.WriteFile(dest, binaryData, 0755); err != nil {
		return fmt.Errorf("writing binary to %s: %w", dest, err)
	}
	return nil
}

type npmVersionResponse struct {
	Version string `json:"version"`
}

func latestVersion() (string, error) {
	resp, err := http.Get("https://registry.npmjs.org/opencode-ai/latest")
	if err != nil {
		return "", fmt.Errorf("fetching npm registry: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("npm registry: HTTP %d", resp.StatusCode)
	}

	var v npmVersionResponse
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", fmt.Errorf("decoding npm response: %w", err)
	}
	return v.Version, nil
}

func packageName() string {
	base := fmt.Sprintf("opencode-%s-%s", platName(), archName())
	var suffix string
	if runtime.GOARCH == "amd64" && !hasAVX2() {
		suffix += "-baseline"
	}
	if runtime.GOOS == "linux" && isMusl() {
		suffix += "-musl"
	}
	return base + suffix
}

func platName() string {
	switch runtime.GOOS {
	case "linux":
		return "linux"
	case "darwin":
		return "darwin"
	case "windows":
		return "windows"
	}
	return runtime.GOOS
}

func archName() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	}
	return runtime.GOARCH
}

func isMusl() bool {
	if _, err := os.Stat("/etc/alpine-release"); err == nil {
		return true
	}
	out, err := exec.Command("ldd", "--version").CombinedOutput()
	return err == nil && strings.Contains(strings.ToLower(string(out)), "musl")
}

func hasAVX2() bool {
	if runtime.GOARCH != "amd64" {
		return false
	}
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(data)), "avx2")
}

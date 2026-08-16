package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func runUpdate() {
	// A deferred Windows swap (see update_swap.go) records its outcome in
	// <exe>.update.result only AFTER this process exits — the swap helper
	// waits for the parent, so the parent cannot wait on it. Surface any
	// stale marker before doing anything else: a FAILED swap from the
	// previous run must not be silently ignored.
	if execPath, err := os.Executable(); err == nil {
		reportUpdateResultMarker(execPath)
	}

	fmt.Println("freebuff-proxy self-updater")
	fmt.Println("===========================")
	fmt.Printf("Current version: %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/trefeon/freebuff-proxy/releases/latest", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: build request: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("User-Agent", "freebuff-proxy/"+version)
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: check latest release: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "ERROR: GitHub API returned status %d\n", resp.StatusCode)
		os.Exit(1)
	}

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: decode release json: %v\n", err)
		os.Exit(1)
	}

	if rel.TagName == "" {
		fmt.Fprintln(os.Stderr, "ERROR: release has no tag_name")
		os.Exit(1)
	}

	fmt.Printf("Latest release: %s\n", rel.TagName)
	if version != "dev" && (version == rel.TagName || "v"+version == rel.TagName) {
		fmt.Println("Already up to date!")
		os.Exit(0)
	}

	// Match asset for platform
	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	assetSuffix := fmt.Sprintf("%s_%s%s", runtime.GOOS, runtime.GOARCH, ext)

	var assetURL, checksumURL string
	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, assetSuffix) {
			assetURL = a.BrowserDownloadURL
		}
		if a.Name == "checksums.txt" {
			checksumURL = a.BrowserDownloadURL
		}
	}

	if assetURL == "" {
		fmt.Fprintf(os.Stderr, "ERROR: no release asset found matching platform suffix %q\n", assetSuffix)
		os.Exit(1)
	}

	fmt.Printf("Downloading %s ...\n", assetURL)
	assetBytes, err := downloadURL(ctx, client, assetURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: download asset: %v\n", err)
		os.Exit(1)
	}

	if checksumURL != "" {
		if err := verifyChecksum(ctx, client, checksumURL, assetBytes); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Checksum verified successfully [ok]")
	}

	// Extract binary
	var binaryBytes []byte
	binaryName := "freebuff-proxy"
	if runtime.GOOS == "windows" {
		binaryName = "freebuff-proxy.exe"
	}

	if strings.HasSuffix(assetURL, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(assetBytes), int64(len(assetBytes)))
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: read zip: %v\n", err)
			os.Exit(1)
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) == binaryName {
				rc, err := f.Open()
				if err != nil {
					fmt.Fprintf(os.Stderr, "ERROR: open zip file: %v\n", err)
					os.Exit(1)
				}
				var readErr error
				binaryBytes, readErr = io.ReadAll(rc)
				_ = rc.Close()
				if readErr != nil {
					fmt.Fprintf(os.Stderr, "ERROR: read zip entry: %v\n", readErr)
					os.Exit(1)
				}
				break
			}
		}
	} else {
		gzr, err := gzip.NewReader(bytes.NewReader(assetBytes))
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: read gzip: %v\n", err)
			os.Exit(1)
		}
		tr := tar.NewReader(gzr)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			if filepath.Base(hdr.Name) == binaryName {
				var readErr error
				binaryBytes, readErr = io.ReadAll(tr)
				if readErr != nil {
					fmt.Fprintf(os.Stderr, "ERROR: read tar entry: %v\n", readErr)
					os.Exit(1)
				}
				break
			}
		}
	}

	if len(binaryBytes) == 0 {
		fmt.Fprintf(os.Stderr, "ERROR: binary %q not found in downloaded release archive\n", binaryName)
		os.Exit(1)
	}

	execPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: find current executable path: %v\n", err)
		os.Exit(1)
	}

	// Write the new binary to a temp file in the SAME directory as the
	// executable so the final swap is an atomic rename on the same volume.
	tmp, err := os.CreateTemp(filepath.Dir(execPath), filepath.Base(execPath)+".tmp-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: create temp file for updated binary: %v\n", err)
		os.Exit(1)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(binaryBytes); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		fmt.Fprintf(os.Stderr, "ERROR: write updated binary: %v\n", err)
		os.Exit(1)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		fmt.Fprintf(os.Stderr, "ERROR: close temp file: %v\n", err)
		os.Exit(1)
	}
	if err := os.Chmod(tmpPath, 0755); err != nil {
		_ = os.Remove(tmpPath)
		fmt.Fprintf(os.Stderr, "ERROR: set permissions on updated binary: %v\n", err)
		os.Exit(1)
	}

	deferredMsg, err := replaceExecutable(execPath, tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		fmt.Fprintf(os.Stderr, "ERROR: install updated binary: %v\n", err)
		os.Exit(1)
	}
	if deferredMsg != "" {
		// The swap is deferred: the helper runs after this process exits and
		// records the outcome in the result marker. Do not print a SUCCESS
		// line until that marker reads OK.
		fmt.Println(deferredMsg)
		os.Exit(0)
	}

	fmt.Printf("\nSUCCESS: freebuff-proxy updated to %s!\n", rel.TagName)
	fmt.Println("Please restart freebuff-proxy to run the new version.")
	os.Exit(0)
}

// reportUpdateResultMarker consumes the result marker left by a deferred
// Windows swap from a previous -update run: it prints the recorded outcome
// ("OK" or "FAILED: ...", see update_swap.go) and deletes the marker so it is
// reported exactly once. No-op when no marker exists. Returns the marker
// contents ("" when none) for tests.
func reportUpdateResultMarker(execPath string) string {
	marker := updateResultMarker(execPath)
	data, err := os.ReadFile(marker)
	if err != nil {
		return "" // no stale marker: no deferred swap, or already reported
	}
	result := strings.TrimSpace(string(data))
	if result != "" {
		fmt.Printf("Previous deferred update result: %s\n", result)
	}
	_ = os.Remove(marker)
	return result
}

func downloadURL(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// verifyChecksum downloads checksums.txt and confirms it lists the sha256 of
// assetBytes. The update must never proceed unverified (see
// .github/SECURITY.md), so a checksums.txt fetch failure aborts with an
// error instead of being silently skipped.
func verifyChecksum(ctx context.Context, client *http.Client, checksumURL string, assetBytes []byte) error {
	checksumBytes, err := downloadURL(ctx, client, checksumURL)
	if err != nil {
		return fmt.Errorf("download checksums.txt: %w", err)
	}
	computed := sha256.Sum256(assetBytes)
	computedHex := hex.EncodeToString(computed[:])
	if !strings.Contains(string(checksumBytes), computedHex) {
		return fmt.Errorf("checksum mismatch! Calculated: %s", computedHex)
	}
	return nil
}

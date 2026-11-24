package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const currentVersion = "2026.11.24.3"
const maxUpdateSize = 256 << 20

type updatePackage struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type updateManifest struct {
	Version   string                   `json:"version"`
	Platforms map[string]updatePackage `json:"platforms"`
}

type updateSettings struct {
	Manifest string `json:"manifest"`
}

type updateRun struct {
	manifestURL string
	currentPath string
	client      *http.Client
	verify      func(string) error
}

func validUpdateURL(value *url.URL) bool {
	if value == nil || value.User != nil {
		return false
	}
	if value.Scheme == "https" {
		return true
	}
	return value.Scheme == "http" && (value.Hostname() == "127.0.0.1" || value.Hostname() == "localhost" || value.Hostname() == "::1")
}

func fetchUpdate(ctx context.Context, client *http.Client, address string, limit int64) ([]byte, string, error) {
	parsed, err := url.Parse(address)
	if err != nil || !validUpdateURL(parsed) {
		return nil, "", fmt.Errorf("only HTTPS update URLs are allowed")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("Update HTTP %d", response.StatusCode)
	}
	if !validUpdateURL(response.Request.URL) {
		return nil, "", fmt.Errorf("unsafe update redirect")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > limit {
		return nil, "", fmt.Errorf("update file is too large")
	}
	return data, response.Request.URL.String(), nil
}

func newerVersion(candidate, current string) (bool, error) {
	parse := func(value string) ([]int, error) {
		parts := strings.Split(value, ".")
		if len(parts) == 0 || len(parts) > 8 {
			return nil, fmt.Errorf("invalid version number: %s", value)
		}
		result := make([]int, len(parts))
		for index, part := range parts {
			number, err := strconv.Atoi(part)
			if err != nil || number < 0 {
				return nil, fmt.Errorf("invalid version number: %s", value)
			}
			result[index] = number
		}
		return result, nil
	}
	left, err := parse(candidate)
	if err != nil {
		return false, err
	}
	right, err := parse(current)
	if err != nil {
		return false, err
	}
	for index := 0; index < max(len(left), len(right)); index++ {
		a, b := 0, 0
		if index < len(left) {
			a = left[index]
		}
		if index < len(right) {
			b = right[index]
		}
		if a != b {
			return a > b, nil
		}
	}
	return false, nil
}

func unpackUpdate(data []byte) ([]byte, error) {
	return unpackUpdateForPlatform(data, runtime.GOOS, runtime.GOARCH)
}

func unpackUpdateForPlatform(data []byte, goos, goarch string) ([]byte, error) {
	compressed, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("update is not a gzip tarball: %w", err)
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	var binary []byte
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		clean := path.Clean(header.Name)
		executableName := "spotirip-lossless"
		if goos == "windows" {
			executableName += ".exe"
		}
		if clean != executableName && clean != fmt.Sprintf("spotirip-lossless-%s-%s/%s", goos, goarch, executableName) {
			continue
		}
		if header.Typeflag != tar.TypeReg || binary != nil {
			return nil, fmt.Errorf("invalid executable in update package")
		}
		binary, err = io.ReadAll(io.LimitReader(reader, maxUpdateSize+1))
		if err != nil {
			return nil, err
		}
		if len(binary) > maxUpdateSize {
			return nil, fmt.Errorf("update executable is too large")
		}
	}
	if len(binary) == 0 {
		return nil, fmt.Errorf("no executable found in update package")
	}
	return binary, nil
}

func performUpdate(ctx context.Context, run updateRun) (bool, error) {
	if run.client == nil {
		run.client = &http.Client{Timeout: 60 * time.Second}
	}
	data, finalManifest, err := fetchUpdate(ctx, run.client, run.manifestURL, 1<<20)
	if err != nil {
		return false, fmt.Errorf("Update-Manifest: %w", err)
	}
	var manifest updateManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return false, fmt.Errorf("invalid update manifest: %w", err)
	}
	newer, err := newerVersion(manifest.Version, currentVersion)
	if err != nil {
		return false, err
	}
	if !newer {
		return false, nil
	}
	platform := manifest.Platforms[runtime.GOOS+"-"+runtime.GOARCH]
	expected, err := hex.DecodeString(platform.SHA256)
	if err != nil || len(expected) != sha256.Size {
		return false, fmt.Errorf("SHA-256 is missing or invalid in update manifest")
	}
	manifestAddress, err := url.Parse(finalManifest)
	if err != nil {
		return false, err
	}
	packageAddress, err := url.Parse(platform.URL)
	if err != nil {
		return false, err
	}
	packageURL := manifestAddress.ResolveReference(packageAddress).String()
	archive, _, err := fetchUpdate(ctx, run.client, packageURL, maxUpdateSize)
	if err != nil {
		return false, fmt.Errorf("update package: %w", err)
	}
	if platform.Size > 0 && int64(len(archive)) != platform.Size {
		return false, fmt.Errorf("update size mismatch: got %d bytes, expected %d", len(archive), platform.Size)
	}
	if sum := sha256.Sum256(archive); !bytes.Equal(sum[:], expected) {
		return false, fmt.Errorf("update package SHA-256 mismatch")
	}
	binary, err := unpackUpdate(archive)
	if err != nil {
		return false, err
	}
	directory := filepath.Dir(run.currentPath)
	temporary, err := os.CreateTemp(directory, ".spotirip-lossless-update-*")
	if err != nil {
		return false, fmt.Errorf("creating update file in program directory: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(binary); err != nil {
		temporary.Close()
		return false, err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return false, err
	}
	if err := temporary.Chmod(0o755); err != nil {
		temporary.Close()
		return false, err
	}
	if err := temporary.Close(); err != nil {
		return false, err
	}
	if run.verify != nil {
		if err := run.verify(temporaryPath); err != nil {
			return false, fmt.Errorf("new executable failed the self-test: %w", err)
		}
	}
	backup := fmt.Sprintf("%s.backup-%d", run.currentPath, time.Now().UnixNano())
	if err := os.Rename(run.currentPath, backup); err != nil {
		return false, fmt.Errorf("backing up current executable: %w", err)
	}
	if err := os.Rename(temporaryPath, run.currentPath); err != nil {
		if restoreErr := os.Rename(backup, run.currentPath); restoreErr != nil {
			return false, fmt.Errorf("update failed: %v; rollback failed: %v (backup: %s)", err, restoreErr, backup)
		}
		return false, fmt.Errorf("update failed and was rolled back: %w", err)
	}
	fmt.Printf("Installed version %s. Backup: %s\n", manifest.Version, backup)
	return true, nil
}

func updateSettingsPath(options cliOptions) string {
	return ".spotirip-update.json"
}

func loadUpdateSettings(options cliOptions) updateSettings {
	var settings updateSettings
	data, err := os.ReadFile(updateSettingsPath(options))
	if err == nil {
		_ = json.Unmarshal(data, &settings)
	}
	if settings.Manifest == "" {
		settings.Manifest = os.Getenv("SPOTIRIP_LOSSLESS_UPDATE_URL")
	}
	return settings
}

func saveUpdateSettings(options cliOptions) error {
	data, err := json.MarshalIndent(updateSettings{Manifest: options.updateManifest}, "", "  ")
	if err != nil {
		return err
	}
	path := updateSettingsPath(options)
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func runSelfUpdate(ctx context.Context, options cliOptions) (bool, error) {
	settings := loadUpdateSettings(options)
	if options.updateManifest == "" {
		options.updateManifest = settings.Manifest
	}
	if options.updateManifest == "" {
		return false, fmt.Errorf("update manifest missing: use --update-manifest URL or SPOTIRIP_LOSSLESS_UPDATE_URL")
	}
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	verify := func(path string) error {
		command := exec.CommandContext(ctx, path, "--self-test", "--credentials", options.credentialPath)
		output, err := command.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w\n%s", err, output)
		}
		return nil
	}
	updated, err := performUpdate(ctx, updateRun{manifestURL: options.updateManifest, currentPath: executable, verify: verify})
	if err != nil {
		return false, err
	}
	if err := saveUpdateSettings(options); err != nil {
		return updated, fmt.Errorf("saving update settings: %w", err)
	}
	if !updated {
		fmt.Printf("Version %s is already up to date.\n", currentVersion)
	}
	return updated, nil
}

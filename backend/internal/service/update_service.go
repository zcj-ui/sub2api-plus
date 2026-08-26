package service

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"golang.org/x/mod/semver"
)

var (
	ErrNoUpdateAvailable                = infraerrors.Conflict("ALREADY_UP_TO_DATE", "no update available; current version is latest")
	ErrRollbackVersionNotAllowed        = infraerrors.BadRequest("ROLLBACK_VERSION_NOT_ALLOWED", "version is not in the allowed rollback list")
	ErrInPlaceUpdateUnsupportedPlatform = infraerrors.Conflict(
		"UPDATE_IN_PLACE_UNSUPPORTED_PLATFORM",
		"in-place updates are supported only on Linux binary deployments; use the normal update procedure for this deployment",
	)
	ErrInPlaceUpdateUnsupportedContainer = infraerrors.Conflict(
		"UPDATE_IN_PLACE_UNSUPPORTED_CONTAINER",
		"in-place updates are unavailable in container deployments; pull the target image and recreate the container using deployment tooling",
	)
	ErrUpdateAssetNotAvailable = infraerrors.Conflict(
		"UPDATE_ASSET_NOT_AVAILABLE",
		"the selected release has no compatible update asset for this deployment",
	)
	ErrUpdateAssetInvalid = infraerrors.Conflict(
		"UPDATE_ASSET_INVALID",
		"the selected release asset cannot be used for an in-place update",
	)
	ErrUpdateDownloadFailed = infraerrors.ServiceUnavailable(
		"UPDATE_DOWNLOAD_FAILED",
		"failed to download the release asset; verify network or update-proxy access and retry",
	)
	ErrUpdateChecksumDownloadFailed = infraerrors.ServiceUnavailable(
		"UPDATE_CHECKSUM_DOWNLOAD_FAILED",
		"failed to download release checksums; verify network or update-proxy access and retry",
	)
	ErrUpdateChecksumVerificationFailed = infraerrors.Conflict(
		"UPDATE_CHECKSUM_VERIFICATION_FAILED",
		"release asset checksum verification failed; retry the update or verify the release",
	)
	ErrUpdateArchiveInvalid = infraerrors.Conflict(
		"UPDATE_ARCHIVE_INVALID",
		"release asset could not be unpacked as an executable",
	)
	ErrUpdateRollbackBackupNotAvailable = infraerrors.Conflict(
		"UPDATE_ROLLBACK_BACKUP_NOT_AVAILABLE",
		"no local update backup is available; select a published rollback version or use deployment tooling",
	)
	ErrUpdateReleaseLookupFailed = infraerrors.ServiceUnavailable(
		"UPDATE_RELEASE_LOOKUP_FAILED",
		"failed to retrieve release information; verify network or update-proxy access and retry",
	)
	ErrUpdateFilesystemOperationFailed = infraerrors.InternalServer(
		"UPDATE_FILESYSTEM_OPERATION_FAILED",
		"failed to modify the installed binary; verify the deployment directory and retry",
	)
)

const (
	updateCacheKey    = "update_check_cache"
	updateCacheTTL    = 1200 // 20 minutes
	defaultGitHubRepo = "zcj-ui/sub2api-plus"

	// Security: allowed download domains for updates
	allowedDownloadHost = "github.com"
	allowedAssetHost    = "objects.githubusercontent.com"

	// Security: max download size (500MB)
	maxDownloadSize = 500 * 1024 * 1024

	// Rollback: expose at most the 3 most recent versions older than current
	maxRollbackVersions = 3
	// Fetch a few extra releases so filtering (current/newer/prerelease) still leaves enough candidates
	rollbackFetchPageSize = 15
)

func updateFilesystemError(operation, directory string, err error) error {
	wrapped := fmt.Errorf("%s in %s: %w", operation, filepath.Clean(directory), err)
	if errors.Is(err, os.ErrPermission) {
		return infraerrors.Conflict(
			"UPDATE_DIRECTORY_NOT_WRITABLE",
			"the service user cannot write to the update directory; fix its ownership or permissions and retry",
		).WithCause(wrapped)
	}
	return infraerrors.Clone(ErrUpdateFilesystemOperationFailed).WithCause(wrapped)
}

// UpdateCache defines cache operations for update service
type UpdateCache interface {
	GetUpdateInfo(ctx context.Context) (string, error)
	SetUpdateInfo(ctx context.Context, data string, ttl time.Duration) error
}

// GitHubReleaseClient 获取 GitHub release 信息的接口
type GitHubReleaseClient interface {
	FetchLatestRelease(ctx context.Context, repo string) (*GitHubRelease, error)
	FetchRecentReleases(ctx context.Context, repo string, perPage int) ([]*GitHubRelease, error)
	DownloadFile(ctx context.Context, url, dest string, maxSize int64) error
	FetchChecksumFile(ctx context.Context, url string) ([]byte, error)
}

// UpdateService handles software updates
type UpdateService struct {
	cache          UpdateCache
	githubClient   GitHubReleaseClient
	currentVersion string
	buildType      string // "source" for manual builds, "dev" for snapshots, "release" for stable builds
	updateRepo     string
	runtimeInfo    func() updateRuntimeInfo
}

type updateRuntimeInfo struct {
	goos          string
	containerized bool
}

// NewUpdateService creates a new UpdateService
func NewUpdateService(cache UpdateCache, githubClient GitHubReleaseClient, version, buildType, updateRepo string) *UpdateService {
	return &UpdateService{
		cache:          cache,
		githubClient:   githubClient,
		currentVersion: version,
		buildType:      buildType,
		updateRepo:     normalizeUpdateRepo(updateRepo),
		runtimeInfo:    detectUpdateRuntime,
	}
}

// InPlaceUpdateCapability tells the UI whether this process can safely replace
// its own executable. It is intentionally separate from HasUpdate: an update
// can exist even when the current deployment must use Docker or platform
// tooling instead of the in-process updater.
type InPlaceUpdateCapability struct {
	Supported          bool   `json:"supported"`
	RestrictionReason  string `json:"restriction_reason,omitempty"`
	RestrictionMessage string `json:"restriction_message,omitempty"`
}

func (s *UpdateService) currentUpdateRuntime() updateRuntimeInfo {
	if s != nil && s.runtimeInfo != nil {
		return s.runtimeInfo()
	}
	return detectUpdateRuntime()
}

func detectUpdateRuntime() updateRuntimeInfo {
	info := updateRuntimeInfo{goos: runtime.GOOS}
	if info.goos == "linux" {
		info.containerized = isContainerizedRuntime()
	}
	return info
}

func isContainerizedRuntime() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}

	if strings.TrimSpace(os.Getenv("container")) != "" ||
		strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST")) != "" {
		return true
	}

	cgroup, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	lower := strings.ToLower(string(cgroup))
	for _, marker := range []string{"docker", "containerd", "kubepods", "libpod", "podman"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func (s *UpdateService) inPlaceUpdateCapability() InPlaceUpdateCapability {
	runtimeInfo := s.currentUpdateRuntime()
	if runtimeInfo.goos != "linux" {
		return InPlaceUpdateCapability{
			Supported:          false,
			RestrictionReason:  "UPDATE_IN_PLACE_UNSUPPORTED_PLATFORM",
			RestrictionMessage: "In-place updates are supported only on Linux binary deployments. Use this deployment's normal update procedure.",
		}
	}
	if runtimeInfo.containerized {
		return InPlaceUpdateCapability{
			Supported:          false,
			RestrictionReason:  "UPDATE_IN_PLACE_UNSUPPORTED_CONTAINER",
			RestrictionMessage: "In-place updates are unavailable in containers. Pull the target image and recreate the container using deployment tooling.",
		}
	}
	return InPlaceUpdateCapability{Supported: true}
}

func (s *UpdateService) requireInPlaceUpdateSupport() error {
	capability := s.inPlaceUpdateCapability()
	if capability.Supported {
		return nil
	}
	if capability.RestrictionReason == "UPDATE_IN_PLACE_UNSUPPORTED_CONTAINER" {
		return infraerrors.Clone(ErrInPlaceUpdateUnsupportedContainer)
	}
	return infraerrors.Clone(ErrInPlaceUpdateUnsupportedPlatform)
}

// UpdateInfo contains update information
type UpdateInfo struct {
	CurrentVersion string                  `json:"current_version"`
	LatestVersion  string                  `json:"latest_version"`
	HasUpdate      bool                    `json:"has_update"`
	ReleaseInfo    *ReleaseInfo            `json:"release_info,omitempty"`
	Cached         bool                    `json:"cached"`
	Warning        string                  `json:"warning,omitempty"`
	BuildType      string                  `json:"build_type"` // "source", "dev", or "release"
	UpdateRepo     string                  `json:"update_repo"`
	InPlaceUpdate  InPlaceUpdateCapability `json:"in_place_update"`
}

// ReleaseInfo contains GitHub release details
type ReleaseInfo struct {
	Name        string  `json:"name"`
	Body        string  `json:"body"`
	PublishedAt string  `json:"published_at"`
	HTMLURL     string  `json:"html_url"`
	Assets      []Asset `json:"assets,omitempty"`
}

// Asset represents a release asset
type Asset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"download_url"`
	Size        int64  `json:"size"`
}

// GitHubRelease represents GitHub API response
type GitHubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	Body        string        `json:"body"`
	PublishedAt string        `json:"published_at"`
	HTMLURL     string        `json:"html_url"`
	Draft       bool          `json:"draft"`
	Prerelease  bool          `json:"prerelease"`
	Assets      []GitHubAsset `json:"assets"`
}

// RollbackVersion describes a release version the system can roll back to
type RollbackVersion struct {
	Version     string `json:"version"` // without "v" prefix, e.g. "0.1.146"
	PublishedAt string `json:"published_at"`
	HTMLURL     string `json:"html_url"`
}

type GitHubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// CheckUpdate checks for available updates
func (s *UpdateService) CheckUpdate(ctx context.Context, force bool) (*UpdateInfo, error) {
	// Try cache first
	if !force {
		if cached, err := s.getFromCache(ctx); err == nil && cached != nil {
			return cached, nil
		}
	}

	// Fetch from GitHub
	info, err := s.fetchLatestRelease(ctx)
	if err != nil {
		// Return cached on error
		if cached, cacheErr := s.getFromCache(ctx); cacheErr == nil && cached != nil {
			cached.Warning = "Using cached data: " + updateCheckWarning(err)
			return cached, nil
		}
		return &UpdateInfo{
			CurrentVersion: s.currentVersion,
			LatestVersion:  s.currentVersion,
			HasUpdate:      false,
			Warning:        updateCheckWarning(err),
			BuildType:      s.buildType,
			UpdateRepo:     s.updateRepo,
			InPlaceUpdate:  s.inPlaceUpdateCapability(),
		}, nil
	}

	// Cache result
	s.saveToCache(ctx, info)
	return info, nil
}

func updateCheckWarning(err error) string {
	if err == nil {
		return ""
	}
	if message := strings.TrimSpace(infraerrors.Message(err)); message != "" &&
		infraerrors.Reason(err) != "" {
		return message
	}
	return "unable to check for updates; verify network or update-proxy access and retry"
}

// PerformUpdate downloads and applies the update
// Uses atomic file replacement pattern for safe in-place updates
func (s *UpdateService) PerformUpdate(ctx context.Context) error {
	if err := s.requireInPlaceUpdateSupport(); err != nil {
		return err
	}

	info, err := s.CheckUpdate(ctx, true)
	if err != nil {
		return err
	}

	if !info.HasUpdate {
		return ErrNoUpdateAvailable
	}

	return s.applyReleaseAssets(ctx, info.ReleaseInfo.Assets)
}

// applyReleaseAssets downloads the platform archive from the given release assets,
// verifies its checksum, and atomically swaps the running binary.
// Shared by PerformUpdate (latest) and RollbackToVersion (specific older version).
func (s *UpdateService) applyReleaseAssets(ctx context.Context, releaseAssets []Asset) error {
	// Find matching archive and checksum for current platform
	archiveName := s.getArchiveName()
	var downloadURL string
	var checksumURL string

	for _, asset := range releaseAssets {
		if strings.Contains(asset.Name, archiveName) && !strings.HasSuffix(asset.Name, ".txt") {
			downloadURL = asset.DownloadURL
		}
		if asset.Name == "checksums.txt" {
			checksumURL = asset.DownloadURL
		}
	}

	if downloadURL == "" {
		return infraerrors.Clone(ErrUpdateAssetNotAvailable)
	}

	// SECURITY: Validate download URL is from trusted domain
	if err := validateDownloadURL(downloadURL); err != nil {
		return infraerrors.Clone(ErrUpdateAssetInvalid).WithCause(err)
	}
	if checksumURL != "" {
		if err := validateDownloadURL(checksumURL); err != nil {
			return infraerrors.Clone(ErrUpdateAssetInvalid).WithCause(err)
		}
	}

	// Get current executable path
	exePath, err := os.Executable()
	if err != nil {
		return updateFilesystemError("failed to determine the executable path", ".", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return updateFilesystemError("failed to resolve the executable path", filepath.Dir(exePath), err)
	}

	exeDir := filepath.Dir(exePath)

	// Create temp directory in the SAME directory as executable
	// This ensures os.Rename is atomic (same filesystem)
	tempDir, err := os.MkdirTemp(exeDir, ".sub2api-update-*")
	if err != nil {
		return updateFilesystemError("failed to create update temp directory", exeDir, err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	// Download archive
	archivePath := filepath.Join(tempDir, filepath.Base(downloadURL))
	if err := s.downloadFile(ctx, downloadURL, archivePath); err != nil {
		return err
	}

	// Verify checksum if available
	if checksumURL != "" {
		if err := s.verifyChecksum(ctx, archivePath, checksumURL); err != nil {
			return err
		}
	}

	// Extract binary from archive
	newBinaryPath := filepath.Join(tempDir, "sub2api")
	if err := s.extractBinary(archivePath, newBinaryPath); err != nil {
		return err
	}

	// Set executable permission before replacement
	if err := os.Chmod(newBinaryPath, 0755); err != nil {
		return updateFilesystemError("failed to set executable permissions", exeDir, err)
	}

	// Atomic replacement using rename pattern:
	// 1. Rename current -> backup (atomic on Unix)
	// 2. Rename new -> current (atomic on Unix, same filesystem)
	// If step 2 fails, restore backup
	backupPath := exePath + ".backup"

	// Remove old backup if exists. A stale, non-removable backup otherwise makes
	// the following rename fail with a misleading error.
	if err := os.Remove(backupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return updateFilesystemError("failed to remove the previous update backup", exeDir, err)
	}

	// Step 1: Move current binary to backup
	if err := os.Rename(exePath, backupPath); err != nil {
		return updateFilesystemError("failed to back up the current binary", exeDir, err)
	}

	// Step 2: Move new binary to target location (atomic, same filesystem)
	if err := os.Rename(newBinaryPath, exePath); err != nil {
		// Restore backup on failure
		if restoreErr := os.Rename(backupPath, exePath); restoreErr != nil {
			return infraerrors.Clone(ErrUpdateFilesystemOperationFailed).WithCause(
				fmt.Errorf("replace failed: %w; restore failed: %v", err, restoreErr),
			)
		}
		return updateFilesystemError("failed to replace the binary; the backup was restored", exeDir, err)
	}

	// Success - backup file is kept for rollback capability
	// It will be cleaned up on next successful update
	return nil
}

// Rollback restores the previous version
func (s *UpdateService) Rollback() error {
	if err := s.requireInPlaceUpdateSupport(); err != nil {
		return err
	}

	exePath, err := os.Executable()
	if err != nil {
		return updateFilesystemError("failed to determine the executable path", ".", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return updateFilesystemError("failed to resolve the executable path", filepath.Dir(exePath), err)
	}

	backupFile := exePath + ".backup"
	if _, err := os.Stat(backupFile); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return infraerrors.Clone(ErrUpdateRollbackBackupNotAvailable)
		}
		return updateFilesystemError("failed to inspect the local update backup", filepath.Dir(exePath), err)
	}

	// Replace current with backup
	if err := os.Rename(backupFile, exePath); err != nil {
		return updateFilesystemError("failed to restore the backup binary", filepath.Dir(exePath), err)
	}

	return nil
}

// ListRollbackVersions returns up to maxRollbackVersions release versions that are
// strictly older than the current version (the current version itself is excluded),
// newest first. Draft and prerelease entries are skipped.
func (s *UpdateService) ListRollbackVersions(ctx context.Context) ([]RollbackVersion, error) {
	releases, err := s.fetchRollbackCandidates(ctx)
	if err != nil {
		return nil, err
	}

	versions := make([]RollbackVersion, 0, len(releases))
	for _, r := range releases {
		versions = append(versions, RollbackVersion{
			Version:     strings.TrimPrefix(r.TagName, "v"),
			PublishedAt: r.PublishedAt,
			HTMLURL:     r.HTMLURL,
		})
	}
	return versions, nil
}

// RollbackToVersion downloads and installs a specific older version.
// The target must be one of the versions returned by ListRollbackVersions;
// anything else (including the current version) is rejected.
func (s *UpdateService) RollbackToVersion(ctx context.Context, version string) error {
	if err := s.requireInPlaceUpdateSupport(); err != nil {
		return err
	}

	target := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if target == "" {
		return ErrRollbackVersionNotAllowed
	}

	releases, err := s.fetchRollbackCandidates(ctx)
	if err != nil {
		return err
	}

	var match *GitHubRelease
	for _, r := range releases {
		if strings.TrimPrefix(r.TagName, "v") == target {
			match = r
			break
		}
	}
	if match == nil {
		return ErrRollbackVersionNotAllowed
	}

	assets := make([]Asset, len(match.Assets))
	for i, a := range match.Assets {
		assets[i] = Asset{
			Name:        a.Name,
			DownloadURL: a.BrowserDownloadURL,
			Size:        a.Size,
		}
	}

	return s.applyReleaseAssets(ctx, assets)
}

// fetchRollbackCandidates fetches recent releases and keeps the newest
// maxRollbackVersions entries strictly older than the current version.
func (s *UpdateService) fetchRollbackCandidates(ctx context.Context) ([]*GitHubRelease, error) {
	releases, err := s.githubClient.FetchRecentReleases(ctx, s.updateRepo, rollbackFetchPageSize)
	if err != nil {
		return nil, infraerrors.Clone(ErrUpdateReleaseLookupFailed).WithCause(err)
	}

	seen := make(map[string]bool, len(releases))
	candidates := make([]*GitHubRelease, 0, maxRollbackVersions)
	for _, r := range releases {
		if r == nil || r.Draft || r.Prerelease {
			continue
		}
		v := strings.TrimPrefix(r.TagName, "v")
		if v == "" || seen[v] {
			continue
		}
		// Only versions strictly older than current (also excludes current itself)
		if compareVersions(v, s.currentVersion) >= 0 {
			continue
		}
		seen[v] = true
		candidates = append(candidates, r)
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return compareVersions(
			strings.TrimPrefix(candidates[i].TagName, "v"),
			strings.TrimPrefix(candidates[j].TagName, "v"),
		) > 0
	})

	if len(candidates) > maxRollbackVersions {
		candidates = candidates[:maxRollbackVersions]
	}
	return candidates, nil
}

func (s *UpdateService) fetchLatestRelease(ctx context.Context) (*UpdateInfo, error) {
	release, err := s.githubClient.FetchLatestRelease(ctx, s.updateRepo)
	if err != nil {
		return nil, infraerrors.Clone(ErrUpdateReleaseLookupFailed).WithCause(err)
	}
	if release == nil {
		return nil, infraerrors.Clone(ErrUpdateReleaseLookupFailed).WithCause(
			fmt.Errorf("release lookup returned an empty release"),
		)
	}

	latestVersion := strings.TrimPrefix(release.TagName, "v")

	assets := make([]Asset, len(release.Assets))
	for i, a := range release.Assets {
		assets[i] = Asset{
			Name:        a.Name,
			DownloadURL: a.BrowserDownloadURL,
			Size:        a.Size,
		}
	}

	return &UpdateInfo{
		CurrentVersion: s.currentVersion,
		LatestVersion:  latestVersion,
		HasUpdate:      compareVersions(s.currentVersion, latestVersion) < 0,
		ReleaseInfo: &ReleaseInfo{
			Name:        release.Name,
			Body:        release.Body,
			PublishedAt: release.PublishedAt,
			HTMLURL:     release.HTMLURL,
			Assets:      assets,
		},
		Cached:        false,
		BuildType:     s.buildType,
		UpdateRepo:    s.updateRepo,
		InPlaceUpdate: s.inPlaceUpdateCapability(),
	}, nil
}

func (s *UpdateService) downloadFile(ctx context.Context, downloadURL, dest string) error {
	if err := s.githubClient.DownloadFile(ctx, downloadURL, dest, maxDownloadSize); err != nil {
		return infraerrors.Clone(ErrUpdateDownloadFailed).WithCause(err)
	}
	return nil
}

func (s *UpdateService) getArchiveName() string {
	osName := s.currentUpdateRuntime().goos
	arch := runtime.GOARCH
	return fmt.Sprintf("%s_%s", osName, arch)
}

// validateDownloadURL checks if the URL is from an allowed domain
// SECURITY: This prevents SSRF and ensures downloads only come from trusted GitHub domains
func validateDownloadURL(rawURL string) error {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	// Must be HTTPS
	if parsedURL.Scheme != "https" {
		return fmt.Errorf("only HTTPS URLs are allowed")
	}

	// Check against allowed hosts
	host := parsedURL.Host
	// GitHub release URLs can be from github.com or objects.githubusercontent.com
	if host != allowedDownloadHost &&
		!strings.HasSuffix(host, "."+allowedDownloadHost) &&
		host != allowedAssetHost &&
		!strings.HasSuffix(host, "."+allowedAssetHost) {
		return fmt.Errorf("download from untrusted host: %s", host)
	}

	return nil
}

func (s *UpdateService) verifyChecksum(ctx context.Context, filePath, checksumURL string) error {
	// Download checksums file
	checksumData, err := s.githubClient.FetchChecksumFile(ctx, checksumURL)
	if err != nil {
		return infraerrors.Clone(ErrUpdateChecksumDownloadFailed).WithCause(err)
	}

	// Calculate file hash
	f, err := os.Open(filePath)
	if err != nil {
		return infraerrors.Clone(ErrUpdateChecksumVerificationFailed).WithCause(err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return infraerrors.Clone(ErrUpdateChecksumVerificationFailed).WithCause(err)
	}
	actualHash := hex.EncodeToString(h.Sum(nil))

	// Find expected hash in checksums file
	fileName := filepath.Base(filePath)
	scanner := bufio.NewScanner(strings.NewReader(string(checksumData)))
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == fileName {
			if parts[0] == actualHash {
				return nil
			}
			return infraerrors.Clone(ErrUpdateChecksumVerificationFailed).WithCause(
				fmt.Errorf("checksum mismatch for %s", fileName),
			)
		}
	}
	if err := scanner.Err(); err != nil {
		return infraerrors.Clone(ErrUpdateChecksumVerificationFailed).WithCause(err)
	}

	return infraerrors.Clone(ErrUpdateChecksumVerificationFailed).WithCause(
		fmt.Errorf("checksum not found for %s", fileName),
	)
}

func (s *UpdateService) extractBinary(archivePath, destPath string) (err error) {
	defer func() {
		if err != nil {
			err = infraerrors.Clone(ErrUpdateArchiveInvalid).WithCause(err)
		}
	}()

	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	var reader io.Reader = f

	// Handle gzip compression
	if strings.HasSuffix(archivePath, ".gz") || strings.HasSuffix(archivePath, ".tar.gz") || strings.HasSuffix(archivePath, ".tgz") {
		gzr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer func() { _ = gzr.Close() }()
		reader = gzr
	}

	// Handle tar archive
	if strings.Contains(archivePath, ".tar") {
		tr := tar.NewReader(reader)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}

			// SECURITY: Prevent Zip Slip / Path Traversal attack
			// Only allow files with safe base names, no directory traversal
			baseName := filepath.Base(hdr.Name)

			// Check for path traversal attempts
			if strings.Contains(hdr.Name, "..") {
				return fmt.Errorf("path traversal attempt detected: %s", hdr.Name)
			}

			// Validate the entry is a regular file
			if hdr.Typeflag != tar.TypeReg {
				continue // Skip directories and special files
			}

			// Only extract the specific binary we need
			if baseName == "sub2api" || baseName == "sub2api.exe" {
				// Additional security: limit file size (max 500MB)
				const maxBinarySize = 500 * 1024 * 1024
				if hdr.Size > maxBinarySize {
					return fmt.Errorf("binary too large: %d bytes (max %d)", hdr.Size, maxBinarySize)
				}

				out, err := os.Create(destPath)
				if err != nil {
					return err
				}

				// Use LimitReader to prevent decompression bombs
				limited := io.LimitReader(tr, maxBinarySize)
				if _, err := io.Copy(out, limited); err != nil {
					_ = out.Close()
					return err
				}
				if err := out.Close(); err != nil {
					return err
				}
				return nil
			}
		}
		return fmt.Errorf("binary not found in archive")
	}

	// Direct copy for non-tar files (with size limit)
	const maxBinarySize = 500 * 1024 * 1024
	out, err := os.Create(destPath)
	if err != nil {
		return err
	}

	limited := io.LimitReader(reader, maxBinarySize)
	if _, err := io.Copy(out, limited); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func (s *UpdateService) getFromCache(ctx context.Context) (*UpdateInfo, error) {
	data, err := s.cache.GetUpdateInfo(ctx)
	if err != nil {
		return nil, err
	}

	var cached struct {
		Latest      string       `json:"latest"`
		ReleaseInfo *ReleaseInfo `json:"release_info"`
		Timestamp   int64        `json:"timestamp"`
		UpdateRepo  string       `json:"update_repo"`
	}
	if err := json.Unmarshal([]byte(data), &cached); err != nil {
		return nil, err
	}

	if time.Now().Unix()-cached.Timestamp > updateCacheTTL {
		return nil, fmt.Errorf("cache expired")
	}
	if cached.UpdateRepo != s.updateRepo {
		return nil, fmt.Errorf("cache belongs to a different update repository")
	}

	return &UpdateInfo{
		CurrentVersion: s.currentVersion,
		LatestVersion:  cached.Latest,
		HasUpdate:      compareVersions(s.currentVersion, cached.Latest) < 0,
		ReleaseInfo:    cached.ReleaseInfo,
		Cached:         true,
		BuildType:      s.buildType,
		UpdateRepo:     s.updateRepo,
		InPlaceUpdate:  s.inPlaceUpdateCapability(),
	}, nil
}

func (s *UpdateService) saveToCache(ctx context.Context, info *UpdateInfo) {
	cacheData := struct {
		Latest      string       `json:"latest"`
		ReleaseInfo *ReleaseInfo `json:"release_info"`
		Timestamp   int64        `json:"timestamp"`
		UpdateRepo  string       `json:"update_repo"`
	}{
		Latest:      info.LatestVersion,
		ReleaseInfo: info.ReleaseInfo,
		Timestamp:   time.Now().Unix(),
		UpdateRepo:  s.updateRepo,
	}

	data, _ := json.Marshal(cacheData)
	_ = s.cache.SetUpdateInfo(ctx, string(data), time.Duration(updateCacheTTL)*time.Second)
}

// compareVersions compares two semantic versions
func compareVersions(current, latest string) int {
	currentSemver := normalizeSemver(current)
	latestSemver := normalizeSemver(latest)
	if currentSemver != "" && latestSemver != "" {
		return semver.Compare(currentSemver, latestSemver)
	}

	currentParts := parseVersion(current)
	latestParts := parseVersion(latest)

	for i := 0; i < 3; i++ {
		if currentParts[i] < latestParts[i] {
			return -1
		}
		if currentParts[i] > latestParts[i] {
			return 1
		}
	}
	return 0
}

func normalizeUpdateRepo(repo string) string {
	repo = strings.TrimSpace(repo)
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || !validGitHubRepoPart(parts[0]) || !validGitHubRepoPart(parts[1]) {
		return defaultGitHubRepo
	}
	return repo
}

func validGitHubRepoPart(part string) bool {
	if part == "" || part == "." || part == ".." {
		return false
	}
	for _, r := range part {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func parseVersion(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	result := [3]int{0, 0, 0}
	for i := 0; i < len(parts) && i < 3; i++ {
		if parsed, err := strconv.Atoi(parts[i]); err == nil {
			result[i] = parsed
		}
	}
	return result
}

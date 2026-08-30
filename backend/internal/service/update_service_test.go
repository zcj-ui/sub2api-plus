//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type updateServiceCacheStub struct {
	data string
}

func (s *updateServiceCacheStub) GetUpdateInfo(context.Context) (string, error) {
	if s.data == "" {
		return "", errors.New("cache miss")
	}
	return s.data, nil
}

func (s *updateServiceCacheStub) SetUpdateInfo(_ context.Context, data string, _ time.Duration) error {
	s.data = data
	return nil
}

type updateServiceGitHubClientStub struct {
	release        *GitHubRelease
	latestErr      error
	recentReleases []*GitHubRelease
	recentErr      error
	latestRepo     string
	recentRepo     string
	downloadErr    error
	checksumData   []byte
	checksumErr    error
}

func (s *updateServiceGitHubClientStub) FetchLatestRelease(_ context.Context, repo string) (*GitHubRelease, error) {
	s.latestRepo = repo
	return s.release, s.latestErr
}

func (s *updateServiceGitHubClientStub) FetchRecentReleases(_ context.Context, repo string, _ int) ([]*GitHubRelease, error) {
	s.recentRepo = repo
	return s.recentReleases, s.recentErr
}

func (s *updateServiceGitHubClientStub) DownloadFile(context.Context, string, string, int64) error {
	if s.downloadErr != nil {
		return s.downloadErr
	}
	panic("DownloadFile should not be called when no update is available")
}

func (s *updateServiceGitHubClientStub) FetchChecksumFile(context.Context, string) ([]byte, error) {
	if s.checksumErr != nil {
		return nil, s.checksumErr
	}
	if s.checksumData != nil {
		return s.checksumData, nil
	}
	panic("FetchChecksumFile should not be called when no update is available")
}

func forceLinuxBinaryUpdateRuntime(svc *UpdateService) *UpdateService {
	svc.runtimeInfo = func() updateRuntimeInfo {
		return updateRuntimeInfo{goos: "linux"}
	}
	return svc
}

func TestUpdateServicePerformUpdateNoUpdateReturnsSentinel(t *testing.T) {
	svc := forceLinuxBinaryUpdateRuntime(NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{
			release: &GitHubRelease{
				TagName: "v0.1.132",
				Name:    "v0.1.132",
			},
		},
		"0.1.132",
		"release",
		"example/sub2api",
	))

	err := svc.PerformUpdate(context.Background())

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoUpdateAvailable))
	require.ErrorIs(t, err, ErrNoUpdateAvailable)
}

func TestUpdateFilesystemPermissionErrorIsActionable(t *testing.T) {
	directory := filepath.Join("opt", "sub2api")
	err := updateFilesystemError("failed to create update temp directory", directory, os.ErrPermission)

	require.Equal(t, http.StatusConflict, infraerrors.Code(err))
	require.Equal(t, "UPDATE_DIRECTORY_NOT_WRITABLE", infraerrors.Reason(err))
	require.NotContains(t, infraerrors.Message(err), filepath.Clean(directory))
	require.Contains(t, infraerrors.Message(err), "fix its ownership or permissions")
	require.ErrorIs(t, err, os.ErrPermission)
}

func TestUpdateFilesystemFailureDoesNotExposeCause(t *testing.T) {
	err := updateFilesystemError("failed to replace binary", "/srv/secret-deployment", errors.New("credential=hidden"))

	require.Equal(t, http.StatusInternalServerError, infraerrors.Code(err))
	require.Equal(t, "UPDATE_FILESYSTEM_OPERATION_FAILED", infraerrors.Reason(err))
	require.NotContains(t, infraerrors.Message(err), "secret-deployment")
	require.NotContains(t, infraerrors.Message(err), "credential=hidden")
}

func TestUpdateServiceInPlaceCapabilityIsRuntimeIndependent(t *testing.T) {
	tests := []struct {
		name                string
		runtime             updateRuntimeInfo
		wantSupported       bool
		wantRestrictionCode string
	}{
		{
			name:          "linux binary",
			runtime:       updateRuntimeInfo{goos: "linux"},
			wantSupported: true,
		},
		{
			name:                "linux container",
			runtime:             updateRuntimeInfo{goos: "linux", containerized: true},
			wantRestrictionCode: "UPDATE_IN_PLACE_UNSUPPORTED_CONTAINER",
		},
		{
			name:                "windows binary",
			runtime:             updateRuntimeInfo{goos: "windows"},
			wantRestrictionCode: "UPDATE_IN_PLACE_UNSUPPORTED_PLATFORM",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewUpdateService(
				&updateServiceCacheStub{},
				&updateServiceGitHubClientStub{release: &GitHubRelease{TagName: "v0.1.133"}},
				"0.1.132",
				"release",
				"example/sub2api",
			)
			svc.runtimeInfo = func() updateRuntimeInfo { return tt.runtime }

			info, err := svc.CheckUpdate(context.Background(), true)

			require.NoError(t, err)
			require.Equal(t, tt.wantSupported, info.InPlaceUpdate.Supported)
			require.Equal(t, tt.wantRestrictionCode, info.InPlaceUpdate.RestrictionReason)
			if tt.wantSupported {
				require.Empty(t, info.InPlaceUpdate.RestrictionMessage)
			} else {
				require.NotEmpty(t, info.InPlaceUpdate.RestrictionMessage)
			}
		})
	}
}

func TestUpdateServiceRejectsUnsupportedInPlaceRuntimeBeforeIO(t *testing.T) {
	tests := []struct {
		name    string
		runtime updateRuntimeInfo
		wantErr error
	}{
		{
			name:    "container",
			runtime: updateRuntimeInfo{goos: "linux", containerized: true},
			wantErr: ErrInPlaceUpdateUnsupportedContainer,
		},
		{
			name:    "platform",
			runtime: updateRuntimeInfo{goos: "darwin"},
			wantErr: ErrInPlaceUpdateUnsupportedPlatform,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewUpdateService(
				&updateServiceCacheStub{},
				&updateServiceGitHubClientStub{},
				"0.1.132",
				"release",
				"example/sub2api",
			)
			svc.runtimeInfo = func() updateRuntimeInfo { return tt.runtime }

			for _, operation := range []struct {
				name string
				run  func() error
			}{
				{name: "update", run: func() error { return svc.PerformUpdate(context.Background()) }},
				{name: "rollback", run: svc.Rollback},
				{name: "versioned rollback", run: func() error { return svc.RollbackToVersion(context.Background(), "0.1.131") }},
			} {
				t.Run(operation.name, func(t *testing.T) {
					err := operation.run()
					require.ErrorIs(t, err, tt.wantErr)
					require.Equal(t, http.StatusConflict, infraerrors.Code(err))
					require.NotContains(t, infraerrors.Message(err), "example/sub2api")
				})
			}
		})
	}
}

func TestUpdateServiceAssetAndTransportFailuresAreStructured(t *testing.T) {
	svc := forceLinuxBinaryUpdateRuntime(NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{},
		"0.1.132",
		"release",
		"example/sub2api",
	))

	err := svc.applyReleaseAssets(context.Background(), nil)
	require.ErrorIs(t, err, ErrUpdateAssetNotAvailable)
	require.Equal(t, "UPDATE_ASSET_NOT_AVAILABLE", infraerrors.Reason(err))

	err = svc.applyReleaseAssets(context.Background(), []Asset{{
		Name:        "sub2api_" + svc.getArchiveName() + ".tar.gz",
		DownloadURL: "http://untrusted.invalid/sub2api.tar.gz",
	}})
	require.ErrorIs(t, err, ErrUpdateAssetInvalid)
	require.Equal(t, "UPDATE_ASSET_INVALID", infraerrors.Reason(err))

	client := &updateServiceGitHubClientStub{downloadErr: errors.New("proxy-password=secret")}
	svc.githubClient = client
	err = svc.downloadFile(context.Background(), "https://github.com/example/asset", filepath.Join(t.TempDir(), "asset"))
	require.ErrorIs(t, err, ErrUpdateDownloadFailed)
	require.Equal(t, http.StatusServiceUnavailable, infraerrors.Code(err))
	require.Equal(t, "UPDATE_DOWNLOAD_FAILED", infraerrors.Reason(err))
	require.NotContains(t, infraerrors.Message(err), "proxy-password")
}

func TestUpdateServiceRejectsReleaseWithoutChecksumAsset(t *testing.T) {
	// The GitHub client stub panics if DownloadFile is reached.  Rejection must
	// happen while inspecting release metadata, before any executable bytes are
	// written to disk.
	svc := forceLinuxBinaryUpdateRuntime(NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{},
		"0.1.132",
		"release",
		"example/sub2api",
	))

	err := svc.applyReleaseAssets(context.Background(), []Asset{{
		Name:        "sub2api_linux_amd64.tar.gz",
		DownloadURL: "https://github.com/example/sub2api/releases/download/v0.1.133/sub2api_linux_amd64.tar.gz",
	}})

	require.ErrorIs(t, err, ErrUpdateChecksumVerificationFailed)
	require.Equal(t, "UPDATE_CHECKSUM_VERIFICATION_FAILED", infraerrors.Reason(err))
	require.NotContains(t, infraerrors.Message(err), "example/sub2api")
}

func TestUpdateServiceChecksumAndArchiveFailuresAreStructured(t *testing.T) {
	dir := t.TempDir()
	assetPath := filepath.Join(dir, "sub2api_linux_amd64.tar.gz")
	require.NoError(t, os.WriteFile(assetPath, []byte("not-an-archive"), 0600))

	svc := forceLinuxBinaryUpdateRuntime(NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{checksumErr: errors.New("proxy-token=secret")},
		"0.1.132",
		"release",
		"example/sub2api",
	))
	err := svc.verifyChecksum(context.Background(), assetPath, "https://github.com/example/checksums.txt")
	require.ErrorIs(t, err, ErrUpdateChecksumDownloadFailed)
	require.Equal(t, http.StatusServiceUnavailable, infraerrors.Code(err))
	require.Equal(t, "UPDATE_CHECKSUM_DOWNLOAD_FAILED", infraerrors.Reason(err))
	require.NotContains(t, infraerrors.Message(err), "proxy-token")

	svc.githubClient = &updateServiceGitHubClientStub{
		checksumData: []byte("0000000000000000000000000000000000000000000000000000000000000000  " + filepath.Base(assetPath) + "\n"),
	}
	err = svc.verifyChecksum(context.Background(), assetPath, "https://github.com/example/checksums.txt")
	require.ErrorIs(t, err, ErrUpdateChecksumVerificationFailed)
	require.Equal(t, http.StatusConflict, infraerrors.Code(err))
	require.Equal(t, "UPDATE_CHECKSUM_VERIFICATION_FAILED", infraerrors.Reason(err))

	err = svc.extractBinary(assetPath, filepath.Join(dir, "sub2api"))
	require.ErrorIs(t, err, ErrUpdateArchiveInvalid)
	require.Equal(t, http.StatusConflict, infraerrors.Code(err))
	require.Equal(t, "UPDATE_ARCHIVE_INVALID", infraerrors.Reason(err))
}

func TestUpdateServiceCheckFailureKeepsWarningActionableAndSecretFree(t *testing.T) {
	svc := forceLinuxBinaryUpdateRuntime(NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{latestErr: errors.New("proxy-password=secret")},
		"0.1.132",
		"release",
		"example/sub2api",
	))

	info, err := svc.CheckUpdate(context.Background(), true)

	require.NoError(t, err)
	require.False(t, info.HasUpdate)
	require.Equal(t, "0.1.132", info.CurrentVersion)
	require.Equal(t, "0.1.132", info.LatestVersion)
	require.Contains(t, info.Warning, "network or update-proxy")
	require.NotContains(t, info.Warning, "proxy-password")
}

func newRollbackTestService(current string, releases []*GitHubRelease) *UpdateService {
	return forceLinuxBinaryUpdateRuntime(NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentReleases: releases},
		current,
		"release",
		"example/sub2api",
	))
}

func TestUpdateServiceListRollbackVersionsFiltersAndCaps(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.148", PublishedAt: "2026-07-09T00:00:00Z"},                       // newer than current: excluded
		{TagName: "v0.1.147", PublishedAt: "2026-07-08T00:00:00Z"},                       // current: excluded
		{TagName: "v0.1.146-rc1", PublishedAt: "2026-07-07T12:00:00Z", Prerelease: true}, // prerelease: excluded
		{TagName: "v0.1.146", PublishedAt: "2026-07-07T00:00:00Z"},
		{TagName: "v0.1.145", PublishedAt: "2026-07-06T00:00:00Z", Draft: true}, // draft: excluded
		{TagName: "v0.1.144", PublishedAt: "2026-07-05T00:00:00Z"},
		{TagName: "v0.1.144", PublishedAt: "2026-07-05T00:00:00Z"}, // duplicate: excluded
		{TagName: "v0.1.143", PublishedAt: "2026-07-04T00:00:00Z"},
		{TagName: "v0.1.142", PublishedAt: "2026-07-03T00:00:00Z"}, // beyond cap of 3: excluded
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "0.1.146", versions[0].Version)
	require.Equal(t, "0.1.144", versions[1].Version)
	require.Equal(t, "0.1.143", versions[2].Version)
}

func TestUpdateServiceListRollbackVersionsSortsUnorderedInput(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.144"},
		{TagName: "v0.1.146"},
		{TagName: "v0.1.145"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "0.1.146", versions[0].Version)
	require.Equal(t, "0.1.145", versions[1].Version)
	require.Equal(t, "0.1.144", versions[2].Version)
}

func TestUpdateServiceListRollbackVersionsEmptyWhenNoneOlder(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.147"},
		{TagName: "v0.1.148"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Empty(t, versions)
}

func TestUpdateServiceListRollbackVersionsPropagatesFetchError(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentErr: errors.New("github unavailable")},
		"0.1.147",
		"release",
		"example/sub2api",
	)

	_, err := svc.ListRollbackVersions(context.Background())

	require.ErrorIs(t, err, ErrUpdateReleaseLookupFailed)
	require.Equal(t, http.StatusServiceUnavailable, infraerrors.Code(err))
	require.Equal(t, "UPDATE_RELEASE_LOOKUP_FAILED", infraerrors.Reason(err))
	require.NotContains(t, infraerrors.Message(err), "github unavailable")
}

func TestUpdateServiceRollbackToVersionRejectsDisallowedTargets(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.148"},
		{TagName: "v0.1.147"},
		{TagName: "v0.1.146"},
		{TagName: "v0.1.145"},
		{TagName: "v0.1.144"},
		{TagName: "v0.1.143"},
		{TagName: "v0.1.142"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	for _, target := range []string{
		"",         // empty
		"0.1.147",  // current version
		"v0.1.147", // current version with prefix
		"0.1.148",  // newer than current
		"0.1.142",  // older than the 3 most recent
		"9.9.9",    // nonexistent
	} {
		err := svc.RollbackToVersion(context.Background(), target)
		require.ErrorIs(t, err, ErrRollbackVersionNotAllowed, "target %q should be rejected", target)
	}
}

func TestUpdateServiceRollbackToVersionAcceptsVPrefix(t *testing.T) {
	// No platform asset in the release: the target passes the allowlist check
	// and fails later at asset lookup, proving the version itself was accepted.
	releases := []*GitHubRelease{
		{TagName: "v0.1.147"},
		{TagName: "v0.1.146"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	err := svc.RollbackToVersion(context.Background(), "v0.1.146")

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrRollbackVersionNotAllowed)
	require.ErrorIs(t, err, ErrUpdateAssetNotAvailable)
	require.Equal(t, "UPDATE_ASSET_NOT_AVAILABLE", infraerrors.Reason(err))
}

func TestUpdateServiceUsesConfiguredRepository(t *testing.T) {
	client := &updateServiceGitHubClientStub{release: &GitHubRelease{TagName: "v0.1.177"}}
	svc := NewUpdateService(&updateServiceCacheStub{}, client, "0.1.176", "release", "friend/sub2api-custom")

	info, err := svc.CheckUpdate(context.Background(), true)

	require.NoError(t, err)
	require.Equal(t, "friend/sub2api-custom", client.latestRepo)
	require.Equal(t, "friend/sub2api-custom", info.UpdateRepo)
}

func TestUpdateServiceRejectsCacheFromDifferentRepository(t *testing.T) {
	cache := &updateServiceCacheStub{}
	firstClient := &updateServiceGitHubClientStub{release: &GitHubRelease{TagName: "v0.1.177"}}
	first := NewUpdateService(cache, firstClient, "0.1.176", "release", "first/sub2api")
	_, err := first.CheckUpdate(context.Background(), true)
	require.NoError(t, err)

	secondClient := &updateServiceGitHubClientStub{release: &GitHubRelease{TagName: "v0.1.178"}}
	second := NewUpdateService(cache, secondClient, "0.1.176", "release", "second/sub2api")
	info, err := second.CheckUpdate(context.Background(), false)

	require.NoError(t, err)
	require.Equal(t, "second/sub2api", secondClient.latestRepo)
	require.Equal(t, "0.1.178", info.LatestVersion)
}

func TestCompareVersionsUnderstandsPrereleases(t *testing.T) {
	require.Less(t, compareVersions("0.1.177-dev.12", "0.1.177"), 0)
	require.Greater(t, compareVersions("0.1.177", "0.1.177-rc.1"), 0)
	require.Less(t, compareVersions("0.1.177-dev.12", "0.1.177-dev.13"), 0)
}

func TestNormalizeUpdateRepoFallsBackForInvalidValues(t *testing.T) {
	for _, value := range []string{"", "owner", "owner/repo/extra", "https://github.com/owner/repo", "owner/repo?x=1"} {
		require.Equal(t, defaultGitHubRepo, normalizeUpdateRepo(value), value)
	}
	require.Equal(t, "owner/repo.name", normalizeUpdateRepo("owner/repo.name"))
}

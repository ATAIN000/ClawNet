package cli

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ChatChatTech/ClawNet/clawnet-cli/internal/daemon"
	"github.com/ChatChatTech/ClawNet/clawnet-cli/internal/i18n"
)

const (
	updateOwner  = "ChatChatTech"
	updateRepo   = "ClawNet"
	npmScope     = "@cctech2077"
)

// npm registries to try in order (npmmirror first for China users)
var npmRegistries = []string{
	"https://registry.npmmirror.com",
	"https://registry.npmjs.org",
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Name    string    `json:"name"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

func cmdUpdate() error {
	current := "v" + daemon.Version
	fmt.Println(i18n.Tf("update.current", current))

	// Parse --source flag: auto (default), npm, github
	source := "auto"
	for _, a := range os.Args[2:] {
		switch a {
		case "--npm":
			source = "npm"
		case "--github":
			source = "github"
		}
	}

	fmt.Println(i18n.T("update.checking"))

	release, err := fetchLatestRelease()
	if err != nil {
		return fmt.Errorf("check update: %w", err)
	}

	if release.TagName == current || release.TagName == daemon.Version {
		fmt.Println(i18n.T("update.up_to_date"))
		return nil
	}

	fmt.Println(i18n.Tf("update.available", release.TagName))

	ver := strings.TrimPrefix(release.TagName, "v")

	// Download to temp file
	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find current binary: %w", err)
	}
	tmpPath := binPath + ".update"
	defer os.Remove(tmpPath)

	var dlErr error
	switch source {
	case "npm":
		dlErr = downloadFromNpm(ver, tmpPath)
	case "github":
		dlErr = downloadFromGitHub(release, tmpPath)
	default: // auto: npm first, then GitHub
		dlErr = downloadFromNpm(ver, tmpPath)
		if dlErr != nil {
			fmt.Println(i18n.T("update.npm_failed_trying_github"))
			dlErr = downloadFromGitHub(release, tmpPath)
		}
	}
	if dlErr != nil {
		return fmt.Errorf("download: %w", dlErr)
	}

	// Make executable
	if err := os.Chmod(tmpPath, 0755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	// Atomic replace: rename over the current binary
	if err := os.Rename(tmpPath, binPath); err != nil {
		return fmt.Errorf("replace binary: %w", err)
	}

	fmt.Println(i18n.Tf("update.success", release.TagName))
	fmt.Println(i18n.T("update.restart_hint"))
	return nil
}

// downloadFromGitHub downloads the binary from GitHub Releases.
func downloadFromGitHub(release *ghRelease, dest string) error {
	assetName := fmt.Sprintf("clawnet-%s-%s", runtime.GOOS, runtime.GOARCH)
	var asset *ghAsset
	for i := range release.Assets {
		if strings.Contains(release.Assets[i].Name, assetName) {
			asset = &release.Assets[i]
			break
		}
	}
	if asset == nil {
		for i := range release.Assets {
			if release.Assets[i].Name == "clawnet" {
				asset = &release.Assets[i]
				break
			}
		}
	}
	if asset == nil {
		return fmt.Errorf("no binary for %s/%s in release", runtime.GOOS, runtime.GOARCH)
	}

	fmt.Println(i18n.Tf("update.downloading_github", asset.Name, asset.Size))
	return downloadAsset(asset.BrowserDownloadURL, dest)
}

// downloadFromNpm downloads the binary from npm registry (npmmirror → npmjs).
func downloadFromNpm(version, dest string) error {
	// Map Go OS/arch to npm package naming
	npmOS := runtime.GOOS
	if npmOS == "windows" {
		npmOS = "win32"
	}
	npmArch := runtime.GOARCH
	if npmArch == "amd64" {
		npmArch = "x64"
	}

	pkgBase := fmt.Sprintf("clawnet-%s-%s", npmOS, npmArch)
	pkgName := fmt.Sprintf("%s/%s", npmScope, pkgBase)

	client := &http.Client{Timeout: 2 * time.Minute}

	for _, registry := range npmRegistries {
		tarballURL := fmt.Sprintf("%s/%s/-/%s-%s.tgz", registry, pkgName, pkgBase, version)
		fmt.Println(i18n.Tf("update.trying_npm", registry))

		req, err := http.NewRequest("GET", tarballURL, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "clawnet/"+daemon.Version)

		resp, err := client.Do(req)
		if err != nil {
			continue
		}

		if resp.StatusCode != 200 {
			resp.Body.Close()
			continue
		}

		// Extract binary from tarball: package/bin/clawnet
		binName := "clawnet"
		if runtime.GOOS == "windows" {
			binName = "clawnet.exe"
		}

		err = extractBinaryFromTgz(resp.Body, filepath.Join("package", "bin", binName), dest)
		resp.Body.Close()
		if err != nil {
			continue
		}

		info, _ := os.Stat(dest)
		if info != nil && info.Size() > 0 {
			fmt.Println(i18n.Tf("update.downloaded_npm", info.Size()))
			return nil
		}
	}
	return fmt.Errorf("npm download failed for all registries")
}

// extractBinaryFromTgz extracts a single file from a .tgz archive.
func extractBinaryFromTgz(r io.Reader, targetPath, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Name == targetPath || strings.HasSuffix(hdr.Name, "/bin/clawnet") || strings.HasSuffix(hdr.Name, "/bin/clawnet.exe") {
			out, err := os.Create(dest)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, io.LimitReader(tr, 200<<20))
			out.Close()
			return err
		}
	}
	return fmt.Errorf("binary not found in tarball")
}

func fetchLatestRelease() (*ghRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", updateOwner, updateRepo)
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "clawnet/"+daemon.Version)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(body))
	}

	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func downloadAsset(url, dest string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download returned %d", resp.StatusCode)
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	// Limit read to 200 MB to prevent abuse
	_, err = io.Copy(out, io.LimitReader(resp.Body, 200<<20))
	return err
}

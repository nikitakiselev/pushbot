// Package upgrade replaces the installed pushbot with a GitHub release and
// restarts the service.
package upgrade

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
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxDownload bounds release downloads.
const maxDownload = 100 << 20

// Options for Run.
type Options struct {
	Current      string // running version, e.g. "1.1.0" (or "dev")
	Version      string // target release, e.g. "v1.0.0"; "" means the latest
	Repo         string // owner/name hosting the releases
	APIBase      string // default https://api.github.com
	DownloadBase string // default https://github.com
	Token        string // optional GitHub token for the API request
	Arch         string // GOARCH of the release archive
	BinaryPath   string // e.g. /usr/local/bin/pushbot
	UnitPath     string // e.g. /etc/systemd/system/pushbot.service
	Client       *http.Client
	Systemctl    func(args ...string) error
	Out          io.Writer
}

// Run downloads the release, verifies its checksum, replaces the binary and the
// systemd unit, and restarts the service if it is running.
func Run(ctx context.Context, o Options) error {
	if o.APIBase == "" {
		o.APIBase = "https://api.github.com"
	}
	if o.DownloadBase == "" {
		o.DownloadBase = "https://github.com"
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 5 * time.Minute}
	}

	tag := o.Version
	if tag == "" {
		latest, err := LatestTag(ctx, o.Client, o.APIBase, o.Repo, o.Token)
		if err != nil {
			return fmt.Errorf("find the latest release: %w", err)
		}
		tag = latest
	}
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	target := strings.TrimPrefix(tag, "v")
	if target == o.Current {
		fmt.Fprintf(o.Out, "pushbot %s is already up to date.\n", o.Current)
		return nil
	}

	archiveName := "pushbot_linux_" + o.Arch + ".tar.gz"
	base := fmt.Sprintf("%s/%s/releases/download/%s/", o.DownloadBase, o.Repo, tag)
	fmt.Fprintf(o.Out, "Downloading pushbot %s (%s)...\n", tag, archiveName)
	archive, err := download(ctx, o.Client, base+archiveName)
	if err != nil {
		return fmt.Errorf("release %s: %w", tag, err)
	}
	sums, err := download(ctx, o.Client, base+"checksums.txt")
	if err != nil {
		return fmt.Errorf("release %s: %w", tag, err)
	}
	if err := verifyChecksum(archive, sums, archiveName); err != nil {
		return err
	}
	fmt.Fprintln(o.Out, "Checksum OK.")

	files, err := extract(archive, "pushbot", "pushbot.service")
	if err != nil {
		return fmt.Errorf("release %s: %w", tag, err)
	}
	unitChanged, err := replaceIfChanged(o.UnitPath, files["pushbot.service"], 0o644)
	if err != nil {
		return fmt.Errorf("install %s: %w", o.UnitPath, err)
	}
	if _, err := replaceIfChanged(o.BinaryPath, files["pushbot"], 0o755); err != nil {
		return fmt.Errorf("install %s: %w", o.BinaryPath, err)
	}
	fmt.Fprintf(o.Out, "Installed %s (%s → %s).\n", o.BinaryPath, o.Current, target)

	if unitChanged {
		if err := o.Systemctl("daemon-reload"); err != nil {
			return fmt.Errorf("systemctl daemon-reload: %w", err)
		}
	}
	if err := o.Systemctl("is-active", "--quiet", "pushbot"); err != nil {
		fmt.Fprintln(o.Out, "The pushbot service is not running; start it with: sudo systemctl start pushbot")
		return nil
	}
	fmt.Fprintln(o.Out, "Restarting pushbot (waits for running deploys to finish)...")
	if err := o.Systemctl("restart", "pushbot"); err != nil {
		return fmt.Errorf("systemctl restart pushbot: %w", err)
	}
	fmt.Fprintf(o.Out, "Done: pushbot %s is running.\n", target)
	return nil
}

// LatestTag returns the tag of the latest release of repo, e.g. "v1.2.0".
func LatestTag(ctx context.Context, client *http.Client, apiBase, repo, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned %s", resp.Status)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.TagName == "" {
		return "", errors.New("unexpected GitHub API response")
	}
	return body.TagName, nil
}

func download(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("not found (%s)", url)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	if len(data) > maxDownload {
		return nil, fmt.Errorf("download %s: larger than %d bytes", url, maxDownload)
	}
	return data, nil
}

// verifyChecksum checks data against the "<sha256>  <name>" line in sums.
func verifyChecksum(data, sums []byte, name string) error {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != fields[0] {
				return fmt.Errorf("checksum mismatch for %s: the download is corrupted or tampered with", name)
			}
			return nil
		}
	}
	return fmt.Errorf("checksums.txt has no entry for %s", name)
}

// extract returns the named files from a gzipped tar archive.
func extract(archive []byte, names ...string) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	files := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg || !want[hdr.Name] {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxDownload))
		if err != nil {
			return nil, err
		}
		files[hdr.Name] = data
	}
	for _, n := range names {
		if _, ok := files[n]; !ok {
			return nil, fmt.Errorf("archive has no %s", n)
		}
	}
	return files, nil
}

// replaceIfChanged atomically writes data to path unless it already has that content.
func replaceIfChanged(path string, data []byte, mode os.FileMode) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".new-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), path)
}

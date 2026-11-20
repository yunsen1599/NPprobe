// Package upgrade 自升级：面板在测试目标里下发 target_version（latest 或 x.y.z），
// 和当前版本不同时，从 GitHub Release 下载对应架构的安装包，按 checksums.txt 校验后替换自身，
// 然后退出进程，由 systemd（Restart=always）用新版本拉起。
package upgrade

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ErrRestart 新版本已就位，调用方应退出进程让 systemd 重启
var ErrRestart = errors.New("upgraded, restart required")

type Upgrader struct {
	Repo    string // owner/name
	Current string // 当前版本
	// Exe 要替换的可执行文件，空 = os.Executable()
	Exe string
	// Base GitHub 地址，测试可替换
	Base string
	HTTP *http.Client
}

func (u *Upgrader) base() string {
	if u.Base != "" {
		return strings.TrimRight(u.Base, "/")
	}
	return "https://github.com"
}

func (u *Upgrader) client() *http.Client {
	if u.HTTP != nil {
		return u.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func clean(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }

// Resolve 把 latest 解析成具体版本（GitHub releases/latest 会重定向到 /tag/<版本>）
func (u *Upgrader) Resolve(ctx context.Context, target string) (string, error) {
	target = clean(target)
	if target != "latest" {
		return target, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, fmt.Sprintf("%s/%s/releases/latest", u.base(), u.Repo), nil)
	if err != nil {
		return "", err
	}
	c := *u.client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return "", fmt.Errorf("resolve latest release: unexpected response %s", resp.Status)
	}
	return clean(loc[i+len("/tag/"):]), nil
}

// Needed 目标版本是否与当前版本不同（dev 版本不自升级，避免开发环境被替换）
func (u *Upgrader) Needed(resolved string) bool {
	return resolved != "" && clean(u.Current) != "dev" && clean(u.Current) != resolved
}

// Apply 下载并替换为 version；成功返回 ErrRestart
func (u *Upgrader) Apply(ctx context.Context, version string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("self-upgrade only supported on linux")
	}
	pkg := fmt.Sprintf("npprobe-linux-%s.tar.gz", runtime.GOARCH)
	dl := fmt.Sprintf("%s/%s/releases/download/%s", u.base(), u.Repo, version)
	sums, err := u.get(ctx, dl+"/checksums.txt", 1<<20)
	if err != nil {
		return fmt.Errorf("download checksums: %w", err)
	}
	want, err := checksumFor(sums, pkg)
	if err != nil {
		return err
	}
	archive, err := u.get(ctx, dl+"/"+pkg, 200<<20)
	if err != nil {
		return fmt.Errorf("download %s: %w", pkg, err)
	}
	sum := sha256.Sum256(archive)
	if hex.EncodeToString(sum[:]) != want {
		return fmt.Errorf("checksum mismatch for %s", pkg)
	}
	bin, err := extract(archive, "npprobe")
	if err != nil {
		return err
	}
	return u.replace(bin)
}

func (u *Upgrader) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

func checksumFor(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("%s not found in checksums.txt", name)
}

func extract(archive []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s not found in archive", name)
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(h.Name) == name && h.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, 200<<20))
		}
	}
}

// replace 写到同目录的临时文件再 rename，替换是原子的
func (u *Upgrader) replace(bin []byte) error {
	exe := u.Exe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return err
		}
		if exe, err = filepath.EvalSymlinks(exe); err != nil {
			return err
		}
	}
	tmp := exe + ".new"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return fmt.Errorf("write new binary: %w", err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace binary: %w", err)
	}
	return ErrRestart
}

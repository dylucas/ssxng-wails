package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Small shared helpers: string picking, Finder reveal, downloads and
// atomic file writes.

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func revealInFinder(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("文件不存在: %s", path)
	}
	if err := exec.Command("/usr/bin/open", "-R", path).Run(); err != nil {
		return fmt.Errorf("无法在 Finder 中显示 %s: %w", path, err)
	}
	return nil
}

// downloadFile fetches a URL. When proxy is not empty the request goes through
// it (used to fetch rules through our own tunnel).
func downloadFile(url, proxy string, limit int64) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "SSXNG/"+AppVersion)
	client := &http.Client{Timeout: 90 * time.Second}
	if proxy != "" {
		purl, perr := neturl.Parse(proxy)
		if perr != nil {
			return nil, fmt.Errorf("本地代理地址无效: %w", perr)
		}
		client.Transport = &http.Transport{Proxy: http.ProxyURL(purl)}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("读取下载内容失败: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("下载内容为空")
	}
	return data, nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

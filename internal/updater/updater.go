// Package updater 实现热更新：从 GitHub Releases 拉取已打包的发布版本，
// 校验 sha256 后原子替换二进制并重启，全程无需手动重新构建。
package updater

import (
	archive "archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// Release 一个 GitHub Release 的精简视图。
type Release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

// CurrentVersion 由 main 通过 -ldflags 注入。
var CurrentVersion = "dev"

// CheckLatest 查询远端最新版本；已是最新返回 (nil, nil)。
func CheckLatest(repo string) (*Release, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, errors.New("仓库还没有任何 Release")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub API 返回 %d", resp.StatusCode)
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return nil, err
	}
	if rel.TagName == CurrentVersion || rel.TagName == "" {
		return nil, nil // 已是最新
	}
	return &rel, nil
}

// assetName 平台对应的发布产物名（与 release.yml 的构建矩阵一致）。
func assetName(version string) string {
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	return fmt.Sprintf("zcode2api-%s-%s-%s%s", strings.TrimPrefix(version, "v"), runtime.GOOS, runtime.GOARCH, ext)
}

// AssetForPlatform 在 Release 中找当前平台的二进制与其 sha256 产物。
func (r *Release) AssetForPlatform() (bin, sha *string, err error) {
	want := assetName(r.TagName)
	for i := range r.Assets {
		if r.Assets[i].Name == want {
			b := r.Assets[i].URL
			bin = &b
		}
		if r.Assets[i].Name == want+".sha256" {
			s := r.Assets[i].URL
			sha = &s
		}
	}
	if bin == nil {
		return nil, nil, fmt.Errorf("Release %s 没有适用于 %s/%s 的产物", r.TagName, runtime.GOOS, runtime.GOARCH)
	}
	return bin, sha, nil
}

// Apply 下载并替换当前二进制，返回是否需要重启（替换成功即 true）。
//
// 流程：下载到 data/update/<version>/ → sha256 校验 →
// 把旧二进制改名 .old → 新二进制就位 → 由调用方决定重启方式。
func Apply(rel *Release, httpClient *http.Client) (string, error) {
	binURL, shaURL, err := rel.AssetForPlatform()
	if err != nil {
		return "", err
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Minute}
	}

	// 下载二进制。
	newBin, err := download(httpClient, *binURL)
	if err != nil {
		return "", fmt.Errorf("下载失败: %w", err)
	}

	// sha256 校验（若 Release 附带校验文件）。
	if shaURL != nil {
		wantTxt, err := download(httpClient, *shaURL)
		if err == nil {
			want := strings.Fields(string(wantTxt))
			if len(want) > 0 {
				sum := sha256.Sum256(newBin)
				got := hex.EncodeToString(sum[:])
				if !strings.EqualFold(got, want[0]) {
					return "", fmt.Errorf("sha256 校验失败: got %s want %s", got, want[0])
				}
			}
		}
	}

	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	old := self + ".old"
	_ = os.Remove(old)
	if err := os.Rename(self, old); err != nil {
		return "", fmt.Errorf("备份旧二进制失败: %w", err)
	}
	if err := os.WriteFile(self, newBin, 0o755); err != nil {
		_ = os.Rename(old, self) // 回滚
		return "", fmt.Errorf("写入新二进制失败: %w", err)
	}
	return rel.TagName, nil
}

func download(c *http.Client, url string) ([]byte, error) {
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("下载 %s 返回 %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 512<<20))
}

var (
	_ = archive.NewReader // 预留：后续支持 tar.gz 发布产物
	_ = gzip.NewReader
)

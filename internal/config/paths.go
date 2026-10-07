package config

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
)

// atomicWrite 原子写文件：先写临时文件再 rename，避免写一半崩溃损坏配置。
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// EnsureConfigFile 若配置文件不存在则写入模板，返回是否新建。
func EnsureConfigFile(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	}
	c := Default()
	c.DeviceMid = newUUID()
	if err := Save(path, c); err != nil {
		return false, err
	}
	return true, nil
}

// DataDir 返回状态目录（账号库、日志等落盘点）。
// 优先级：ZG_STORE_DIR 环境变量 > 可执行文件同目录/data > 当前目录/data。
func DataDir() string {
	if storeDirOverride != "" {
		return storeDirOverride
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "data")
	}
	return filepath.Join(".", "data")
}

// newUUID 生成 UUIDv4（标准库实现，零依赖）。
// 格式：8-4-4-4-12，共 36 字符。
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}

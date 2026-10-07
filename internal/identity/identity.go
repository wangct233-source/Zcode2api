// Package identity 为每个账号构造独立、稳定的上游客户端身份。
//
// 核心思想（继承自对原型机制的分析）：N 个账号对上游表现为 N 台
// 互不相关的桌面客户端 —— 每账号独立 deviceMid、一致的真实客户端 UA。
package identity

import (
	"fmt"
	"regexp"
	"strings"
)

// AppVersion 伪装的官方客户端版本（可通过 ZG_APP_VERSION 覆盖）。
var AppVersion = "3.14.4"

// Headers 为账号构造上游请求所需的身份头。
// deviceMid 为该账号创建时生成、永不改变的 UUIDv4。
func Headers(deviceMid string) [][2]string {
	return [][2]string{
		{"User-Agent", fmt.Sprintf("ZCode/%s", AppVersion)},
		{"X-Device-Mid", deviceMid},
		{"X-Title", "cli"},
		{"HTTP-Referer", "https://zcode.z.ai"},
	}
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidUUID 校验 deviceMid 格式。
func ValidUUID(s string) bool { return uuidRe.MatchString(strings.TrimSpace(s)) }

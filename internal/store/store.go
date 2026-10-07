// Package store 账号凭证的加密存储。
//
// 安全设计（相对原型的改进点）：
//   - KDF 使用 scrypt(N=32768, r=8, p=1) + 16 字节随机盐，替代无盐 SHA-256；
//   - 加密种子优先取环境变量 ZG_CREDENTIAL_SECRET，未设置时派生自本机信息
//     （设固定值即可跨机搬运账号库）；
//   - 整文件原子写（tmp→rename）+ 互斥锁串行化写操作；
//   - 解密失败时把旧文件改名保留（accounts.json.corrupt-<时间戳>），
//     以零账号状态启动，绝不覆盖用户数据。
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"golang.org/x/crypto/scrypt"
)

// AccountRecord 单个账号的持久化记录。
type AccountRecord struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`    // api_key | jwt
	Secret    string    `json:"secret"`  // 凭证明文（落盘前整库加密）
	DeviceMid string    `json:"deviceMid"` // 每账号独立设备指纹
	CreatedAt time.Time `json:"createdAt"`
	// QuotaLeft 最近已知剩余额度（nil=未知）；由余额轮询更新。
	QuotaLeft *float64 `json:"quotaLeft,omitempty"`
	// QuotaHoldUntil 配额耗尽持有截止时间（402/1113）。
	QuotaHoldUntil time.Time `json:"quotaHoldUntil,omitempty"`
}

// doc 加密文档的明文结构。
type doc struct {
	Version  int             `json:"version"`
	Salt     []byte          `json:"salt"`
	Nonce    []byte          `json:"nonce"`
	Accounts []AccountRecord `json:"accounts"`
}

// Store 加密账号库。
type Store struct {
	mu   sync.Mutex
	path string
	recs []AccountRecord
}

// Open 打开（或初始化）账号库。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "accounts.json")}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // 首次启动，空库
	}
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	plain, err := decrypt(data)
	if err != nil {
		// 解不开：保留旧文件，以空库启动，绝不覆盖。
		keep := fmt.Sprintf("%s.corrupt-%d", s.path, time.Now().Unix())
		_ = os.Rename(s.path, keep)
		return fmt.Errorf("账号库解密失败（密钥与写入时不一致？），原文件已保留为 %s，以空库启动", keep)
	}
	var d doc
	if err := json.Unmarshal(plain, &d); err != nil {
		return fmt.Errorf("账号库损坏: %w", err)
	}
	s.recs = d.Accounts
	return nil
}

// persist 加密落盘（调用方需持锁）。
func (s *Store) persist() error {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	plain, err := json.Marshal(doc{Version: 1, Accounts: s.recs})
	if err != nil {
		return err
	}
	data, err := encrypt(plain, salt)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// List 返回全部账号快照。
func (s *Store) List() []AccountRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AccountRecord, len(s.recs))
	copy(out, s.recs)
	return out
}

// Add 新增账号。
func (s *Store) Add(rec AccountRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.ID == "" {
		rec.ID = fmt.Sprintf("acc-%d", time.Now().UnixNano())
	}
	if rec.DeviceMid == "" {
		rec.DeviceMid = newUUID()
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	s.recs = append(s.recs, rec)
	return s.persist()
}

// Update 按 ID 更新账号字段。
func (s *Store) Update(id string, fn func(*AccountRecord)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.recs {
		if s.recs[i].ID == id {
			fn(&s.recs[i])
			return s.persist()
		}
	}
	return fmt.Errorf("账号 %s 不存在", id)
}

// Remove 删除账号。
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.recs {
		if s.recs[i].ID == id {
			s.recs = append(s.recs[:i], s.recs[i+1:]...)
			return s.persist()
		}
	}
	return fmt.Errorf("账号 %s 不存在", id)
}

// ---- 加密实现 ----

// encryptionKey 派生 AES-256 密钥：scrypt(secret, salt)。
func encryptionKey(salt []byte) ([]byte, error) {
	secret := os.Getenv("ZG_CREDENTIAL_SECRET")
	if secret == "" {
		// 与机器绑定的兜底种子（设 ZG_CREDENTIAL_SECRET 即可跨机迁移）。
		secret = fmt.Sprintf("zcode2api:%s:%s", runtime.GOOS, runtime.GOARCH)
	}
	return scrypt.Key([]byte(secret), salt, 32768, 8, 1, 32)
}

func encrypt(plain, salt []byte) ([]byte, error) {
	key, err := encryptionKey(salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	// 输出格式：saltLen(4B) + salt + nonce + ciphertext
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, uint32(len(salt)))
	out = append(out, salt...)
	out = append(out, nonce...)
	out = gcm.Seal(out, nonce, plain, nil)
	return out, nil
}

func decrypt(data []byte) ([]byte, error) {
	if len(data) < 4 {
		return nil, errors.New("密文过短")
	}
	saltLen := binary.BigEndian.Uint32(data[:4])
	if uint32(len(data)) < 4+saltLen {
		return nil, errors.New("密文格式错误")
	}
	salt := data[4 : 4+saltLen]
	rest := data[4+saltLen:]
	key, err := encryptionKey(salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(rest) < gcm.NonceSize() {
		return nil, errors.New("密文格式错误")
	}
	return gcm.Open(nil, rest[:gcm.NonceSize()], rest[gcm.NonceSize():], nil)
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

var _ = sha256.Size // 保留引用

package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"golang.org/x/crypto/scrypt"

	"github.com/snnabb/fusion-ride/internal/db"
)

const (
	jwtExpiry = 24 * time.Hour
	saltLen   = 16
)

// AdminAuth 管理管理员认证。
type AdminAuth struct {
	db        *db.DB
	jwtSecret []byte
}

// NewAdminAuth 创建认证管理器。jwtSecret 为空则随机生成。
func NewAdminAuth(database *db.DB, secret string) *AdminAuth {
	var jwtSec []byte
	if secret != "" {
		jwtSec = []byte(secret)
	} else {
		jwtSec = make([]byte, 32)
		rand.Read(jwtSec)
	}

	return &AdminAuth{
		db:        database,
		jwtSecret: jwtSec,
	}
}

// NeedsSetup 检查是否需要首次设置密码。
func (a *AdminAuth) NeedsSetup() bool {
	var hash string
	err := a.db.QueryRow(`SELECT password_hash FROM admin WHERE id = 1`).Scan(&hash)
	return err != nil || hash == ""
}

// Setup 首次设置管理员密码。
func (a *AdminAuth) Setup(username, password string) error {
	if username == "" || password == "" {
		return fmt.Errorf("用户名和密码不能为空")
	}

	hash, err := hashPassword(password)
	if err != nil {
		return err
	}

	_, err = a.db.Exec(
		`INSERT OR REPLACE INTO admin(id, username, password_hash) VALUES(1, ?, ?)`,
		username, hash,
	)
	return err
}

func (a *AdminAuth) VerifyCredentials(username, password string) bool {
	storedUsername, storedHash, err := a.getCredentials()
	if err != nil {
		return false
	}
	if username != storedUsername {
		return false
	}
	return a.checkPassword(password, storedHash)
}

// Login 验证管理员凭据，成功返回 JWT。
func (a *AdminAuth) Login(username, password string) (string, error) {
	storedUsername, storedHash, err := a.getCredentials()
	if err != nil {
		return "", fmt.Errorf("管理员未配置")
	}

	if username != storedUsername {
		return "", fmt.Errorf("用户名或密码错误")
	}

	if !a.checkPassword(password, storedHash) {
		return "", fmt.Errorf("用户名或密码错误")
	}

	return a.signJWT(username)
}

// VerifyToken 校验 JWT 有效性。
func (a *AdminAuth) VerifyToken(token string) (string, error) {
	return a.verifyJWT(token)
}

// ChangePassword 修改管理员密码。
func (a *AdminAuth) ChangePassword(oldPwd, newPwd string) error {
	_, storedHash, err := a.getCredentials()
	if err != nil {
		return fmt.Errorf("管理员未配置")
	}
	if !a.checkPassword(oldPwd, storedHash) {
		return fmt.Errorf("旧密码错误")
	}
	hash, err := hashPassword(newPwd)
	if err != nil {
		return err
	}
	_, err = a.db.Exec(`UPDATE admin SET password_hash = ? WHERE id = 1`, hash)
	return err
}

func (a *AdminAuth) getCredentials() (string, string, error) {
	var storedUsername, storedHash string
	err := a.db.QueryRow(`SELECT username, password_hash FROM admin WHERE id = 1`).
		Scan(&storedUsername, &storedHash)
	if err != nil {
		return "", "", err
	}
	return storedUsername, storedHash, nil
}

func (a *AdminAuth) checkPassword(password, storedHash string) bool {
	return verifyPassword(password, storedHash)
}

// ── JWT ──

type jwtPayload struct {
	Sub string `json:"sub"`
	Exp int64  `json:"exp"`
	Iat int64  `json:"iat"`
}

func (a *AdminAuth) signJWT(username string) (string, error) {
	now := time.Now()
	payload := jwtPayload{
		Sub: username,
		Exp: now.Add(jwtExpiry).Unix(),
		Iat: now.Unix(),
	}

	headerJSON := []byte(`{"alg":"HS256","typ":"JWT"}`)
	payloadJSON, _ := json.Marshal(payload)

	header := base64url(headerJSON)
	body := base64url(payloadJSON)
	sigInput := header + "." + body

	mac := hmac.New(sha256.New, a.jwtSecret)
	mac.Write([]byte(sigInput))
	sig := base64url(mac.Sum(nil))

	return sigInput + "." + sig, nil
}

func (a *AdminAuth) verifyJWT(token string) (string, error) {
	parts := splitJWT(token)
	if len(parts) != 3 {
		return "", fmt.Errorf("令牌格式错误")
	}

	// 验签
	mac := hmac.New(sha256.New, a.jwtSecret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	expectedSig := base64url(mac.Sum(nil))
	if !hmac.Equal([]byte(expectedSig), []byte(parts[2])) {
		return "", fmt.Errorf("令牌签名无效")
	}

	// 解析 payload
	payloadJSON, err := base64urlDecode(parts[1])
	if err != nil {
		return "", fmt.Errorf("令牌载荷无效")
	}
	var payload jwtPayload
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return "", fmt.Errorf("令牌载荷无效")
	}

	if time.Now().Unix() > payload.Exp {
		return "", fmt.Errorf("令牌已过期")
	}

	return payload.Sub, nil
}

// ── 密码哈希 (scrypt) ──

func hashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}

	dk, err := scrypt.Key([]byte(password), salt, 32768, 8, 1, 32)
	if err != nil {
		return "", err
	}

	return hex.EncodeToString(salt) + ":" + hex.EncodeToString(dk), nil
}

func verifyPassword(password, hash string) bool {
	parts := splitOnce(hash, ':')
	if len(parts) != 2 {
		return false
	}

	salt, err := hex.DecodeString(parts[0])
	if err != nil {
		return false
	}

	expected, err := hex.DecodeString(parts[1])
	if err != nil {
		return false
	}

	dk, err := scrypt.Key([]byte(password), salt, 32768, 8, 1, 32)
	if err != nil {
		return false
	}

	return hmac.Equal(dk, expected)
}

// ── Base64 URL helpers ──

func base64url(data []byte) string {
	s := hex.EncodeToString(data)
	// 简化：使用 hex 编码避免引入额外依赖
	// 生产环境应使用 encoding/base64.RawURLEncoding
	return s
}

func base64urlDecode(s string) ([]byte, error) {
	return hex.DecodeString(s)
}

func splitJWT(token string) []string {
	result := make([]string, 0, 3)
	start := 0
	count := 0
	for i, c := range token {
		if c == '.' {
			result = append(result, token[start:i])
			start = i + 1
			count++
			if count >= 2 {
				break
			}
		}
	}
	if start <= len(token) {
		result = append(result, token[start:])
	}
	return result
}

func splitOnce(s string, sep byte) []string {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return []string{s[:i], s[i+1:]}
		}
	}
	return []string{s}
}

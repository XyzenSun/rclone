package api

// preAuth 数字信封、取票与票据缓存。
//
// 认证通道走 https://idaas.cmpassport.com（移动 IDaaS），与业务通道分离。
// 取票流程完全复刻 skill SDK（IDaaS_AUT_KMS_v3.1.0）的调用序列：
// configDelivery（配置下发，防替换攻击的仪式性调用）→ preAuth（数字信封）。

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/lib/rest"
)

// skill 注册凭据：这是"本 backend 作为技能客户端"的身份，
// 来自官方 skill 包 cm-cloud-manage-2.0.0，用户无从获取也无须配置，
// 因此硬编码而不作为 Options 暴露。
const (
	skillDevID    = "1403"
	skillClientID = "8d9bae50e6b8439aae62ed8148e1967e"
	skillID       = "5b00f72c4709f27dc9abd117763e0cb6"
	skillName     = "中国移动云盘skill"
	skillStrategy = "d872deb58067415daf2c4e1883aff777"
	skillTemplate = "4jqgxa3ofp6acetcuues9a8psdmexuk1"
	skillScopeIDs = "10008,10009"

	interfaceVersion = "3.1"
	sdkVersion       = "IDaaS_AUT_KMS_v3.1.0"

	configDeliveryPath = "/keyManage/custody/configDelivery"
	preAuthPath        = "/keyManage/custody/preAuth"
)

// preAuth 的 resultCode 语义
const (
	resultCodeSuccess      = "103000" // 取票成功
	resultCodeAuthRequired = "103001" // 需要用户授权
	resultCodeAuthPending  = "130088" // 会话未完成授权
)

// embeddedPublicKeyPEM 是内嵌的 IDaaS RSA 公钥（preAuth 数字信封用）。
// 服务端轮换密钥对时需要同步更新此常量并发布新版本（与 SDK 同一策略）。
const embeddedPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQCJp+9qWjK4Z9luY/0musMaRV4x
Jyfsn3EQ1OQqgI+2ZaNsqbl5PZWlrp57WfirfZ4Y/9+xVmC2H4rE08Jo4hXjubCI
h3iJdZuVNtrlgYVZ7tyA8yELsbcaFg31fNRfWlp7tZRE5YKnO7Oe3ag2Tt3lHI9z
BC98i9bSXBD1u0TQrwIDAQAB
-----END PUBLIC KEY-----`

// 票据有效期按实测结论取 300 秒，提前 60 秒刷新。SDK 的 expireTime
// 单位契约模糊（默认按分钟解释又被客户端 300 秒上限钳制），直接按
// "取票后 240 秒内有效"处理，不依赖服务端下发的 expireTime。
const (
	creditTokenTTL    = 240 * time.Second
	creditTokenMinTTL = 30 * time.Second // 剩余寿命低于此值也触发重取
)

// parsedPublicKey 在包初始化时解析一次，解析失败说明常量被改坏，直接 panic。
var parsedPublicKey *rsa.PublicKey

func init() {
	block, _ := pem.Decode([]byte(embeddedPublicKeyPEM))
	if block == nil {
		panic("cmcloud: embedded public key is not valid PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		panic(fmt.Sprintf("cmcloud: embedded public key parse failed: %v", err))
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		panic("cmcloud: embedded public key is not RSA")
	}
	parsedPublicKey = rsaPub
}

// Auth 管理 IDaaS 认证：长期 accessToken 换短票据 creditToken。
// accessToken 与 clawUid 由调用方（config 向导）写入配置后传入，
// 本结构只在内存中缓存 creditToken，不落盘。
type Auth struct {
	accessToken string
	clawUID     string
	srv         *rest.Client // idaas.cmpassport.com 认证通道

	mu             sync.Mutex
	creditToken    string
	creditTokenExp time.Time // 该 creditToken 的本地失效时刻
}

// NewAuth 创建认证管理器。srv 必须已 SetRoot(TokenBaseURL)。
func NewAuth(accessToken, clawUID string, srv *rest.Client) *Auth {
	return &Auth{
		accessToken: accessToken,
		clawUID:     clawUID,
		srv:         srv,
	}
}

// ClawUID 返回设备身份（业务请求头需要）。
func (a *Auth) ClawUID() string {
	return a.clawUID
}

// PreAuthResult 是 preAuth 的结果。
type PreAuthResult struct {
	ResultCode string // 103000/103001/130088 或其他透传码
	Desc       string
	// 103000 时有效
	CreditToken string
	AccessToken string // 服务端同时下发的长期票据（可能与本地不同，向导保存它）
	// 103001 时有效
	AuthPageURL string
	UUID        string
}

// preAuthResponse 是 preAuth/configDelivery 的响应外壳。
type preAuthResponse struct {
	ResultCode string       `json:"resultCode"`
	Desc       string       `json:"desc"`
	Data       *preAuthData `json:"data"`
}

type preAuthData struct {
	CreditToken string `json:"creditToken"`
	ExpireTime  any    `json:"expireTime"`
	AccessToken string `json:"accessToken"`
	AuthPageURL string `json:"authPageUrl"`
	UUID        string `json:"uuid"`
}

// preAuthPayload 是数字信封的明文载荷。字段与 SDK getToken 的
// payload 一一对应；uuid/accessToken 仅在对应场景携带。
type preAuthPayload struct {
	ClawUID           string `json:"clawUid"`
	SkillID           string `json:"skillId"`
	SkillName         string `json:"skillName"`
	DevID             string `json:"devId"`
	StrategyID        string `json:"strategyId"`
	Sign              string `json:"sign"`
	TemplateID        string `json:"templateId"`
	ScopeIDs          string `json:"scopeIds"`
	UUID              string `json:"uuid,omitempty"`
	AccessToken       string `json:"accessToken,omitempty"`
	OSType            string `json:"osType"`
	OSVersion         string `json:"osVersion"`
	HostName          string `json:"hostName"`
	CPUID             string `json:"cpuId"`
	AgentPlatformPath string `json:"agentPlatformPath"`
	AgentPlatform     string `json:"agentPlatform"`
}

// PreAuth 执行完整取票序列（configDelivery + preAuth）。
// uuid 非空时为授权环的第二步（用户已在浏览器完成授权）；
// accessToken 非空时服务端直接换发票据，无需再走授权页。
func (a *Auth) PreAuth(ctx context.Context, authUUID string) (*PreAuthResult, error) {
	traceID := uuid.NewString()
	timestamp := makeTimestamp()

	// configDelivery：SDK 每次取票都先调它，公钥不从响应读取（防替换），
	// 保留这一步以完全复刻官方客户端的调用序列。
	if err := a.configDelivery(ctx, traceID, timestamp); err != nil {
		return nil, err
	}

	// sign = md5(clientId + interfaceVersion + timestamp + traceId
	//            + clawUid + skillId + skillName + devId)
	signInput := skillClientID + interfaceVersion + timestamp + traceID +
		a.clawUID + skillID + skillName + skillDevID
	sum := md5.Sum([]byte(signInput)) //nolint:gosec // 协议规定的 MD5 签名，非安全用途
	sign := hex.EncodeToString(sum[:])

	payload := preAuthPayload{
		ClawUID:           a.clawUID,
		SkillID:           skillID,
		SkillName:         skillName,
		DevID:             skillDevID,
		StrategyID:        skillStrategy,
		Sign:              sign,
		TemplateID:        skillTemplate,
		ScopeIDs:          skillScopeIDs,
		UUID:              authUUID,
		AccessToken:       a.accessToken,
		OSType:            osType(),
		OSVersion:         osVersion(),
		HostName:          hostName(),
		CPUID:             "",
		AgentPlatformPath: agentPlatformPath(),
		AgentPlatform:     "",
	}
	// Go json.Marshal 会把中文转义成 \uXXXX（官方 SDK 原样输出中文）。
	// 转义后的 JSON 服务端同样可解析（真实凭据验证确认），无须手拼原样输出。
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("cmcloud: preAuth payload marshal failed: %w", err)
	}
	reqdata, encrypted, err := envelopeEncrypt(plaintext)
	if err != nil {
		return nil, err
	}

	body := map[string]string{
		"reqdata":   reqdata,
		"encrypted": encrypted,
	}
	opts := rest.Opts{
		Method: "POST",
		Path:   preAuthPath,
		ExtraHeaders: map[string]string{
			"clientId":         skillClientID,
			"timestamp":        timestamp,
			"interfaceVersion": interfaceVersion,
			"sdkVersion":       sdkVersion,
			"traceId":          traceID,
		},
		ContentType: "application/json",
	}
	var result preAuthResponse
	_, err = a.srv.CallJSON(ctx, &opts, body, &result)
	if err != nil {
		return nil, fmt.Errorf("cmcloud: preAuth request failed: %w", err)
	}
	if result.ResultCode == "" {
		return nil, errors.New("cmcloud: preAuth response missing resultCode")
	}
	out := &PreAuthResult{
		ResultCode: result.ResultCode,
		Desc:       result.Desc,
	}
	if result.Data != nil {
		out.CreditToken = result.Data.CreditToken
		out.AccessToken = result.Data.AccessToken
		out.AuthPageURL = result.Data.AuthPageURL
		out.UUID = result.Data.UUID
	}
	return out, nil
}

// configDelivery 调用配置下发接口。签名 = base64(sha256(traceId+timestamp+"2.0"))，
// 响应 resultCode 必须为 103000；公钥不读取响应值，只用内嵌常量。
func (a *Auth) configDelivery(ctx context.Context, traceID, timestamp string) error {
	digest := sha256.Sum256([]byte(traceID + timestamp + "2.0"))
	body := map[string]string{
		"sign": base64.StdEncoding.EncodeToString(digest[:]),
	}
	opts := rest.Opts{
		Method: "POST",
		Path:   configDeliveryPath,
		ExtraHeaders: map[string]string{
			"interfaceVersion": "2.0",
			"timestamp":        timestamp,
			"traceId":          traceID,
		},
		ContentType: "application/json",
	}
	var result preAuthResponse
	_, err := a.srv.CallJSON(ctx, &opts, body, &result)
	if err != nil {
		return fmt.Errorf("cmcloud: configDelivery request failed: %w", err)
	}
	if result.ResultCode != resultCodeSuccess {
		return fmt.Errorf("cmcloud: configDelivery failed (resultCode=%s): %s", result.ResultCode, result.Desc)
	}
	return nil
}

// CreditToken 返回有效的短票据，过期或临期时自动重新取票。
func (a *Auth) CreditToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.creditToken != "" && time.Now().Before(a.creditTokenExp.Add(-creditTokenMinTTL)) {
		return a.creditToken, nil
	}
	result, err := a.PreAuth(ctx, "")
	if err != nil {
		return "", err
	}
	if result.ResultCode != resultCodeSuccess {
		// 运行期不应再出现需要授权的分支（向导已完成授权并保存 accessToken）；
		// 出现 103001/130088 说明 accessToken 已失效，需要重新走授权。
		return "", fmt.Errorf("cmcloud: preAuth returned %s (%s): re-run `rclone config` to re-authorize this remote", result.ResultCode, result.Desc)
	}
	if result.CreditToken == "" {
		return "", errors.New("cmcloud: preAuth success but no creditToken in response")
	}
	a.creditToken = result.CreditToken
	a.creditTokenExp = time.Now().Add(creditTokenTTL)
	return a.creditToken, nil
}

// InvalidateCreditToken 作废缓存的短票据（票据失效码触发重取时调用）。
func (a *Auth) InvalidateCreditToken() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.creditToken = ""
	a.creditTokenExp = time.Time{}
}

// envelopeEncrypt 实现数字信封：
//  1. 生成 16 字节随机 AES 密钥（SDK 实测行为：AES-128-GCM，
//     其文档字符串里的"AES-256"是陈旧描述，以能通过服务端解密的
//     16 字节 key 为准）
//  2. AES-GCM 加密 payload，IV = key 前 12 字节（由双方从 key 派生，
//     不随密文传输），输出 = ciphertext || tag
//  3. RSA-OAEP-SHA256 加密 AES 密钥（MGF1 亦为 SHA-256，与 Java
//     端显式 OAEPParameterSpec 等价，Go 的 EncryptOAEP 默认即此组合）
//
// 返回 base64(密文+tag) 与 base64(加密后的密钥)。
func envelopeEncrypt(plaintext []byte) (reqdata, encrypted string, err error) {
	aesKey := make([]byte, 16)
	if _, err = rand.Read(aesKey); err != nil {
		return "", "", fmt.Errorf("cmcloud: AES key generation failed: %w", err)
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", "", fmt.Errorf("cmcloud: AES cipher init failed: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", fmt.Errorf("cmcloud: GCM init failed: %w", err)
	}
	sealed := gcm.Seal(nil, aesKey[:12], plaintext, nil)
	encryptedKey, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, parsedPublicKey, aesKey, nil)
	if err != nil {
		return "", "", fmt.Errorf("cmcloud: RSA-OAEP encryption failed: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sealed),
		base64.StdEncoding.EncodeToString(encryptedKey), nil
}

// makeTimestamp 生成 17 位时间戳：YYYYMMDDHHMMSS + 毫秒后 3 位（本地时间）。
func makeTimestamp() string {
	now := time.Now()
	return now.Format("20060102150405") + fmt.Sprintf("%03d", now.Nanosecond()/1e6)
}

// osType 映射到 SDK 的枚举（windows/macos/linux/other）。
func osType() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "macos"
	case "linux":
		return "linux"
	}
	return "other"
}

// osVersion 尽力获取内核版本（Linux 读 /proc，其余平台留空）。
// 这些是设备风控参数，SDK 采集失败时也发送空串，空值不影响取票。
func osVersion() string {
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

// hostName 返回大写主机名（与 SDK 的 socket.gethostname().upper() 对齐）。
func hostName() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.ToUpper(name)
}

// agentPlatformPath 返回本进程可执行文件路径（对应 SDK 的部署路径字段）。
func agentPlatformPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

// CheckAuthResult 判断 preAuth 结果是否为"需要用户授权"（103001/130088）。
func CheckAuthResult(result *PreAuthResult) bool {
	return result.ResultCode == resultCodeAuthRequired || result.ResultCode == resultCodeAuthPending
}

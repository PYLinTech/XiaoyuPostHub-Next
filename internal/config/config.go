// Package config 只负责"启动前必须知道的那几个参数"。
//
// # 最少只需要一个端口
//
// 除监听地址以外，其余全部有可用默认值，**一个环境变量都不配也能跑起来**：
// 数据库落 `./data/xph.db`、数据目录 `./data`、master secret 自动生成到
// `./data/master.key`。剩下的业务配置（主密钥、存储凭据、限额、站点信息）
// 都在 SQLite 里，由前端引导页在首次访问时写入。
//
// 为什么这几项不能入库：
//
//   - **监听地址**：要在任何配置可读之前就把服务挂起来，否则管理员没有入口
//     去配置其它东西；
//   - **数据库路径**：它是"读配置"这个动作本身的前提，无法自举；
//   - **数据/临时目录**：在打开数据库之前就要建好目录、放好密钥文件；
//   - **master secret**：用于加密数据库里的敏感配置，不能与它保护的数据放在
//     一起——否则"拿到数据库"就等于"拿到全部密钥"。
//
// 这几项都可以用环境变量覆盖，但都不设也能启动；启动日志会明确打印实际生效的
// 落盘位置与 master secret 的来源。
package config

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// MasterSecretFile 是自动生成的 master secret 的文件名（位于数据目录下）。
const MasterSecretFile = "master.key"

// masterSecretMinBytes 是对显式配置的 master secret 的最小长度要求。
// 低于这个长度意味着熵不足，用它派生出来的密钥是可以被暴力枚举的。
const masterSecretMinBytes = 16

// Bootstrap 是自举配置。
type Bootstrap struct {
	// Listen 是 HTTP 监听地址，例如 ":8080"。
	Listen string
	// DBPath 是 SQLite 数据库文件路径。
	DBPath string
	// DataDir 是进程可写目录（数据库、密钥文件、备份）。
	DataDir string
	// TempDir 是上传暂存与密文缓存目录。
	TempDir string
	// MasterSecret 用于加密数据库中的敏感配置值。
	MasterSecret []byte
	// MasterSecretSource 说明它从哪来：env / file / generated。
	MasterSecretSource string
	// StaticDir 是前端构建产物的目录，**留空表示用二进制内嵌的那份**。
	//
	// 正式构建走 build.sh，前端会被 go:embed 打进二进制，发布时只需要拷一个
	// 文件。这个字段保留下来是为了两个场景：一是运维临时换一套前端，不值得
	// 为此重新编译；二是在未内嵌前端的源码上跑 go run 联调。
	//
	// 它没有默认值，这是刻意的：给了默认路径（过去的 frontend/dist）之后，
	// 磁盘上那份和内嵌那份谁生效就取决于当前工作目录——同一个二进制在不同
	// 目录下启动会看到不同的页面。宁可显式指定，也不要这种隐式覆盖。
	//
	// 之所以不做成数据库配置项：它必须在启动时就确定，而启动阶段读不了数据库
	// 之外的东西——这一点与监听地址同源。
	StaticDir string
}

// LoadBootstrap 读取自举配置并确保目录与 master secret 就位。
func LoadBootstrap() (*Bootstrap, error) {
	b := &Bootstrap{
		Listen:  env("XPH_LISTEN", ":8080"),
		DBPath:  env("XPH_DB_PATH", filepath.Join("data", "xph.db")),
		DataDir: env("XPH_DATA_DIR", "data"),
	}
	b.TempDir = env("XPH_TEMP_DIR", filepath.Join(b.DataDir, "tmp"))
	// 刻意不给默认值：留空 = 用内嵌产物。理由见 StaticDir 的注释。
	b.StaticDir = strings.TrimSpace(os.Getenv("XPH_STATIC_DIR"))

	if err := b.prepare(); err != nil {
		return nil, err
	}
	return b, nil
}

// prepare 建目录并解析 master secret。
func (b *Bootstrap) prepare() error {
	for _, dir := range []string{b.DataDir, b.TempDir, filepath.Join(b.TempDir, "uploads")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}
	// 数据库可能被挂到独立卷上，其所在目录未必等于 DataDir。
	if dir := filepath.Dir(b.DBPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("创建数据库目录 %s 失败: %w", dir, err)
		}
	}
	return b.resolveMasterSecret()
}

// resolveMasterSecret 按 环境变量 → 密钥文件 → 自动生成 的顺序解析。
//
// 自动生成是刻意的默认行为：要求管理员自己发明一个强随机串，实际结果往往是
// 弱口令或干脆留空。生成后写入权限 0600 的密钥文件，安全性不低于"写在部署
// 脚本里"，而可用性高得多。代价是密钥文件丢了这个数据库就解不开敏感配置——
// 这一点必须在启动日志里说清楚。
func (b *Bootstrap) resolveMasterSecret() error {
	if raw := strings.TrimSpace(os.Getenv("XPH_MASTER_SECRET")); raw != "" {
		if len(raw) < masterSecretMinBytes {
			return fmt.Errorf("XPH_MASTER_SECRET 至少需要 %d 个字符", masterSecretMinBytes)
		}
		b.MasterSecret = []byte(raw)
		b.MasterSecretSource = "env"
		return nil
	}

	path := filepath.Join(b.DataDir, MasterSecretFile)
	if raw, err := os.ReadFile(path); err == nil {
		secret := strings.TrimSpace(string(raw))
		if len(secret) < masterSecretMinBytes {
			return fmt.Errorf("密钥文件 %s 内容过短，已损坏或不是本程序生成的文件", path)
		}
		b.MasterSecret = []byte(secret)
		b.MasterSecretSource = "file"
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("读取密钥文件 %s 失败: %w", path, err)
	}

	secret := make([]byte, 48)
	if _, err := rand.Read(secret); err != nil {
		return fmt.Errorf("生成 master secret 失败: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(secret)
	// 先以 0600 创建再写入，避免"先建后改权限"之间的窗口。
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("创建密钥文件 %s 失败: %w", path, err)
	}
	if _, err := file.WriteString(encoded); err != nil {
		file.Close()
		return fmt.Errorf("写入密钥文件失败: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭密钥文件失败: %w", err)
	}
	b.MasterSecret = []byte(encoded)
	b.MasterSecretSource = "generated"
	return nil
}

// WarnGeneratedSecret 在启动日志里说明 master secret 的来源。单独成函数而不是
// 塞进 LoadBootstrap，因为这是给人看的告警，测试里不需要它。
func (b *Bootstrap) WarnGeneratedSecret() {
	switch b.MasterSecretSource {
	case "generated":
		log.Printf("已生成 master secret 并写入 %s（权限 0600）。"+
			"它用于加密数据库中的敏感配置；**请务必备份该文件**，丢失后需要重新录入 123 凭据与主密钥",
			filepath.Join(b.DataDir, MasterSecretFile))
	case "env":
		log.Printf("master secret 来自环境变量 XPH_MASTER_SECRET")
	}
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

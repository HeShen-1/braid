package tools

// 工具包签名测试(T10.3):验签原语(Ed25519,签什么验什么——篡改清单
// 字节/签名、错钥即拒)、清单校验(version 钉死、工具名词表封闭、字段面
// 封闭、尾随数据拒)、env 矩阵(fail closed 全表:无钥不可验、半配即败、
// 签名态 unsigned 源即拒、验签失败即拒且无降级)。私钥只存在于测试进程
// 内(ed25519.GenerateKey 临时钥),仓库零密钥材料。

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// toolPkgManifest 双工具完整清单:验签原语与装配矩阵共用。
const toolPkgManifest = `{"version":1,"tools":` +
	`{"http":{"endpoints":{"docs_api":"https://docs.example.com"}},` +
	`"file_read":{"dirs":["/var/braid/docs"]}}}`

// b64Of 测试内统一 base64(StdEncoding,与生产 env 三键同形)。
func b64Of(t *testing.T, raw []byte) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(raw)
}

// signedToolPkgEnv 用进程内临时钥对签出三键齐全的 env(extra 后置叠加,
// 可覆盖任一键构造半配/换包用例)。
func signedToolPkgEnv(t *testing.T, manifest string, extra map[string]string) map[string]string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	sig, err := signToolPackage(priv, []byte(manifest))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	env := map[string]string{
		"BRAID_TOOL_SIGNING_PUBLIC_KEY": b64Of(t, pub),
		"BRAID_TOOL_PACKAGE":            b64Of(t, []byte(manifest)),
		"BRAID_TOOL_PACKAGE_SIGNATURE":  sig,
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// signToolPackage 发布侧签名原语(测试侧):Ed25519 私钥对清单原始字节
// 签名,返回 base64。生产构建只验不签(verifyToolPackage 头注释);发布
// 物由 openssl 产出(README §8),未来 Go 发布工具出现时以正确导出面
// 回归生产文件。
func signToolPackage(priv ed25519.PrivateKey, manifest []byte) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("tools: sign tool package: want %d-byte ed25519 private key, got %d",
			ed25519.PrivateKeySize, len(priv))
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, manifest)), nil
}

func TestSignToolPackage(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	sig, err := signToolPackage(priv, []byte(toolPkgManifest))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		t.Fatalf("sig not base64: %v", err)
	}
	if len(raw) != ed25519.SignatureSize {
		t.Fatalf("sig len = %d, want %d", len(raw), ed25519.SignatureSize)
	}
	// 坏私钥长度响亮拒(装配缺陷启动即现纪律)。
	if _, err := signToolPackage(ed25519.PrivateKey(make([]byte, 3)), nil); err == nil ||
		!strings.Contains(err.Error(), "private key") {
		t.Fatalf("err = %v, want private key size rejection", err)
	}
}

func TestVerifyToolPackageRoundtrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	sig, err := signToolPackage(priv, []byte(toolPkgManifest))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	p, err := verifyToolPackage(b64Of(t, pub), b64Of(t, []byte(toolPkgManifest)), sig)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got := p.Tools["http"].Endpoints["docs_api"]; got != "https://docs.example.com" {
		t.Fatalf("http endpoints = %q, want docs_api endpoint", got)
	}
	if got := p.Tools["file_read"].Dirs; len(got) != 1 || got[0] != "/var/braid/docs" {
		t.Fatalf("file_read dirs = %v, want [/var/braid/docs]", got)
	}
}

func TestVerifyToolPackageRejects(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	sig, err := signToolPackage(priv, []byte(toolPkgManifest))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}
	otherSig, err := signToolPackage(otherPriv, []byte(toolPkgManifest))
	if err != nil {
		t.Fatalf("sign other: %v", err)
	}
	tampered := strings.Replace(toolPkgManifest, "docs.example.com", "evil.example.org", 1)
	sigRaw, _ := base64.StdEncoding.DecodeString(sig)
	sigRaw[0] ^= 0xff
	flippedSig := b64Of(t, sigRaw)

	cases := []struct {
		name    string
		pub     string
		pkg     string
		sig     string
		wantErr string
	}{
		{"tampered manifest bytes", b64Of(t, pub), b64Of(t, []byte(tampered)), sig, "mismatch"},
		{"tampered signature bytes", b64Of(t, pub), b64Of(t, []byte(toolPkgManifest)), flippedSig, "mismatch"},
		{"wrong key", b64Of(t, pub), b64Of(t, []byte(toolPkgManifest)), otherSig, "mismatch"},
		{"truncated signature", b64Of(t, pub), b64Of(t, []byte(toolPkgManifest)), b64Of(t, sigRaw[:63]), "64-byte"},
		{"bad signature base64", b64Of(t, pub), b64Of(t, []byte(toolPkgManifest)), "!!not-base64!!", "invalid base64"},
		{"short public key", b64Of(t, pub[:31]), b64Of(t, []byte(toolPkgManifest)), sig, "32-byte"},
		{"bad public key base64", "@@not-base64@@", b64Of(t, []byte(toolPkgManifest)), sig, "invalid base64"},
		{"bad manifest base64", b64Of(t, pub), "%%not-base64%%", sig, "invalid base64"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := verifyToolPackage(c.pub, c.pkg, c.sig)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}

func TestToolPackageManifestValidation(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	rejects := []struct {
		name     string
		manifest string
		wantErr  string
	}{
		{"version 2", `{"version":2,"tools":{}}`, "unsupported"},
		{"version missing", `{"tools":{}}`, "unsupported"},
		{"tools key missing", `{"version":1}`, `"tools" is required`},
		{"tools null is not an explicit empty publication", `{"version":1,"tools":null}`, `"tools" is required`},
		{"unknown tool name", `{"version":1,"tools":{"command":{"dirs":["/x"]}}}`, "unknown tool"},
		{"unknown top-level field", `{"version":1,"tools":{},"publisher":"x"}`, "unknown field"},
		{"unknown per-tool field", `{"version":1,"tools":{"http":{"endpoints":{"a":"https://a.test"},"schema":"x"}}}`, "unknown field"},
		{"http carrying dirs", `{"version":1,"tools":{"http":{"dirs":["/x"]}}}`, "must not carry dirs"},
		{"file_read carrying endpoints", `{"version":1,"tools":{"file_read":{"endpoints":{"a":"https://a.test"}}}}`, "must not carry endpoints"},
		{"trailing data", `{"version":1,"tools":{}} trailing`, "manifest"},
		{"malformed json", `{`, "manifest"},
	}
	for _, c := range rejects {
		t.Run(c.name, func(t *testing.T) {
			sig, err := signToolPackage(priv, []byte(c.manifest))
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			_, err = verifyToolPackage(b64Of(t, priv.Public().(ed25519.PublicKey)), b64Of(t, []byte(c.manifest)), sig)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, c.wantErr)
			}
		})
	}

	// 签名有效 + 结构合法的边界形态:验证层放行,语义收口在装配层
	// (空工具集 = 合法空发布;声明空配置 = 构造器响亮拒)。
	for _, c := range []struct {
		name     string
		manifest string
	}{
		{"empty tools is a valid publication", `{"version":1,"tools":{}}`},
		{"declared-empty tool passes verify", `{"version":1,"tools":{"http":{}}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			sig, err := signToolPackage(priv, []byte(c.manifest))
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			if _, err := verifyToolPackage(b64Of(t, priv.Public().(ed25519.PublicKey)), b64Of(t, []byte(c.manifest)), sig); err != nil {
				t.Fatalf("verify: %v, want nil (assembly-layer semantics)", err)
			}
		})
	}
}

// TestBuiltinToolsFromEnvSignedPackage:T10.3 env 矩阵全表(经真实装配
// 缝;off 模式缺省)。签名态 = BRAID_TOOL_SIGNING_PUBLIC_KEY 在场:
// package+signature 全配、验签通过、unsigned 内置键必须缺席——任一违反
// 启动即败(fail closed,无降级开关)。
func TestBuiltinToolsFromEnvSignedPackage(t *testing.T) {
	cases := []struct {
		name       string
		manifest   string
		extra      map[string]string
		wantErr    string
		wantSource string // 错误归因键:签名态构造器失败必须如实指 BRAID_TOOL_PACKAGE
		wantNames  string
	}{
		{
			name:      "both tools from verified package",
			manifest:  toolPkgManifest,
			wantNames: "file_read,http",
		},
		{
			name:      "http only",
			manifest:  `{"version":1,"tools":{"http":{"endpoints":{"docs_api":"https://docs.example.com"}}}}`,
			wantNames: "http",
		},
		{
			name:      "empty publication registers nothing",
			manifest:  `{"version":1,"tools":{}}`,
			wantNames: "",
		},
		{
			name:       "declared-empty tool rejected by constructor",
			manifest:   `{"version":1,"tools":{"http":{}}}`,
			wantErr:    "empty endpoints",
			wantSource: "BRAID_TOOL_PACKAGE",
		},
		{
			name:       "package endpoint values reach constructor validation",
			manifest:   `{"version":1,"tools":{"http":{"endpoints":{"api":"http://a.test"}}}}`,
			wantErr:    "must be https",
			wantSource: "BRAID_TOOL_PACKAGE",
		},
		{
			name:       "package dir values reach constructor validation",
			manifest:   `{"version":1,"tools":{"file_read":{"dirs":["rel"]}}}`,
			wantErr:    "absolute",
			wantSource: "BRAID_TOOL_PACKAGE",
		},
		{
			name:     "docker carrier rejects bad signed dir at assembly",
			manifest: `{"version":1,"tools":{"file_read":{"dirs":["/var/braid/my docs"]}}}`,
			extra: map[string]string{
				"BRAID_TOOL_SANDBOX": "docker",
			},
			wantErr:    "docker mount value domain",
			wantSource: "BRAID_TOOL_PACKAGE",
		},
		{
			name:     "unsigned http endpoints forbidden in signed mode",
			manifest: toolPkgManifest,
			extra: map[string]string{
				"BRAID_HTTP_ENDPOINTS": `{"docs_api":"https://docs.example.com"}`,
			},
			wantErr: "BRAID_HTTP_ENDPOINTS forbidden",
		},
		{
			name:     "unsigned fileread dirs forbidden in signed mode",
			manifest: toolPkgManifest,
			extra: map[string]string{
				"BRAID_FILEREAD_DIRS": `["/var/braid/docs"]`,
			},
			wantErr: "BRAID_FILEREAD_DIRS forbidden",
		},
		{
			name:     "package missing with pubkey set",
			manifest: toolPkgManifest,
			extra: map[string]string{
				"BRAID_TOOL_PACKAGE":           "",
				"BRAID_TOOL_PACKAGE_SIGNATURE": "",
			},
			wantErr: "both required",
		},
		{
			name:     "signature missing with pubkey set",
			manifest: toolPkgManifest,
			extra: map[string]string{
				"BRAID_TOOL_PACKAGE_SIGNATURE": "",
			},
			wantErr: "both required",
		},
		{
			name:     "package without pubkey cannot verify",
			manifest: toolPkgManifest,
			extra: map[string]string{
				"BRAID_TOOL_SIGNING_PUBLIC_KEY": "",
			},
			wantErr: "cannot verify",
		},
		{
			name:     "signature without pubkey cannot verify",
			manifest: "",
			extra: map[string]string{
				"BRAID_TOOL_SIGNING_PUBLIC_KEY": "",
				"BRAID_TOOL_PACKAGE":            "",
			},
			wantErr: "cannot verify",
		},
		{
			name:     "signature over different manifest rejected",
			manifest: toolPkgManifest,
			extra: map[string]string{
				"BRAID_TOOL_PACKAGE": b64Of(t, []byte(`{"version":1,"tools":{"http":{"endpoints":{"api":"https://a.test"}}}}`)),
			},
			wantErr: "rejected",
		},
		{
			name:     "unknown tool in manifest rejected",
			manifest: `{"version":1,"tools":{"command":{"dirs":["/x"]}}}`,
			wantErr:  "unknown tool",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := signedToolPkgEnv(t, c.manifest, c.extra)
			ts, err := BuiltinToolsFromEnv(envOf(env))
			switch {
			case c.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, c.wantErr)
				}
				if c.wantSource != "" && !strings.Contains(err.Error(), c.wantSource) {
					t.Fatalf("err = %v, want source attribution %q", err, c.wantSource)
				}
			case err != nil:
				t.Fatalf("err = %v, want nil", err)
			default:
				if got := strings.Join(registryOf(t, ts...).Names(), ","); got != c.wantNames {
					t.Fatalf("names = %q, want %q", got, c.wantNames)
				}
			}
		})
	}

	t.Run("bad signature base64 rejected", func(t *testing.T) {
		env := signedToolPkgEnv(t, toolPkgManifest, map[string]string{
			"BRAID_TOOL_PACKAGE_SIGNATURE": "!!not-base64!!",
		})
		_, err := BuiltinToolsFromEnv(envOf(env))
		if err == nil || !strings.Contains(err.Error(), "invalid base64") {
			t.Fatalf("err = %v, want base64 decode rejection", err)
		}
	})

	t.Run("bad pubkey base64 rejected", func(t *testing.T) {
		env := signedToolPkgEnv(t, toolPkgManifest, map[string]string{
			"BRAID_TOOL_SIGNING_PUBLIC_KEY": "!!not-base64!!",
		})
		_, err := BuiltinToolsFromEnv(envOf(env))
		if err == nil || !strings.Contains(err.Error(), "invalid base64") {
			t.Fatalf("err = %v, want base64 decode rejection", err)
		}
	})

	// legacy 回归锚:签名三键全缺省时,内置 env 路径行为不变
	// (既有 TestBuiltinToolsFromEnv 全表仍走该路径,此处钉「共存不串」:
	// 同设签名键与 legacy 键 = 拒,绝无双源合并)。
	t.Run("legacy env still works when signing keys absent", func(t *testing.T) {
		ts, err := BuiltinToolsFromEnv(envOf(map[string]string{
			"BRAID_HTTP_ENDPOINTS": `{"docs_api":"https://docs.example.com"}`,
		}))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(ts) != 1 || ts[0].Name() != "http" {
			t.Fatalf("tools = %v, want [http]", ts)
		}
		if _, ok := ts[0].(*HTTPTool); !ok {
			t.Fatalf("tool[0] = %T, want *HTTPTool", ts[0])
		}
	})
}

// TestBuiltinToolsFromEnvSignedPackageSandboxDocker:签名包路径与 T10.1
// 沙箱装配缝正交——docker 模式同名换装 sandboxCarrier、toolConfig 词形与
// helper 重建面同构、单例信号量共享(镜像 TestBuiltinToolsFromEnvSandbox
// DockerWrap,唯一差异 = eps/dirs 源自验签后清单)。
func TestBuiltinToolsFromEnvSignedPackageSandboxDocker(t *testing.T) {
	ts, err := BuiltinToolsFromEnv(envOf(signedToolPkgEnv(t, toolPkgManifest, map[string]string{
		"BRAID_TOOL_SANDBOX": "docker",
	})))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(ts) != 2 {
		t.Fatalf("tools = %d, want 2", len(ts))
	}
	hc, ok := ts[0].(*sandboxCarrier)
	if !ok {
		t.Fatalf("tool[0] = %T, want *sandboxCarrier", ts[0])
	}
	fc, ok := ts[1].(*sandboxCarrier)
	if !ok {
		t.Fatalf("tool[1] = %T, want *sandboxCarrier", ts[1])
	}
	if hc.Name() != "http" || fc.Name() != "file_read" {
		t.Fatalf("names = %q/%q, want http/file_read", hc.Name(), fc.Name())
	}
	if hc.limiter != fc.limiter {
		t.Fatal("carriers must share one limiter")
	}
	// toolConfig 词形与 env 路径逐字节同构(容器内重建吃同一份词形)。
	if string(hc.toolConfig) != `{"endpoints":{"docs_api":"https://docs.example.com"}}` {
		t.Fatalf("http toolConfig = %s", hc.toolConfig)
	}
	if string(fc.toolConfig) != `{"dirs":["/var/braid/docs"]}` {
		t.Fatalf("file_read toolConfig = %s", fc.toolConfig)
	}
	if _, err := rebuildSandboxTool("http", hc.toolConfig); err != nil {
		t.Fatalf("rebuild http: %v", err)
	}
	if _, err := rebuildSandboxTool("file_read", fc.toolConfig); err != nil {
		t.Fatalf("rebuild file_read: %v", err)
	}
}

// TestToolPackageOpenSSLGolden:openssl 互操作 golden 钉子——发布物真实
// 形态(openssl genpkey SPKI-DER 尾 32B 公钥 + pkeyutl -sign -rawin 原始
// 签名)必须被 verifyToolPackage 接受(README §8 的兼容声明从「实测过」
// 升级为「持续被证」)。向量由 openssl 3.x 一次性产出后硬编码;私钥已
// 弃置,公钥/签名非秘密。
func TestToolPackageOpenSSLGolden(t *testing.T) {
	const (
		goldenPub = "VnYRGnJWgI0lmT9GCV6MZhwu5v5JrvwxWnaCDItA8lE="
		goldenPkg = "eyJ2ZXJzaW9uIjoxLCJ0b29scyI6eyJodHRwIjp7ImVuZHBvaW50cyI6eyJnb2xkZW4iOiJodHRwczovL2dvbGRlbi5leGFtcGxlLmNvbSJ9fX19"
		goldenSig = "imhr3/raaikig9BXNY6xwHxHGdBCDjxNKVDzYPvDZgrA/4s1t2ugxchc1UwTBNqnfSV2ZTins1h25jMVx88fDg=="
	)
	p, err := verifyToolPackage(goldenPub, goldenPkg, goldenSig)
	if err != nil {
		t.Fatalf("verify openssl-signed golden: %v", err)
	}
	if got := p.Tools["http"].Endpoints["golden"]; got != "https://golden.example.com" {
		t.Fatalf("golden endpoint = %q", got)
	}
	// 篡改 manifest 字节(golden→golden!再编码)→ 签名失配即拒。
	raw, err := base64.StdEncoding.DecodeString(goldenPkg)
	if err != nil {
		t.Fatalf("decode golden pkg: %v", err)
	}
	idx := strings.Index(string(raw), "golden.example.com")
	if idx < 0 {
		t.Fatalf("golden manifest lacks expected endpoint")
	}
	raw[idx] = 'e' // golden → eolden
	if _, err := verifyToolPackage(goldenPub, b64Of(t, raw), goldenSig); err == nil ||
		!strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("err = %v, want signature mismatch on tampered golden manifest", err)
	}
}

// TestToolPackageMCPManifest:T13.2 MCP 声明族的清单校验表(签名后过
// verifyToolPackage;结构面在 parseToolPackage,值面在 NewMCPTools)。
func TestToolPackageMCPManifest(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	const withSrv = `{"version":1,"mcp_servers":{"weather":"https://mcp.example.com"},"tools":%s}`
	rejects := []struct {
		name     string
		manifest string
		wantErr  string
	}{
		{"builtin name carrying mcp", `{"version":1,"mcp_servers":{"w":"https://a.test"},"tools":{"http":{"mcp":{"server":"w","tool":"t"}}}}`, "must not carry mcp"},
		{"db_query carrying mcp", `{"version":1,"mcp_servers":{"w":"https://a.test"},"tools":{"db_query":{"mcp":{"server":"w","tool":"t"}}}}`, "must not carry mcp"},
		{"mcp tool carrying endpoints", fmt.Sprintf(withSrv, `{"mcp_x":{"endpoints":{"a":"https://a.test"},"mcp":{"server":"weather","tool":"t"}}}`), "must not carry endpoints/dirs"},
		{"mcp tool carrying dirs", fmt.Sprintf(withSrv, `{"mcp_x":{"dirs":["/x"],"mcp":{"server":"weather","tool":"t"}}}`), "must not carry endpoints/dirs"},
		{"mcp tool name shape", `{"version":1,"mcp_servers":{"w":"https://a.test"},"tools":{"Mcp-X":{"mcp":{"server":"w","tool":"t"}}}}`, "tool name"},
		{"undeclared server ref", `{"version":1,"mcp_servers":{"w":"https://a.test"},"tools":{"mcp_x":{"mcp":{"server":"other","tool":"t"}}}}`, "undeclared server"},
		{"remote tool name shape", fmt.Sprintf(withSrv, `{"mcp_x":{"mcp":{"server":"weather","tool":"not allowed!"}}}`), "remote tool name"},
		{"unreferenced server", `{"version":1,"mcp_servers":{"w":"https://a.test","dead":"https://b.test"},"tools":{"mcp_x":{"mcp":{"server":"w","tool":"t"}}}}`, "not referenced"},
		{"server alias shape", `{"version":1,"mcp_servers":{"W":"https://a.test"},"tools":{"mcp_x":{"mcp":{"server":"W","tool":"t"}}}}`, "alias"},
		{"unknown field in mcp ref", fmt.Sprintf(withSrv, `{"mcp_x":{"mcp":{"server":"weather","tool":"t","extra":1}}}`), "unknown field"},
	}
	for _, c := range rejects {
		t.Run(c.name, func(t *testing.T) {
			sig, err := signToolPackage(priv, []byte(c.manifest))
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			_, err = verifyToolPackage(b64Of(t, priv.Public().(ed25519.PublicKey)), b64Of(t, []byte(c.manifest)), sig)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, c.wantErr)
			}
		})
	}

	// 合法形:server + 引用闭合 + 显式空 tools 与空 mcp_servers 共存;
	// mcp_servers null = 缺席(与键不在场同判——存量清单零破坏,空发布
	// 的显式性要求只约束 "tools" 键)。
	for _, c := range []struct {
		name     string
		manifest string
	}{
		{"valid mcp tool", `{"version":1,"mcp_servers":{"w":"https://a.test"},"tools":{"mcp_x":{"mcp":{"server":"w","tool":"t"}}}}`},
		{"builtin and mcp coexist", `{"version":1,"mcp_servers":{"w":"https://a.test"},"tools":{"http":{"endpoints":{"a":"https://a.test"}},"mcp_x":{"mcp":{"server":"w","tool":"t"}}}}`},
		{"explicit empty mcp_servers", `{"version":1,"mcp_servers":{},"tools":{}}`},
		{"null mcp_servers equals absent", `{"version":1,"mcp_servers":null,"tools":{}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			sig, err := signToolPackage(priv, []byte(c.manifest))
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			if _, err := verifyToolPackage(b64Of(t, priv.Public().(ed25519.PublicKey)), b64Of(t, []byte(c.manifest)), sig); err != nil {
				t.Fatalf("verify: %v, want nil", err)
			}
		})
	}
}

// TestResolveToolSetMCPMatrix:T13.2 env 矩阵——签名态 unsigned MCP 键拒
// (凭证键例外)、非签名态双键解析 + 引用闭合半配置拒、合法态 toolSet
// 填充(装配/egress 派生共同消费面)。
func TestResolveToolSetMCPMatrix(t *testing.T) {
	const servers = `{"weather":"https://mcp.example.com"}`
	const tools = `{"mcp_weather_lookup":{"server":"weather","tool":"lookup_weather"}}`

	t.Run("signed state rejects unsigned mcp keys", func(t *testing.T) {
		for key, val := range map[string]string{
			"BRAID_MCP_SERVERS": servers,
			"BRAID_MCP_TOOLS":   tools,
		} {
			env := signedToolPkgEnv(t, toolPkgManifest, map[string]string{key: val})
			_, err := resolveToolSet(mustGetenv(env))
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("err = %v, want rejection naming %s", err, key)
			}
		}
	})

	t.Run("signed state tolerates credential token keys", func(t *testing.T) {
		env := signedToolPkgEnv(t, toolPkgManifest, map[string]string{"BRAID_MCP_TOKEN_WEATHER": "sekrit"})
		if _, err := resolveToolSet(mustGetenv(env)); err != nil {
			t.Fatalf("credential channel must not trip unsigned-source rejection: %v", err)
		}
	})

	t.Run("signed manifest populates toolset", func(t *testing.T) {
		manifest := `{"version":1,"mcp_servers":{"weather":"https://mcp.example.com"},` +
			`"tools":{"mcp_weather_lookup":{"mcp":{"server":"weather","tool":"lookup_weather"}}}}`
		env := signedToolPkgEnv(t, manifest, nil)
		ts, err := resolveToolSet(mustGetenv(env))
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if ts.mcpSource != "BRAID_TOOL_PACKAGE" || len(ts.mcpServers) != 1 || len(ts.mcpTools) != 1 {
			t.Fatalf("toolset mcp fields = %+v", ts)
		}
		if ref, ok := ts.mcpTools["mcp_weather_lookup"]; !ok || ref.Server != "weather" || ref.Tool != "lookup_weather" {
			t.Fatalf("mcpTools = %+v", ts.mcpTools)
		}
	})

	t.Run("env path parses and enforces reference closure", func(t *testing.T) {
		ts, err := resolveToolSet(mustGetenv(map[string]string{
			"BRAID_MCP_SERVERS": servers,
			"BRAID_MCP_TOOLS":   tools,
		}))
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if ts.mcpSource != "BRAID_MCP_SERVERS/BRAID_MCP_TOOLS" || len(ts.mcpServers) != 1 || len(ts.mcpTools) != 1 {
			t.Fatalf("toolset = %+v", ts)
		}
	})

	t.Run("env half configs reject", func(t *testing.T) {
		cases := []struct {
			name string
			env  map[string]string
			want string
		}{
			{"tools without servers", map[string]string{"BRAID_MCP_TOOLS": tools}, "not declared in BRAID_MCP_SERVERS"},
			{"servers without tools", map[string]string{"BRAID_MCP_SERVERS": servers}, "not referenced"},
			{"bad servers json", map[string]string{"BRAID_MCP_SERVERS": "{", "BRAID_MCP_TOOLS": tools}, "BRAID_MCP_SERVERS"},
			{"bad tools json", map[string]string{"BRAID_MCP_SERVERS": servers, "BRAID_MCP_TOOLS": "["}, "BRAID_MCP_TOOLS"},
			{"unknown field in mcp ref env", map[string]string{"BRAID_MCP_SERVERS": servers, "BRAID_MCP_TOOLS": `{"mcp_x":{"server":"weather","name":"typo"}}`}, "BRAID_MCP_TOOLS"},
			{"trailing garbage in servers", map[string]string{"BRAID_MCP_SERVERS": servers + " TRAILING", "BRAID_MCP_TOOLS": tools}, "trailing data"},
			{"second json doc in tools", map[string]string{"BRAID_MCP_SERVERS": servers, "BRAID_MCP_TOOLS": tools + ` {"mcp_y":{"server":"weather","tool":"t2"}}`}, "trailing data"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				_, err := resolveToolSet(mustGetenv(c.env))
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("err = %v, want containing %q", err, c.want)
				}
			})
		}
	})
}

// mustGetenv 把 map 适配成 getenv 函数(缺失键 = 空值,与 os.Getenv 同判)。
func mustGetenv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

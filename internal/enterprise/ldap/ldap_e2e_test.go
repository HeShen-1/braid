package ldap

// LDAP bind 容器化 e2e(T17.6 验收方案,D-2026-10-04-t176):钉版
// osixia/openldap 测试内自起(sandbox e2e 同款 docker-in-test 模式,零
// ci.yml 依赖;docker 缺席自动 skip,BRAID_TEST_REQUIRE_STACK=1 下不可达
// 即红 = CI 真跑),MySQL(t13 栈)承载本地锚定面 + apiflow 断言。
// 种子目录:alice(本地 ACTIVE,角色取本地库 = D-t111 锚定活证)/bob
// (本地 DISABLED)/ghost(目录有、本地无)/twin ×2(两 OU 歧义);服务
// 账号 = admin 充任(见下方 ACL 注记)。StartTLS/ldaps 不在本面:客户端验证走系统信任库,
// 私有 CA 属部署层责任(OS 信任库装载),登记 D-t176。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"braid/internal/api"
	"braid/internal/auth"
	"braid/internal/db/sqltest"
	"braid/internal/scheduler"
	"braid/internal/snowflake"
	"braid/internal/tools"
)

// ldapE2EImage 钉版镜像(mcr/playwright/mysql/redis 同款钉版纪律);
// 1.5.0 = osixia 稳定线,变动须人工同步复核。
const ldapE2EImage = "osixia/openldap:1.5.0"

// ldapE2EDomain 与种子 LDIF、服务账号 DN 同源;admin 密码 = 一次性本地
// 容器占位值,非任何真实系统凭证(t13/migrate-test 同纪律)。服务账号
// 以 admin 充任:osixia 默认 ACL = `{2}to * by self read by cn=admin
// write by * none`(1.5.0 实证)——目录树对非 self/admin 主体零读权,
// 专用服务账号的读授权是目录侧运维动作(braid 不碰目录 ACL),生产
// 形态(BindDN = 专用账号)由 .env.example 文档承载。
const (
	ldapE2EDomain     = "example.org"
	ldapE2EAdminPass  = "e2e-admin-pass"
	ldapE2ESvcBindDN  = "cn=admin,dc=example,dc=org"
	ldapE2ESvcPass    = ldapE2EAdminPass
	ldapE2EBaseDN     = "dc=example,dc=org"
	ldapE2EStartupSec = 60
)

// ldapSeedLDIF:osixia bootstrap custom ldif(管理员身份导入)。明文
// userPassword = slapd 简单 bind 直比(测试目录专用,不涉生产)。
const ldapSeedLDIF = `# braid t17.6 ldap e2e seed
dn: ou=people,dc=example,dc=org
objectClass: organizationalUnit
ou: people

dn: ou=contractors,dc=example,dc=org
objectClass: organizationalUnit
ou: contractors

dn: uid=alice,ou=people,dc=example,dc=org
objectClass: inetOrgPerson
uid: alice
cn: Alice Example
sn: Example
userPassword: alice-ldap-pass

dn: uid=bob,ou=people,dc=example,dc=org
objectClass: inetOrgPerson
uid: bob
cn: Bob Example
sn: Example
userPassword: bob-ldap-pass

dn: uid=ghost,ou=people,dc=example,dc=org
objectClass: inetOrgPerson
uid: ghost
cn: Ghost Example
sn: Example
userPassword: ghost-ldap-pass

dn: uid=twin,ou=people,dc=example,dc=org
objectClass: inetOrgPerson
uid: twin
cn: Twin One
sn: Example
userPassword: twin-pass

dn: uid=twin,ou=contractors,dc=example,dc=org
objectClass: inetOrgPerson
uid: twin
cn: Twin Two
sn: Example
userPassword: twin-pass
`

// ldapGateSkipOrFail:docker 不可达时按环境分流(本地 Skipf / CI
// BRAID_TEST_REQUIRE_STACK=1 Fatalf——sandbox e2e 同口径,「CI 绿 =
// 集成测试真跑」)。
func ldapGateSkipOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("BRAID_TEST_REQUIRE_STACK") == "1" {
		t.Fatalf("ldap e2e gated off but stack required (docker must be reachable): "+format, args...)
	}
	t.Skipf(format, args...)
}

// ldapDockerGate:docker bin/daemon 可达性门(sandbox e2e 同款;探活
// 命令 docker info,冷启动 daemon 不误判)。
func ldapDockerGate(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		ldapGateSkipOrFail(t, "docker binary not in PATH: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").Run(); err != nil {
		ldapGateSkipOrFail(t, "docker daemon unreachable within 15s (cold-start?): %v", err)
	}
}

// ldapSlotNumber:BRAID_TEST_REDIS_DB 语义即槽位号(容器命名后缀 =
// 并行 worktree 互不互踩,sandbox e2e 同纪律)。
func ldapSlotNumber(t *testing.T) int {
	t.Helper()
	raw := os.Getenv("BRAID_TEST_REDIS_DB")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > 15 {
		t.Fatalf("BRAID_TEST_REDIS_DB %q invalid as slot number (want 0..15)", raw)
	}
	return n
}

// ldapE2EStack 起钉版 openldap 容器 + 自管种子,返回容器内可达的
// ldap://127.0.0.1:<随机端口>(-p 127.0.0.1::389,随机宿主端口 = 同
// 槽位并行亦零撞;容器名带槽位后缀,崩溃残留可人工 rm)。种子路线 =
// 裸容器就绪后 docker cp + ldapadd(管理员身份):镜像的 custom ldif
// bind-mount 挂载与其启动尾部 rm -rf 自清理结构性冲突(单文件挂载另
// 吃 sed 原地 rename,均 EBUSY 实证)——自管种子零镜像内部机制依赖。
// 就绪判据 = 管理员 bind 成功(base dc=example,dc=org 由 LDAP_DOMAIN
// 生成;服务账号在种子后才存在,故就绪探测用管理员)。
func ldapE2EStack(t *testing.T, name string) string {
	t.Helper()
	// 撞名预清只清非 running 残留(对抗审计 LOW 修):前轮崩溃留下的
	// stopped 同名容器会让 docker run 撞名响亮红,自愈优于手工;但同槽位
	// 若有活容器在跑(并行公约违例的信号),强删 = 互删对方 lane——此时
	// 不动它,Fatalf 让公约违例响亮现形(容器名确定性命名是设计面:
	// 崩溃残留可归因,活容器归属可查)。
	if out, err := exec.Command("docker", "inspect", "--format", "{{.State.Running}}", name).Output(); err == nil {
		if strings.TrimSpace(string(out)) == "true" {
			t.Fatalf("container %s already running: live same-slot sibling (slot collision — resolve it, refusing to rm a live lane)", name)
		}
		if out, err := exec.Command("docker", "rm", "-f", name).CombinedOutput(); err == nil {
			t.Logf("pre-cleaned stale container %s", strings.TrimSpace(string(out)))
		}
	}
	run := exec.Command("docker", "run", "-d", "--name", name,
		"-e", "LDAP_ORGANIZATION=Example",
		"-e", "LDAP_DOMAIN="+ldapE2EDomain,
		"-e", "LDAP_ADMIN_PASSWORD="+ldapE2EAdminPass,
		"-p", "127.0.0.1::389",
		ldapE2EImage)
	var runOut, runErr bytes.Buffer
	run.Stdout, run.Stderr = &runOut, &runErr
	if err := run.Run(); err != nil {
		t.Fatalf("docker run %s: %v\n%s", ldapE2EImage, err, runErr.String())
	}
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "rm", "-f", name).CombinedOutput(); err != nil {
			t.Logf("cleanup: rm -f %s: %v\n%s", name, err, out)
		}
	})
	// 端口发现:docker port <name> 389 → "389/tcp -> 127.0.0.1:327XX"。
	portOut, err := exec.Command("docker", "port", name, "389").Output()
	if err != nil {
		t.Fatalf("docker port %s: %v", name, err)
	}
	fields := strings.Fields(string(portOut))
	if len(fields) == 0 {
		t.Fatalf("docker port %s: empty mapping\n%s", name, portOut)
	}
	addr := "ldap://" + fields[len(fields)-1]

	// 就绪轮询(双触判稳):管理员 bind 成功后隔 1.5s 再 bind 成功才算
	// 就绪——osixia 启动双阶段:init 期 slapd 起而复停(bootstrap 导入
	// 后 stop),process 管理器再拉起正式实例;单触会撞进重启窗
	//(实证:前几臂过、后续全 EOF)。生产路径 openServiceConn,凭据
	// 临时换管理员——服务账号在种子后才存在;base 域生成完即可达。
	deadline := time.Now().Add(ldapE2EStartupSec * time.Second)
	probe := LDAPConfig{Addr: addr, BindDN: "cn=admin," + ldapE2EBaseDN,
		BindPassword: ldapE2EAdminPass, BaseDN: ldapE2EBaseDN, Filter: DefaultLDAPFilter}
	for {
		if conn, err := openServiceConn(probe); err == nil {
			_ = conn.Close()
			time.Sleep(1500 * time.Millisecond)
			if conn2, err2 := openServiceConn(probe); err2 == nil {
				_ = conn2.Close()
				break
			}
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", "--tail", "80", name).CombinedOutput()
			t.Fatalf("openldap not ready (stable) within %ds at %s\nlogs:\n%s", ldapE2EStartupSec, addr, logs)
		}
		time.Sleep(700 * time.Millisecond)
	}

	// 种子:docker cp + ldapadd(容器内 ldap-utils;失败响亮带输出)。
	seedHost := filepath.Join(t.TempDir(), "50-seed.ldif")
	if err := os.WriteFile(seedHost, []byte(ldapSeedLDIF), 0o644); err != nil {
		t.Fatalf("write seed ldif: %v", err)
	}
	if out, err := exec.Command("docker", "cp", seedHost, name+":/tmp/50-seed.ldif").CombinedOutput(); err != nil {
		t.Fatalf("docker cp seed: %v\n%s", err, out)
	}
	add := exec.Command("docker", "exec", name, "ldapadd", "-x", "-H", "ldap://localhost",
		"-D", "cn=admin,"+ldapE2EBaseDN, "-w", ldapE2EAdminPass, "-f", "/tmp/50-seed.ldif")
	if out, err := add.CombinedOutput(); err != nil {
		logs, _ := exec.Command("docker", "logs", "--tail", "40", name).CombinedOutput()
		t.Fatalf("ldapadd seed: %v\n%s\nlogs:\n%s", err, out, logs)
	}
	return addr
}

// TestLDAPBindE2E:容器化全链(T17.6 验收)。前置 = docker + t13 MySQL
// (锚定面),缺一即 skip(CI REQUIRE_STACK=1 下红)。臂:构造探测 /
// 成功锚定(角色取本地)/ 错密 / 目录零命中 / 目录有本地无(锚定
// unknown_user)/ 本地停用 / 双条目歧义(500 族)/ api 全链
// (login_audit.method='ldap')。
func TestLDAPBindE2E(t *testing.T) {
	ldapDockerGate(t)
	slot := ldapSlotNumber(t)
	// t13 MySQL:锚定 + 审计面(栈不可达 MustOpenAt 自行 skip/红)。
	dbh := sqltest.MustOpenAt(t, "braid_ent_ldap_test")
	t.Cleanup(func() {
		_, _ = dbh.Exec(`DELETE FROM login_audit WHERE username IN ('alice', 'bob')`)
		_, _ = dbh.Exec(`DELETE FROM login_throttle`)
		_, _ = dbh.Exec(`DELETE FROM user_role`)
		_, _ = dbh.Exec(`DELETE FROM users`)
	})

	cfg := LDAPConfig{
		Addr: "ldap://placeholder-resolved-by-stack-helper", BindDN: ldapE2ESvcBindDN,
		BindPassword: ldapE2ESvcPass, BaseDN: ldapE2EBaseDN, Filter: DefaultLDAPFilter,
	}
	addr := ldapE2EStack(t, fmt.Sprintf("braid-ldap-e2e-t%d", slot))
	cfg.Addr = addr

	prov, err := NewLDAPBindProvider(context.Background(), cfg, dbh)
	if err != nil {
		t.Fatalf("startup probe against live directory: %v", err)
	}

	// 本地种子(经导出面装配,entoidc fixtures 同形):alice ACTIVE
	//(角色 author——断言角色取本地库,非目录)、bob 先建后停用
	//(DISABLED 判定在锚定缝,密码后判定同款)。
	ldapCreateUser(t, dbh, "alice", auth.RoleAuthor)
	ldapCreateUser(t, dbh, "bob", auth.RoleAuthor)
	if _, err := dbh.Exec(`UPDATE users SET status='DISABLED' WHERE username='bob'`); err != nil {
		t.Fatalf("disable bob: %v", err)
	}

	assertCreds := func(t *testing.T, err error, wantReason string) {
		t.Helper()
		var ce *auth.CredentialsError
		if !errors.As(err, &ce) {
			t.Fatalf("err = %v, want CredentialsError", err)
		}
		if ce.Reason != wantReason {
			t.Fatalf("reason = %q, want %q", ce.Reason, wantReason)
		}
		if !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("err 链缺 ErrInvalidCredentials: %v", err)
		}
	}

	t.Run("success anchors local roles (bind verdict external, identity local)", func(t *testing.T) {
		user, err := prov.Authenticate(context.Background(), "alice", "alice-ldap-pass")
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if user.Username != "alice" || len(user.Roles) != 1 || user.Roles[0] != auth.RoleAuthor {
			t.Fatalf("user = %+v, want alice/[author](角色自本地库,D-t111)", user)
		}
	})
	t.Run("wrong password maps to wrong_password", func(t *testing.T) {
		_, err := prov.Authenticate(context.Background(), "alice", "not-the-pass")
		assertCreds(t, err, auth.ReasonWrongPassword)
	})
	t.Run("directory zero-hit maps to unknown_user", func(t *testing.T) {
		_, err := prov.Authenticate(context.Background(), "nobody-here", "whatever")
		assertCreds(t, err, auth.ReasonUnknownUser)
	})
	t.Run("dir-only subject anchors to unknown_user (no local account, same 401 shape)", func(t *testing.T) {
		_, err := prov.Authenticate(context.Background(), "ghost", "ghost-ldap-pass")
		assertCreds(t, err, auth.ReasonUnknownUser)
	})
	t.Run("disabled local account maps to ErrUserDisabled after bind", func(t *testing.T) {
		_, err := prov.Authenticate(context.Background(), "bob", "bob-ldap-pass")
		if !errors.Is(err, auth.ErrUserDisabled) {
			t.Fatalf("err = %v, want ErrUserDisabled", err)
		}
	})
	t.Run("ambiguous entries are an identity-source fault, never a verdict", func(t *testing.T) {
		_, err := prov.Authenticate(context.Background(), "twin", "twin-pass")
		if err == nil {
			t.Fatal("ambiguity must fail")
		}
		var ce *auth.CredentialsError
		if errors.As(err, &ce) {
			t.Fatalf("identity-source fault misclassified as login verdict: %v", err)
		}
		if !strings.Contains(err.Error(), "entries match") {
			t.Fatalf("err = %v, want ambiguity wrap", err)
		}
	})

	t.Run("api /auth/login flow writes method='ldap'", func(t *testing.T) {
		ids, err := snowflake.New(1)
		if err != nil {
			t.Fatalf("snowflake: %v", err)
		}
		reg, err := tools.NewRegistry()
		if err != nil {
			t.Fatalf("registry: %v", err)
		}
		gin.SetMode(gin.TestMode)
		srv := api.NewServer(api.Config{Addr: api.DefaultAddr, JWTSecret: ldapTestSecret,
			SnowflakeNode:  snowflake.Explicit(1),
			LoginRateLimit: 100000, LoginRateWindow: time.Minute,
			LoginMaxFailures: 100000, LoginLockout: time.Minute},
			dbh, ids, reg, ldapFakeTrigger{id: 1}, prov)
		srv.CredentialLoginMethod(api.LoginMethodLDAP) // 装配面同 cmd/api
		r := srv.Router()

		code, out := ldapDoJSON(t, r, http.MethodPost, "/api/v1/auth/login", "",
			[]byte(`{"username":"alice","password":"alice-ldap-pass"}`))
		if code != http.StatusOK || out["token"] == nil {
			t.Fatalf("login = %d %v, want 200 + token", code, out)
		}
		code, out = ldapDoJSON(t, r, http.MethodPost, "/api/v1/auth/login", "",
			[]byte(`{"username":"alice","password":"wrong"}`))
		if code != http.StatusUnauthorized || out["error"] != "invalid_credentials" {
			t.Fatalf("login(wrong) = %d %v, want 401 invalid_credentials", code, out)
		}
		// 审计断言:method='ldap'(0014 归因词经装配旋钮落行),成功行
		// reason NULL、失败行 reason=wrong_password(reason 词表单源)。
		rows, err := dbh.Query(`SELECT event_type, IFNULL(reason,'') FROM login_audit
			WHERE username='alice' ORDER BY id`)
		if err != nil {
			t.Fatalf("query audit: %v", err)
		}
		defer func() { _ = rows.Close() }()
		got := [][2]string{}
		for rows.Next() {
			var ev, rs string
			if err := rows.Scan(&ev, &rs); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, [2]string{ev, rs})
		}
		// 调用序:先成功后错密 → 按 id 自然序 succeeded 在前。
		want := [][2]string{{"login.succeeded", ""}, {"login.failed", "wrong_password"}}
		if len(got) != 2 {
			t.Fatalf("login_audit rows = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("login_audit rows = %v, want %v (method 另查)", got, want)
			}
		}
		var methods string
		if err := dbh.QueryRow(`SELECT GROUP_CONCAT(method ORDER BY id) FROM login_audit
			WHERE username='alice'`).Scan(&methods); err != nil {
			t.Fatalf("query methods: %v", err)
		}
		if methods != "ldap,ldap" {
			t.Fatalf("login_audit methods = %q, want \"ldap,ldap\"(0014 归因词)", methods)
		}
	})
}

// ldapTestSecret:测试密钥纪律(gitleaks 实测同款,低熵词组占位)。
const ldapTestSecret = "unit-test-jwt-signing-secret-words-only-placeholder"

// ldapSeqIDs:计数器 IDGen 桩(entoidc fixtures 同形;Snowflake 列契约
// 交由应用侧生成,裸 INSERT 不成立)。
type ldapSeqIDs struct {
	base uint64
	cur  atomic.Uint64
}

func (s *ldapSeqIDs) Next() (uint64, error) { return s.base + s.cur.Add(1), nil }

// ldapCreateUser:auth.CreateUser 导出面装配的种子捷径(entoidc
// fixtures 同形;本地口令永不用于登录——认证走目录 bind)。
func ldapCreateUser(t *testing.T, d *sql.DB, username string, roles ...auth.Role) {
	t.Helper()
	hash, err := auth.HashPassword("local-unused-"+username, 4)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := auth.WithTx(context.Background(), d, func(q auth.DBTX) error {
		return auth.CreateUser(context.Background(), q, &ldapSeqIDs{base: uint64(time.Now().UnixNano())}, auth.CreateUserParams{
			Username: username, PasswordHash: hash, Roles: roles,
		})
	}); err != nil {
		t.Fatalf("create user %q: %v", username, err)
	}
}

// ldapFakeTrigger:api.ExecutionTrigger 缝桩(entoidc apiflow 同形复制,
// 本包测试无法触达他包内桩)。
type ldapFakeTrigger struct {
	id  uint64
	err error
}

func (f ldapFakeTrigger) Instantiate(context.Context, scheduler.InstantiateParams) (uint64, error) {
	return f.id, f.err
}

func (f ldapFakeTrigger) RollbackTo(_ context.Context, p scheduler.RollbackParams) (*scheduler.RollbackResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &scheduler.RollbackResult{TargetNodeID: p.TargetNodeID}, nil
}

// CancelExecution(T19.3):fake 面与 RollbackTo 同构。
func (f ldapFakeTrigger) CancelExecution(context.Context, scheduler.CancelParams) (*scheduler.CancelResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &scheduler.CancelResult{CancelledCount: 1}, nil
}

// ldapDoJSON:api 包 doJSON 同形复制(httptest 往返)。
func ldapDoJSON(t *testing.T, r *gin.Engine, method, path, token string, body []byte) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("non-JSON body (code %d): %q", w.Code, w.Body.String())
		}
	}
	return w.Code, out
}

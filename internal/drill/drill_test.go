package drill

// T15.6 多副本整体演练 runner(D-t151 决策 6 注入集五项 + R8 复核):
//  ①双 worker 抢 claim 零 1062 + 租约号互异;
//  ②kill -9 持租约 worker → 残窗按决策 2 语义收敛 + reaper 判死接管;
//  ③滚动重启沙箱在飞 → owner-scoped 清扫不互杀 + t20.4 语义追认(t20.8):
//    同 owner 活租约在场 ⇒ 启动清扫跳过,清行/TTL 过期后下次启动收敛
//    (recreate 孤儿/runbook 另验);
//  ④Redis 重启(AOF)→ outbox 恢复投递 + 重派收敛;
//  ⑤mode=all 硬闸断言(钉在「新 api 收到的请求」,演练栈 api 单副本);
//  ⑦least-privilege 回滚缝(驳回→resume→重跑,授权缺角运行时红);
//  ⑥终态:哈希链全链绿(D-t93③/R8 口径复核)+ 组件日志零 1062。
//
// 栈:deploy/drill.docker-compose.yml(worker×2 / reaper×2 / api×1 / relay×1,
// 零 container_name,Redis AOF 卷,时间旋钮压缩;租约 TTL 保真 30s)。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"braid/internal/audit"
	"braid/internal/tools"
)

const (
	drillUserAuthor = "drillops" // author+approver+admin:发布/触发/审批全权
	drillUserApprv  = "drillapp" // approver:mode=all 第二签
	drillPassword   = "drill-pass-not-a-secret"

	apiPortBase   = 17400 // 槽位偏移基:api = 17400+slot
	mysqlPortBase = 17410 // mysql = 17410+slot(host-netns worker 消费面)
	redisPortBase = 17420 // redis = 17420+slot(host-netns worker 消费面)

	bootstrapWait = 90 * time.Second // 栈起 + migrate/grants + healthz
	armWait       = 60 * time.Second // 单臂收敛预算(压缩旋钮下秒级到达)
)

// drillAnyDSL:四连审批(approval-only,零 LLM/工具依赖)——claim/接管/
// Redis 恢复三臂共用。
func drillAnyDSL(name string) []byte {
	node := func(id string) string {
		return fmt.Sprintf(`{"id":%q,"type":"approval","approvers":["%s"]}`, id, drillUserAuthor)
	}
	return fmt.Appendf(nil, `{"dsl":"1.0","name":%q,
"edges":[{"from":"a","to":"b"},{"from":"b","to":"c"},{"from":"c","to":"d"}],
"nodes":[%s,%s,%s,%s],"outputs":{}}`,
		name, node("a"), node("b"), node("c"), node("d"))
}

// drillAllDSL:会签 mode=all(硬闸臂;双人签)。
func drillAllDSL(name string) []byte {
	return fmt.Appendf(nil, `{"dsl":"1.0","name":%q,
"nodes":[{"id":"sign","type":"approval","approvers":["%s","%s"],"mode":"all","timeout_hours":24}],
"outputs":{}}`, name, drillUserAuthor, drillUserApprv)
}

// --- 栈 ----------------------------------------------------------------------

type stack struct {
	t       *testing.T
	root    string
	slot    int
	project string
	apiURL  string
	rootDSN string
	dbh     *sql.DB
	tokens  map[string]string
	execIDs []uint64 // 哈希链终验收集面
}

func TestMultiReplicaDrill(t *testing.T) {
	slot, ok := drillSlot(t)
	if !ok {
		return
	}
	if !dockerReachable(t) {
		return
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	s := &stack{
		t:       t,
		root:    root,
		slot:    slot,
		project: fmt.Sprintf("braid-drill-t%d", slot),
		apiURL:  fmt.Sprintf("http://127.0.0.1:%d", apiPortBase+slot),
		rootDSN: fmt.Sprintf("root:drill-root@tcp(127.0.0.1:%d)/braid?loc=UTC&parseTime=true&time_zone=%%27%%2B00%%3A00%%27", mysqlPortBase+slot),
		tokens:  map[string]string{},
	}
	s.buildImages(root)
	egressBefore := egressNetworks(t)
	s.up(root)
	t.Cleanup(func() {
		s.down()
		cleanupEgressNetworks(t, egressBefore)
	})
	s.mustBootstrap()

	t.Run("arm1 dual worker claim", s.armClaim)
	t.Run("arm2 kill9 residual and takeover", s.armKillTakeover)
	t.Run("arm3 owner scoped sweep", s.armOwnerSweep)
	t.Run("arm4 redis restart outbox recovery", s.armRedisRestart)
	t.Run("arm5 mode=all fleet gate", s.armFleetGate)
	t.Run("arm7 rollback under least privilege", s.armRollbackResume)
	t.Run("arm8 lease fencing deposes thawed zombie", s.armLeaseFencing)
	t.Run("arm6 hash chain green", s.armChainVerify)
}

// drillSlot:槽位 = BRAID_TEST_REDIS_DB(0..3;缺省 0)。本包零 t13 键空间
// 消费,槽位只作 docker 资源命名空间(决策 6 裁定,见 doc.go)。
func drillSlot(t *testing.T) (int, bool) {
	t.Helper()
	raw := os.Getenv("BRAID_TEST_REDIS_DB")
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > 3 {
		t.Fatalf("BRAID_TEST_REDIS_DB=%q: want slot 0..3", raw)
	}
	return n, true
}

// dockerReachable:显式 opt-in 门(GATE 1 拍板)——BRAID_TEST_REQUIRE_DRILL=1
// 才跑(docker/栈不可达即红);未设恒 skip(docker 在场也不跑:演练是验收
// 仪式不是每 commit 回归,go test ./... 与 CI test 作业零触碰;不继承
// REQUIRE_STACK)。compose 锚测试不受此门(零 docker 依赖恒跑)。
func dockerReachable(t *testing.T) bool {
	t.Helper()
	if os.Getenv("BRAID_TEST_REQUIRE_DRILL") != "1" {
		t.Skip("drill is opt-in: set BRAID_TEST_REQUIRE_DRILL=1 (see AGENTS.md 命令节 / deploy/README §11)")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("BRAID_TEST_REQUIRE_DRILL=1 but docker not in PATH: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Fatalf("BRAID_TEST_REQUIRE_DRILL=1 but docker daemon unreachable: %v", err)
	}
	return true
}

func (s *stack) run(args ...string) []byte {
	s.t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = s.root
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		s.t.Fatalf("%s: %v\nstderr: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.Bytes()
}

// buildImages 恒构建(sandbox e2e 契约:防 daemon 滞留旧二进制,构建层缓存
// 使重复构建廉价)。GOPROXY 从宿主 go env 透传(根 Dockerfile 的 build-arg
// 面:受限网络下 proxy.golang.org 不可达会使容器内 go mod download 失败)。
func (s *stack) buildImages(root string) {
	proxy := strings.TrimSpace(string(s.run("go", "env", "GOPROXY")))
	base := fmt.Sprintf("braid-drill-base:t%d", s.slot)
	full := fmt.Sprintf("braid-drill:t%d", s.slot)
	sbx := fmt.Sprintf("braid-tool-sandbox:t%d", s.slot)
	s.t.Logf("drill: building images (slot %d)", s.slot)
	s.run("docker", "build", "-f", "Dockerfile", "--build-arg", "GOPROXY="+proxy, "-t", base, ".")
	s.run("docker", "build", "-f", "deploy/drill.Dockerfile",
		"--build-arg", "BASE="+base, "-t", full, ".")
	// 沙箱镜像 = COPY-only 契约(sandbox e2e 同款):helper 宿主侧预构建进
	// 临时 context,默认 target(final stage)只拷二进制,零 golang 镜像拉取。
	sbxCtx := s.t.TempDir()
	s.run("go", "build", "-trimpath", "-o", filepath.Join(sbxCtx, "braid-sandbox-exec"), "./cmd/braid-sandbox-exec")
	s.run("docker", "build", "-f", "deploy/sandbox.Dockerfile", "-t", sbx, sbxCtx)
}

func (s *stack) composeArgs() []string {
	return []string{"docker", "compose", "-f", "deploy/drill.docker-compose.yml", "-p", s.project}
}

func (s *stack) up(root string) {
	s.t.Helper()
	env := append(os.Environ(),
		fmt.Sprintf("BRAID_DRILL_SLOT=%d", s.slot),
		fmt.Sprintf("BRAID_DRILL_API_PORT=%d", apiPortBase+s.slot),
		fmt.Sprintf("BRAID_DRILL_MYSQL_PORT=%d", mysqlPortBase+s.slot),
		fmt.Sprintf("BRAID_DRILL_REDIS_PORT=%d", redisPortBase+s.slot),
	)
	args := append(s.composeArgs(), "up", "-d", "--wait")
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = root
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		s.t.Fatalf("compose up: %v\n%s", err, out)
	}
	// healthz 无认证;组件无容器级 healthcheck(交付面口径),到达性由本探测承载。
	healthy := false
	deadline := time.Now().Add(bootstrapWait)
	for time.Now().Before(deadline) && !healthy {
		resp, err := http.Get(s.apiURL + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			healthy = resp.StatusCode == http.StatusOK
		}
		if !healthy {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if !healthy {
		s.t.Fatalf("api /healthz unreachable within %s", bootstrapWait)
	}
	dbh, err := sql.Open("mysql", s.rootDSN)
	if err != nil {
		s.t.Fatalf("open drill mysql: %v", err)
	}
	if err := dbh.Ping(); err != nil {
		s.t.Fatalf("ping drill mysql: %v", err)
	}
	s.dbh = dbh
}

func (s *stack) down() {
	// host-netns worker 的 35s 排水宽限使 down 总时长可观,偶发部分失败
	//(容器已死/网络半拆);重试一次后仍败仅记录——残留由下一轮 up -d
	//--wait 收敛(compose 同项目名幂等重建)。
	for attempt := 0; attempt < 2; attempt++ {
		args := append(s.composeArgs(), "down", "-v", "--remove-orphans")
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = s.root
		if out, err := cmd.CombinedOutput(); err != nil {
			if attempt == 1 {
				s.t.Logf("compose down: %v\n%s", err, out)
			}
			continue
		}
		break
	}
	if s.dbh != nil {
		_ = s.dbh.Close()
	}
}

// --- bootstrap ---------------------------------------------------------------

// mustBootstrap:双用户 + 登录取 token。id 取固定高位字面量(> 现行雪花
// 产出域 ~7.3e18,零碰撞;tmpfs 栈 down 即清,幂等无妨)。
func (s *stack) mustBootstrap() {
	s.t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(drillPassword), bcrypt.MinCost)
	if err != nil {
		s.t.Fatalf("bcrypt: %v", err)
	}
	seed := func(userID uint64, name string, roles ...string) {
		if _, err := s.dbh.Exec(
			`INSERT INTO users (id, username, password_hash) VALUES (?, ?, ?)
			 ON DUPLICATE KEY UPDATE password_hash = VALUES(password_hash)`,
			userID, name, string(hash)); err != nil {
			s.t.Fatalf("seed user %s: %v", name, err)
		}
		for i, role := range roles {
			if _, err := s.dbh.Exec(
				`INSERT INTO user_role (id, user_id, role) VALUES (?, ?, ?)
				 ON DUPLICATE KEY UPDATE role = VALUES(role)`,
				userID*100+uint64(i), userID, role); err != nil {
				s.t.Fatalf("seed role %s/%s: %v", name, role, err)
			}
		}
	}
	seed(9200000000000000001, drillUserAuthor, "admin", "author", "approver")
	seed(9200000000000000002, drillUserApprv, "approver")

	for _, name := range []string{drillUserAuthor, drillUserApprv} {
		body, _ := json.Marshal(map[string]string{"username": name, "password": drillPassword})
		resp, err := http.Post(s.apiURL+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
		if err != nil {
			s.t.Fatalf("login %s: %v", name, err)
		}
		var out struct {
			Token string `json:"token"`
		}
		derr := json.NewDecoder(resp.Body).Decode(&out)
		if cerr := resp.Body.Close(); cerr != nil {
			s.t.Fatalf("login %s: close body: %v", name, cerr)
		}
		if derr != nil || out.Token == "" {
			s.t.Fatalf("login %s: code %d decode %v", name, resp.StatusCode, derr)
		}
		s.tokens[name] = out.Token
	}
}

// --- 通用 helper --------------------------------------------------------------

func (s *stack) api(method, path, user string, body []byte) (int, map[string]any) {
	s.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, s.apiURL+path, rd)
	if err != nil {
		s.t.Fatalf("new request %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		req.Header.Set("Authorization", "Bearer "+s.tokens[user])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	if cerr := resp.Body.Close(); cerr != nil {
		s.t.Fatalf("%s %s: close body: %v", method, path, cerr)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func (s *stack) eventually(desc string, timeout time.Duration, cond func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	s.t.Fatalf("drill: %s not converged within %s", desc, timeout)
}

func (s *stack) qInt(query string, args ...any) int {
	s.t.Helper()
	var n int
	if err := s.dbh.QueryRow(query, args...).Scan(&n); err != nil {
		s.t.Fatalf("query %q: %v", query, err)
	}
	return n
}

func (s *stack) uniqName(tag string) string {
	return fmt.Sprintf("drill-%s-%d", tag, time.Now().UnixNano()%1_000_000_000)
}

// publishDef 走真实 api 发布面,返回定义名。
func (s *stack) publishDef(doc []byte) string {
	s.t.Helper()
	code, body := s.api(http.MethodPost, "/api/v1/definitions", drillUserAuthor, doc)
	if code != http.StatusCreated {
		s.t.Fatalf("create def: %d %v", code, body)
	}
	id, _ := body["id"].(string)
	code, body = s.api(http.MethodPost, "/api/v1/drafts/"+id+"/publish", drillUserAuthor, nil)
	if code != http.StatusOK {
		s.t.Fatalf("publish def: %d %v", code, body)
	}
	name, _ := body["name"].(string)
	return name
}

// triggerExec 走真实触发面,登记终验收集。
func (s *stack) triggerExec(name string, wantCode int) uint64 {
	s.t.Helper()
	code, body := s.api(http.MethodPost, "/api/v1/executions", drillUserAuthor,
		[]byte(`{"definition_name":"`+name+`"}`))
	if code != wantCode {
		s.t.Fatalf("trigger %s: %d %v, want %d", name, code, body, wantCode)
	}
	if wantCode != http.StatusCreated {
		return 0
	}
	idStr, _ := body["id"].(string)
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		s.t.Fatalf("execution id %q: %v", idStr, err)
	}
	s.execIDs = append(s.execIDs, id)
	return id
}

// waitNodeStatus:node 到达目标状态(approval 续跑链推进的统一观测点)。
func (s *stack) waitNodeStatus(execID uint64, nodeID, status string) {
	s.t.Helper()
	s.eventually(fmt.Sprintf("node %s -> %s (exec %d)", nodeID, status, execID), armWait, func() bool {
		return s.qInt(`SELECT COUNT(*) FROM node_execution WHERE execution_id=? AND node_id=? AND status=?`,
			execID, nodeID, status) == 1
	})
}

// approveAll:按序签掉 a→b→c→d(mode=any 单签即过)。
func (s *stack) approveAll(execID uint64, nodes ...string) {
	s.t.Helper()
	for _, n := range nodes {
		s.waitNodeStatus(execID, n, "WAITING_APPROVAL")
		code, body := s.api(http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/approvals/%s/decision", execID, n),
			drillUserAuthor, []byte(`{"decision":"approve"}`))
		if code != http.StatusCreated {
			s.t.Fatalf("approve %s: %d %v", n, code, body)
		}
	}
}

func (s *stack) waitSucceeded(execID uint64) {
	s.t.Helper()
	s.eventually(fmt.Sprintf("execution %d SUCCEEDED", execID), armWait, func() bool {
		return s.qInt(`SELECT COUNT(*) FROM workflow_execution WHERE id=? AND status='SUCCEEDED'`, execID) == 1
	})
}

// --- 五臂 --------------------------------------------------------------------

// armClaim:①双 worker 抢 claim 零 1062 + 租约号互异。
func (s *stack) armClaim(t *testing.T) {
	// 前置:真实舰队已宣告(启动期 acquire 写入,决策 6 宣告时点)。
	s.eventually("fleet leases acquired", armWait, func() bool {
		return s.qInt(`SELECT COUNT(*) FROM snowflake_node_lease WHERE kind='worker' AND capabilities IS NOT NULL`) == 2
	})
	// 租约号互异:双 worker auto 抢注不同 node_id。
	if n := s.qInt(`SELECT COUNT(DISTINCT node_id) FROM snowflake_node_lease WHERE kind='worker'`); n != 2 {
		t.Fatalf("distinct worker node_ids = %d, want 2(租约号互异)", n)
	}

	name := s.publishDef(drillAnyDSL(s.uniqName("claim")))
	execID := s.triggerExec(name, http.StatusCreated)
	s.approveAll(execID, "a", "b", "c", "d")
	s.waitSucceeded(execID)
	// 恰一次执行:四节点各一行,无重复 attempt。
	if n := s.qInt(`SELECT COUNT(*) FROM node_execution WHERE execution_id=?`, execID); n != 4 {
		t.Fatalf("node rows = %d, want 4", n)
	}
	if n := s.qInt(`SELECT COUNT(*) FROM node_attempt WHERE execution_id=? AND status='RUNNING'`, execID); n != 0 {
		t.Fatalf("lingering RUNNING attempts = %d, want 0", n)
	}
}

// armKillTakeover:②docker kill -9(stop -t 0,kill 语义且不被 restart 策略
// 拉起)持租约 worker → 残窗在场(行 ≤TTL 30s 保真)+ reaper 判死接管。
// 接管链确定性钉法(对抗审计 HIGH 修正):真实审批链的 attempt 在
// claim→suspend 间毫秒级 SUCCEEDED 收口,reaper 只扫 RUNNING——心跳回拨
// 命不中;改为 SQL 直种 stale RUNNING attempt(worker1 名下,心跳 60s 前)
// + node EXECUTING,reaper 判死 → 重派(attempts≥2 的第二条 attempt)→
// worker2 认领挂起,因果由 attempts 计数承载。
func (s *stack) armKillTakeover(t *testing.T) {
	name := s.publishDef(drillAnyDSL(s.uniqName("kill")))
	defID := s.defIDByName(name)

	container := s.containerName("worker1")
	killedAt := time.Now().UTC()
	s.run("docker", "stop", "-t", "0", container) // SIGKILL + 保持死亡(unless-stopped 不复活显式 stop)

	// 残窗:租约行仍在场(kill -9 无 SIGTERM 释放;≤TTL 30s)。
	if n := s.qInt(`SELECT COUNT(*) FROM snowflake_node_lease
		WHERE holder_id='worker:braid-drill-worker1' AND expires_at >= NOW(3)`); n < 1 {
		t.Fatalf("worker1 lease after kill-9 = %d in-fleet rows, want ≥1(残窗在场)", n)
	}

	// 判死接管:种子链(execution RUNNING + node a EXECUTING + attempt
	// RUNNING 心跳 60s 前,worker1 名下)→ reaper(扫描 1s + DEAD_GRACE
	// 20s,心跳早已超宽限)判死重派 → worker2 认领挂起。
	execID := uint64(8800000000000000001)
	nodeExecID := uint64(8800000000000000002)
	attID := uint64(8800000000000000003)
	ins := func(q string, args ...any) {
		t.Helper()
		if _, err := s.dbh.Exec(q, args...); err != nil {
			t.Fatalf("seed takeover chain: %v", err)
		}
	}
	ins(`INSERT INTO workflow_execution
		(id, definition_id, definition_name, definition_version, definition_hash, status, triggered_by, input)
		VALUES (?, ?, ?, 1, (SELECT definition_hash FROM workflow_definition WHERE id=?), 'RUNNING', 'drill-seed', '{}')`,
		execID, defID, name, defID)
	// 种子链完整性(演练实证):a 行须携审批快照(挂起事务的消费面),b/c/d
	// 须有 PENDING 行——挂起事务推进后继时按「定义/行漂移」守卫拒缺行。
	ins(`INSERT INTO node_execution
		(id, execution_id, node_id, type, status, attempts, max_attempts, worker_id, version, deadline_at, input_snapshot)
		VALUES (?, ?, 'a', 'approval', 'EXECUTING', 1, 3, 'braid-drill-worker1', 1, NOW(3) + INTERVAL 30 MINUTE, ?)`,
		nodeExecID, execID, `{"approvers":["`+drillUserAuthor+`"],"mode":"any","timeout_hours":24,"on_timeout":"escalate","instructions":""}`)
	for nid, id := range map[string]uint64{"b": 8800000000000000004, "c": 8800000000000000005, "d": 8800000000000000006} {
		ins(`INSERT INTO node_execution
			(id, execution_id, node_id, type, status, attempts, max_attempts, version)
			VALUES (?, ?, ?, 'approval', 'PENDING', 0, 3, 1)`, id, execID, nid)
	}
	ins(`INSERT INTO node_attempt
		(id, node_execution_id, execution_id, attempt_no, worker_id, status,
		 idempotency_key, asynq_task_id, started_at, heartbeat_at)
		VALUES (?,?,?,?, 'braid-drill-worker1', 'RUNNING', ?, ?, NOW(3) - INTERVAL 60000000 MICROSECOND, NOW(3) - INTERVAL 60000000 MICROSECOND)`,
		attID, nodeExecID, execID, 1, fmt.Sprintf("att:%d:1", nodeExecID), fmt.Sprintf("att:%d:1", nodeExecID))
	s.execIDs = append(s.execIDs, execID)

	// 接管完成的因果判据:attempts ≥ 2(重派)+ WAITING_APPROVAL(worker2
	// 认领新 attempt 并挂起)。断掉 reaper 判死/重派任一环,此断言不达。
	s.eventually("reaper takeover redispatched node a (attempts>=2)", armWait, func() bool {
		return s.qInt(`SELECT COUNT(*) FROM node_execution
			WHERE execution_id=? AND node_id='a' AND status='WAITING_APPROVAL' AND attempts>=2`, execID) == 1
	})
	s.approveAll(execID, "a", "b", "c", "d")
	s.waitSucceeded(execID)

	// 复航:worker1 重启重新 acquire(auto)。续期证明 = 到期时刻越过冻结
	// 包络:冻结行上界 = kill+30s,真续期会把 expires_at 推到 kill+40s+
	//(对抗审计 HIGH:vacuous 首轮满足在此关死)。
	s.run("docker", "start", container)
	s.eventually("worker1 lease renewed beyond frozen envelope", armWait, func() bool {
		var exp time.Time
		return s.dbh.QueryRow(`SELECT expires_at FROM snowflake_node_lease
			WHERE holder_id='worker:braid-drill-worker1' AND expires_at >= NOW(3)
			ORDER BY expires_at DESC LIMIT 1`).Scan(&exp) == nil &&
			exp.After(killedAt.Add(32*time.Second))
	})
	// 双 worker 现役租约号互异(按 holder 取现役行;auto 复航可能换号,
	// 旧行惰性过期不算现役)。
	qNode := func(holder string) (int, bool) {
		var n int
		err := s.dbh.QueryRow(`SELECT node_id FROM snowflake_node_lease
			WHERE holder_id=? AND expires_at >= NOW(3) ORDER BY expires_at DESC LIMIT 1`, holder).Scan(&n)
		return n, err == nil
	}
	s.eventually("both workers hold live leases", armWait, func() bool {
		_, ok1 := qNode("worker:braid-drill-worker1")
		_, ok2 := qNode("worker:braid-drill-worker2")
		return ok1 && ok2
	})
	n1, _ := qNode("worker:braid-drill-worker1")
	n2, _ := qNode("worker:braid-drill-worker2")
	if n1 == n2 {
		t.Fatalf("active worker node ids collide: %d", n1)
	}
}

// defIDByName 反查定义行 id(种子链外键)。
func (s *stack) defIDByName(name string) uint64 {
	s.t.Helper()
	var id uint64
	if err := s.dbh.QueryRow(`SELECT id FROM workflow_definition WHERE name=? ORDER BY version DESC LIMIT 1`, name).Scan(&id); err != nil {
		s.t.Fatalf("def by name %s: %v", name, err)
	}
	return id
}

// armOwnerSweep:③滚动重启(docker restart,身份稳定)下的沙箱清扫,t20.8
// 起追认 T20.4 语义(D-t204 边界①/⑤,先存回归登记见 #174 评论):同 owner
// 活租约在场 ⇒ 启动清扫跳过(误跳过窗);显式清行(runbook)/优雅释放/
// TTL 过期后,下次启动清扫收敛——w1 的 braid.sandbox 容器被自清,w2 的
// 原样存活(互杀红线)。T15.6 原版「restart 即清扫」期望在 t20.4 合入后
// 确定性红(arm2 kill -9 有机残行恰落在 arm3 boot 的 TTL 窗内),本版
// 把期望改写为 T20.4 的真实契约。
func (s *stack) armOwnerSweep(t *testing.T) {
	seed := func(owner string, i int) string {
		cname := fmt.Sprintf("drill-sbx-t%d-%s-%d", s.slot, owner, i)
		s.run("docker", "run", "-d", "--name", cname,
			"--label", "braid.sandbox=1", "--label", "braid.sandbox.owner="+owner,
			"alpine:3.22", "sleep", "300")
		return cname
	}
	var w1Names, w2Names []string
	// 清理先于播种注册(闭包按变量捕获,增长后的切片清理时可见):seed 循环
	// 中途失败已起的容器不泄漏。
	t.Cleanup(func() {
		for _, n := range append(append([]string{}, w1Names...), w2Names...) {
			_ = exec.Command("docker", "rm", "-f", n).Run()
		}
	})
	for i := 0; i < 2; i++ {
		w1Names = append(w1Names, seed("braid-drill-worker1", i))
		w2Names = append(w2Names, seed("braid-drill-worker2", i))
	}

	const holder = "worker:braid-drill-worker1"
	// fakeNode:伪活租约行的 node 号(与 arm5 硬闸假行 900 错开;两行均为
	// 臂内瞬态,各自清理)。
	const fakeNode = 901
	liveRows := func() int {
		return s.qInt(`SELECT COUNT(*) FROM snowflake_node_lease
			WHERE holder_id=? AND expires_at >= NOW(3)`, holder)
	}
	skips := func() int {
		// 双词面同行计数:主短语与 tools 发射点共用导出常量(编译期钉死,
		// 改文案必须显式同步两处);「live peer node」子短语来自 cmd/worker
		// sandboxSweepHoldoff 的 reason 拼装——两词同现把断言归因钉到
		// 「活体命中 ⇒ 跳过」分支,排除复核不可达 fail-safe 跳过
		//(reason = "liveness probe failed: ...")的冒名顶替。
		n := 0
		for _, ln := range strings.Split(s.serviceLogs("worker1"), "\n") {
			if strings.Contains(ln, tools.SweepSkipWarnPhrase) && strings.Contains(ln, "live peer node") {
				n++
			}
		}
		return n
	}

	// Phase 1 误跳过窗(D-t204 边界①):伪同 owner 活租约行(+300s,arm5
	// 假行同款手法)钉住「活租约在场 ⇒ 跳过」——arm2 kill -9 的有机残行
	// 也命中同语义,但其 TTL 窗有限,伪行使断言不踩时序边际。播种先清后插
	//(同栈重跑幂等,防硬杀残行撞 PK 1062);DELETE 带 holder 守卫对齐
	// runbook UPGRADE §7 清行模板(node+holder 二元组,误删活体面归零)。
	if _, err := s.dbh.Exec(`DELETE FROM snowflake_node_lease WHERE node_id=? AND holder_id=?`, fakeNode, holder); err != nil {
		t.Fatalf("pre-clean fake lease row: %v", err)
	}
	if _, err := s.dbh.Exec(`INSERT INTO snowflake_node_lease
		(node_id, kind, holder_id, expires_at, capabilities)
		VALUES (?, 'worker', ?, DATE_ADD(NOW(3), INTERVAL 300 SECOND), NULL)`, fakeNode, holder); err != nil {
		t.Fatalf("seed fake live lease row: %v", err)
	}
	t.Cleanup(func() {
		if _, err := s.dbh.Exec(`DELETE FROM snowflake_node_lease WHERE node_id=? AND holder_id=?`, fakeNode, holder); err != nil {
			t.Logf("cleanup fake lease row %d: %v", fakeNode, err)
		}
	})

	container := s.containerName("worker1")
	skipsBefore := skips()
	s.run("docker", "restart", container) // 优雅重启:SIGTERM + 35s 宽限(当前租约行随 Release 删除)
	// 跳过决策锚:boot 到达 SandboxStartupCheck 且按 D-t204 语义跳过
	//(holdoff 非空 ⇒ WARN;WARN 出现即清扫路径早退,容器负断言零时序边际)。
	s.eventually("startup sweep skipped under live same-owner lease (D-t204 holdoff)", armWait, func() bool {
		return skips() > skipsBefore
	})
	for _, n := range w1Names {
		if !containerExists(n) {
			t.Fatalf("worker1 sandbox container %q swept despite live same-owner lease(D-t204 holdoff 回归)", n)
		}
	}
	for _, n := range w2Names {
		if !containerExists(n) {
			t.Fatalf("worker2 sandbox container %q swept by worker1 restart(互杀红线)", n)
		}
	}

	// Phase 2 清行收敛(①的 runbook 收敛路径 + ⑤优雅释放路径):删伪行
	//(显式清行)→ 优雅停机(Release 删当前行;即便停机释放失败,残行也走
	// TTL 过期)→ 轮询 holder 现役行清零(进程已停无人续期,确定性)→
	// 下次启动复核无行 ⇒ 清扫执行。
	if _, err := s.dbh.Exec(`DELETE FROM snowflake_node_lease WHERE node_id=? AND holder_id=?`, fakeNode, holder); err != nil {
		t.Fatalf("clear fake lease row: %v", err)
	}
	s.run("docker", "stop", "-t", "35", container)
	s.eventually("worker1 live lease rows settled to zero", armWait, func() bool {
		return liveRows() == 0
	})
	s.run("docker", "start", container)
	// 启动清扫 = SandboxStartupCheck 的 owner 二元组过滤(T15.3)。
	s.eventually("worker1 own sandbox containers swept", armWait, func() bool {
		for _, n := range w1Names {
			if containerExists(n) {
				return false
			}
		}
		return true
	})
	for _, n := range w2Names {
		if !containerExists(n) {
			t.Fatalf("worker2 sandbox container %q swept by worker1 restart(互杀红线)", n)
		}
	}
}

// armRedisRestart:④Redis 重启(AOF 卷)→ relay 重连(压缩退避秒级)
// → outbox 恢复投递 + 重派收敛。
func (s *stack) armRedisRestart(t *testing.T) {
	// 断窗保证(对抗审计):先停 relay,触发后 outbox 行必然积压 PENDING
	//(投递面缺席),再重启队列 Redis(AOF 卷恢复),最后 relay 复航——
	// 「断窗内确有在途行」由 PENDING 计数前置断言钉住,臂不退化为
	// 「重启后系统照常工作」。
	relayC := s.containerName("relay")
	s.run("docker", "stop", relayC)
	name := s.publishDef(drillAnyDSL(s.uniqName("redis")))
	execID := s.triggerExec(name, http.StatusCreated)
	s.eventually("outbox rows pending during relay outage", armWait, func() bool {
		return s.qInt(`SELECT COUNT(*) FROM outbox WHERE execution_id=? AND status='PENDING'`, execID) >= 1
	})
	s.run("docker", "restart", s.containerName("redis-queue"))
	s.run("docker", "start", relayC)
	// 恢复收敛:relay 重连(压缩退避秒级)→ outbox 投递 → worker 认领 →
	// 审批链走通 → 终态零积压。
	s.approveAll(execID, "a", "b", "c", "d")
	s.waitSucceeded(execID)
	s.eventually("outbox fully dispatched after redis restart", armWait, func() bool {
		return s.qInt(`SELECT COUNT(*) FROM outbox WHERE execution_id=? AND status='PENDING'`, execID) == 0
	})
}

// armFleetGate:⑤mode=all 硬闸——断言钉在演练 api(单副本,决策 6);
// 全宣告放行 → 伪造未宣告在场行 → 发布/触发双面 503 → 清除后放行。
func (s *stack) armFleetGate(t *testing.T) {
	allName := s.publishDef(drillAllDSL(s.uniqName("gate")))

	// 伪造混跑窗:在场未宣告的「旧二进制」租约行(capabilities NULL)。
	if _, err := s.dbh.Exec(`INSERT INTO snowflake_node_lease
		(node_id, kind, holder_id, expires_at, capabilities)
		VALUES (900, 'worker', 'worker:drill-fake-old', DATE_ADD(NOW(3), INTERVAL 300 SECOND), NULL)`); err != nil {
		t.Fatalf("seed fake old lease: %v", err)
	}
	// 触发面 503 = 分类码 only(诊断明细在 slog/计数器,REST 触发核无
	// gin 上下文——发布面 body 才携 undeclared 清单,见 approvalgate.go)。
	code, body := s.api(http.MethodPost, "/api/v1/executions", drillUserAuthor,
		[]byte(`{"definition_name":"`+allName+`"}`))
	if code != http.StatusServiceUnavailable || body["error"] != "fleet_capability_gate" {
		t.Fatalf("trigger under fake undeclared lease = %d %v, want 503 fleet_capability_gate", code, body)
	}
	// 发布面同判:新草稿发布被拒。
	draftName := s.uniqName("gatepub")
	code, dbody := s.api(http.MethodPost, "/api/v1/definitions", drillUserAuthor, drillAllDSL(draftName))
	if code != http.StatusCreated {
		t.Fatalf("create gate draft: %d %v", code, dbody)
	}
	draftID, _ := dbody["id"].(string)
	code, pbody := s.api(http.MethodPost, "/api/v1/drafts/"+draftID+"/publish", drillUserAuthor, nil)
	if code != http.StatusServiceUnavailable || pbody["error"] != "fleet_capability_gate" {
		t.Fatalf("publish under fake undeclared lease = %d %v, want 503 gate", code, pbody)
	}
	// 发布面诊断明细:未宣告 holder 一目了然(运维定位混跑窗旧二进制)。
	und, _ := pbody["undeclared"].([]any)
	if len(und) != 1 {
		t.Fatalf("publish undeclared = %v, want 1(fake holder)", pbody["undeclared"])
	}
	if row, _ := und[0].(map[string]any); row["holder"] != "worker:drill-fake-old" {
		t.Fatalf("publish undeclared[0] = %v, want worker:drill-fake-old", und[0])
	}

	// 滚动完成形态:清除伪造行 → 触发放行,会签双人走完(真 mode=all 面)。
	if _, err := s.dbh.Exec(`DELETE FROM snowflake_node_lease WHERE holder_id='worker:drill-fake-old'`); err != nil {
		t.Fatalf("remove fake lease: %v", err)
	}
	execID := s.triggerExec(allName, http.StatusCreated)
	s.waitNodeStatus(execID, "sign", "WAITING_APPROVAL")
	for _, user := range []string{drillUserAuthor, drillUserApprv} {
		code, body := s.api(http.MethodPost,
			fmt.Sprintf("/api/v1/executions/%d/approvals/sign/decision", execID), user,
			[]byte(`{"decision":"approve"}`))
		if code != http.StatusCreated {
			t.Fatalf("mode=all decide by %s: %d %v", user, code, body)
		}
	}
	s.waitSucceeded(execID)
}

// armRollbackResume:⑦least-privilege 回滚缝(对抗审计 HIGH/MEDIUM 收口)
// ——驳回 → FAILED → POST /resume(RestartExecution 对 workflow_execution
// 的 UPDATE 在 braid_api 连接上执行,授权缺角的回归在此运行时红)→
// 节点重跑重新挂起 → 重签走完。
func (s *stack) armRollbackResume(t *testing.T) {
	name := s.publishDef(drillAnyDSL(s.uniqName("rb")))
	execID := s.triggerExec(name, http.StatusCreated)
	s.waitNodeStatus(execID, "a", "WAITING_APPROVAL")
	code, body := s.api(http.MethodPost,
		fmt.Sprintf("/api/v1/executions/%d/approvals/a/decision", execID), drillUserAuthor,
		[]byte(`{"decision":"reject"}`))
	if code != http.StatusCreated {
		t.Fatalf("reject a: %d %v", code, body)
	}
	s.eventually("execution FAILED after reject", armWait, func() bool {
		return s.qInt(`SELECT COUNT(*) FROM workflow_execution WHERE id=? AND status='FAILED'`, execID) == 1
	})
	code, body = s.api(http.MethodPost, fmt.Sprintf("/api/v1/executions/%d/resume", execID),
		drillUserAuthor, nil)
	if code != http.StatusCreated {
		// 失败取证:api 侧 ERROR 行随断言输出(least-privilege 缝的定位面)。
		var errs []string
		for _, ln := range strings.Split(s.serviceLogs("api"), "\n") {
			if strings.Contains(ln, "ERROR") {
				errs = append(errs, ln)
			}
		}
		if len(errs) > 6 {
			errs = errs[len(errs)-6:]
		}
		t.Fatalf("resume: %d %v(least-privilege 回滚缝)\napi errors:\n%s", code, body, strings.Join(errs, "\n"))
	}
	// 重跑:a 重新挂起(新轮次),重签(本轮 carol 系单签即过)。
	s.waitNodeStatus(execID, "a", "WAITING_APPROVAL")
	code, body = s.api(http.MethodPost,
		fmt.Sprintf("/api/v1/executions/%d/approvals/a/decision", execID), drillUserAuthor,
		[]byte(`{"decision":"approve"}`))
	if code != http.StatusCreated {
		t.Fatalf("re-approve a: %d %v", code, body)
	}
	s.approveAll(execID, "b", "c", "d")
	s.waitSucceeded(execID)
}

// armChainVerify:⑥终态——全部演练执行哈希链全链绿(D-t93③/R8 口径)
// + 组件日志零 1062(定档 A 后 flush 单跑;claim CAS 无 dup-key)。
// arm8:⑧ 租约 fencing 废黜(T20.1,D-2026-10-06-t201):冻结 zombie 的
// fleet 级停写证据。链路:docker pause worker1(整容器 SIGSTOP = 进程
// 冻结/长 GC 的确定性注入)→ 租约滑出在场窗(TTL 保真 30s)→ 易主注入
// (过期行 epoch+1,接管者等价;真实接管路径的 bump 语义由
// internal/snowflake 表驱动锚定)→ 解冻 = zombie 苏醒(持有字符串未变、
// 代际已失配)→ 旧代际续期结构性落空(lost 词面)→ Exit(1) →
// unless-stopped 复活 → 同 holder 复占 bump(≥3,新代际复航而非旧代际
// 复燃)→ 易主后执行照常收敛 + 代际不回退。
// 回归锚:master(续期无代际谓词)上解冻进程会静默续上过期行自加冕,
// 本臂在 lost 词面与 epoch≥3 两处红。
func (s *stack) armLeaseFencing(t *testing.T) {
	name := s.publishDef(drillAnyDSL(s.uniqName("fence")))
	container := s.containerName("worker1")
	const holder = "worker:braid-drill-worker1"

	s.run("docker", "pause", container)
	t.Cleanup(func() { // 幂等兜底解冻:臂内已 unpause(或容器已换代)时报
		// 「not paused」退出 1,尽力语义吞掉——要防的是冻结残留,不是重复解冻。
		if out, err := exec.Command("docker", "unpause", container).CombinedOutput(); err != nil &&
			!strings.Contains(string(out), "is not paused") {
			s.t.Logf("drill: cleanup unpause %s: %v (%s)", container, err, out)
		}
	})

	// 冻结包络:租约行滑出在场窗(kill 不释放、pause 不释放,同残窗形态)。
	s.eventually("worker1 lease slid out of fleet under pause", armWait, func() bool {
		return s.qInt(`SELECT COUNT(*) FROM snowflake_node_lease
			WHERE holder_id=? AND expires_at >= NOW(3)`, holder) == 0
	})

	// 易主注入(接管者等价形态):行已被他人 CAS 接管 = 代际 +1。
	if _, err := s.dbh.Exec(
		`UPDATE snowflake_node_lease SET fencing_epoch = fencing_epoch + 1 WHERE holder_id=?`, holder,
	); err != nil {
		t.Fatalf("supersede frozen lease: %v", err)
	}

	// 解冻 = zombie 苏醒:旧代际续期必须落空 → lost(停发 + 响亮)。
	s.run("docker", "unpause", container)
	s.eventually("worker1 re-crowned with fresh epoch (>=3)", armWait, func() bool {
		return s.qInt(`SELECT COUNT(*) FROM snowflake_node_lease
			WHERE holder_id=? AND expires_at >= NOW(3) AND fencing_epoch >= 3`, holder) == 1
	})
	// 废黜证据:解冻进程在旧代际上的续期落空词面(Exit(1) → 复活复占)。
	logs := s.serviceLogs("worker1")
	if !strings.Contains(logs, "lease renewal lost row") && !strings.Contains(logs, "lease renewal error") {
		t.Fatalf("worker1 logs missing deposed-renewal evidence (epoch-mismatch lost): %s", lastLines(logs, 20))
	}

	// 易主后执行照常收敛(裁决/执行面不被 fence 扰动)。attempt 归属由
	// claim 竞速决定——复航后的 worker1 是新代历持有者,其物化属正常调度
	//(失冠进程零物化的确定性断言在单测层:TestClaimAttemptFence
	// RejectsSupersededLease,drill 不重复断言竞速时序)。
	execID := s.triggerExec(name, http.StatusCreated)
	s.approveAll(execID, "a", "b", "c", "d")
	s.waitSucceeded(execID)
	if n := s.qInt(`SELECT COUNT(*) FROM node_attempt WHERE execution_id=?`, execID); n < 4 {
		t.Fatalf("execution %d attempts = %d, want ≥4(四节点链完整物化)", execID, n)
	}
	// 代际不回退:worker1 现役行保持新代际(≥3;旧代际不得复燃)。
	if e := s.qInt(`SELECT COALESCE(MAX(fencing_epoch),0) FROM snowflake_node_lease WHERE holder_id=?`, holder); e < 3 {
		t.Fatalf("worker1 lease epoch = %d, want ≥3(新代际复航,旧代际被废黜)", e)
	}
}

// lastLines 截尾日志(失败诊断面;全量日志过巨)。
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func (s *stack) armChainVerify(t *testing.T) {
	if len(s.execIDs) == 0 {
		t.Fatalf("no executions collected for chain verify")
	}
	for _, execID := range s.execIDs {
		outcome, err := audit.VerifyChain(context.Background(), s.dbh, execID)
		if err != nil {
			t.Fatalf("VerifyChain exec %d: %v", execID, err)
		}
		if outcome.Broken || !outcome.Verified {
			t.Fatalf("chain exec %d: verified=%v broken=%v reason=%q(R8 复核)", execID, outcome.Verified, outcome.Broken, outcome.Reason)
		}
	}
	for _, svc := range []string{"worker1", "worker2", "relay", "reaper1", "reaper2"} {
		logs := s.serviceLogs(svc)
		if strings.Contains(logs, "Error 1062") { // MySQL 错误词面,防雪花 id/时长撞裸数字
			t.Fatalf("service %s logs contain 1062 (dup-key path hit)", svc)
		}
	}
}

// --- docker 杂项 --------------------------------------------------------------

func (s *stack) containerName(service string) string {
	s.t.Helper()
	// -a:含已停容器(kill -9 残窗臂在容器死亡后仍需解析/复航)。
	out := strings.TrimSpace(string(s.run(append(s.composeArgs(),
		"ps", "-aq", service)...)))
	if out == "" || strings.Contains(out, "\n") {
		s.t.Fatalf("resolve container for %s: %q", service, out)
	}
	return out
}

func (s *stack) serviceLogs(service string) string {
	s.t.Helper()
	return string(s.run(append(s.composeArgs(), "logs", "--no-color", service)...))
}

func containerExists(name string) bool {
	return exec.Command("docker", "inspect", name).Run() == nil
}

// egressNetworks:worker 经 sock 建的 egress 网在 compose 域外,down 不收
// ——按前缀快照差分清理(绝不碰其他会话的网)。
func egressNetworks(t *testing.T) map[string]bool {
	t.Helper()
	out, err := exec.Command("docker", "network", "ls", "--filter", "name=braid-sbx-egress-", "-q").Output()
	if err != nil {
		t.Logf("list egress networks: %v", err)
		return nil
	}
	set := map[string]bool{}
	for _, id := range strings.Fields(string(out)) {
		set[id] = true
	}
	return set
}

func cleanupEgressNetworks(t *testing.T, before map[string]bool) {
	t.Helper()
	for id := range egressNetworks(t) {
		if before[id] {
			continue
		}
		if out, err := exec.Command("docker", "network", "rm", id).CombinedOutput(); err != nil {
			t.Logf("rm egress network %s: %v %s", id, err, out)
		}
	}
}

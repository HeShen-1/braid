// seed.go 实现 `braid seed`(T16.3,issue #140):一键 demo——迁移 +
// 建号 + 示例定义发布 + 触发,幂等先行(迁移 no-op / 建号 ErrConflict
// 跳过 / 定义按名判存 / 触发固定幂等键重放)。两段形态:db 段只触
// MySQL(dev-up.sh 在起四进程前调,未迁移库上组件会启动失败);rest 段
// 走 API(healthz 后调);all = db→rest(栈已在跑时的独立重跑形态)。
//
// 叙事每步一句(dev-up.sh 文风,人读 = 开源 demo 入口);复用面 =
// db.Migrate / createUserCore / publishTwoHop / executionBody,零语义
// 复制。凭证纪律:seed 用自建 alice 凭据 login,无视 ambient
// BRAID_TOKEN(环境 token 主体无 author 角色保证);口令经
// BRAID_SEED_*_PASSWORD env,dev 缺省占位与 .env.example 同信任级。
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"braid/internal/auth"
	"braid/internal/db"
)

// seed 演示账号(agent-council 审批名单 alice/bob 与示例 DSL 一致)与
// 口令 env 契约。
const (
	seedAuthorUser              = "alice"
	seedApproverUser            = "bob"
	seedAuthorPasswordEnv       = "BRAID_SEED_AUTHOR_PASSWORD"
	seedApproverPasswordEnv     = "BRAID_SEED_APPROVER_PASSWORD"
	seedAuthorPasswordDefault   = "dev-author-1"   // dev 占位(README 既有,同 MYSQL_ROOT_PASSWORD 信任级)
	seedApproverPasswordDefault = "dev-approver-1" // 同上
)

// seed 演示输入与默认旋钮。
const (
	seedDefaultDefinition = "docs/examples/agent-council.json"
	seedDefaultWait       = 2 * time.Minute
	seedPollInterval      = 2 * time.Second
)

// demoProposal 是 agent-council 的内置演示输入(评审会待评审的技术方案)。
// 改这段文案会换触发幂等键(键含输入摘要)→ 下次 seed 触发新执行而非
// 重放,语义正确。
const demoProposal = "为中小团队提供一个基于大模型的智能工单分派服务:接入现有 IM,用工单文本分类并路由到对应处理人,复杂工单先由 LLM 生成处理建议再人工确认。目标:首轮响应时间从小时级降到分钟级。约束:不引入新的独立账号体系,数据不出既有内网边界,月度推理成本控制在千元以内。"

// runSeed 是 `braid seed` 的 argv 层:用法错误退出 2;段分发后各体层
// 自归因(0/1)。
func runSeed(ctx context.Context, args []string, env func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("braid seed", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stage := fs.String("stage", "all", "db | rest | all (db = migrate + users only; rest = publish + trigger only)")
	wait := fs.Duration("wait", seedDefaultWait, "bounded poll budget for execution landing, 0 = skip")
	addr := fs.String("addr", "", "API base URL (default $BRAID_API_ADDR or http://127.0.0.1:8081)")
	definition := fs.String("definition", seedDefaultDefinition, "demo DSL file path")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 0 {
		say(stderr, "braid seed: unexpected positional arguments %v\n", fs.Args())
		return ExitUsage
	}
	if *stage != "db" && *stage != "rest" && *stage != "all" {
		sayln(stderr, "%s", "braid seed: -stage must be one of db, rest, all")
		return ExitUsage
	}
	if *wait < 0 {
		sayln(stderr, "%s", "braid seed: -wait must be >= 0")
		return ExitUsage
	}
	switch *stage {
	case "db":
		return seedDB(ctx, env, stdout, stderr)
	case "rest":
		return seedREST(ctx, env, seedParams{addr: *addr, definition: *definition, wait: *wait}, stdout, stderr)
	default:
		if code := seedDB(ctx, env, stdout, stderr); code != ExitOK {
			return code
		}
		return seedREST(ctx, env, seedParams{addr: *addr, definition: *definition, wait: *wait}, stdout, stderr)
	}
}

// seedParams 是 rest 段的已解析入参。
type seedParams struct {
	addr       string
	definition string
	wait       time.Duration
}

// seedDB 是 db 段:迁移 + 建号。只触 MySQL,可在组件进程起前跑。
func seedDB(ctx context.Context, env func(string) string, stdout, stderr io.Writer) int {
	dsn := env("BRAID_MYSQL_DSN")
	if dsn == "" {
		sayln(stderr, "%s", "braid seed: BRAID_MYSQL_DSN is required (same env contract as cmd/api)")
		return ExitError
	}
	if err := db.Migrate(dsn); err != nil {
		say(stderr, "braid seed: migrate: %v\n", err)
		return ExitError
	}
	sayln(stdout, "%s", "seed ① 迁移:schema 已就绪(幂等,重复执行为 no-op)")
	if env(seedAuthorPasswordEnv) == "" || env(seedApproverPasswordEnv) == "" {
		// 对抗审计 advisory(security 维):默认占位口令授出 author+approver
		// 角色的响亮警示——姿势按 D-t163 决策 4(同 MYSQL_ROOT_PASSWORD
		// 信任级),不设 opt-in 门以保零配置 demo;生产必须覆盖 env。
		sayln(stderr, "%s", "seed ⚠ 口令走 dev 默认占位(well-known):仅限 dev/回环栈使用,生产必须经 BRAID_SEED_AUTHOR_PASSWORD / BRAID_SEED_APPROVER_PASSWORD 覆盖")
	}
	for _, u := range []struct{ name, rolesRaw, display, passwordEnv, passwordDefault string }{
		{seedAuthorUser, "author,approver", "author, approver", seedAuthorPasswordEnv, seedAuthorPasswordDefault},
		{seedApproverUser, "approver", "approver", seedApproverPasswordEnv, seedApproverPasswordDefault},
	} {
		roles, err := parseRoles(u.rolesRaw)
		if err != nil { // 静态词表,到达即编程错;防御归因不吞
			say(stderr, "braid seed: roles: %v\n", err)
			return ExitError
		}
		password := env(u.passwordEnv)
		if password == "" {
			password = u.passwordDefault
		}
		err = createUserCore(ctx, dsn, u.name, password, roles, nil, 0, stderr)
		switch {
		case err == nil:
			sayln(stdout, "seed ② 建号:%s(%s)已创建", u.name, u.display)
		case errors.Is(err, auth.ErrConflict):
			sayln(stdout, "seed ② 建号:%s 已存在,跳过", u.name)
		default:
			say(stderr, "braid seed: create user %s: %v\n", u.name, err)
			return ExitError
		}
	}
	return ExitOK
}

// seedREST 是 rest 段:判存发布 + 幂等触发 + 有界等待落点叙事。
func seedREST(ctx context.Context, env func(string) string, p seedParams, stdout, stderr io.Writer) int {
	base := resolveAPIBase(p.addr, env("BRAID_API_ADDR"))
	doc, err := os.ReadFile(p.definition)
	if err != nil {
		say(stderr, "braid seed: read %s: %v\n", p.definition, err)
		return ExitError
	}
	var dsl struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(doc, &dsl); err != nil || dsl.Name == "" {
		say(stderr, "braid seed: definition file missing name field: %s\n", p.definition)
		return ExitError
	}
	hc := newHTTPClient()
	// seed 只用自建 alice 凭据 login(不走 resolveToken:ambient BRAID_TOKEN
	// 主体无 author 角色保证)。alice 已存在但口令与 seed env 不符 → 401,
	// 指引覆盖 env 或重建栈。
	authorPass := env(seedAuthorPasswordEnv)
	if authorPass == "" {
		authorPass = seedAuthorPasswordDefault
	}
	token, err := login(ctx, hc, base, seedAuthorUser, authorPass)
	if err != nil {
		say(stderr, "braid seed: login %s: %v(alice 已存在但口令不符时:设 %s 覆盖,或重建栈后重跑)\n",
			seedAuthorUser, err, seedAuthorPasswordEnv)
		return ExitError
	}

	// ③ 定义按名判存:200 = 已发布跳过;404 = 两跳发布。
	st, _, err := getJSON(ctx, hc, base+"/api/v1/definitions/"+dsl.Name, token,
		http.StatusOK, http.StatusNotFound)
	if err != nil {
		say(stderr, "braid seed: probe definition: %v\n", err)
		return ExitError
	}
	switch st {
	case http.StatusOK:
		sayln(stdout, "seed ③ 定义:%s 已发布,跳过", dsl.Name)
	case http.StatusNotFound:
		detail, err := publishTwoHop(ctx, hc, base, token, doc)
		if err != nil {
			say(stderr, "braid seed: publish: %v\n", err)
			return ExitError
		}
		var pub struct {
			Version uint32 `json:"version"`
		}
		_ = json.Unmarshal(detail, &pub) // 版本号仅叙事用,缺字段不致败
		sayln(stdout, "seed ③ 定义:%s 已发布(v%d)", dsl.Name, pub.Version)
	}

	// ④ 触发:固定幂等键 = seed-<name>-<输入摘要前 8 hex>——重跑重放既有
	// 执行(200),改 demoProposal 文案 → 键变 → 新执行。
	input, err := json.Marshal(map[string]string{"proposal": demoProposal})
	if err != nil {
		say(stderr, "braid seed: encode input: %v\n", err)
		return ExitError
	}
	sum := sha256.Sum256(input)
	idemKey := fmt.Sprintf("seed-%s-%x", dsl.Name, sum[:4])
	payload, err := executionBody(dsl.Name, 0, input)
	if err != nil {
		say(stderr, "braid seed: encode body: %v\n", err)
		return ExitError
	}
	st, detail, err := postJSON(ctx, hc, base+"/api/v1/executions", token, payload, idemKey,
		http.StatusOK, http.StatusCreated)
	if err != nil {
		say(stderr, "braid seed: trigger: %v\n", err)
		return ExitError
	}
	exec := struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}{}
	if err := json.Unmarshal(detail, &exec); err != nil || exec.ID == "" {
		say(stderr, "braid seed: trigger response missing id: %s\n", string(detail))
		return ExitError
	}
	if st == http.StatusOK {
		sayln(stdout, "seed ④ 触发:幂等键命中,回显既有执行 %s", exec.ID)
	} else {
		sayln(stdout, "seed ④ 触发:执行 %s 已创建", exec.ID)
	}

	// ⑤ 有界等待落点:先轮询后睡(预算 0 或已落点即零轮询);轮询失败
	// 不致败(触发已成功,报最后已知状态)。
	status := seedWaitLanding(ctx, hc, base, token, exec.ID, exec.Status, p.wait, stderr)
	switch {
	case status == "WAITING_APPROVAL":
		sayln(stdout, "seed ⑤ 执行 %s:WAITING_APPROVAL(等待人工审批)—— 打开 %s/approvals/ 用 %s 或 %s 登录后批准(%s);工作流画布 %s/workflows/;队列现场 Asynqmon http://127.0.0.1:8080",
			exec.ID, base, seedAuthorUser, seedApproverUser, seedCredHint(env), base)
	case seedSettled(status):
		sayln(stdout, "seed ⑤ 执行 %s:%s(终态;详情 GET %s/api/v1/executions/%s)", exec.ID, status, base, exec.ID)
	case p.wait == 0:
		sayln(stdout, "seed ⑤ 执行 %s:%s(-wait=0,未等待落点)", exec.ID, status)
	default:
		sayln(stdout, "seed ⑤ 等待 %s 预算耗尽:执行 %s 当前 %s(非失败,可在 %s/approvals/ 或 Asynqmon 继续跟踪)", p.wait, exec.ID, status, base)
	}
	return ExitOK
}

// seedWaitLanding 轮询执行状态至落点(WAITING_APPROVAL 或终态)或预算
// 耗尽,返回最后已知状态;单次轮询失败记 stderr 后以现值返回(不致败)。
func seedWaitLanding(ctx context.Context, hc *http.Client, base, token, execID, initial string, wait time.Duration, stderr io.Writer) string {
	status := initial
	if wait <= 0 || seedSettled(status) {
		return status
	}
	deadline := time.Now().Add(wait)
	for {
		_, detail, err := getJSON(ctx, hc, base+"/api/v1/executions/"+execID, token, http.StatusOK)
		if err != nil {
			say(stderr, "braid seed: poll execution: %v\n", err)
			return status
		}
		var got struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(detail, &got); err == nil && got.Status != "" {
			status = got.Status
		}
		if seedSettled(status) {
			return status
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return status
		}
		interval := seedPollInterval
		if remaining < interval {
			interval = remaining
		}
		select {
		case <-ctx.Done():
			return status
		case <-time.After(interval):
		}
	}
}

// seedSettled 报状态是否落点(WAITING_APPROVAL 或终态;词表 = engine
// WorkflowState,字符串集本地钉死避免 cli→engine 新 import 边)。
func seedSettled(status string) bool {
	switch status {
	case "WAITING_APPROVAL", "SUCCEEDED", "FAILED", "CANCELLED":
		return true
	}
	return false
}

// seedCredHint ⑤ 叙事里的口令提示:仅当两个口令都走 dev 缺省时回显占位
// 值;任一被 env 覆盖 → 只报名不回显(密钥不进日志纪律)。
func seedCredHint(env func(string) string) string {
	if env(seedAuthorPasswordEnv) == "" && env(seedApproverPasswordEnv) == "" {
		return "口令:" + seedAuthorPasswordDefault + " / " + seedApproverPasswordDefault + "(dev 默认占位)"
	}
	return "口令:" + seedAuthorPasswordEnv + " / " + seedApproverPasswordEnv + " 所设值(非默认,不回显)"
}

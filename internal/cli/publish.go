package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// runDefinitionPublish 是 `braid definition publish` 的 argv 层:恰一个
// DSL 文件位置参;两跳 REST(POST /definitions → POST /drafts/:id/publish)
// 在 publishRun(D-2026-10-03-t162:走真路径,不复制 handler 语义)。
func runDefinitionPublish(ctx context.Context, args []string, env func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("braid definition publish", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "", "API base URL (default $BRAID_API_ADDR or http://127.0.0.1:8081)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 1 {
		sayln(stderr, "%s", "braid definition publish: exactly one DSL file argument required")
		return ExitUsage
	}
	return publishRun(ctx, env, publishParams{
		addr:    *addr,
		dslFile: fs.Arg(0),
	}, stdout, stderr)
}

// publishParams 是 definition publish 的已解析入参。
type publishParams struct {
	addr    string // 空 = 体层解析 $BRAID_API_ADDR → 默认回环
	dslFile string
}

// publishRun 发布体:读文件 + 凭据解析后委托 publishTwoHop;stdout =
// 发布 detail 原文(机器消费面,T16.3 seed 依赖)。
func publishRun(ctx context.Context, env func(string) string, p publishParams, stdout, stderr io.Writer) int {
	base := resolveAPIBase(p.addr, env("BRAID_API_ADDR"))
	doc, err := os.ReadFile(p.dslFile)
	if err != nil {
		say(stderr, "braid definition publish: read %s: %v\n", p.dslFile, err)
		return ExitError
	}
	hc := newHTTPClient()
	token, err := resolveToken(ctx, hc, base, env)
	if err != nil {
		say(stderr, "braid definition publish: %v\n", err)
		return ExitError
	}
	detail, err := publishTwoHop(ctx, hc, base, token, doc)
	if err != nil {
		say(stderr, "braid definition publish: %v\n", err)
		return ExitError
	}
	if _, err := fmt.Fprintln(stdout, strings.TrimSpace(string(detail))); err != nil {
		say(stderr, "braid definition publish: write stdout: %v\n", err)
		return ExitError
	}
	return ExitOK
}

// publishTwoHop 两跳发布体(T16.3 抽出供 seed 复用):POST /definitions
// 建草案 → POST /drafts/:id/publish,返回发布 detail。失败路径每跳独立
// 归因(submit/publish 前缀);hop2 失败带草稿 id(发布面无幂等键,
// 盲目重跑 hop1 会再留孤儿草案——对抗审计 MEDIUM:失败态可见性)。
func publishTwoHop(ctx context.Context, hc *http.Client, base, token string, doc []byte) ([]byte, error) {
	_, body, err := postJSON(ctx, hc, base+"/api/v1/definitions", token, doc, "", http.StatusCreated)
	if err != nil {
		return nil, fmt.Errorf("submit draft: %w", err)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		return nil, fmt.Errorf("submit response missing draft id: %s", strings.TrimSpace(string(body)))
	}
	_, detail, err := postJSON(ctx, hc, base+"/api/v1/drafts/"+created.ID+"/publish", token, nil, "", http.StatusOK)
	if err != nil {
		return nil, fmt.Errorf("publish draft %s: %w", created.ID, err)
	}
	return detail, nil
}

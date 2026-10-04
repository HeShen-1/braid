// client.go 是发布/触发共用的最小 REST 客户端(D-2026-10-03-t162:
// 走真路径,RBAC/审计/幂等原生生效)。凭证纪律:token 只经 env 解析链,
// 零 argv 凭据。
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultAPIBase 是 $BRAID_API_ADDR 与 -addr 都缺省时的 dev 栈默认
// (cmd/api 回环绑定契约)。
const defaultAPIBase = "http://127.0.0.1:8081"

// maxRespExcerpt 是错误 body 摘录上界(服务端错误面是短 JSON code,
// 8KiB 覆盖 DSL 校验 issues 聚合;防失控响应撑爆 stderr)。只作用于
// 错误消息拼装——成功 body 一律全量读(见 maxRespBody)。
const maxRespExcerpt = 8 << 10

// maxRespBody 是响应体读入上界:服务端 DSL 上限 1MiB(api.maxDSLBody,
// handlers.go readBodyLimited 同闸也辖触发 body),detail DTO 内嵌全量
// DSL 原文,故成功响应常态可达 1MiB 量级;2MiB = DSL 上限 + DTO 封套
// 余量。越界响亮报错,不静默截断(stdout 是 T16.3 seed 的机器消费面)。
const maxRespBody = 2 << 20

// httpClientTimeout 罩住整个请求生命周期:CLI 三调用都是同步短应答
// (触发是异步入队),30s 覆盖慢 dev 栈;防脚本化场景对端挂起无限等
// (对抗审计 MEDIUM:无人值守面只有 ctx 信号取消能救)。
const httpClientTimeout = 30 * time.Second

// newHTTPClient 是发布/触发共用的客户端构造器(超时单点)。
func newHTTPClient() *http.Client {
	return &http.Client{Timeout: httpClientTimeout}
}

// resolveAPIBase 三层解析:-addr 值 > $BRAID_API_ADDR > dev 默认。值域
// 兼容两种形态:完整 URL(http://…)或裸 host:port(后者即 cmd/api 的
// BRAID_API_ADDR 监听值——同名 env 一名两义,CLI 按 http:// 前缀垫片,
// source deploy/.env 不踩雷);尾斜杠归一(拼路径不产生 //)。
func resolveAPIBase(flagAddr, envAddr string) string {
	base := flagAddr
	if base == "" {
		base = envAddr
	}
	if base == "" {
		base = defaultAPIBase
	}
	base = strings.TrimRight(base, "/")
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	return base
}

// resolveToken 解析链:$BRAID_TOKEN 直用(零请求);否则
// $BRAID_USERNAME+$BRAID_PASSWORD 走 login 换 Bearer;都不在 → 指引性
// 错误(提及 env 名,凭据永不进 argv)。
func resolveToken(ctx context.Context, hc *http.Client, base string, env func(string) string) (string, error) {
	if tok := env("BRAID_TOKEN"); tok != "" {
		return tok, nil
	}
	user, pass := env("BRAID_USERNAME"), env("BRAID_PASSWORD")
	if user == "" || pass == "" {
		return "", fmt.Errorf("no credentials: set BRAID_TOKEN, or BRAID_USERNAME + BRAID_PASSWORD for login (credentials never go on the command line)")
	}
	return login(ctx, hc, base, user, pass)
}

// login POST /api/v1/auth/login 换 token(login.go 契约:200
// {"token","token_type",...};错误面防枚举统一 401 {"error":...})。
func login(ctx context.Context, hc *http.Client, base, username, password string) (string, error) {
	payload, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		return "", fmt.Errorf("login: encode body: %w", err)
	}
	_, body, err := postJSON(ctx, hc, base+"/api/v1/auth/login", "", payload, "", http.StatusOK)
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("login: decode response: %w", err)
	}
	if resp.Token == "" {
		return "", fmt.Errorf("login: response missing token")
	}
	return resp.Token, nil
}

// postJSON 发 JSON POST(Bearer 可空 = login 面;Idempotency-Key 可选
// 透传),wantStatuses 是可接受状态集(触发面 = 200|201:服务端幂等重放
// 契约回 200 + 原执行回显,execution.go;单值面传一个即可)。状态集外
// → 错误带状态与 body 摘录(8KiB);成功返回状态码与 body 全文(不二
// 次解析丢字段,detail 可达 1MiB 量级)。
func postJSON(ctx context.Context, hc *http.Client, url, token string, payload []byte, idempotencyKey string, wantStatuses ...int) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // 只读路径
	return finishResp(resp, wantStatuses)
}

// getJSON 发带 Bearer 的 GET(T16.3 seed:定义探针 {200,404} 双接受、
// 执行轮询 {200});响应处理与 postJSON 同则(finishResp 单源)。
func getJSON(ctx context.Context, hc *http.Client, url, token string, wantStatuses ...int) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // 只读路径
	return finishResp(resp, wantStatuses)
}

// finishResp 是 GET/POST 共享的响应尾:全量读(上限 maxRespBody)、
// 状态集匹配,集外错误带摘录(8KiB)。
func finishResp(resp *http.Response, wantStatuses []int) (int, []byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBody+1))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response: %w", err)
	}
	if len(body) > maxRespBody {
		return resp.StatusCode, nil, fmt.Errorf("response exceeds %d bytes (server cap misaligned?)", maxRespBody)
	}
	for _, want := range wantStatuses {
		if resp.StatusCode == want {
			return resp.StatusCode, body, nil
		}
	}
	excerpt := body
	if len(excerpt) > maxRespExcerpt {
		excerpt = excerpt[:maxRespExcerpt]
	}
	return resp.StatusCode, body, fmt.Errorf("HTTP %d (want %v): %s", resp.StatusCode, wantStatuses, strings.TrimSpace(string(excerpt)))
}

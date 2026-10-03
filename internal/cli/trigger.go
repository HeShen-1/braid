package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
)

// runExecutionTrigger 是 `braid execution trigger` 的 argv 层:-input 支
// 持 `-`(stdin);触发体在 triggerRun(REST 真路径,幂等键透传)。
func runExecutionTrigger(ctx context.Context, args []string, env func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("braid execution trigger", flag.ContinueOnError)
	fs.SetOutput(stderr)
	definition := fs.String("definition", "", "published definition name (required)")
	version := fs.Uint64("version", 0, "pinned version, 0 = latest published")
	input := fs.String("input", "", "input JSON file path, or - for stdin (required)")
	idemKey := fs.String("idempotency-key", "", "Idempotency-Key header passthrough, optional")
	addr := fs.String("addr", "", "API base URL (default $BRAID_API_ADDR or http://127.0.0.1:8081)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *definition == "" || *input == "" {
		sayln(stderr, "%s", "braid execution trigger: -definition and -input are required")
		return ExitUsage
	}
	if *version > math.MaxUint32 { // API Version 是 *uint32,越界属 argv 层结构错
		say(stderr, "braid execution trigger: -version %d exceeds uint32 max\n", *version)
		return ExitUsage
	}
	if fs.NArg() != 0 { // 与 publish 的位置参闸同则:多余位置参即用法错,不静默吞
		say(stderr, "braid execution trigger: unexpected positional arguments %v\n", fs.Args())
		return ExitUsage
	}
	return triggerRun(ctx, env, stdin, triggerParams{
		addr:           *addr,
		definition:     *definition,
		version:        *version,
		inputFile:      *input,
		idempotencyKey: *idemKey,
	}, stdout, stderr)
}

// triggerParams 是 execution trigger 的已解析入参。
type triggerParams struct {
	addr           string // 空 = 体层解析 $BRAID_API_ADDR → 默认回环
	definition     string
	version        uint64 // 0 = 最新已发布版
	inputFile      string // "-" = stdin
	idempotencyKey string
}

// triggerRun 触发体:POST /executions(definition_name/version?/input
// 原文嵌入;Idempotency-Key 透传),stdout = 执行 detail 原文。
func triggerRun(ctx context.Context, env func(string) string, stdin io.Reader, p triggerParams, stdout, stderr io.Writer) int {
	base := resolveAPIBase(p.addr, env("BRAID_API_ADDR"))
	raw, err := readInput(p.inputFile, stdin)
	if err != nil {
		say(stderr, "braid execution trigger: read input: %v\n", err)
		return ExitError
	}
	if !json.Valid(raw) {
		sayln(stderr, "%s", "braid execution trigger: input is not valid JSON")
		return ExitError
	}
	payload := map[string]any{
		"definition_name": p.definition,
		"input":           json.RawMessage(raw),
	}
	if p.version > 0 {
		payload["version"] = uint32(p.version)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		say(stderr, "braid execution trigger: encode body: %v\n", err)
		return ExitError
	}
	hc := newHTTPClient()
	token, err := resolveToken(ctx, hc, base, env)
	if err != nil {
		say(stderr, "braid execution trigger: %v\n", err)
		return ExitError
	}
	// 200|201 双接受(服务端幂等契约 execution.go):201 = 新执行;
	// 200 = 同 Idempotency-Key 重放回显原执行(机器语义同为成功,退出 0)。
	status, detail, err := postJSON(ctx, hc, base+"/api/v1/executions", token, encoded, p.idempotencyKey,
		http.StatusOK, http.StatusCreated)
	if err != nil {
		say(stderr, "braid execution trigger: %v\n", err)
		return ExitError
	}
	if status == http.StatusOK && p.idempotencyKey != "" {
		sayln(stderr, "%s", "braid execution trigger: replayed (idempotency key matched an existing execution)")
	}
	if _, err := fmt.Fprintln(stdout, strings.TrimSpace(string(detail))); err != nil {
		say(stderr, "braid execution trigger: write stdout: %v\n", err)
		return ExitError
	}
	return ExitOK
}

// maxInputBody 是输入 JSON 客户端读入上界,对齐服务端请求体闸
// (api handlers.go readBodyLimited,DSL 与触发同闸 1MiB):先在客户端
// 响亮拒绝,不做「吃满内存再被服务端 413」的空转。
const maxInputBody = 1 << 20

// readInput 读输入 JSON:- = stdin,否则文件路径(错误自带路径名);
// 越上界响亮报错(读 maxInputBody+1 探测)。
func readInput(path string, stdin io.Reader) ([]byte, error) {
	var r = stdin // io.Reader 位(文件路径置换)
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close() //nolint:errcheck // 只读路径
		r = f
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxInputBody+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxInputBody {
		return nil, fmt.Errorf("input exceeds %d bytes (server cap)", maxInputBody)
	}
	return raw, nil
}

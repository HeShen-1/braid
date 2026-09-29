package audit

import (
	"encoding/hex"
	"strings"
	"testing"
)

// T9.3 哈希链规范化(D-2026-09-29-t93 #3)的纯函数锚:payload 语义规范化
// (MySQL JSON 列读回形态 ≠ Go 写入字节 ⇒ 链锚定等价类而非字节)、cost 六位
// 定标、19 列编码的确定性与字段隔离(篡改检测的机制根基)。

func TestCanonicalPayloadEquivalence(t *testing.T) {
	cases := []struct {
		name string
		a, b string // 同值两形(Go 紧凑形 vs MySQL 读回形/键序漂移形)
	}{
		{"go_compact_vs_mysql_spaced", `{"a":1,"b":2}`, `{"a": 1, "b": 2}`},
		{"key_order", `{"a":1,"b":2}`, `{"b": 2, "a": 1}`},
		{"nested_key_order", `{"x":{"b":1,"a":2},"y":3}`, `{"y":3,"x":{"a":2,"b":1}}`},
		{"string_escape", `{"k":"A"}`, `{"k":"A"}`},
		{"float_shortest", `{"c":1.1}`, `{"c":1.10}`},
		{"exponent_form", `{"c":1.5e+21}`, `{"c":1.5e21}`},
		{"empty_object", `{}`, `{ }`},
		{"empty_array", `[]`, `[ ]`},
		{"null_bool", `{"n":null,"t":true,"f":false}`, `{"f":false, "t":true, "n":null}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ca, err := canonicalPayload([]byte(c.a))
			if err != nil {
				t.Fatalf("canonical a: %v", err)
			}
			cb, err := canonicalPayload([]byte(c.b))
			if err != nil {
				t.Fatalf("canonical b: %v", err)
			}
			if string(ca) != string(cb) {
				t.Fatalf("同值不同形未归一:\n a=%s → %s\n b=%s → %s", c.a, ca, c.b, cb)
			}
		})
	}
}

func TestCanonicalPayloadInvariants(t *testing.T) {
	// 数组序保持(语义上有序,不得排序)
	a1, _ := canonicalPayload([]byte(`[1,2]`))
	a2, _ := canonicalPayload([]byte(`[2,1]`))
	if string(a1) == string(a2) {
		t.Fatalf("数组序被折叠:[1,2] 与 [2,1] 同串 %s", a1)
	}
	// 整数逐字精确(≥2^53 雪花 id 不失真)
	big := `{"id":733262088954220546}`
	got, err := canonicalPayload([]byte(big))
	if err != nil {
		t.Fatalf("canonical big int: %v", err)
	}
	if !strings.Contains(string(got), "733262088954220546") {
		t.Fatalf("大整数失真: %s", got)
	}
	big2 := `{"id":733262088954220547}` // 末位 +1:float64 通道会折叠,必须仍可区分
	got2, _ := canonicalPayload([]byte(big2))
	if string(got) == string(got2) {
		t.Fatalf("大整数末位篡改被折叠: %s", got)
	}
	// 前导零是非法 JSON(JSON 数域禁 leading zero,Go/MySQL 同拒)——响亮臂
	if _, err := canonicalPayload([]byte(`{"n":007}`)); err == nil {
		t.Fatalf("非法前导零 JSON 静默通过")
	}
	// 坏输入响亮
	for _, bad := range []string{`{`, `nul`, `{"a":}`} {
		if _, err := canonicalPayload([]byte(bad)); err == nil {
			t.Fatalf("坏 JSON %q 静默通过", bad)
		}
	}
	// 非法 UTF-8(镜像 validate 的门,canonicalizer 自持防线)
	if _, err := canonicalPayload([]byte("{\"k\":\"\xff\xfe\"}")); err == nil {
		t.Fatalf("非法 UTF-8 静默通过")
	}
}

func TestCostScale6(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"2.5", "2.500000"},
		{"2", "2.000000"},
		{"12345678.123456", "12345678.123456"},
		{"0.000001", "0.000001"},
	}
	for _, c := range cases {
		got, err := costScale6(c.in)
		if err != nil {
			t.Fatalf("costScale6(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("costScale6(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "1.1234567", "-1.0", "1.2.3", "abc", "123456789.1"} {
		if _, err := costScale6(bad); err == nil {
			t.Fatalf("costScale6(%q) 静默通过", bad)
		}
	}
}

func baseChainRow() chainRow {
	return chainRow{
		ID:            733262088954220546,
		EventType:     EventNodeStateChanged,
		ExecutionID:   100,
		NodeExecID:    201,
		NodeID:        "n1",
		AttemptID:     301,
		ActorType:     ActorWorker,
		ActorID:       "worker-1",
		TraceID:       "0123456789abcdef0123456789abcdef",
		PIIMasked:     false,
		PromptTokens:  10,
		TotalTokens:   30,
		CostAmount:    "1.500000",
		CostCurrency:  "USD",
		CostEstimated: true,
		Payload:       []byte(`{"from":"PENDING","to":"RUNNING"}`),
	}
}

func TestRowHashDeterministic(t *testing.T) {
	r := baseChainRow()
	// payload 同值 MySQL 形 → 同 hash(等价类锚)
	r2 := r
	r2.Payload = []byte(`{"to": "RUNNING", "from": "PENDING"}`)
	// cost "1.5" 与 "1.500000" 同 hash(六位定标锚)
	r3 := r
	r3.CostAmount = "1.5"

	h1, err := r.rowHash("")
	if err != nil {
		t.Fatalf("rowHash: %v", err)
	}
	h2, err := r2.rowHash("")
	if err != nil {
		t.Fatalf("rowHash r2: %v", err)
	}
	h3, err := r3.rowHash("")
	if err != nil {
		t.Fatalf("rowHash r3: %v", err)
	}
	if h1 != h2 || h1 != h3 {
		t.Fatalf("同值不同形 hash 不一致:\n h1=%s\n h2=%s\n h3=%s", h1, h2, h3)
	}
	if len(h1) != 64 {
		t.Fatalf("hash 长度 %d, want 64", len(h1))
	}
	if hex.EncodeToString([]byte{0xde, 0xad}) != "dead" {
		t.Fatal("hex 假设失效")
	}
	for _, c := range h1 {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("hash 非小写 hex: %s", h1)
		}
	}
	// prev 参与:不同前驱 → 不同行 hash(链的串行化语义)
	h4, err := r.rowHash(h1)
	if err != nil {
		t.Fatalf("rowHash chained: %v", err)
	}
	if h4 == h1 {
		t.Fatal("prev_hash 未参与行 hash")
	}
}

func TestRowHashFieldIsolation(t *testing.T) {
	base := baseChainRow()
	baseHash, err := base.rowHash("")
	if err != nil {
		t.Fatalf("base rowHash: %v", err)
	}
	// 每列翻转必改 hash(篡改检测;NULL 语义列含「值 ⇄ NULL」翻转)
	flip := map[string]func(*chainRow){
		"id":            func(r *chainRow) { r.ID++ },
		"event_type":    func(r *chainRow) { r.EventType = EventBranchEvaluated },
		"execution_id":  func(r *chainRow) { r.ExecutionID++ },
		"node_exec_id":  func(r *chainRow) { r.NodeExecID = 0 },
		"node_id":       func(r *chainRow) { r.NodeID = "" },
		"attempt_id":    func(r *chainRow) { r.AttemptID = 0 },
		"actor_type":    func(r *chainRow) { r.ActorType = ActorSystem },
		"actor_id":      func(r *chainRow) { r.ActorID = "other" },
		"trace_id":      func(r *chainRow) { r.TraceID = "" },
		"linked_trace":  func(r *chainRow) { r.LinkedTraceID = "ffffffffffffffffffffffffffffffff" },
		"pii_masked":    func(r *chainRow) { r.PIIMasked = true },
		"model":         func(r *chainRow) { r.Model = "gpt-x" },
		"prompt_tokens": func(r *chainRow) { r.PromptTokens = 11 },
		"compl_tokens":  func(r *chainRow) { r.CompletionTokens = 1 },
		"total_tokens":  func(r *chainRow) { r.TotalTokens = 31 },
		"cost_amount":   func(r *chainRow) { r.CostAmount = "1.500001" },
		"cost_currency": func(r *chainRow) { r.CostCurrency = "" },
		"cost_est":      func(r *chainRow) { r.CostEstimated = false },
		"payload":       func(r *chainRow) { r.Payload = []byte(`{"from":"PENDING","to":"FAILED"}`) },
	}
	for name, mutate := range flip {
		t.Run(name, func(t *testing.T) {
			r := baseChainRow()
			mutate(&r)
			h, err := r.rowHash("")
			if err != nil {
				t.Fatalf("rowHash: %v", err)
			}
			if h == baseHash {
				t.Fatalf("翻转 %s 未改变 hash(列未入链)", name)
			}
		})
	}
	// 零值与 NULL 语义在编码中显式分立(NodeID "" = NULL 标记;非空同列)
	r := baseChainRow()
	r.NodeID = ""
	if _, err := r.rowHash(""); err != nil {
		t.Fatalf("nullable row: %v", err)
	}
}

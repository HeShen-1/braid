package audit

// T9.3 审计哈希链的规范化与行哈希(D-2026-09-29-t93 #3)。两个迫使「规范面
// 而非字节面」的物理事实:① MySQL JSON 列读回形态 ≠ Go 写入字节(空白/
// 键序/数字格式被列存储规范化),写侧与校验侧必须走同一 canonicalizer 才能
// 对得上;② DECIMAL(14,6) 读回恒 6 位小数,引擎侧金额本就是十进制字符串
// (engine.AttemptUsage,Validate 钉 ≤6 位小数)——补零到恰 6 位即与读回
// 形态字节一致,零浮点往返(DECIMAL 舍入边界不可达:输入域已 ≤6 位)。
//
// 入链 19 列 = 应用侧已知全集;排除 event_at/created_at(DB 端时钟,写入前
// 不可知,D-t61 #7)与 exec_date(暂存期未物化;校验期按
// workflow_execution.created_at re-derivation 比对补面)。链保 payload 的
// **语义等价类**:存储仍零 remarshal(D-t26 #5 立场不动),规范化只发生
// 在 hash 输入副本上。
//
// 编码 = 定宽标量 + 长度前缀字节串 + 显式 NULL 标记(字段序固定);输出
// SHA-256 小写 hex 64(对齐 DDL CHAR(64) ascii)。

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// chainRow 是哈希链的 19 列规范化输入。零值规则与 Event 落库归一同形:
// NodeExecID/AttemptID/PromptTokens 等 0 = NULL,空串列("" = NULL);
// CostAmount 空串 = NULL(Billed=false),非空 = 已过 costScale6。
type chainRow struct {
	ID               uint64
	EventType        string
	ExecutionID      uint64
	NodeExecID       uint64 // 0 = NULL
	NodeID           string // "" = NULL
	AttemptID        uint64 // 0 = NULL
	ActorType        string
	ActorID          string
	TraceID          string // "" = NULL
	LinkedTraceID    string // "" = NULL
	PIIMasked        bool
	Model            string // "" = NULL
	PromptTokens     uint64 // 0 = NULL
	CompletionTokens uint64 // 0 = NULL
	TotalTokens      uint64 // 0 = NULL
	CostAmount       string // "" = NULL;非空 = 六位定标十进制
	CostCurrency     string // "" = NULL
	CostEstimated    bool
	Payload          []byte // 任意合法等价形(rowHash 内部规范化)
}

// rowHash 计算本行哈希:SHA-256(prev_hash ‖ 编码(19 列))。prev = ” 即
// 链首。payload 在此内部规范化(单一收口点:写侧 Go 形 / 校验侧 MySQL
// 读回形不经调用方手工归一,直接进同一 canonicalizer)。纯函数、无 DB
// 触达——写侧(落行前)与校验侧(读回后)共享唯一实现。
func (r chainRow) rowHash(prev string) (string, error) {
	if prev != "" && !isHex64(prev) {
		return "", fmt.Errorf("audit: prev_hash %q not 64-hex", prev)
	}
	payload, err := canonicalPayload(r.Payload)
	if err != nil {
		return "", err
	}
	cost := r.CostAmount // "" = NULL 原样;非空内部定标(同 payload 单一收口)
	if cost != "" {
		cost, err = costScale6(cost)
		if err != nil {
			return "", err
		}
	}
	var b []byte
	b = append(b, prev...)
	b = binary.BigEndian.AppendUint64(b, r.ID)
	b = putStr(b, r.EventType)
	b = binary.BigEndian.AppendUint64(b, r.ExecutionID)
	b = putOptU64(b, r.NodeExecID)
	b = putOptStr(b, r.NodeID)
	b = putOptU64(b, r.AttemptID)
	b = putStr(b, r.ActorType)
	b = putStr(b, r.ActorID)
	b = putOptStr(b, r.TraceID)
	b = putOptStr(b, r.LinkedTraceID)
	b = binary.BigEndian.AppendUint64(b, boolWord(r.PIIMasked))
	b = putOptStr(b, r.Model)
	b = putOptU64(b, r.PromptTokens)
	b = putOptU64(b, r.CompletionTokens)
	b = putOptU64(b, r.TotalTokens)
	b = putOptStr(b, cost)
	b = putOptStr(b, r.CostCurrency)
	b = binary.BigEndian.AppendUint64(b, boolWord(r.CostEstimated))
	// payload NOT NULL(无正文存 {}),恒走非空臂;长度前缀消歧。
	b = putStr(b, string(payload))
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func boolWord(v bool) uint64 {
	if v {
		return 1
	}
	return 0
}

// putStr:非空字节串 = 8 字节长度 + 字节(定宽长度前缀,零变体)。
func putStr(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint64(b, uint64(len(s)))
	return append(b, s...)
}

// putOptStr:可空字节串 = 1 字节在场标记 + [putStr]。NULL 与空串在编码层
// 显式分立("" 空串在词表列不可能,但编码不依赖上层约定)。
func putOptStr(b []byte, s string) []byte {
	if s == "" {
		return append(b, 0)
	}
	b = append(b, 1)
	return putStr(b, s)
}

// putOptU64:可空整数 = 1 字节在场标记 + 8 字节值。0 = NULL(与 Event 落库
// 归一同形:零值 → NULL 列)。
func putOptU64(b []byte, v uint64) []byte {
	if v == 0 {
		return append(b, 0)
	}
	b = append(b, 1)
	return binary.BigEndian.AppendUint64(b, v)
}

var hex64Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

func isHex64(s string) bool { return hex64Re.MatchString(s) }

// costScale6 把引擎侧十进制金额串(^\d{1,8}(\.\d{1,6})?$,engine.Validate
// 同域)补零到恰 6 位小数 = DECIMAL(14,6) 读回形态。校验侧输入本就 6 位
// (passthrough);写侧 "2.5" → "2.500000"。坏形状响亮(防御臂:绕过
// Event 校验的直传调用方在此红)。
func costScale6(s string) (string, error) {
	if !costShapeRe.MatchString(s) {
		return "", fmt.Errorf("audit: cost %q not ^\\d{1,8}(\\.\\d{1,6})?$", s)
	}
	intPart, frac, hasFrac := strings.Cut(s, ".")
	if !hasFrac {
		frac = ""
	}
	return intPart + "." + frac + strings.Repeat("0", 6-len(frac)), nil
}

var costShapeRe = regexp.MustCompile(`^\d{1,8}(\.\d{1,6})?$`)

// canonicalPayload 产出语义规范化 JSON:对象键按字节排序(逐层)、空白
// 无关、整数逐字精确(≥2^53 雪花 id 不失真)、浮点取 float64 最短形。
// Go 写入形与 MySQL 读回形同值必同串——链锚定等价类的机器保证。
// 坏 JSON / 非法 UTF-8 响亮拒绝(镜像 validate 的门,canonicalizer 自持)。
func canonicalPayload(raw []byte) ([]byte, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("audit: payload not valid UTF-8")
	}
	v, err := parseJSONValue(raw)
	if err != nil {
		return nil, fmt.Errorf("audit: payload canonicalize: %w", err)
	}
	var b []byte
	b, err = appendCanonical(b, v)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// parseJSONValue 用 UseNumber 解析(json.Number 保字面,整数路径零浮点)。
func parseJSONValue(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	// 尾随垃圾(`{}{}`):Decode 只消费首值,拒绝非空白残余。
	if dec.More() {
		return nil, fmt.Errorf("trailing content after JSON value")
	}
	return v, nil
}

func appendCanonical(b []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(b, "null"...), nil
	case bool:
		if x {
			return append(b, "true"...), nil
		}
		return append(b, "false"...), nil
	case json.Number:
		return appendNumber(b, x)
	case string:
		return appendJSONString(b, x), nil
	case []any:
		b = append(b, '[')
		for i, e := range x {
			if i > 0 {
				b = append(b, ',')
			}
			var err error
			if b, err = appendCanonical(b, e); err != nil {
				return nil, err
			}
		}
		return append(b, ']'), nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b = append(b, '{')
		for i, k := range keys {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendJSONString(b, k)
			b = append(b, ':')
			var err error
			if b, err = appendCanonical(b, x[k]); err != nil {
				return nil, err
			}
		}
		return append(b, '}'), nil
	default:
		// jsonDecoder 的 UseNumber 保证只出 json.Number,any 其他类型
		// = 解码器行为漂移,响亮不吞。
		return nil, fmt.Errorf("audit: canonicalize: unexpected node %T", v)
	}
}

// appendJSONString:转义规则对齐 encoding/json(HTML 转义关闭、非法 UTF-8
// 已在入口拒绝)——输出形态两侧(写/校验)同走本函数,自洽即确定。
func appendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	for _, r := range s {
		switch r {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			if r < 0x20 {
				b = append(b, fmt.Sprintf("\\u%04x", r)...)
			} else {
				b = append(b, string(r)...)
			}
		}
	}
	return append(b, '"')
}

var intLiteralRe = regexp.MustCompile(`^-?[0-9]+$`)

// appendNumber:整数逐字(经 big.Int 归一前导零与 -0),浮点 float64 最短
// 形('g' -1:Go 写入 "1.5e+21" 与 MySQL 读回 "1.5e21" 同落一处)。
// 非有限值(原文 NaN/Infinity 非合法 JSON,不可达)响亮。
func appendNumber(b []byte, n json.Number) ([]byte, error) {
	lit := string(n)
	if intLiteralRe.MatchString(lit) {
		i := new(big.Int)
		if _, ok := i.SetString(lit, 10); !ok {
			return nil, fmt.Errorf("audit: canonicalize: bad integer literal %q", lit)
		}
		return append(b, i.String()...), nil
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return nil, fmt.Errorf("audit: canonicalize: bad number %q: %w", lit, err)
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	return append(b, s...), nil
}

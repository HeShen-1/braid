package audit

// T6.1 PII 脱敏钩子(ARCHITECTURE §3.5「PII 脱敏钩子 P0」)。口径继承
// T1.1「prompt 模板与用户数据分离标注」:不可信用户数据进入 prompt 的唯一
// 通道是 user_data 标注块——脱敏钩子沿用同一标注约定,payload 内任何
// user_data 键下的子树视为用户数据面,叶子值掩蔽、键形保留(键 = T1.1
// 标签名,非 PII;保留形状让掩蔽事实可诊断)。
//
// 时机契约(DDL pii_masked 列注释):脱敏先于任何落库完成——同步路径
// (Append)与异步路径(Enqueue → audit_pending)都在 INSERT 之前过钩子,
// 「原始永不入 payload」含暂存表;append-only 禁止事后 UPDATE,事后补脱
// 敏结构性不可行。
//
// 现有 payload 词表(D-t26 §4)只含 id/码/枚举,无自由文本 ⇒ P0 净效果
// 为零掩蔽;本钩子是为放开自由文本(llm 观测面等)预铺的基建 + 验收面。
// 未掩蔽路径原字节返回(零 remarshal):payload 字节稳定性是 P1 哈希链
// 规范化的前提,也让钩子的零成本路径可观测。

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// userDataKey 是 T1.1 标注约定的 JSON 键(精确匹配,不做前缀/子串放宽)。
const userDataKey = "user_data"

// maskToken 是固定掩蔽令牌:定长不泄原始长度,无字符集信息。
const maskToken = "***"

// SanitizePayload 对 payload 应用 PII 脱敏钩子:递归遍历 JSON 树,任何
// user_data 键的值子树(对象/数组递归到叶子,标量直接)叶子值替换为
// maskToken;null 保持(非 PII)。返回 (处理后的字节, 是否发生掩蔽, 错误)。
//
// 未发生掩蔽时返回原 raw(字节不变);错误仅在 JSON 非法时出现——常规
// 调用序列中 validate() 先行已拒非法字节,此臂防御独立调用面。
func SanitizePayload(raw json.RawMessage) (json.RawMessage, bool, error) {
	// UseNumber 解码:数字保形为 json.Number(词素原样),掩蔽路径的 remarshal
	// 不经 float64 往返——payload 内 >2^53 的整数(雪花 id 量级)在 user_data
	// 之外的兄弟字段上逐字保持(对抗审计 MEDIUM:float64 往返静默失真)。
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, false, fmt.Errorf("%w: sanitize: payload not parseable: %v", ErrInvalidEvent, err)
	}
	masked := false
	root = walk(root, &masked)
	if !masked {
		return raw, false, nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		// any 树由 json.Unmarshal 产出,Marshal 只会因通道/循环引用失败——
		// 两者不可达,保响亮不吞。
		return nil, false, fmt.Errorf("%w: sanitize: remarshal: %v", ErrInvalidEvent, err)
	}
	return out, true, nil
}

// walk 遍历:遇 user_data 键即掩蔽其值子树,否则继续下潜;返回(可能
// 原样的)子树。对象键序由 Go map 语义决定(无序),语义无关(JSON 列由
// MySQL 规范化存储)。
func walk(v any, masked *bool) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == userDataKey {
				t[k] = maskSubtree(val, masked)
				continue
			}
			t[k] = walk(val, masked)
		}
	case []any:
		for i := range t {
			t[i] = walk(t[i], masked)
		}
	}
	return v
}

// maskSubtree 掩蔽标注子树:对象/数组递归到叶子;字符串/数字/布尔替换为
// maskToken;null 原样(非 PII,保留「键存在但无值」事实)。
func maskSubtree(v any, masked *bool) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = maskSubtree(val, masked)
		}
		return t
	case []any:
		for i := range t {
			t[i] = maskSubtree(t[i], masked)
		}
		return t
	case string, float64, bool, json.Number:
		*masked = true
		return maskToken
	default: // nil(以及任何解码产物之外的形态)
		return v
	}
}

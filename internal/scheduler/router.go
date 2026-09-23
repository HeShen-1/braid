package scheduler

import (
	"encoding/json"
	"fmt"
	"strings"

	"braid/internal/engine/dsl"
)

// Router 是分支路由缝(T3.4 的唯一替换点):对 branch 节点求值路由目标。
// resolve 把纯引用文本解析为当前值(未命中 ok=false)。调度逻辑(跳过传播、
// 取路推进)不在缝内——路由只换实现,不换调度。
type Router func(b *dsl.BranchNode, resolve func(ref string) (any, bool)) (string, error)

// CondRouter 是完整条件求值路由(T3.4,替换 T2.4 桩):按 cases 声明序对 when
// 谓词完整求值,首个真 → 该 case.next,全假 → default。求值错(引用未命中
// [exists 除外]、null 参与比较、类型漂移、数值不可解析)由调度器收口为执行级
// branch_eval 失败(fail-fast,D-2026-09-22-t24-scheduler-semantics);首个命中
// 后短路,谓词错即中止不续扫。
//
// 操作数口径:运行值经 UseNumber 解码,数值恒 json.Number——比较统一走
// compareNumbers(int64 精确优先,超界/小数退化 float64,故 1 == 1.0);字符串
// 比较为 Go 字节序;相等判断 equalValues 递归深比较(容器内数值同样归一)。
// exists 语义 = 引用可解析(值 null 亦算存在):探测剪枝臂产出的唯一谓词,
// 未命中是 false 而非错误。
func CondRouter(b *dsl.BranchNode, resolve func(ref string) (any, bool)) (string, error) {
	for i := range b.Cases {
		take, err := evalCondition(i, &b.Cases[i].When, resolve)
		if err != nil {
			return "", err
		}
		if take {
			return b.Cases[i].Next, nil
		}
	}
	return b.Default, nil
}

// evalCondition 对单个 when 谓词求值;idx 仅用于错误定位。
func evalCondition(idx int, c *dsl.Condition, resolve func(ref string) (any, bool)) (bool, error) {
	if c.Op == "exists" {
		_, ok := resolve(c.Left)
		return ok, nil
	}
	where := fmt.Sprintf("cond router: cases[%d]", idx)
	lv, ok := resolve(c.Left)
	if !ok {
		return false, fmt.Errorf("%s.left %s unresolved", where, c.Left)
	}
	if lv == nil {
		return false, fmt.Errorf("%s.left %s is null; op %s requires non-null", where, c.Left, c.Op)
	}
	rv, err := rightValue(idx, c, resolve)
	if err != nil {
		return false, err
	}
	if rv == nil {
		return false, fmt.Errorf("%s.right is null", where)
	}
	switch c.Op {
	case "eq", "ne":
		eq, err := equalValues(lv, rv)
		if err != nil {
			return false, fmt.Errorf("%s %s: %v", where, c.Op, err)
		}
		if c.Op == "ne" {
			return !eq, nil
		}
		return eq, nil
	case "gt", "gte", "lt", "lte":
		cmp, err := compareOrdered(lv, rv)
		if err != nil {
			return false, fmt.Errorf("%s %s: %v", where, c.Op, err)
		}
		switch c.Op {
		case "gt":
			return cmp > 0, nil
		case "gte":
			return cmp >= 0, nil
		case "lt":
			return cmp < 0, nil
		default:
			return cmp <= 0, nil
		}
	case "contains":
		switch xs := lv.(type) {
		case string:
			ys, ok := rv.(string)
			if !ok {
				return false, fmt.Errorf("%s contains: %v", where, errKind(lv, rv))
			}
			return strings.Contains(xs, ys), nil
		case []any:
			// 右值类型不受限(T1.2:array×any),元素类型不合 = 非成员,不是错误。
			for _, el := range xs {
				if eq, err := equalValues(el, rv); err == nil && eq {
					return true, nil
				}
			}
			return false, nil
		default:
			return false, fmt.Errorf("%s contains: %v", where, errKind(lv, rv))
		}
	}
	return false, fmt.Errorf("%s: unknown op %q", where, c.Op)
}

// rightValue 解析 when.right:字面量(UseNumber 保真)或纯引用字符串(→引用
// 比较,镜像 T1.2 静态口径:纯引用形态的字符串即引用)。exists 不消费右值。
func rightValue(idx int, c *dsl.Condition, resolve func(ref string) (any, bool)) (any, error) {
	if len(c.Right) == 0 {
		return nil, fmt.Errorf("cond router: cases[%d] op %s requires a right operand", idx, c.Op)
	}
	var v any
	if err := unmarshalUseNumber(c.Right, &v); err != nil {
		return nil, fmt.Errorf("cond router: cases[%d].right undecodable: %v", idx, err)
	}
	if s, ok := v.(string); ok {
		if _, isRef := dsl.ParsePureRef(s); isRef {
			rv, ok := resolve(s)
			if !ok {
				return nil, fmt.Errorf("cond router: cases[%d].right %s unresolved", idx, s)
			}
			return rv, nil
		}
	}
	return v, nil
}

// compareOrdered 有序比较:number×number(数值归一)或 string×string(字节序)。
func compareOrdered(lv, rv any) (int, error) {
	if xs, ok := lv.(string); ok {
		ys, ok := rv.(string)
		if !ok {
			return 0, errKind(lv, rv)
		}
		return strings.Compare(xs, ys), nil
	}
	xn, ok := lv.(json.Number)
	if !ok {
		return 0, errKind(lv, rv)
	}
	yn, ok := rv.(json.Number)
	if !ok {
		return 0, errKind(lv, rv)
	}
	return compareNumbers(xn, yn)
}

// compareNumbers 数值比较口径:int64 可整型化两边 → 精确比较(|v| < 2^63 零
// 失真);否则 float64(超出其精度按最近似;不可解析 = 类型错误)。
func compareNumbers(a, b json.Number) (int, error) {
	ai, aerr := a.Int64()
	bi, berr := b.Int64()
	if aerr == nil && berr == nil {
		switch {
		case ai < bi:
			return -1, nil
		case ai > bi:
			return 1, nil
		default:
			return 0, nil
		}
	}
	af, aerr := a.Float64()
	bf, berr := b.Float64()
	if aerr != nil || berr != nil {
		return 0, fmt.Errorf("invalid number %s vs %s", a.String(), b.String())
	}
	switch {
	case af < bf:
		return -1, nil
	case af > bf:
		return 1, nil
	default:
		return 0, nil
	}
}

// equalValues JSON 值深相等:数值经 compareNumbers 归一(1 == 1.0);容器递归
// 比较长度与逐元素/逐键(内层 null 参与相等判断——顶层 null 由谓词层拦截)。
func equalValues(a, b any) (bool, error) {
	switch x := a.(type) {
	case nil:
		return b == nil, nil
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false, errKind(a, b)
		}
		cmp, err := compareNumbers(x, y)
		return cmp == 0, err
	case string:
		y, ok := b.(string)
		if !ok {
			return false, errKind(a, b)
		}
		return x == y, nil
	case bool:
		y, ok := b.(bool)
		if !ok {
			return false, errKind(a, b)
		}
		return x == y, nil
	case []any:
		y, ok := b.([]any)
		if !ok {
			return false, errKind(a, b)
		}
		if len(x) != len(y) {
			return false, nil
		}
		for i := range x {
			eq, err := equalValues(x[i], y[i])
			if err != nil || !eq {
				return false, err
			}
		}
		return true, nil
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			return false, errKind(a, b)
		}
		if len(x) != len(y) {
			return false, nil
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok {
				return false, nil
			}
			eq, err := equalValues(xv, yv)
			if err != nil || !eq {
				return false, err
			}
		}
		return true, nil
	}
	return false, fmt.Errorf("unsupported value type %T", a)
}

// errKind 类型漂移错误;kindName 词表对齐 T1.2 jsonTypeName。
func errKind(a, b any) error {
	return fmt.Errorf("%s vs %s", kindName(a), kindName(b))
}

func kindName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

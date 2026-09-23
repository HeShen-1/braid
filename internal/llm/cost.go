package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"braid/internal/engine"
)

// Price 是单模型单价:每百万 token 的十进制金额(字符串承载,解析即整数
// 定点,零浮点——DECIMAL(14,6) 台账不容二进制舍入)+ ISO-4217 币种。
type Price struct {
	InputPerMTok  string `json:"input_per_mtok"`
	OutputPerMTok string `json:"output_per_mtok"`
	Currency      string `json:"currency"`
}

// fallbackKey 是单价表保留键:未知模型的估算单价(T3.1 estimated=true 路径;
// 模型名与此字面量同名的不可表达,词表足够)。
const fallbackKey = "fallback"

// priceRe:非负、≤9 位整数 + ≤6 位小数(micro 换算后恒在 int64 域内)。
var priceRe = regexp.MustCompile(`^\d{1,9}(\.\d{1,6})?$`)

// currencyRe:ISO-4217 形状(3 大写字母;完整代码表校验归 T7.1 配置体系)。
var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

// modelPrice 是解析后的整数定点单价(micro 货币单位 / 百万 token)。
type modelPrice struct {
	input, output int64
	currency      string
}

// PriceTable 是供应商单价表(JSON-in-env 配置,拍板 ③;T7.1 配置体系迁居)。
type PriceTable struct {
	models   map[string]modelPrice
	fallback *modelPrice
}

// ParsePriceTable 解析 JSON 单价表('{"<model>":{"input_per_mtok":"2",
// "output_per_mtok":"8","currency":"CNY"},...,"fallback":{...}}');
// 形状/域违规响亮报错(启动即败,不进运行期)。
func ParsePriceTable(raw string) (PriceTable, error) {
	if strings.TrimSpace(raw) == "" {
		return PriceTable{}, errors.New("llm: empty price table")
	}
	var m map[string]Price
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return PriceTable{}, fmt.Errorf("llm: price table: %w", err)
	}
	t := PriceTable{models: make(map[string]modelPrice, len(m))}
	for k, p := range m {
		mp, err := parsePrice(p)
		if err != nil {
			return PriceTable{}, fmt.Errorf("llm: price table entry %q: %w", k, err)
		}
		if k == fallbackKey {
			t.fallback = &mp
			continue
		}
		if k == "" {
			return PriceTable{}, errors.New("llm: price table: empty model key")
		}
		t.models[k] = mp
	}
	return t, nil
}

func parsePrice(p Price) (modelPrice, error) {
	var mp modelPrice
	var err error
	if mp.input, err = parseMicro(p.InputPerMTok); err != nil {
		return mp, fmt.Errorf("input_per_mtok: %w", err)
	}
	if mp.output, err = parseMicro(p.OutputPerMTok); err != nil {
		return mp, fmt.Errorf("output_per_mtok: %w", err)
	}
	if !currencyRe.MatchString(p.Currency) {
		return mp, fmt.Errorf("currency %q not ISO-4217 (3 uppercase letters)", p.Currency)
	}
	mp.currency = p.Currency
	return mp, nil
}

// parseMicro 把十进制单价解析为 micro 单位整数值(精确,无浮点参与)。
func parseMicro(s string) (int64, error) {
	if !priceRe.MatchString(s) {
		return 0, fmt.Errorf("%q not a non-negative decimal (≤9 int + ≤6 frac digits)", s)
	}
	intPart, fracPart, _ := strings.Cut(s, ".")
	fracPart += "000000" // 右侧补零到 6 位(priceRe 已保证 ≤6 位)
	v, err := strconv.ParseInt(intPart+fracPart[:6], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", s, err)
	}
	return v, nil
}

// Len 返回已登记(非 fallback)模型数(配置校验用)。
func (t PriceTable) Len() int { return len(t.models) }

// maxAmountMicro = 99999999.999999(DECIMAL(14,6) 域)的 micro 表示。
const maxAmountMicro = int64(99_999_999_999_999)

// ErrCostOverflow:计算结果超出 DECIMAL(14,6) 域(单 attempt 成本 ≥ 1 亿),
// 响亮拒绝而非截断入账。
var ErrCostOverflow = errors.New("llm: cost exceeds DECIMAL(14,6)")

// Cost 是一次调用的计费结果(台账写入参数的算术前身)。
type Cost struct {
	AmountMicro int64 // micro 货币单位(10^-6);Billed=false 时无意义
	Currency    string
	Estimated   bool // true = 非精确(未知模型估算 fallback)
	Billed      bool // false = 不计费(台账写 NULL,与免费模型 0 严格区分)
}

// Cost 计费:known model 单价精确(Billed, Estimated=false);未知模型用
// fallback 估算(Billed, Estimated=true);两者皆无 → 不计费(Billed=false,
// Estimated=true)。整数定点(big.Int 承载 token×单价乘积防溢出),唯一舍入点
// = 最终除 1e6 四舍五入(half-up)。
func (t PriceTable) Cost(model string, u Usage) (Cost, error) {
	mp, known := t.models[model]
	if !known {
		if t.fallback == nil {
			return Cost{Estimated: true}, nil
		}
		mp = *t.fallback
	}
	num := new(big.Int).Mul(big.NewInt(int64(u.PromptTokens)), big.NewInt(mp.input))
	num.Add(num, new(big.Int).Mul(big.NewInt(int64(u.CompletionTokens)), big.NewInt(mp.output)))
	q, r := new(big.Int).QuoRem(num, big.NewInt(1_000_000), new(big.Int))
	if r.Mul(r, big.NewInt(2)).Cmp(big.NewInt(1_000_000)) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() || q.Int64() > maxAmountMicro {
		return Cost{}, fmt.Errorf("%w: model %s usage %d/%d",
			ErrCostOverflow, model, u.PromptTokens, u.CompletionTokens)
	}
	return Cost{AmountMicro: q.Int64(), Currency: mp.currency, Estimated: !known, Billed: true}, nil
}

// AmountString 返回 DECIMAL 文本形("0.003200";6 位小数定点,由 micro 精确
// 直出)。
func (c Cost) AmountString() string {
	return fmt.Sprintf("%d.%06d", c.AmountMicro/1_000_000, c.AmountMicro%1_000_000)
}

// AttemptUsage 把计费结果映射为 engine 台账写入参数(双录两半边的生产桥:
// T3.6 执行器在 worker.Outcome.Usage 携带;engine 是列域单点,本函数是
// 算术单点)。
func (c Cost) AttemptUsage(model string, u Usage) engine.AttemptUsage {
	au := engine.AttemptUsage{
		Model:            model,
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		Estimated:        c.Estimated,
	}
	if c.Billed {
		au.Billed = true
		au.CostAmount = c.AmountString()
		au.CostCurrency = c.Currency
	}
	return au
}

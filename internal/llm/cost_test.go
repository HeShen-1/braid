package llm

// 单测:单价表解析、整数定点计费、舍入与域拒绝、engine 台账映射。零 DB
// 零网络(t13 不可达也跑)。
//
// 变异自查(实现错 → 哪条红):
//   - parseMicro 用浮点 → rounding 表内 0.000001 级用例红;
//   - 舍入点移位/重复舍入 → half-up 两例红;
//   - 溢出无守卫 → overflow 红;
//   - fallback/known 分支倒置 → known/unknown 两例的 Estimated 红。

import (
	"errors"
	"strings"
	"testing"

	"braid/internal/engine"
)

const testTable = `{
  "glm-5.3":    {"input_per_mtok": "2",       "output_per_mtok": "8", "currency": "CNY"},
  "free-model": {"input_per_mtok": "0",       "output_per_mtok": "0", "currency": "USD"},
  "frac":       {"input_per_mtok": "0.000001","output_per_mtok": "0", "currency": "USD"},
  "fallback":   {"input_per_mtok": "1",       "output_per_mtok": "1", "currency": "USD"}
}`

func TestParsePriceTable(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string // 空串 = 应成功
	}{
		{"valid", testTable, ""},
		{"fallback only", `{"fallback":{"input_per_mtok":"1","output_per_mtok":"1","currency":"USD"}}`, ""},
		{"empty", "", "empty price table"},
		{"blank", "   ", "empty price table"},
		{"bad json", "{", "price table"},
		{"negative price", `{"m":{"input_per_mtok":"-1","output_per_mtok":"1","currency":"USD"}}`, "entry"},
		{"seven decimals", `{"m":{"input_per_mtok":"1.1234567","output_per_mtok":"1","currency":"USD"}}`, "entry"},
		{"nine int digits ok", `{"m":{"input_per_mtok":"123456789.5","output_per_mtok":"1","currency":"USD"}}`, ""},
		{"lowercase currency", `{"m":{"input_per_mtok":"1","output_per_mtok":"1","currency":"usd"}}`, "entry"},
		{"missing currency", `{"m":{"input_per_mtok":"1","output_per_mtok":"1"}}`, "entry"},
		{"empty model key", `{"":{"input_per_mtok":"1","output_per_mtok":"1","currency":"USD"}}`, "empty model key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePriceTable(tc.raw)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ParsePriceTable() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ParsePriceTable() = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestCost(t *testing.T) {
	table, err := ParsePriceTable(testTable)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	noFallbackRaw := `{"glm-5.3":{"input_per_mtok":"2","output_per_mtok":"8","currency":"CNY"}}`
	noFallback, err := ParsePriceTable(noFallbackRaw)
	if err != nil {
		t.Fatalf("parse no-fallback: %v", err)
	}

	cases := []struct {
		name  string
		table PriceTable
		model string
		usage Usage
		want  Cost
	}{
		// (1000×2 + 150×8) / 1e6 = 3200 micro = 0.003200 CNY(精确)
		{"known exact", table, "glm-5.3", Usage{1000, 150, 1150}, Cost{3200, "CNY", false, true}},
		// 半舍入:500000×1/1e6 = 0.5 → 1;half-up 锚
		{"round half-up", table, "frac", Usage{500000, 0, 500000}, Cost{1, "USD", false, true}},
		// 半舍以下:0.499999 → 0
		{"round down", table, "frac", Usage{499999, 0, 499999}, Cost{0, "USD", false, true}},
		// 免费模型:0 落账,与 NULL 语义分立在 engine 层断言
		{"free model billed zero", table, "free-model", Usage{10, 10, 20}, Cost{0, "USD", false, true}},
		// 未知模型走 fallback 估算:1000×1/1e6 = 1000 micro,Estimated=true
		{"unknown via fallback", table, "mystery", Usage{1000, 0, 1000}, Cost{1000, "USD", true, true}},
		// 无 fallback 表的未知模型:不计费(NULL),Estimated=true
		{"unknown no fallback", noFallback, "mystery", Usage{1000, 0, 1000}, Cost{0, "", true, false}},
		// 零用量 known 模型:0.000000 落账
		{"zero usage", table, "glm-5.3", Usage{}, Cost{0, "CNY", false, true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.table.Cost(tc.model, tc.usage)
			if err != nil {
				t.Fatalf("Cost() = %v", err)
			}
			if got != tc.want {
				t.Fatalf("Cost() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCostOverflowRejected(t *testing.T) {
	// 999999999 micro/mtok × 4.29e9 tokens ≫ DECIMAL(14,6) 域:big.Int 承载
	// 计算不溢出,域守卫响亮拒绝。
	table, err := ParsePriceTable(`{"huge":{"input_per_mtok":"999999999","output_per_mtok":"0","currency":"USD"}}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = table.Cost("huge", Usage{PromptTokens: 4294967295})
	if !errors.Is(err, ErrCostOverflow) {
		t.Fatalf("Cost() = %v, want ErrCostOverflow", err)
	}
}

func TestAmountString(t *testing.T) {
	cases := []struct {
		micro int64
		want  string
	}{
		{3200, "0.003200"},
		{0, "0.000000"},
		{1_500_000, "1.500000"},
		{99_999_999_999_999, "99999999.999999"}, // DECIMAL(14,6) 域顶
	}
	for _, tc := range cases {
		if got := (Cost{AmountMicro: tc.micro}).AmountString(); got != tc.want {
			t.Errorf("AmountString(%d) = %q, want %q", tc.micro, got, tc.want)
		}
	}
}

func TestCostAttemptUsage(t *testing.T) {
	table, err := ParsePriceTable(testTable)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// billed:金额/币种字符串就位,Estimated=false
	c, err := table.Cost("glm-5.3", Usage{1000, 150, 1150})
	if err != nil {
		t.Fatalf("cost: %v", err)
	}
	au := c.AttemptUsage("glm-5.3", Usage{1000, 150, 1150})
	want := engine.AttemptUsage{
		Model: "glm-5.3", PromptTokens: 1000, CompletionTokens: 150, TotalTokens: 1150,
		CostAmount: "0.003200", CostCurrency: "CNY", Billed: true,
	}
	if au != want {
		t.Errorf("billed AttemptUsage = %+v, want %+v", au, want)
	}
	if err := au.Validate(); err != nil {
		t.Errorf("billed Validate() = %v, want nil(engine 域校验必须通过)", err)
	}
	// unbilled:cost 字段为空(Billed=false → NULL),Estimated=true
	nc, err := table.Cost("mystery", Usage{10, 0, 10}) // fallback 表内 → billed estimated
	if err != nil {
		t.Fatalf("cost fallback: %v", err)
	}
	nau := nc.AttemptUsage("mystery", Usage{10, 0, 10})
	if !nau.Estimated || !nau.Billed || nau.CostAmount != "0.000010" {
		t.Errorf("fallback AttemptUsage = %+v, want billed estimated 0.000010", nau)
	}
	ub := Cost{Estimated: true}.AttemptUsage("mystery", Usage{10, 0, 10})
	if ub.Billed || ub.CostAmount != "" || ub.CostCurrency != "" || !ub.Estimated {
		t.Errorf("unbilled AttemptUsage = %+v, want empty cost fields + estimated", ub)
	}
	if err := ub.Validate(); err != nil {
		t.Errorf("unbilled Validate() = %v, want nil", err)
	}
}

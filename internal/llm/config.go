package llm

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"time"

	"braid/internal/scheduler"
)

// llm 配置面(relay/config.go 同款四件套:Config + DefaultConfig + Validate +
// ConfigFromEnv;字段名即未来配置键,T7.1 已换装收编)。密钥先例
// 对齐 auth:BRAID_LLM_API_KEY 仅经 env 注入,无默认,缺省即 Validate 失败;
// 不进代码/日志/审计(铁律)。
type Config struct {
	// BaseURL 是 OpenAI 兼容端点根(含版本路径,如 https://api.openai.com/v1);
	// 适配器在其后拼 /chat/completions。P0 单供应商单端点,多供应商注册表非
	// 本任务面。http:// 被有意放行(P0 私有化内网端点是合法形态:本地
	// vLLM/Ollama、TLS 由反代终结;此值由部署管理员配置,属可信边界内——
	// 与 T3.3 对不可信用户配置 host 的「仅 https + SSRF 三层修」规则刻意
	// 分立,review advisory #4 的回应)。Bearer 凭证经明文 http 传输的风险
	// 由部署侧网络分段承担,T7.3 安装自检可再收紧。
	BaseURL string
	// APIKey Bearer 凭证,仅经 BRAID_LLM_API_KEY 注入。
	APIKey string
	// Timeout 单次 HTTP 调用预算(每次重试各得一整个新预算,非全程共享)。
	Timeout time.Duration
	// MaxAttempts 单 attempt 内总尝试上限(含首次;节点级重试权威在平台
	// 调度层,与此零叠加)。
	MaxAttempts int
	// BackoffBase/BackoffFactor/BackoffMax 指数退避三参(relay 同式:
	// base*factor^(n-1) 封顶 max);Jitter 固定 ±0.2 不暴露 env(非运维旋钮)。
	BackoffBase   time.Duration
	BackoffFactor float64
	BackoffMax    time.Duration
	// PriceTable 供应商单价表(JSON-in-env,拍板 ③);至少一个模型,可选
	// "fallback" 保留键做未知模型估算。
	PriceTable PriceTable
	// TokenBudget 每执行总 token 预算(T18.2,D-t182 决策 1):Σ已计费
	// usage + 当次估算超限即 pre-call 拒绝;0 = 闸门关闭(缺省)。
	TokenBudget TokenBudget
}

// dispatchMargin 是组合预算对 NodeTimeout 的扣减余量:asynq 每次投递的
// handler ctx 默认 30min 超时自 **dequeue** 起算,而 attempt 的 reaper
// deadline 自 **claim** 起算——dequeue 恒早于 claim(relay 轮询 + asynq
// forwarder 轮询,常态秒级,实测重试时延下界由 forwarder ~5s 主导),预算
// 恰吃满 NodeTimeout 的合法配置会被 asynq ctx 先掐 mid-call(执行器把取消
// 归基础设施失败留 RUNNING,供应商在途调用可能已完成计费但 usage 丢失)。
// 5min 保守覆盖常态派发延迟(T3.6 对抗审计 HIGH①)。
const dispatchMargin = 5 * time.Minute

// DefaultConfig 返回生产默认值(BaseURL/APIKey/单价表故意留空:必须显式供给)。
func DefaultConfig() Config {
	return Config{
		Timeout:       120 * time.Second,
		MaxAttempts:   3,
		BackoffBase:   time.Second,
		BackoffFactor: 2,
		BackoffMax:    30 * time.Second,
	}
}

// Validate 报告配置矛盾(启动即败,不进运行期)。上界对着真实约束:Timeout
// ≤10m(LLM 长响应上限量级,超过属配置错);MaxAttempts ≤10(每次尝试占住
// worker 槽位,退避序列 1s×2^n 已到分钟级);NaN/±Inf 显式拒绝(relay 同规)。
func (c Config) Validate() error {
	if c.BaseURL == "" {
		return fmt.Errorf("llm: base url empty")
	}
	if u, err := url.Parse(c.BaseURL); err != nil || u.Host == "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("llm: base url %q not absolute http(s)", c.BaseURL)
	}
	switch {
	case c.APIKey == "":
		return fmt.Errorf("llm: api key empty")
	case c.Timeout <= 0 || c.Timeout > 10*time.Minute:
		return fmt.Errorf("llm: timeout %s out of (0,10m]", c.Timeout)
	case c.MaxAttempts < 1 || c.MaxAttempts > 10:
		return fmt.Errorf("llm: max attempts %d out of [1,10]", c.MaxAttempts)
	case c.BackoffBase <= 0:
		return fmt.Errorf("llm: backoff base %s <= 0", c.BackoffBase)
	case math.IsNaN(c.BackoffFactor) || math.IsInf(c.BackoffFactor, 0) || c.BackoffFactor < 1:
		return fmt.Errorf("llm: backoff factor %v not finite >= 1", c.BackoffFactor)
	case c.BackoffMax < c.BackoffBase:
		return fmt.Errorf("llm: backoff max %s < base %s", c.BackoffMax, c.BackoffBase)
	case c.attemptBudget() > scheduler.NodeTimeout("llm")-dispatchMargin:
		return fmt.Errorf(
			"llm: worst-case attempt budget %s exceeds llm node budget %s (deadline %s - %s dispatch margin; asynq per-delivery ctx and reaper both cut at ~30min from dequeue/claim): tighten Timeout/MaxAttempts/backoff",
			c.attemptBudget(), scheduler.NodeTimeout("llm")-dispatchMargin, scheduler.NodeTimeout("llm"), dispatchMargin)
	case c.PriceTable.Len() == 0:
		return fmt.Errorf("llm: price table empty (>=1 model required; \"fallback\" key optional)")
	case c.TokenBudget > MaxTokenBudget:
		return fmt.Errorf("llm: token budget %d over upper bound %d (misconfig guard; 0 = gate off)",
			uint64(c.TokenBudget), uint64(MaxTokenBudget))
	}
	return nil
}

// attemptBudget 是单 attempt 内的最坏停留上界(HIGH① 拍板:组合上界在
// Validate 启动即败,deadline 词表单源锚 scheduler.NodeTimeout("llm") 减
// dispatchMargin——asynq 投递窗口恒先于 reaper 窗收紧,见 dispatchMargin):
// 全部尝试超时 + 精确退避序列。退避项与适配器 backoff 同式
// (base×factor^(i-1) 封顶 BackoffMax)再加 ±jitter 抖动上界,逐项 ceil
// 保守化——预算恒 ≥ 实际最坏等待,宁可误拒临界配置不放过超窗。
func (c Config) attemptBudget() time.Duration {
	total := time.Duration(c.MaxAttempts) * c.Timeout
	wait := 0.0
	for i := 1; i < c.MaxAttempts; i++ {
		w := float64(c.BackoffBase) * math.Pow(c.BackoffFactor, float64(i-1))
		if w > float64(c.BackoffMax) {
			w = float64(c.BackoffMax)
		}
		wait += math.Ceil(w * (1 + jitter))
	}
	return total + time.Duration(wait)
}

// ConfigFromEnv 从环境变量构造配置(BRAID_LLM_ 前缀;未设键取默认,
// BaseURL/API_KEY/PRICE_TABLE 无默认)。getenv 注入 os.Getenv,测试换桩;
// 构造末尾过 Validate。
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := DefaultConfig()
	c.BaseURL = getenv("BRAID_LLM_BASE_URL")
	c.APIKey = getenv("BRAID_LLM_API_KEY")
	var err error
	if c.Timeout, err = envDuration(getenv, "BRAID_LLM_TIMEOUT", c.Timeout); err != nil {
		return c, err
	}
	if c.MaxAttempts, err = envInt(getenv, "BRAID_LLM_MAX_ATTEMPTS", c.MaxAttempts); err != nil {
		return c, err
	}
	if c.BackoffBase, err = envDuration(getenv, "BRAID_LLM_BACKOFF_BASE", c.BackoffBase); err != nil {
		return c, err
	}
	if c.BackoffFactor, err = envFloat(getenv, "BRAID_LLM_BACKOFF_FACTOR", c.BackoffFactor); err != nil {
		return c, err
	}
	if c.BackoffMax, err = envDuration(getenv, "BRAID_LLM_BACKOFF_MAX", c.BackoffMax); err != nil {
		return c, err
	}
	if raw := getenv("BRAID_LLM_PRICE_TABLE"); raw != "" {
		if c.PriceTable, err = ParsePriceTable(raw); err != nil {
			return c, err
		}
	}
	if c.TokenBudget, err = envTokenBudget(getenv, "BRAID_LLM_TOKEN_BUDGET"); err != nil {
		return c, err
	}
	return c, c.Validate()
}

// envTokenBudget 解析 BRAID_LLM_TOKEN_BUDGET(非负整数,0/未设 = 闸门关);
// 上界校验归 Validate(与 Timeout/MaxAttempts 的分工一致:这里只管词法)。
func envTokenBudget(getenv func(string) string, key string) (TokenBudget, error) {
	raw := getenv(key)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("llm: %s=%q: non-negative integer required: %w", key, raw, err)
	}
	return TokenBudget(n), nil
}

func envDuration(getenv func(string) string, key string, def time.Duration) (time.Duration, error) {
	raw := getenv(key)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return def, fmt.Errorf("llm: %s=%q: %w", key, raw, err)
	}
	return d, nil
}

func envInt(getenv func(string) string, key string, def int) (int, error) {
	raw := getenv(key)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def, fmt.Errorf("llm: %s=%q: %w", key, raw, err)
	}
	return n, nil
}

func envFloat(getenv func(string) string, key string, def float64) (float64, error) {
	raw := getenv(key)
	if raw == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return def, fmt.Errorf("llm: %s=%q: %w", key, raw, err)
	}
	return f, nil
}

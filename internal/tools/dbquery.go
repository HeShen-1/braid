package tools

// db_query 内置工具(T10.4,issue #100)。对运维钉死的只读 MySQL 账号
// (连接串 env 注入)执行**参数化 SELECT**;三层只读纵深:
//
//	客户端参数化门(本文件,注入面结构拒)
//	  + 会话级 transaction_read_only=1(DSN 强制参数,连接建立即 SET)
//	  + 账号 GRANT SELECT-only(部署纪律,SQL 见 deploy/README.md §10)
//
// 载体(T10.1 装配缝):off = 进程内直连;docker = 同名换装 sandboxCarrier,
// MySQL 连接从容器内经 egress 代理 CONNECT 明文隧道(T10.2 拓扑不变:
// 容器唯一可达面仍是代理;私网豁免语义 = D-2026-09-30-t104-dbquery-egress)。
// TLS 是否启用留运营方 DSN(tls= 参数原样保留)——代理与 worker 同进程
// 同信任域,加密是运营方对链路的裁量,不是本工具的契约。
//
// 输入契约(DSL 声明位):
//
//	sql   必填,单条 SELECT/WITH 语句,占位符 ?
//	args  可选,标量数组(string/number/boolean/null;嵌套结构响亮拒)
//
// 参数化强制(非参数化拒)的执法线:字符串字面量与注释是一切 SQL 注入的
// 载体,门对其结构拒('、"、--、/*、#、多语句、顶层 DML/管理动词(含
// MySQL 8 的 WITH 前缀 CTE-DML;fail-closed:反引号标识符含门字符或动词
// 词按拒收,宁可误拒);数值字面量放行——数字词素不可能改变语句结构,
// 且 LIMIT 在 MySQL 预处理语法里常以字面量书写。占位符计数不在客户端数
// (反引号内的 ? 会数错),服务端 PREPARE 自然拒。参数化本身由驱动结构
// 性保证:interpolateParams=false(强制)+ 服务端预处理语句。
//
// 结果集上限(GATE 1 拍板):**超限报错,绝不静默截断**——行上限读到
// max+1 行即 tool_failed(错误指明上限与 LIMIT 建议),输出字节上限在
// marshal 后校验(防单行巨值绕过行上限);与 http 工具 body 超限同判,
// LLM 拿到的要么完整结果要么明确失败,没有「看似完整的半份数据」。
//
// 超时:整体耗时由 ctx(attempt deadline)强制约束,本层不另设超时
// (http 工具同款硬契约;沙箱模式信封 DeadlineMs 已链通)。
//
// 凭证红线:DSN 恒 env 键 BRAID_DBQUERY_DSN 注入(签名工具包只声明不携
// 带——签名工件零凭证),错误消息零回显 DSN;审计面只见 sql+args。
// 幂等键:MySQL 协议无传递面(连接不复用、语句只读天然幂等,fileread
// 同判)。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

const (
	// DefaultMaxDBQueryRows 结果集行上限缺省(GATE 1:超限报错不截断;
	// 防御界而非调优旋钮,不设 env 键——更大数据量属分页/LIMIT 语义)。
	DefaultMaxDBQueryRows = 1000
	// DefaultMaxDBQueryOutputBytes 序列化输出字节上限(与 http 工具
	// DefaultMaxHTTPBody 同量级):**扫描期累积执法**——逐行按产出编码
	// 口径计长,超限即拒,多行累积峰值钉在 ~本值(orch-review MEDIUM
	// 收口);单行峰值另受界:driver 在 Scan 返回前整行驻留,上界 =
	// 服务端 max_allowed_packet(运维建议 ≈ 本值,README §10)——
	// 峰值 ≈ max(本值, 最大单行)。marshal 后的事后校验保留为兜底门。
	DefaultMaxDBQueryOutputBytes = 8 << 20 // 8 MiB
	// maxDBQuerySQLBytes sql 输入长度上限(http path 同量级的收窄;
	// 超长语句对分析查询只会报错,提前响亮拒)。
	maxDBQuerySQLBytes = 8 << 10
	// maxDBQueryArgs 参数个数上限(服务端占位符硬界 65535 之内的
	// 信封预算界;IN (...) 大列表到千级已属应改 JOIN 的形态)。
	maxDBQueryArgs = 1024
	// dbqDefaultPort MySQL 缺省端口(与驱动 ensureHavePort 同判)。
	dbqDefaultPort = 3306
)

// dbqHeadRe 语句头词表:SELECT | WITH(CTE)——只读分析工具的封闭词表,
// EXPLAIN/SHOW 等其余动词不开放(fail-closed;需要时再扩词表)。
var dbqHeadRe = regexp.MustCompile(`(?i)^(select|with)\b`)

// dbqIntoFileRe SELECT ... INTO OUTFILE/DUMPFILE 拒收:SELECT 头的服务端
// FS 写副作用(FILE 权限是账号纪律的兜底层,客户端先拒)。
var dbqIntoFileRe = regexp.MustCompile(`(?i)\bINTO\s+(OUTFILE|DUMPFILE)\b`)

// dbqDMLFuncNamedRe / dbqDMLVerbRe 顶层 DML/管理动词拒收(orch-review
// MEDIUM 收口):MySQL 8 允许 WITH 前缀 DML(`WITH c AS (…) DELETE …`),
// 首词表单挡不住——动词黑名单补位。INSERT/REPLACE 与字符串函数同名
// (RE2 无前瞻):函数形态 `NAME(` 由 dbqContainsDML 逐命中放行,
// 语句形态(后随非括号)拒。反引号标识符含动词词按误拒(宁误拒不误放,
// 与 `a;b` 同判);下划线连接(last_update)无 \b 边界天然不误伤。
var dbqDMLFuncNamedRe = regexp.MustCompile(`(?i)\b(insert|replace)\b`)
var dbqDMLVerbRe = regexp.MustCompile(`(?i)\b(update|delete|merge|truncate|create|alter|drop|rename|call|load|grant|revoke|set|handler|lock|unlock|kill|shutdown)\b`)

// DBQueryConfig 是 db_query 工具的构造配置。DSN 由 env 注入(凭证红线);
// Proxy 非空 = 沙箱 helper 侧装配,MySQL 拨号经 egress 代理明文隧道
// (off 模式恒 nil = 直连)。MaxRows/MaxOutputBytes ≤0 取缺省(测试缝)。
type DBQueryConfig struct {
	DSN            string
	Proxy          *url.URL
	MaxRows        int
	MaxOutputBytes int
}

// DBQueryTool 是内置 db_query 工具(Tool 接口进程内载体;沙箱模式由
// sandboxCarrier 同名替换,容器内 helper 经本构造器重建同构实例)。
type DBQueryTool struct {
	cfg       *mysql.Config
	db        *sql.DB
	maxRows   int
	maxOutput int
}

// NewDBQueryTool 构造 db_query 工具。DSN 解析 + 安全参数强制在构造期
// (装配缺陷启动即现);连接惰性(api 进程构造零 DB 接触)。
func NewDBQueryTool(cfg DBQueryConfig) (*DBQueryTool, error) {
	dsn := strings.TrimSpace(cfg.DSN)
	if dsn == "" {
		return nil, fmt.Errorf("tools: db_query tool with empty DSN (BRAID_DBQUERY_DSN)")
	}
	mc, err := dbqDSNConfig(dsn)
	if err != nil {
		return nil, err
	}
	if cfg.Proxy != nil {
		mc.DialFunc = egressTunnelDialPlain(cfg.Proxy)
	}
	conn, err := mysql.NewConnector(mc)
	if err != nil {
		return nil, fmt.Errorf("tools: db_query: connector: %w", err)
	}
	maxRows := cfg.MaxRows
	if maxRows <= 0 {
		maxRows = DefaultMaxDBQueryRows
	}
	maxOut := cfg.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = DefaultMaxDBQueryOutputBytes
	}
	return &DBQueryTool{cfg: mc, db: sql.OpenDB(conn), maxRows: maxRows, maxOutput: maxOut}, nil
}

// Name 满足 Tool 接口(DSL tool 词表同域:^[a-z][a-z0-9_]{0,63}$)。
func (t *DBQueryTool) Name() string { return "db_query" }

// Run 执行一次参数化只读查询。错误语义(映射契约单点在 runner):本工具
// 无哨兵族——输入门拒/超限/SQL 语义错一律 tools: 前缀裸错误 → tool_failed
// 业务失败(错误信息 LLM 可读,可改写 sql 重试);ctx 取消链回原样上抛。
func (t *DBQueryTool) Run(ctx context.Context, call Call) (Result, error) {
	rawSQL, err := requiredString(call.Inputs, "sql")
	if err != nil {
		return Result{}, fmt.Errorf("tools: db_query: %w", err)
	}
	q, err := dbqSanitizeSQL(rawSQL)
	if err != nil {
		return Result{}, err
	}
	args, err := dbqNormalizeArgs(call.Inputs["args"])
	if err != nil {
		return Result{}, err
	}
	rows, err := t.db.QueryContext(ctx, q, args...)
	if err != nil {
		return Result{}, fmt.Errorf("tools: db_query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return Result{}, fmt.Errorf("tools: db_query: %w", err)
	}
	out := make([][]any, 0, 16)
	// 字节预算在扫描期累积执法(orch-review MEDIUM 收口):行数据按产出
	// 编码口径(同 SetEscapeHTML(false))逐行计长,超限即拒——多行累积
	// 峰值钉在 ~maxOutput;单行巨值(REPEAT 等无引号合法形态)不再多行
	// 叠加后才发现超限,但单行本体在 Scan 返回前已整行驻留(driver 语义,
	// 上界 = max_allowed_packet,见 DefaultMaxDBQueryOutputBytes 注释)。
	// dest/ptrs/encoder 循环外单建复用(orch-review LOW 收口);行独立经
	// 浅拷贝保(dbqJSONifyRow 已把 []byte 转为值语义 string)。
	budget := 0
	budgetBuf := &bytes.Buffer{}
	budgetEnc := json.NewEncoder(budgetBuf)
	budgetEnc.SetEscapeHTML(false)
	dest := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range dest {
		ptrs[i] = &dest[i]
	}
	for rows.Next() {
		// 行上限:第 max+1 行在场即报错(GATE 1 语义:不静默截断)。
		if len(out) == t.maxRows {
			return Result{}, fmt.Errorf("tools: db_query: result set exceeds %d rows (add LIMIT/OFFSET or narrow the query)", t.maxRows)
		}
		if err := rows.Scan(ptrs...); err != nil {
			return Result{}, fmt.Errorf("tools: db_query: scan row %d: %w", len(out), err)
		}
		row := append([]any(nil), dbqJSONifyRow(dest)...) // Encode 尾随 \n 计入(+1B/行,fail-closed)
		budgetBuf.Reset()
		if err := budgetEnc.Encode(row); err != nil {
			return Result{}, fmt.Errorf("tools: db_query: marshal row: %w", err)
		}
		budget += budgetBuf.Len()
		if budget > t.maxOutput {
			return Result{}, fmt.Errorf("tools: db_query: marshaled output exceeds %d bytes while scanning (narrow the query or select fewer columns)", t.maxOutput)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return Result{}, fmt.Errorf("tools: db_query: %w", err)
	}
	raw, err := marshalToolOutput(map[string]any{
		"columns":   cols,
		"rows":      out,
		"row_count": len(out),
	})
	if err != nil {
		return Result{}, fmt.Errorf("tools: db_query: %w", err)
	}
	if len(raw) > t.maxOutput {
		return Result{}, fmt.Errorf("tools: db_query: marshaled output exceeds %d bytes (narrow the query or select fewer columns)", t.maxOutput)
	}
	return Result{Output: raw}, nil
}

// dbqJSONifyRow 把一行 driver 值转成 JSON 友好形:[]byte → string(文本
// 契约;二进制列经 Marshal 的 U+FFFD 替换是有损降级,file_read 同族);
// time.Time(parseTime=true 时)→ RFC3339;数值原生(int64/float64 无
// 精度损失);nil 原样。
func dbqJSONifyRow(vals []any) []any {
	for i, v := range vals {
		switch x := v.(type) {
		case []byte:
			vals[i] = string(x)
		case time.Time:
			vals[i] = x.Format(time.RFC3339Nano)
		}
	}
	return vals
}

// dbqDSNConfig 解析运营方 DSN 并强制安全参数。强制项(运营方同义值被
// 覆盖——结构保证优先于配置):interpolateParams=false(真参数化:值只
// 经服务端预处理语句占位符入协议)、multiStatements=false(单语句)、
// Params["transaction_read_only"]=1(会话只读,纵深第二层)。net 仅 tcp
// (unix socket = FS 路径语义,沙箱隧道也无从承载)。
func dbqDSNConfig(raw string) (*mysql.Config, error) {
	cfg, err := mysql.ParseDSN(raw)
	if err != nil {
		// 凭证红线:ParseDSN 错误不回显 DSN,此处同样只透传错误本体。
		return nil, fmt.Errorf("tools: db_query: invalid DSN (value withheld): %w", err)
	}
	if cfg.Net != "tcp" {
		return nil, fmt.Errorf("tools: db_query: DSN net %q unsupported (want tcp)", cfg.Net)
	}
	cfg.InterpolateParams = false
	cfg.MultiStatements = false
	if cfg.Params == nil {
		cfg.Params = make(map[string]string, 1)
	}
	cfg.Params["transaction_read_only"] = "1"
	return cfg, nil
}

// dbqDSNHostPort 从驱动 Addr 抽取 host 与有效端口(egress 允许集派生与
// 隧道目标共用的归一口):无端口 = 3306(驱动 ensureHavePort 同判);
// IPv6 字面量剥方括号;空 host/越界端口响亮拒。「无端口」按错误身份判定
// (errors.As *net.AddrError,orch-review LOW 收口——字符串后缀比对会随
// stdlib 措辞漂移而脆断)。
func dbqDSNHostPort(addr string) (string, int, error) {
	host, portStr := addr, ""
	if h, p, err := net.SplitHostPort(addr); err == nil {
		host, portStr = h, p
	} else {
		var ae *net.AddrError
		if !errors.As(err, &ae) || ae.Err != "missing port in address" {
			return "", 0, fmt.Errorf("tools: db_query: DSN addr %q invalid: %w", addr, err)
		}
	}
	port := dbqDefaultPort
	if portStr != "" {
		v, err := strconv.Atoi(portStr)
		if err != nil || v < 1 || v > 65535 {
			return "", 0, fmt.Errorf("tools: db_query: DSN port %q invalid", portStr)
		}
		port = v
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return "", 0, fmt.Errorf("tools: db_query: DSN without host")
	}
	return host, port, nil
}

// dbqSanitizeSQL 参数化门(纯函数,进程内/容器内同源执法)。规则序:
// 长度 → 控制字节(放行 \t\n\r,拒其余 <0x20 与 0x7f)→ trim → 剥单个
// 尾分号 → 残余分号拒 → 门字符拒(' " -- /* #)→ INTO OUTFILE/DUMPFILE
// 拒 → 语句头词表(SELECT|WITH)。返回净化后语句(供 PREPARE)。
func dbqSanitizeSQL(raw string) (string, error) {
	if len(raw) > maxDBQuerySQLBytes {
		return "", fmt.Errorf("tools: db_query: sql exceeds %d bytes", maxDBQuerySQLBytes)
	}
	for i := 0; i < len(raw); i++ {
		b := raw[i]
		if b < 0x20 && b != '\t' && b != '\n' && b != '\r' {
			return "", fmt.Errorf("tools: db_query: sql contains control byte 0x%02x (non-parameterized form rejected)", b)
		}
		if b == 0x7f {
			return "", fmt.Errorf("tools: db_query: sql contains control byte 0x7f (non-parameterized form rejected)")
		}
	}
	s := strings.TrimSpace(raw)
	if strings.HasSuffix(s, ";") {
		s = strings.TrimSpace(s[:len(s)-1])
	}
	if s == "" {
		return "", fmt.Errorf("tools: db_query: sql empty")
	}
	if strings.Contains(s, ";") {
		return "", fmt.Errorf("tools: db_query: multi-statement sql rejected (single SELECT/WITH only)")
	}
	for _, forbidden := range []string{"'", `"`, "--", "/*", "#"} {
		if strings.Contains(s, forbidden) {
			return "", fmt.Errorf("tools: db_query: sql contains %q (literals/comments rejected: pass values via args placeholders)", forbidden)
		}
	}
	if dbqIntoFileRe.MatchString(s) {
		return "", fmt.Errorf("tools: db_query: INTO OUTFILE/DUMPFILE rejected (read-only tool)")
	}
	if dbqContainsDML(s) {
		return "", fmt.Errorf("tools: db_query: DML/admin verb rejected (read-only tool; CTE-prefixed DML included)")
	}
	if !dbqHeadRe.MatchString(s) {
		return "", fmt.Errorf("tools: db_query: statement must start with SELECT or WITH (read-only vocabulary)")
	}
	return s, nil
}

// dbqContainsDML 判定语句是否含顶层 DML/管理动词(见词表注释)。INSERT/
// REPLACE 与字符串函数同名:命中后随(跳空白)`(` = 函数调用形态
// (REPLACE(col, 0x61, 0x62) 类带参分析查询合法)放行;其余(含
// INSERT INTO/REPLACE INTO 语句形态与 WITH 前缀 DML)拒。
func dbqContainsDML(s string) bool {
	if dbqDMLVerbRe.MatchString(s) {
		return true
	}
	for _, loc := range dbqDMLFuncNamedRe.FindAllStringIndex(s, -1) {
		rest := strings.TrimLeft(s[loc[1]:], " \t\r\n")
		if !strings.HasPrefix(rest, "(") {
			return true
		}
	}
	return false
}

// dbqNormalizeArgs 归一 args 输入:数组、元素标量。json.Number(全仓
// UseNumber 解码纪律)→ int64 优先/float64 兜底(driver.Value 类型面,
// >2^53 整数经 int64 精确往返);嵌套结构/超量响亮拒。
func dbqNormalizeArgs(raw any) ([]any, error) {
	if raw == nil {
		return nil, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("tools: db_query: input \"args\" must be array, got %T", raw)
	}
	if len(arr) > maxDBQueryArgs {
		return nil, fmt.Errorf("tools: db_query: args exceed %d elements", maxDBQueryArgs)
	}
	out := make([]any, 0, len(arr))
	for i, el := range arr {
		switch v := el.(type) {
		case nil:
			out = append(out, nil)
		case bool:
			out = append(out, v)
		case string:
			out = append(out, v)
		case json.Number:
			if i64, err := v.Int64(); err == nil {
				out = append(out, i64)
			} else if f, ferr := v.Float64(); ferr == nil {
				out = append(out, f)
			} else {
				return nil, fmt.Errorf("tools: db_query: args[%d]: unparsable number %q", i, v.String())
			}
		case float64:
			out = append(out, v)
		case int:
			out = append(out, int64(v))
		case int64:
			out = append(out, v)
		default:
			return nil, fmt.Errorf("tools: db_query: args[%d]: unsupported type %T (want scalar)", i, el)
		}
	}
	return out, nil
}

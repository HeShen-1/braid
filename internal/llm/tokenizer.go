package llm

// T7.2 tiktoken 离线 loader:词表随安装包(go:embed)+ 启动预构建 + 请求前
// 本地估算。断网冷启动零外网 = 结构性保证——包 init 即把 tiktoken 的全局
// BpeLoader 换成 embed 实现,进程内任何 GetEncoding(懒加载含)都只读
// 二进制内嵌词表,不存在可触网的默认下载路径。
//
// 口径纪律(ARCHITECTURE §2 / D-t31):计费真相源 = API 返回 usage,本文件
// 的估算仅用于请求前预算,恒近似(每消息 4 开销 + priming 2 的 cookbook
// 启发式),不进台账不进审计。未知模型与未捆绑编码一律收敛到 cl100k 近似
// (estimated 语义已在 cost 侧,tokenizer 侧不再造语义)。
//
// 供应链:词表一次性取自 openaipublic 官方 blob(2026-09-24),SHA-256 钉死
// 于 vocabChecksums;加载即校验,篡改 = 启动即败(T7.3 安装自检复用本面)。

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/pkoukk/tiktoken-go"
)

//go:embed vocab/cl100k_base.tiktoken vocab/o200k_base.tiktoken
var vocabFS embed.FS

// vocabChecksums 是随包词表的 SHA-256 钉(键 = 词表文件名)。词表是公开
// 数据非密钥,入库只为离线;hash 钉防的是仓库/构建链路上的意外替换。
var vocabChecksums = map[string]string{
	"cl100k_base.tiktoken": "223921b76ee99bde995b7ff738513eef100fb51d18c93597a113bcffe865b2a7",
	"o200k_base.tiktoken":  "446a9538cb6c348e3516120d7c08b09f57c36495e2acfffe59a5bf8b0cfb1a2d",
}

// bundledEncoding 是随安装包发布的编码集合(GATE 1 拍板:cl100k + o200k,
// 覆盖现代 OpenAI 兼容网关常见谱系;p50k/r50k 老谱系估算价值低不携带)。
const (
	encCl100k = "cl100k_base"
	encO200K  = "o200k_base"
)

// init 在包加载时安装离线 loader:必须先于任何 GetEncoding(tiktoken-go
// SetBpeLoader 是裸全局赋值,编码一旦进进程级缓存不再走 loader)。本包是
// 代码库里 tiktoken 的唯一消费方,全局接管无交错面。
func init() {
	tiktoken.SetBpeLoader(embedBpeLoader{})
}

// embedBpeLoader 实现 tiktoken.BpeLoader:按 URL 尾段名读内嵌词表,hash
// 校验后解析。未捆绑编码响亮报错(不静默降级下载——那是断网冷启动要
// 防的失败模式)。
type embedBpeLoader struct{}

func (embedBpeLoader) LoadTiktokenBpe(tiktokenBpeFile string) (map[string]int, error) {
	name := path.Base(tiktokenBpeFile)
	raw, err := vocabFS.ReadFile("vocab/" + name)
	if err != nil {
		return nil, fmt.Errorf("llm: embedded vocab %q unavailable: %w", name, err)
	}
	if err := verifyVocab(name, raw); err != nil {
		return nil, err
	}
	return parseTiktokenBpe(raw)
}

// verifyVocab 校验词表字节与 SHA-256 钉一致;未捆绑文件名直接拒绝。
func verifyVocab(name string, raw []byte) error {
	want, ok := vocabChecksums[name]
	if !ok {
		return fmt.Errorf("llm: vocab %q not in bundled set {%s,%s}", name, encCl100k, encO200K)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("llm: vocab %q checksum mismatch (embedded %s, pinned %s): supply-chain tamper guard", name, got, want)
	}
	return nil
}

// parseTiktokenBpe 解析 .tiktoken 词表(每行 "base64(token) rank",与
// tiktoken-go loadTiktokenBpe 同格式;多一道字段数防御——上游对畸形行会
// 越界 panic,此处响亮拒绝)。
func parseTiktokenBpe(raw []byte) (map[string]int, error) {
	ranks := make(map[string]int)
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, " ")
		if len(parts) != 2 {
			return nil, fmt.Errorf("llm: malformed vocab line %q (want \"<base64> <rank>\")", clampRunes(line, 64))
		}
		token, err := base64.StdEncoding.DecodeString(parts[0])
		if err != nil {
			return nil, fmt.Errorf("llm: vocab token not base64: %w", err)
		}
		rank, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("llm: vocab rank not integer: %w", err)
		}
		ranks[string(token)] = rank
	}
	return ranks, nil
}

// prebuildOnce 保证预构建单次执行;失败粘滞(启动即败语义:首个失败即
// 终止启动,重试同错,不掩盖)。
var (
	prebuildOnce sync.Once
	prebuildErr  error
)

// PrebuildTokenizers 启动时急切构建全部随包编码:词表 SHA-256 校验 +
// ranks 解析 + 每 encoding 一个 Tiktoken 实例(cl100k+o200k 合计 ~5.3MB,
// 数百毫秒量级)。词表损坏/篡改在启动口响亮失败;此后估算调用 = 缓存
// 实例查找 + Encode,零构建成本(tiktoken-go GetEncoding 每次调用都重建
// CoreBPE——2 次 regexp2 编译 + 全词表反转映射,实测 38.6ms/93.6ms 每
// 次,故实例必须自缓存,审计 MEDIUM 修)。幂等,可重复调用。
func PrebuildTokenizers() error {
	prebuildOnce.Do(func() {
		for _, enc := range []string{encCl100k, encO200K} {
			if _, err := tokenizerFor(enc); err != nil {
				prebuildErr = fmt.Errorf("llm: prebuild tokenizer %s: %w", enc, err)
				return
			}
		}
	})
	return prebuildErr
}

// 每 encoding 单例(sync.OnceValues:热路径无锁,构建恰一次,错误粘滞
// ——启动预构建已把错误在启动口暴露,懒路径重复同错不掩盖)。实例构建
// 后只读(CoreBPE/regexp2 文档化并发安全),全进程共享。
var (
	cl100kOnce = sync.OnceValues(func() (*tiktoken.Tiktoken, error) {
		return tiktoken.GetEncoding(encCl100k)
	})
	o200kOnce = sync.OnceValues(func() (*tiktoken.Tiktoken, error) {
		return tiktoken.GetEncoding(encO200K)
	})
)

// tokenizerFor 取 encoding 的进程级单例实例。入参恒为随包编码词表
// (encodingForModel 只产两常量):default 即 cl100k 主路径(未知模型
// 收敛后的常态路由),兼作词表外值的防御性兜底。
func tokenizerFor(enc string) (*tiktoken.Tiktoken, error) {
	switch enc {
	case encO200K:
		return o200kOnce()
	default:
		return cl100kOnce()
	}
}

// encodingForModel 把模型名解析到随包编码:精确表 → 前缀表(五前缀互斥,
// 真实模型名下无歧义)→ 未命中或命中未捆绑编码(p50k/r50k/gpt2)一律
// 收敛 cl100k 近似。估算场景下近似优于报错。
func encodingForModel(model string) string {
	if enc, ok := tiktoken.MODEL_TO_ENCODING[model]; ok && bundled(enc) {
		return enc
	}
	for prefix, enc := range tiktoken.MODEL_PREFIX_TO_ENCODING {
		if strings.HasPrefix(model, prefix) && bundled(enc) {
			return enc
		}
	}
	return encCl100k
}

func bundled(enc string) bool {
	return enc == encCl100k || enc == encO200K
}

// 估算开销常数(cl100k chat 口径,OpenAI cookbook 启发式:每消息 ~4 token
// 框架开销 + 全对话 2 priming;近似值,非计费)。
const (
	chatOverheadPerMessage = 4
	chatPrimingTokens      = 2
)

// estimateSampleRunes 是单条消息 content 的精算采样上限:超过即只精算
// 前 N 个 rune、按 rune 数线性外推。tiktoken-go Encode 对 CJK 是超线性
// 的(实测 cl100k:10k 字 42ms、40k 字 655ms、160k 字 ~111s——审计
// MEDIUM:请求前路径的吞吐/DoS 面),而估算本来就是启发式口径,采样
// 外推把单条消息估算成本钉在 ~10ms 量级,精度损失对外推场景可忽略。
const estimateSampleRunes = 2048

// EstimatePromptTokens 请求前本地估算一段对话的 prompt token 数(仅预算
// 用途;计费真相源 = API 返回 usage,两者永不互换)。并发安全(实例单例
// 构建后只读);超长内容采样外推,见 estimateSampleRunes。
func EstimatePromptTokens(model string, msgs []Message) (int, error) {
	tke, err := tokenizerFor(encodingForModel(model))
	if err != nil {
		return 0, fmt.Errorf("llm: tokenizer for model %q: %w", model, err)
	}
	total := chatPrimingTokens
	for _, m := range msgs {
		total += chatOverheadPerMessage + estimateContent(tke, m.Content)
	}
	return total, nil
}

// estimateContent 数一段 content 的 token:短内容精算;超采样上限时精算
// 前缀后按 rune 比例外推(向上取整,预算估算宁可高估)。全程零 O(content)
// 分配(delta 审计 MEDIUM 修:全量 []rune 镜像会把 1MB CJK 变 4MB 瞬时
// 分配,采样钉住了 CPU 却留内存线性)——rune 计数用 utf8 零拷贝 API,
// 前缀按字节边界前向解出(DecodeRuneInString 对无效字节产 RuneError,
// 与全量 []rune 转换同语义,采样边界不切割多字节字符)。
func estimateContent(tke *tiktoken.Tiktoken, content string) int {
	n := utf8.RuneCountInString(content)
	if n <= estimateSampleRunes {
		// Encode(text,nil,nil) 不启用特殊 token 拦截:用户内容含
		// "<|endoftext|>" 字面量时按普通文本编码,估算不 panic。
		return len(tke.Encode(content, nil, nil))
	}
	end := sampleCut(content, estimateSampleRunes)
	sampled := int64(len(tke.Encode(content[:end], nil, nil)))
	return int((sampled*int64(n) + estimateSampleRunes - 1) / estimateSampleRunes)
}

// sampleCut 返回 content 前 want 个 rune 的字节边界(前向解码恰好 want
// 步,调用方已保证 want <= rune 总数)。
func sampleCut(content string, want int) int {
	end := 0
	for i := 0; i < want; i++ {
		_, size := utf8.DecodeRuneInString(content[end:])
		if size == 0 {
			break // 串尾提前到达(防御;调用方契约下不发生)
		}
		end += size
	}
	return end
}

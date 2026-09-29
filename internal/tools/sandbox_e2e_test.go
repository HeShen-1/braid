package tools

// 沙箱路径 e2e(T10.1 T9,真 docker):file_read 经生产装配构造器全链路
// 跑通——宿主 carrier(argv/信封/stdout 收流)→ 容器内 helper 重建同名
// 工具执行 → 信封回传。验收面 = 基线 §2 行 #3(白名单 ro bind)的
// 「失控即红」证明 + GATE 1 T9 切片:词形逃逸与 symlink 逃逸被拒(跨进程
// 哨兵保真)、沙箱/本地 golden parity、零残留;另含两枚正向对照(T9 审计
// 处置):uid 边界差分(容器执行在场的构造性证明)与多目录多 bind。
//
// 镜像走 sandboxEnsureImage 装配(prod 形态缺省 target,file_read 零 TLS
// 面不消费 e2e CA 注入镜像——那是 T10 的口径);门控 = sandboxDockerGate
// (本地无 docker skip、CI REQUIRE_STACK 继承 Fatalf)。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSandboxE2EFileRead 真 docker 全链路 file_read e2e(子用例共享同一对
// 白名单目录与载体,每子用例独立断言)。
func TestSandboxE2EFileRead(t *testing.T) {
	sandboxDockerGate(t)
	tag := sandboxTestImageTag()
	sandboxEnsureImage(t, tag)
	t.Cleanup(func() {
		_ = exec.Command(sandboxDockerBin, "rmi", tag).Run()
	})

	// 零残留基线快照。差分法证明的精确命题:在「本测试串行持有 daemon 调用」
	// 假设下,before→after 窗内新建的 braid.sandbox=1 容器全部退场(--rm)。
	// 已知残余:并行 lane 若在同窗内新建且在 after 时点仍在飞的带标签容器会被
	// 误计入(假红方向,非假绿;镜像 tag 有槽位隔离而 label 是全局的,与基线
	// §8-11 启动清扫同构)。CI 单 lane;本机并行公约下 he lane 残留物落在
	// before 集内被扣除,仅「窗口期恰好重叠的在飞容器」可触发。
	before := sandboxLabeledContainerIDs(t)

	// 白名单目录 ×2(多目录是 BRAID_FILEREAD_DIRS 的一等生产形态:≥2 个真实
	// 存在目录各发一条 --mount、容器同时收到多个 bind):0755/0644 让容器内
	// uid 65532 可遍历可读(基线 #10 权限矩阵)。显式 chmod 而不依赖
	// WriteFile 的 mode 参数——后者受进程 umask 修正。
	newDir := func() string {
		d := t.TempDir()
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatalf("chmod whitelist dir: %v", err)
		}
		return d
	}
	writeFile := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write whitelist file: %v", err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatalf("chmod whitelist file: %v", err)
		}
	}
	dir1, dir2 := newDir(), newDir()
	// 内容带 &<> 与多字节 UTF-8:载荷经信封编解码(EncodeSandboxResponse
	// SetEscapeHTML(false) 纪律)逐字节保真——信封层任一侧回落 HTML 转义即
	// 红。范围声明(审计处置):这锚的是信封编解码层的非对称漂移;工具输出
	// 编码器(marshalToolOutput)双侧同源,对称回归由单测锚定,本 parity
	// 不背书。
	const body = "line1 & <tag> ünïcode 中文\nline2\n"
	doc1 := filepath.Join(dir1, "doc.txt")
	writeFile(doc1, body)
	doc2 := filepath.Join(dir2, "doc.txt")
	writeFile(doc2, "dir2-"+body)
	// 白名单内 symlink 外指:/etc/passwd 在 alpine 容器内真实在场,
	// EvalSymlinks 确定性解析后界外即拒(宿主本地同款拒绝,双端语义一致)。
	escapeLink := filepath.Join(dir1, "escape-link")
	if err := os.Symlink("/etc/passwd", escapeLink); err != nil {
		t.Fatalf("create escape symlink: %v", err)
	}
	// 白名单内 symlink 内指(别名):词形 ≠ 解析形,是「回显词形而非解析形」
	// 契约的非平凡载荷——把回显改成 real 的变异在此即红(clean==real 的
	// 直读路径杀不死它)。
	aliasLink := filepath.Join(dir1, "alias-link")
	if err := os.Symlink(doc1, aliasLink); err != nil {
		t.Fatalf("create in-whitelist alias symlink: %v", err)
	}
	// 0600 属主可读文件:宿主本地(owner)读成功、容器内 uid 65532 必
	// EACCES——双态差分是「容器执行确实发生」的正向对照(见 uid boundary
	// 子用例)。不 chmod 0644。
	secret := filepath.Join(dir1, "secret.txt")
	if err := os.WriteFile(secret, []byte("host-owner-only"), 0o600); err != nil {
		t.Fatalf("write owner-only file: %v", err)
	}

	// 生产装配构造器(T6 装配缝同款):配置镜像指向测试 tag,其余取基线
	// 档位默认值;载体 c.docker 走 PATH 真 docker。
	carrier, err := newSandboxCarrierFileRead(sandboxConfig{
		Docker:         true,
		Image:          tag,
		Memory:         "256m",
		CPUs:           "0.5",
		PidsLimit:      64,
		MaxConcurrency: 8,
	}, []string{dir1, dir2}, newSandboxLimiter(8))
	if err != nil {
		t.Fatalf("newSandboxCarrierFileRead: %v", err)
	}

	runCall := func(t *testing.T, path string) (Result, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return carrier.Run(ctx, Call{
			Inputs:         map[string]any{"path": path},
			Token:          "scoped-token",
			IdempotencyKey: "att:1:1",
		})
	}

	t.Run("golden parity with local execution", func(t *testing.T) {
		// 本地对照与沙箱载体同配置(双目录),三条路径各跑双份逐字节比对:
		// dir1 直读、白名单内别名(clean≠real 的词形回显载荷)、dir2 第二
		// mount(多 bind 形态真容器证明)。
		local, err := NewFileReadTool(FileReadConfig{Dirs: []string{dir1, dir2}})
		if err != nil {
			t.Fatalf("NewFileReadTool: %v", err)
		}
		for _, path := range []string{doc1, aliasLink, doc2} {
			want, err := local.Run(context.Background(), Call{Inputs: map[string]any{"path": path}})
			if err != nil {
				t.Fatalf("local file_read %q: %v", path, err)
			}
			got, err := runCall(t, path)
			if err != nil {
				t.Fatalf("sandbox file_read %q: %v", path, err)
			}
			if !bytes.Equal(want.Output, got.Output) {
				t.Fatalf("sandbox output != local golden for %q:\n local: %s\n sandbox: %s", path, want.Output, got.Output)
			}
			// parity 之外点名 path 词形回显(失败时可诊断是哪个字段漂移):
			// Clean 后词形与宿主一致 = 基线 #3「同路径同构」的回显半边。
			var fields struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(got.Output, &fields); err != nil {
				t.Fatalf("unmarshal sandbox output for %q: %v", path, err)
			}
			if fields.Path != filepath.Clean(path) {
				t.Fatalf("output.path = %q, want lexical %q", fields.Path, filepath.Clean(path))
			}
		}
	})

	t.Run("container uid boundary is load-bearing", func(t *testing.T) {
		// 正向对照(审计处置):0600 文件宿主 owner 读成功、沙箱(容器内
		// uid 65532 对 bind mount 按宿主 uid 判权)必 EACCES。双态差分把
		// 「容器执行确实发生」变成断言:载体若静默回落本地执行,沙箱侧
		// 会与本地同绿即红。EACCES 是非哨兵业务失败(tool_failed 信封),
		// 分类与消息(Go errno 文案,locale 无关)双断言。
		local, err := NewFileReadTool(FileReadConfig{Dirs: []string{dir1}})
		if err != nil {
			t.Fatalf("NewFileReadTool: %v", err)
		}
		if _, err := local.Run(context.Background(), Call{Inputs: map[string]any{"path": secret}}); err != nil {
			t.Fatalf("local read of owner-only file must succeed (host owner): %v", err)
		}
		_, err = runCall(t, secret)
		if err == nil {
			t.Fatal("sandbox read of 0600 host-owner file succeeded, want EACCES under container uid 65532 (silent local fallback?)")
		}
		if errors.Is(err, ErrPathBlocked) {
			t.Fatalf("err = ErrPathBlocked, want non-sentinel permission failure: %v", err)
		}
		if !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("err = %v, want EACCES (permission denied) in message", err)
		}
	})

	t.Run("lexical escape rejected", func(t *testing.T) {
		// 白名单外绝对路径:首道词形门在容器内零 FS 触达即拒(不泄漏
		// 存在性 oracle);信封 code=path_blocked,宿主重抛真哨兵。
		if _, err := runCall(t, "/etc/passwd"); !errors.Is(err, ErrPathBlocked) {
			t.Fatalf("err = %v, want ErrPathBlocked", err)
		}
	})

	t.Run("raw traversal escape rejected", func(t *testing.T) {
		// 裸 ../ 穿越词形(字符串拼接,不经 filepath.Join——Join 会预
		// Clean 掉 .. 段,让本用例退化为词形门外路径的同构重复,审计确认
		// 的假绿通道)。目标刻意不存在:工具侧 Clean 归一化被删的变异下,
		// 词形门前缀误放行 + EvalSymlinks ENOENT → tool_failed,分类漂移
		// 在此即红;Clean 在位时首道门零 FS 触达即拒。
		escape := dir1 + "/../escape.txt"
		if _, err := runCall(t, escape); !errors.Is(err, ErrPathBlocked) {
			t.Fatalf("err = %v, want ErrPathBlocked for raw traversal %q", err, escape)
		}
	})

	t.Run("symlink escape rejected", func(t *testing.T) {
		if _, err := runCall(t, escapeLink); !errors.Is(err, ErrPathBlocked) {
			t.Fatalf("err = %v, want ErrPathBlocked for symlink %q", err, escapeLink)
		}
	})

	t.Run("zero residue", func(t *testing.T) {
		after := sandboxLabeledContainerIDs(t)
		var leaked []string
		for id := range after {
			if !before[id] {
				leaked = append(leaked, id)
			}
		}
		if len(leaked) > 0 {
			t.Fatalf("sandbox containers leaked past --rm: %v", leaked)
		}
	})
}

// sandboxLabeledContainerIDs 当前 braid.sandbox=1 容器 id 集合(零残留
// 断言的快照原语;与 SandboxSweep 同枚举口径)。
func sandboxLabeledContainerIDs(t *testing.T) map[string]bool {
	t.Helper()
	out, err := exec.Command(sandboxDockerBin, "ps", "-aq", "--filter", "label=braid.sandbox=1").Output()
	if err != nil {
		t.Fatalf("docker ps (label=braid.sandbox=1): %v", err)
	}
	ids := make(map[string]bool)
	for _, id := range strings.Fields(string(out)) {
		ids[id] = true
	}
	return ids
}

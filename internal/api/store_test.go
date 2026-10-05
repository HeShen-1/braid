package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"braid/internal/db/sqltest"
	"braid/internal/engine/dsl"
	"braid/internal/snowflake"
)

// uniqName 生成跨运行唯一的 DSL name(pattern ^[a-z][a-z0-9-]{1,63}$):
// 时间前缀隔离历史运行残留,计数后缀隔离本运行内的多份草稿。
var nameSeq atomic.Int64

func uniqName(prefix string) string {
	n := nameSeq.Add(1)
	return fmt.Sprintf("%s-%09d-%03d", prefix, time.Now().UnixNano()%1_000_000_000, n%1000)
}

// dslDoc 最小合法 DSL(单 LLM 节点)。
func dslDoc(name string) []byte {
	return fmt.Appendf(nil, `{"dsl":"1.0","name":%q,"description":"t54 fixture",
"nodes":[{"id":"greet","type":"llm","prompt":{"template":"用一句话介绍你自己。"},"outputs":{"text":"string"}}],
"outputs":{"greeting":"{{nodes.greet.output.text}}"}}`, name)
}

// toolDSL 引用 http 工具的合法 DSL(白名单校验对象)。
func toolDSL(name string) []byte {
	return fmt.Appendf(nil, `{"dsl":"1.0","name":%q,
"nodes":[{"id":"fetch","type":"tool","tool":"http","inputs":{"method":"GET","path":"/api/v1/x"},"outputs":{"status":"number"}}],
"edges":[],
"outputs":{"s":"{{nodes.fetch.output.status}}"}}`, name)
}

// newTestStore 返回干净的 store + 连接:每用例清空定义表(List 的 total
// 断言依赖全局可见行,共享 schema 下必须从零开始)。T5.5 起真实链测试会
// 落 execution/outbox/audit 行,清理统一走八表链 cleanExecutionChain
// (单表 DELETE 会被 FK RESTRICT 阻拦)。
func newTestStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	dbh := sqltest.MustOpenAt(t, "braid_api_test")
	cleanExecutionChain(t, dbh)
	ids, err := snowflake.New(1)
	if err != nil {
		t.Fatalf("snowflake: %v", err)
	}
	return NewStore(ids), dbh
}

// hashOf 镜像 hashDSL 契约(T3.6 MED① 后):SHA-256(compact(字节))。
// 锚定 compact 形态 = API 响应 dsl 与执行侧复算同字节,消费方可核对。
func hashOf(b []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		buf.Write(b) // 不可达(断言对象是 DB 读回的合法 JSON);与产线同款防御
	}
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:])
}

// createDraft / updateDraft 是 WithTx 包装:CreateDraft/UpdateDraft 是
// 两步写(行 + 读回定稿 hash),必须事务包裹(生产调用方同款)。
func createDraft(t *testing.T, st *Store, dbh *sql.DB, doc []byte, actor string) (Definition, error) {
	t.Helper()
	var d Definition
	err := WithTx(context.Background(), dbh, func(tx DBTX) error {
		var err error
		d, err = st.CreateDraft(context.Background(), tx, 0, doc, actor)
		return err
	})
	return d, err
}

func updateDraft(t *testing.T, st *Store, dbh *sql.DB, id uint64, doc []byte, actor string) (Definition, error) {
	t.Helper()
	var d Definition
	err := WithTx(context.Background(), dbh, func(tx DBTX) error {
		var err error
		d, err = st.UpdateDraft(context.Background(), tx, 0, id, doc, actor)
		return err
	})
	return d, err
}

// assertSameJSON 断言两份 DSL 文档语义等价(字节可能因 JSON 列规范化
// 而不同:分隔符空格、转义形态)。
func assertSameJSON(t *testing.T, a, b []byte) {
	t.Helper()
	var ja, jb any
	if err := json.Unmarshal(a, &ja); err != nil {
		t.Fatalf("unmarshal a: %v", err)
	}
	if err := json.Unmarshal(b, &jb); err != nil {
		t.Fatalf("unmarshal b: %v", err)
	}
	if !reflect.DeepEqual(ja, jb) {
		t.Fatalf("documents differ semantically:\na=%s\nb=%s", a, b)
	}
}

func TestStoreCreateDraftRoundtrip(t *testing.T) {
	st, dbh := newTestStore(t)
	ctx := context.Background()
	doc := dslDoc(uniqName("rt"))
	got, err := createDraft(t, st, dbh, doc, "alice")
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if got.Status != "DRAFT" || got.Version != nil || got.CreatedBy != "alice" || got.UpdatedBy != "alice" {
		t.Fatalf("draft row = %+v", got)
	}
	if got.ID == 0 {
		t.Fatal("id not assigned")
	}
	// hash 钉住存储形态(MySQL JSON 规范化后的字节),文档语义等价输入。
	if got.DSLHash != hashOf(got.DSL) {
		t.Fatalf("dsl_hash = %s, want sha256(stored bytes) %s", got.DSLHash, hashOf(got.DSL))
	}
	assertSameJSON(t, got.DSL, doc)
	back, err := st.GetByID(ctx, dbh, 0, got.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if back.DSLHash != got.DSLHash || back.Name != got.Name || back.Status != "DRAFT" {
		t.Fatalf("re-read mismatch: %+v vs %+v", back, got)
	}
}

func TestStoreCreateDraftRejectsInvalidDSL(t *testing.T) {
	st, dbh := newTestStore(t)
	tests := []struct {
		name     string
		doc      string
		wantCode string // 语义码;空 = 结构层失败
	}{
		{"schema missing nodes", `{"dsl":"1.0","name":"x"}`, ""},
		{"schema bad name", `{"dsl":"1.0","name":"Bad_Name","nodes":[],"outputs":{}}`, ""},
		{"semantic dangling ref", `{"dsl":"1.0","name":"aa","nodes":[{"id":"a","type":"llm","prompt":{"template":"检查","user_data":[{"name":"g","ref":"{{nodes.ghost.output.x}}"}]},"outputs":{"t":"string"}}],"edges":[],"outputs":{"o":"{{nodes.a.output.t}}"}}`, "dangling_ref"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := createDraft(t, st, dbh, []byte(tt.doc), "alice")
			if err == nil {
				t.Fatal("invalid DSL accepted")
			}
			var ide *InvalidDSLError
			if !errors.As(err, &ide) {
				t.Fatalf("err = %v, want *InvalidDSLError", err)
			}
			if tt.wantCode != "" && !HasIssueCode(err, tt.wantCode) {
				t.Fatalf("issues %v, want code %q", ide.Issues, tt.wantCode)
			}
		})
	}
}

func TestStoreUpdateDraft(t *testing.T) {
	st, dbh := newTestStore(t)
	ctx := context.Background()
	name := uniqName("upd")
	d1, err := createDraft(t, st, dbh, dslDoc(name), "alice")
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	d2 := append(dslDoc(name), '\n') // 字节不同、语义相同:hash 变、行为同
	got, err := updateDraft(t, st, dbh, d1.ID, d2, "bob")
	if err != nil {
		t.Fatalf("UpdateDraft: %v", err)
	}
	if got.UpdatedBy != "bob" || got.DSLHash != hashOf(got.DSL) {
		t.Fatalf("updated row = %+v", got)
	}
	assertSameJSON(t, got.DSL, d2)
	if _, err := updateDraft(t, st, dbh, d1.ID, dslDoc(uniqName("other")), "bob"); !errors.Is(err, ErrNameMismatch) {
		t.Fatalf("rename via update err = %v, want ErrNameMismatch", err)
	}
	if _, err := updateDraft(t, st, dbh, 999999, d2, "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id err = %v, want ErrNotFound", err)
	}
	published, err := st.PublishUngated(ctx, dbh, 0, d1.ID, "bob")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := updateDraft(t, st, dbh, published.ID, dslDoc(name), "bob"); !errors.Is(err, ErrConflict) {
		t.Fatalf("update published err = %v, want ErrConflict", err)
	}
}

// TestStoreUpdateDraftIdempotent 钉死 changed-rows 语义:存储形态字节
// 原样回放(赋值与现值全同 ⇒ affected=0)是幂等成功,不得误报 409
// (审查发现 #5 的回归锚;真实触发面 = 序列化器恰与 MySQL JSON 规范
// 形态重合的客户端)。
func TestStoreUpdateDraftIdempotent(t *testing.T) {
	st, dbh := newTestStore(t)
	name := uniqName("idem")
	d, err := createDraft(t, st, dbh, dslDoc(name), "alice")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	again, err := updateDraft(t, st, dbh, d.ID, d.DSL, "alice")
	if err != nil {
		t.Fatalf("idempotent update: %v", err)
	}
	if again.Status != "DRAFT" || again.DSLHash != d.DSLHash || again.Version != nil {
		t.Fatalf("row drifted on no-op update: %+v vs %+v", again, d)
	}
}

func TestStorePublishVersioning(t *testing.T) {
	st, dbh := newTestStore(t)
	ctx := context.Background()
	name := uniqName("pub")
	d1, err := createDraft(t, st, dbh, dslDoc(name), "alice")
	if err != nil {
		t.Fatalf("CreateDraft#1: %v", err)
	}
	p1, err := st.PublishUngated(ctx, dbh, 0, d1.ID, "alice")
	if err != nil {
		t.Fatalf("Publish#1: %v", err)
	}
	if p1.Version == nil || *p1.Version != 1 || p1.Status != "PUBLISHED" {
		t.Fatalf("p1 = %+v, want v1 PUBLISHED", p1)
	}
	if p1.PublishedBy == nil || *p1.PublishedBy != "alice" || p1.PublishedAt == nil || p1.PublishedAt.IsZero() {
		t.Fatalf("publish audit fields missing: %+v", p1)
	}
	d2, err := createDraft(t, st, dbh, dslDoc(name), "bob")
	if err != nil {
		t.Fatalf("CreateDraft#2: %v", err)
	}
	p2, err := st.PublishUngated(ctx, dbh, 0, d2.ID, "bob")
	if err != nil {
		t.Fatalf("Publish#2: %v", err)
	}
	if p2.Version == nil || *p2.Version != 2 {
		t.Fatalf("p2 version = %v, want 2", p2.Version)
	}
	latest, err := st.LatestPublished(ctx, dbh, 0, name)
	if err != nil {
		t.Fatalf("LatestPublished: %v", err)
	}
	if latest.ID != p2.ID {
		t.Fatalf("latest = %d, want %d", latest.ID, p2.ID)
	}
	if _, err := st.PublishUngated(ctx, dbh, 0, p1.ID, "alice"); !errors.Is(err, ErrConflict) {
		t.Fatalf("republish err = %v, want ErrConflict", err)
	}
	if _, err := st.PublishUngated(ctx, dbh, 0, 999999, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id err = %v, want ErrNotFound", err)
	}
	versions, err := st.ListVersions(ctx, dbh, 0, name)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 2 || *versions[0].Version != 1 || *versions[1].Version != 2 {
		t.Fatalf("versions = %+v, want [1 2]", versions)
	}
	if v, err := st.GetVersion(ctx, dbh, 0, name, 1); err != nil || v.ID != p1.ID {
		t.Fatalf("GetVersion(name,1) = %d, %v", v.ID, err)
	}
	if _, err := st.GetVersion(ctx, dbh, 0, name, 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetVersion(name,9) err = %v, want ErrNotFound", err)
	}
}

func TestStorePublishToolAllowlist(t *testing.T) {
	st, dbh := newTestStore(t)
	ctx := context.Background()
	allowed, err := createDraft(t, st, dbh, toolDSL(uniqName("tool-ok")), "alice")
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if _, err := st.PublishUngated(ctx, dbh, 0, allowed.ID, "alice", "http"); err != nil {
		t.Fatalf("publish with allowlist [http]: %v", err)
	}
	blocked, err := createDraft(t, st, dbh, toolDSL(uniqName("tool-no")), "alice")
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if _, err := st.PublishUngated(ctx, dbh, 0, blocked.ID, "alice", "file_read"); err == nil || !HasIssueCode(err, dsl.IssueToolNotAllowed) {
		t.Fatalf("publish with allowlist [file_read] err = %v, want tool_not_allowed", err)
	}
	if _, err := st.PublishUngated(ctx, dbh, 0, blocked.ID, "alice"); err == nil {
		t.Fatal("publish with empty allowlist accepted tool")
	}
	plain, err := createDraft(t, st, dbh, dslDoc(uniqName("tool-na")), "alice")
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if _, err := st.PublishUngated(ctx, dbh, 0, plain.ID, "alice", "http"); err != nil {
		t.Fatalf("publish tool-free DSL with [http] allowlist: %v", err)
	}
}

// TestStorePublishConcurrent 钉死并发闸的**契约**而非交错:同名两草稿
// 并发发布,结果二选一——①真并发(两事务重叠,MAX 读同见 0):恰一胜
// (PUBLISHED v1)一败(ErrConflict,草稿保留可重试),uk 兜底是设计行为
// (0001_init DDL 注释);②完全串行交错(B 的 MAX 读在 A 提交后):双胜
// 合法,B 发的是 v2,uk 不咬。原断言「恰一胜」把交错钉成契约,串行路径
// ≈2% 偶发红(T4.1 会话 100× 压测实证,归因 t5.5 修复);真不变量 =
// ≥1 胜、败方必 ErrConflict、胜方版本集无重号且从 v1 起。
func TestStorePublishConcurrent(t *testing.T) {
	st, dbh := newTestStore(t)
	ctx := context.Background()
	name := uniqName("conc")
	ids := make([]uint64, 2)
	for i := range ids {
		d, err := createDraft(t, st, dbh, dslDoc(name), "alice")
		if err != nil {
			t.Fatalf("CreateDraft#%d: %v", i, err)
		}
		ids[i] = d.ID
	}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = WithTx(ctx, dbh, func(tx DBTX) error {
				_, err := st.PublishUngated(ctx, tx, 0, ids[i], "alice")
				return err
			})
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		}
	}
	if wins == 0 {
		t.Fatalf("errs = [%v, %v], want at least one success", errs[0], errs[1])
	}
	versions := make(map[uint32]bool, wins)
	for i, err := range errs {
		switch {
		case err == nil:
			d, gerr := st.GetByID(ctx, dbh, 0, ids[i])
			if gerr != nil || d.Status != "PUBLISHED" || d.Version == nil {
				t.Fatalf("winner#%d = %+v (err %v), want PUBLISHED", i, d, gerr)
			}
			if versions[*d.Version] {
				t.Fatalf("duplicate version %d — uk 契约被击穿", *d.Version)
			}
			versions[*d.Version] = true
		case errors.Is(err, ErrConflict):
			d, gerr := st.GetByID(ctx, dbh, 0, ids[i])
			if gerr != nil || d.Status != "DRAFT" {
				t.Fatalf("loser#%d = %+v (err %v), want still DRAFT (retryable)", i, d, gerr)
			}
		default:
			t.Fatalf("errs[%d] = %v, want nil or ErrConflict", i, err)
		}
	}
	// 版本集从 v1 起连续:双胜必 {1,2},单胜必 {1}。
	for v := uint32(1); v <= uint32(len(versions)); v++ {
		if !versions[v] {
			t.Fatalf("version set = %v, want contiguous from v1", versions)
		}
	}
}

// TestStorePublishGateEvaluatesLockedContent 钉 T20.3 发布面 TOCTOU 收口的
// 存储层契约:闸回调评估的是与翻转同一份锁定读内容(字节同源),闸拒绝
// 原子中止整个发布(行保持 DRAFT、版本号未被中止尝试消耗)。
func TestStorePublishGateEvaluatesLockedContent(t *testing.T) {
	st, dbh := newTestStore(t)
	ctx := context.Background()
	d, err := createDraft(t, st, dbh, dslDoc(uniqName("gate-lock")), "alice")
	if err != nil {
		t.Fatalf("createDraft: %v", err)
	}

	// 拒绝分支:闸错误透传(调用方按类型分类映射),事务整体回滚。
	sentinel := errors.New("gate rejection")
	err = WithTx(ctx, dbh, func(tx DBTX) error {
		_, err := st.Publish(ctx, tx, 0, d.ID, "alice", func(_ context.Context, _ DBTX, cur Definition) error {
			if cur.Status != statusDraft {
				t.Errorf("gate cur.Status = %q, want DRAFT", cur.Status)
			}
			if !bytes.Equal(cur.DSL, d.DSL) {
				t.Errorf("gate cur.DSL ≠ 草稿内容——锁定读必须与闸对象同源")
			}
			return sentinel
		})
		return err
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Publish gate abort = %v, want sentinel", err)
	}
	after, err := st.GetByID(ctx, dbh, 0, d.ID)
	if err != nil || after.Status != statusDraft || after.Version != nil {
		t.Fatalf("after gate abort = %s %+v (err %v), want DRAFT with nil version", after.Status, after, err)
	}

	// 放行分支:闸所见字节 == 发布产物字节;中止尝试不耗版本号(v1)。
	var gateSaw []byte
	var pub Definition
	err = WithTx(ctx, dbh, func(tx DBTX) error {
		var err error
		pub, err = st.Publish(ctx, tx, 0, d.ID, "alice", func(_ context.Context, _ DBTX, cur Definition) error {
			gateSaw = append([]byte(nil), cur.DSL...)
			return nil
		})
		return err
	})
	if err != nil {
		t.Fatalf("publish with gate: %v", err)
	}
	if !bytes.Equal(gateSaw, pub.DSL) {
		t.Fatalf("gate saw %d bytes, published %d — 闸与翻转必须同一份锁定读", len(gateSaw), len(pub.DSL))
	}
	if pub.Version == nil || *pub.Version != 1 {
		t.Fatalf("version = %v, want 1(中止尝试不耗号)", pub.Version)
	}
}

// TestStorePublishRowLockBlocksSmuggle 竞态钉(T20.3):闸在 Publish 事务
// 内持行锁期间,第二连接改写草案走私 mode=all 必然被 FOR UPDATE 行锁
// 串行化——要么赶在锁定读前提交(闸对那份内容判定),要么等事务提交后
// 才拿到锁(行已非 DRAFT → ErrConflict)。走私失败是确定性结局,不依赖
// 交错时序。
func TestStorePublishRowLockBlocksSmuggle(t *testing.T) {
	st, dbh := newTestStore(t)
	ctx := context.Background()
	name := uniqName("lockrace")
	d, err := createDraft(t, st, dbh, dslDoc(name), "alice")
	if err != nil {
		t.Fatalf("createDraft: %v", err)
	}
	smuggled := make(chan error, 1)
	var gateSaw []byte
	var pub Definition
	err = WithTx(ctx, dbh, func(tx DBTX) error {
		var err error
		pub, err = st.Publish(ctx, tx, 0, d.ID, "alice", func(ctx context.Context, _ DBTX, cur Definition) error {
			gateSaw = append([]byte(nil), cur.DSL...)
			// 闸挂起期间发起走私:第二连接 UpdateDraft 引入 mode=all,
			// 被本事务锁定读的行锁挡在提交外。
			go func() {
				smuggled <- WithTx(ctx, dbh, func(tx2 DBTX) error {
					_, err := st.UpdateDraft(ctx, tx2, 0, d.ID, gateAllDoc(name), "mallory")
					return err
				})
			}()
			time.Sleep(200 * time.Millisecond) // 走私方在此窗内必须阻塞在行锁
			select {
			case err := <-smuggled:
				t.Fatalf("走私在闸持锁期间完成(err=%v)— 锁定读未先于闸持有行锁", err)
			default:
			}
			return nil
		})
		return err
	})
	if err != nil {
		t.Fatalf("publish under smuggling attempt: %v", err)
	}
	if usesAll, perr := dslUsesApprovalAll(pub.DSL); perr != nil || usesAll {
		t.Fatalf("published DSL uses mode=all = %v (err %v) — 走私内容进入了发布面", usesAll, perr)
	}
	if !bytes.Equal(gateSaw, pub.DSL) {
		t.Fatalf("gate saw %d bytes, published %d — 闸与翻转必须同一份锁定读", len(gateSaw), len(pub.DSL))
	}
	select {
	case err := <-smuggled:
		if err == nil {
			t.Fatalf("smuggled UpdateDraft succeeded — mode=all 绕过闸落库(结构性破绽)")
		}
		if !errors.Is(err, ErrConflict) {
			t.Logf("smuggle blocked with non-conflict error(锁等待超时类): %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("smuggling UpdateDraft 未在提交后返回(行锁未释放?)")
	}
}

func TestStoreArchive(t *testing.T) {
	st, dbh := newTestStore(t)
	ctx := context.Background()
	name := uniqName("arch")
	d1, err := createDraft(t, st, dbh, dslDoc(name), "alice")
	if err != nil {
		t.Fatalf("CreateDraft#1: %v", err)
	}
	d2, err := createDraft(t, st, dbh, dslDoc(name), "alice")
	if err != nil {
		t.Fatalf("CreateDraft#2: %v", err)
	}
	p1, err := st.PublishUngated(ctx, dbh, 0, d1.ID, "alice")
	if err != nil {
		t.Fatalf("Publish#1: %v", err)
	}
	if _, err := st.PublishUngated(ctx, dbh, 0, d2.ID, "alice"); err != nil {
		t.Fatalf("Publish#2: %v", err)
	}
	arch, err := st.Archive(ctx, dbh, 0, name, 2, "admin")
	if err != nil {
		t.Fatalf("Archive(name,2): %v", err)
	}
	if arch.Status != "ARCHIVED" || arch.UpdatedBy != "admin" {
		t.Fatalf("archived = %+v", arch)
	}
	// 归档最新版后,默认触发解析回退到上一个 PUBLISHED(一次后向扫描)。
	fallback, err := st.LatestPublished(ctx, dbh, 0, name)
	if err != nil {
		t.Fatalf("LatestPublished after archive: %v", err)
	}
	if fallback.ID != p1.ID {
		t.Fatalf("fallback = %d, want %d", fallback.ID, p1.ID)
	}
	if _, err := st.Archive(ctx, dbh, 0, name, 1, "admin"); err != nil {
		t.Fatalf("Archive(name,1): %v", err)
	}
	if _, err := st.LatestPublished(ctx, dbh, 0, name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("all-archived LatestPublished err = %v, want ErrNotFound", err)
	}
	if _, err := st.Archive(ctx, dbh, 0, name, 1, "admin"); !errors.Is(err, ErrConflict) {
		t.Fatalf("re-archive err = %v, want ErrConflict", err)
	}
	if _, err := st.Archive(ctx, dbh, 0, name, 3, "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("archive unpublished version err = %v, want ErrNotFound", err)
	}
	if _, err := st.Archive(ctx, dbh, 0, "ghost-"+name, 1, "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown name err = %v, want ErrNotFound", err)
	}
}

func TestStoreList(t *testing.T) {
	st, dbh := newTestStore(t)
	ctx := context.Background()
	name := uniqName("list")
	var lastID uint64
	for i := 0; i < 3; i++ {
		d, err := createDraft(t, st, dbh, dslDoc(fmt.Sprintf("%s-%d", name, i)), "alice")
		if err != nil {
			t.Fatalf("CreateDraft#%d: %v", i, err)
		}
		lastID = d.ID
	}
	publishable, err := createDraft(t, st, dbh, dslDoc(name+"-pub"), "alice")
	if err != nil {
		t.Fatalf("CreateDraft pub: %v", err)
	}
	if _, err := st.PublishUngated(ctx, dbh, 0, publishable.ID, "alice"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	items, total, err := st.List(ctx, dbh, 0, "DRAFT", 2, 0)
	if err != nil {
		t.Fatalf("List DRAFT: %v", err)
	}
	if total != 3 || len(items) != 2 {
		t.Fatalf("List DRAFT first page = %d items, total %d; want 2, 3", len(items), total)
	}
	if items[0].ID != lastID {
		t.Fatalf("newest first violated: head = %d, want %d", items[0].ID, lastID)
	}
	page2, total, err := st.List(ctx, dbh, 0, "DRAFT", 2, 2)
	if err != nil {
		t.Fatalf("List DRAFT page2: %v", err)
	}
	if total != 3 || len(page2) != 1 {
		t.Fatalf("page2 = %d items, total %d; want 1, 3", len(page2), total)
	}
	all, total, err := st.List(ctx, dbh, 0, "", 100, 0)
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if total != 4 || len(all) != 4 {
		t.Fatalf("List all = %d items, total %d; want 4, 4", len(all), total)
	}
	for _, it := range all {
		if it.Status == "DRAFT" && it.Version != nil {
			t.Fatalf("draft with version: %+v", it)
		}
	}
}

// TestDefinitionTenantIsolation(D-t112):定义命名空间租户化的 store 级
// 锚——同名跨租户并存(uk 三列化)、各自版本号独立(MAX 作用域)、
// 读/写/发布/归档的跨租户面全 404(与「不存在」同面,不泄露存在性)。
// c4 的 HTTP 层对抗矩阵在 store 谓词之上,本测试是谓词本身的落盘证据。
func TestDefinitionTenantIsolation(t *testing.T) {
	st, dbh := newTestStore(t)
	ctx := context.Background()
	// 测试租户 7(镜像 0011 种子形态,幂等重放)。
	if _, err := dbh.Exec(
		`INSERT INTO tenants (id, slug, name) VALUES (7, 't-seven', 'T Seven') ON DUPLICATE KEY UPDATE id = id`); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	name := uniqName("tenant-iso")
	d0, err := st.CreateDraft(ctx, dbh, 0, dslDoc(name), "alice")
	if err != nil {
		t.Fatalf("platform draft: %v", err)
	}
	d7, err := st.CreateDraft(ctx, dbh, 7, dslDoc(name), "bob")
	if err != nil {
		t.Fatalf("tenant draft: %v", err)
	}
	p0, err := st.PublishUngated(ctx, dbh, 0, d0.ID, "alice")
	if err != nil {
		t.Fatalf("platform publish: %v", err)
	}
	p7, err := st.PublishUngated(ctx, dbh, 7, d7.ID, "bob")
	if err != nil {
		t.Fatalf("tenant publish: %v", err)
	}
	// 版本号作用域 = 租户内:两个 v1 并存。
	if p0.Version == nil || *p0.Version != 1 || p7.Version == nil || *p7.Version != 1 {
		t.Fatalf("per-tenant versioning: p0=%v p7=%v, want 各自 v1", p0.Version, p7.Version)
	}

	// 租户视图:LatestPublished / GetVersion / ListVersions 各归各。
	l0, err := st.LatestPublished(ctx, dbh, 0, name)
	if err != nil || l0.ID != p0.ID {
		t.Fatalf("latest(0) = %v/%v, want p0", l0.ID, err)
	}
	l7, err := st.LatestPublished(ctx, dbh, 7, name)
	if err != nil || l7.ID != p7.ID {
		t.Fatalf("latest(7) = %v/%v, want p7", l7.ID, err)
	}
	if v, err := st.GetVersion(ctx, dbh, 0, name, 1); err != nil || v.ID != p0.ID {
		t.Fatalf("getversion(0) = %v/%v, want p0", v.ID, err)
	}
	vs7, err := st.ListVersions(ctx, dbh, 7, name)
	if err != nil || len(vs7) != 1 || vs7[0].ID != p7.ID {
		t.Fatalf("listversions(7) = %d 行/%v, want p7 only", len(vs7), err)
	}

	// 跨租户点查 = 404。
	if _, err := st.GetByID(ctx, dbh, 7, d0.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross GetByID = %v, want ErrNotFound", err)
	}
	// 跨租户写 = 404(草稿面:租户 7 的作者改不了平台草稿)。
	d0b, err := st.CreateDraft(ctx, dbh, 0, dslDoc(name), "alice")
	if err != nil {
		t.Fatalf("platform draft b: %v", err)
	}
	if _, err := st.UpdateDraft(ctx, dbh, 7, d0b.ID, dslDoc(name), "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross UpdateDraft = %v, want ErrNotFound", err)
	}
	if _, err := st.PublishUngated(ctx, dbh, 7, d0b.ID, "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross Publish = %v, want ErrNotFound", err)
	}
	// 跨租户 Archive:name 只在平台存在时,租户 7 按名归档 = 404
	//(name 双方共存时 (7,name,1) 命中租户 7 自己的 p7,是合法同租户操作)。
	soloName := uniqName("tenant-iso-solo")
	ds0, err := st.CreateDraft(ctx, dbh, 0, dslDoc(soloName), "alice")
	if err != nil {
		t.Fatalf("platform solo draft: %v", err)
	}
	if _, err := st.PublishUngated(ctx, dbh, 0, ds0.ID, "alice"); err != nil {
		t.Fatalf("platform solo publish: %v", err)
	}
	if _, err := st.Archive(ctx, dbh, 7, soloName, 1, "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross Archive = %v, want ErrNotFound", err)
	}

	// 列表互不可见(共享 schema 下 total 断言需清场:newTestStore 已清;
	// d7 已发布,补一份租户 7 在册草稿使 total7 非零)。
	d7b, err := st.CreateDraft(ctx, dbh, 7, dslDoc(name), "bob")
	if err != nil {
		t.Fatalf("tenant draft b: %v", err)
	}
	items0, total0, err := st.List(ctx, dbh, 0, "DRAFT", 100, 0)
	if err != nil {
		t.Fatalf("list(0): %v", err)
	}
	for _, d := range items0 {
		if d.ID == d7b.ID {
			t.Fatalf("list(0) 泄露租户 7 草稿 %d", d7b.ID)
		}
	}
	items7, total7, err := st.List(ctx, dbh, 7, "DRAFT", 100, 0)
	if err != nil {
		t.Fatalf("list(7): %v", err)
	}
	for _, d := range items7 {
		if d.ID == d0b.ID {
			t.Fatalf("list(7) 泄露平台草稿 %d", d0b.ID)
		}
	}
	if total0 == 0 || total7 == 0 {
		t.Fatalf("totals = %d/%d, want 双方各有草稿行", total0, total7)
	}
}

// TestStorePublishToolAllowlistMCPNames:T13.2 面——MCP 声明名经
// registry.Names() 进发布白名单(名字面语义与内置名同判;动态发现无
// 注入面)。需 t13 MySQL(不可达自动 skip)。
func TestStorePublishToolAllowlistMCPNames(t *testing.T) {
	st, dbh := newTestStore(t)
	ctx := context.Background()
	mcpDSL := func(name, tool string) []byte {
		return fmt.Appendf(nil, `{"dsl":"1.0","name":%q,
"nodes":[{"id":"w","type":"tool","tool":%q,"inputs":{"city":"berlin"},"outputs":{"content_text":"string"}}],
"edges":[],
"outputs":{"s":"{{nodes.w.output.content_text}}"}}`, name, tool)
	}
	allowed, err := createDraft(t, st, dbh, mcpDSL(uniqName("mcp-ok"), "mcp_weather_lookup"), "alice")
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if _, err := st.PublishUngated(ctx, dbh, 0, allowed.ID, "alice", "mcp_weather_lookup"); err != nil {
		t.Fatalf("publish mcp-declared tool with matching allowlist: %v", err)
	}
	blocked, err := createDraft(t, st, dbh, mcpDSL(uniqName("mcp-no"), "mcp_undeclared"), "alice")
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if _, err := st.PublishUngated(ctx, dbh, 0, blocked.ID, "alice", "mcp_weather_lookup"); err == nil || !HasIssueCode(err, dsl.IssueToolNotAllowed) {
		t.Fatalf("publish undeclared mcp tool err = %v, want tool_not_allowed", err)
	}
}

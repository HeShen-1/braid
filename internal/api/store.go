// definition 持久化原语:一行 = 一个版本(0001_init DDL 契约)。
// 写入路径全部先过 dsl.Parse(双层校验;发布路径叠加工具白名单)再落库,
// dsl_hash = SHA-256(存储形态字节——MySQL JSON 列规范化后读回定稿),
// 执行侧比对即漂移检测。状态迁移全部是带状态谓词的 guarded UPDATE
// (affected rows = 0 ⇒ 重读状态判真伪),不存在裸 UPDATE;写路径起手
// 锁定读(FOR UPDATE)防校验/翻转 TOCTOU。
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
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"braid/internal/engine/dsl"
)

// 状态词表(与 DDL ENUM 逐字一致;追加状态 = DDL ENUM + 本处同步)。
const (
	statusDraft     = "DRAFT"
	statusPublished = "PUBLISHED"
	statusArchived  = "ARCHIVED"
)

// 包级 sentinel(仓内风格:英文消息、包名前缀;调用方只许 errors.Is /
// errors.As 分类)。HTTP 映射见 handlers.go:NotFound→404、Conflict→409、
// InvalidDSL→400、NameMismatch→400。
var (
	// ErrNotFound 目标行不存在(未知 id / name / version)。
	ErrNotFound = errors.New("api: not found")
	// ErrConflict 状态机违例或唯一键冲突(重复发布、更新非草稿、
	// 并发发布 uk 兜底、snowflake 重号 PK)。
	ErrConflict = errors.New("api: conflict")
	// ErrInvalidDSL DSL 未通过双层校验或工具白名单;结构化发现经
	// *InvalidDSLError 携带。
	ErrInvalidDSL = errors.New("api: invalid dsl")
	// ErrNameMismatch 更新草稿时 DSL $.name 与行 name 不一致
	// (改名 = 新工作流,DDL 注释契约)。
	ErrNameMismatch = errors.New("api: dsl name mismatch")
)

// InvalidDSLError 聚合一次 dsl.Parse 的全部语义/结构发现(不 fail-fast,
// 作者一次修完;auth.CredentialsError 同款 sentinel+typed 形态)。
type InvalidDSLError struct {
	Issues []dsl.Issue
}

func (e *InvalidDSLError) Error() string {
	var b strings.Builder
	b.WriteString(ErrInvalidDSL.Error())
	for _, i := range e.Issues {
		b.WriteString("; ")
		b.WriteString(i.Error())
	}
	return b.String()
}

func (e *InvalidDSLError) Unwrap() error { return ErrInvalidDSL }

// IDGen 是 definition 行 Snowflake 主键的最小生成面(*snowflake.Generator
// 结构性满足);api 不直接 import snowflake 于 store 面,测试可换桩
// (auth.IDGen 同款)。
type IDGen interface {
	Next() (uint64, error)
}

// Store 是 workflow_definition 的持久化原语集。无状态(连接与事务由
// 调用方持有),并发安全。
type Store struct {
	ids IDGen
}

// NewStore 构造 store;ids 生成草稿行 id(发布前可知,供钉版引用)。
func NewStore(ids IDGen) *Store {
	return &Store{ids: ids}
}

// Definition 是 workflow_definition 一行的应用侧模型。
type Definition struct {
	ID          uint64
	Name        string
	Version     *uint32 // NULL = 草稿
	Status      string
	DSLVersion  string
	Description string
	DSL         []byte // 原文字节(真相源,Q3);dsl_hash 即其 SHA-256
	DSLHash     string
	CreatedBy   string
	UpdatedBy   string
	PublishedBy *string    // DRAFT 阶段 NULL(与 published_at 同受 CHECK 钉死)
	PublishedAt *time.Time // DB 端 NOW(3),会话 UTC
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const defCols = "id, name, version, dsl_version, status, description, dsl, dsl_hash, created_by, updated_by, published_by, published_at, created_at, updated_at"

// parseDraft 校验 DSL 草稿形态(无白名单:白名单只在发布点收紧,
// dsl.WithToolAllowlist 语义)并返回规范化字段。
func parseDraft(doc []byte) (*dsl.Workflow, error) {
	wf, err := dsl.Parse(doc)
	if err != nil {
		return nil, asInvalidDSL(err)
	}
	return wf, nil
}

// asInvalidDSL 把 dsl.Parse 的失败统一成 *InvalidDSLError(结构层错误
// 原样包成单条 Issue)。
func asInvalidDSL(err error) error {
	var pe *dsl.ParseError
	if errors.As(err, &pe) {
		return &InvalidDSLError{Issues: pe.Issues}
	}
	return &InvalidDSLError{Issues: []dsl.Issue{{Code: "invalid_document", Detail: err.Error()}}}
}

// hashDSL 计算 dsl_hash:SHA-256(字节,hex)。哈的对象是**读回存储字节
// 的 compact 形态**(T3.6 MED① 修复):MySQL JSON 列会规范化原文(分隔符
// 补空格、键序重排),读回字节与任何 JSON 传输形态都不同——直接锚定读回
// 字节则 API 消费方永远无法复算(JSON 编码必 compact)。锚定 compact 后,
// 执行侧漂移检测(compact(读回字节))与 API 消费方(compact(响应 dsl))
// 复算同一字节,全链一致。
func hashDSL(doc []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, doc); err != nil {
		buf.Reset() // 读回字节必为合法 JSON;防御回退哈原字节
		buf.Write(doc)
	}
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:])
}

// HasIssueCode 报告 err 的 DSL 发现里是否含指定码(InvalidDSLError 不
// 保留 *dsl.ParseError 包装链,api 侧自查;dsl.HasIssue 的本地对应)。
func HasIssueCode(err error, code string) bool {
	var ide *InvalidDSLError
	if !errors.As(err, &ide) {
		return false
	}
	for _, i := range ide.Issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

// isMySQLConflict 报告 err 是否 MySQL 冲突类错误:1062(唯一键冲突——
// 并发发布 uk 兜底、snowflake 重号 PK)或 1213(死锁,InnoDB 回滚一方;
// 重试安全)。两者都映射 ErrConflict(可重试的显式失败)。
func isMySQLConflict(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1062 || me.Number == 1213)
}

// CreateDraft 校验 DSL 并写入一行草稿(version NULL,uk 多 NULL ⇒ 同名
// 多草稿共存)。写入是两步(INSERT + 读回定稿 hash,见 hashDSL),调用方
// 必须经 WithTx 包裹保证原子。返回落库后的完整行(时间戳取库端真源)。
func (s *Store) CreateDraft(ctx context.Context, q DBTX, doc []byte, actor string) (Definition, error) {
	wf, err := parseDraft(doc)
	if err != nil {
		return Definition{}, err
	}
	id, err := s.ids.Next()
	if err != nil {
		return Definition{}, fmt.Errorf("api: id gen: %w", err)
	}
	res, err := q.ExecContext(ctx, `
INSERT INTO workflow_definition
  (id, name, version, dsl_version, status, description, dsl, dsl_hash, created_by, updated_by)
VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?, ?)`,
		id, wf.Name, wf.DSL, statusDraft, wf.Description, []byte(doc), hashDSL(doc), actor, actor)
	if err != nil {
		if isMySQLConflict(err) {
			return Definition{}, fmt.Errorf("%w: insert definition %d", ErrConflict, id)
		}
		return Definition{}, fmt.Errorf("api: insert definition: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Definition{}, fmt.Errorf("api: insert definition: affected %d rows", n)
	}
	if err := s.finalizeHash(ctx, q, id); err != nil {
		return Definition{}, err
	}
	return s.GetByID(ctx, q, id)
}

// finalizeHash 用读回的存储形态字节定稿 dsl_hash(hashDSL 契约)。
func (s *Store) finalizeHash(ctx context.Context, q DBTX, id uint64) error {
	var stored []byte
	if err := q.QueryRowContext(ctx,
		"SELECT dsl FROM workflow_definition WHERE id = ?", id).Scan(&stored); err != nil {
		return fmt.Errorf("api: read back definition %d: %w", id, err)
	}
	if _, err := q.ExecContext(ctx,
		"UPDATE workflow_definition SET dsl_hash = ? WHERE id = ?", hashDSL(stored), id); err != nil {
		return fmt.Errorf("api: finalize dsl_hash of %d: %w", id, err)
	}
	return nil
}

// GetByID 按 id 读行(任意状态;读路径与 404 判定)。
func (s *Store) GetByID(ctx context.Context, q DBTX, id uint64) (Definition, error) {
	row := q.QueryRowContext(ctx, "SELECT "+defCols+" FROM workflow_definition WHERE id = ?", id)
	def, err := scanDefinition(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Definition{}, fmt.Errorf("%w: definition %d", ErrNotFound, id)
	}
	return def, err
}

// getByIDForUpdate 是写路径的锁定读(SELECT ... FOR UPDATE,当前读):
// RR 快照读 + guarded UPDATE 只钉状态不钉内容 ⇒ 校验的是陈旧快照
// (TOCTOU:并发 UpdateDraft 提交的新内容会绕过发布白名单直接落
// PUBLISHED)。锁行串行化 update/publish 交错——败方按状态谓词显式
// 失败(engine.nodecas 同款仓内纪律)。
func (s *Store) getByIDForUpdate(ctx context.Context, q DBTX, id uint64) (Definition, error) {
	row := q.QueryRowContext(ctx,
		"SELECT "+defCols+" FROM workflow_definition WHERE id = ? FOR UPDATE", id)
	def, err := scanDefinition(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Definition{}, fmt.Errorf("%w: definition %d", ErrNotFound, id)
	}
	return def, err
}

// GetVersion 按 (name, version) 读已发布行。
func (s *Store) GetVersion(ctx context.Context, q DBTX, name string, version uint32) (Definition, error) {
	row := q.QueryRowContext(ctx,
		"SELECT "+defCols+" FROM workflow_definition WHERE name = ? AND version = ?", name, version)
	def, err := scanDefinition(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Definition{}, fmt.Errorf("%w: %q v%d", ErrNotFound, name, version)
	}
	return def, err
}

// LatestPublished 解析默认触发目标:该 name 最新 PUBLISHED 版本
// (idx_def_name_status_version 一次后向扫描;全归档 ⇒ ErrNotFound =
// 禁新触发)。T5.5 execution 创建从本方法取钉住四件套。
func (s *Store) LatestPublished(ctx context.Context, q DBTX, name string) (Definition, error) {
	row := q.QueryRowContext(ctx,
		"SELECT "+defCols+" FROM workflow_definition WHERE name = ? AND status = ? ORDER BY version DESC LIMIT 1",
		name, statusPublished)
	def, err := scanDefinition(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Definition{}, fmt.Errorf("%w: no published version of %q", ErrNotFound, name)
	}
	return def, err
}

// ListVersions 列某 name 的全部已发布版本(升序;草稿不占号,走 List)。
func (s *Store) ListVersions(ctx context.Context, q DBTX, name string) ([]Definition, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT "+defCols+" FROM workflow_definition WHERE name = ? AND version IS NOT NULL ORDER BY version",
		name)
	if err != nil {
		return nil, fmt.Errorf("api: list versions of %q: %w", name, err)
	}
	defer rows.Close() //nolint:errcheck // 只读路径,关闭错误无处置义
	return collectDefinitions(rows)
}

// List 分页列出定义(status 空 = 全部;updated_at DESC, id DESC 走
// idx_def_status_updated)。返回 (当前页, 满足条件的总行数)。
func (s *Store) List(ctx context.Context, q DBTX, status string, limit, offset int) ([]Definition, int, error) {
	where := ""
	args := make([]any, 0, 2)
	if status != "" {
		where = " WHERE status = ?"
		args = append(args, status)
	}
	var total int
	if err := q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM workflow_definition"+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("api: count definitions: %w", err)
	}
	rows, err := q.QueryContext(ctx,
		"SELECT "+defCols+" FROM workflow_definition"+where+
			" ORDER BY updated_at DESC, id DESC LIMIT ? OFFSET ?",
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("api: list definitions: %w", err)
	}
	defer rows.Close() //nolint:errcheck // 只读路径,关闭错误无处置义
	items, err := collectDefinitions(rows)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// UpdateDraft 更新草稿 DSL(重校验、重算哈希;仅 DRAFT 可更)。DSL
// $.name 必须与行 name 一致——改名 = 新工作流。写入是两步(UPDATE +
// 读回定稿 hash),调用方必须经 WithTx 包裹。
func (s *Store) UpdateDraft(ctx context.Context, q DBTX, id uint64, doc []byte, actor string) (Definition, error) {
	cur, err := s.getByIDForUpdate(ctx, q, id)
	if err != nil {
		return Definition{}, err
	}
	if cur.Status != statusDraft {
		return Definition{}, fmt.Errorf("%w: definition %d is %s, not DRAFT", ErrConflict, id, cur.Status)
	}
	wf, err := parseDraft(doc)
	if err != nil {
		return Definition{}, err
	}
	if wf.Name != cur.Name {
		return Definition{}, fmt.Errorf("%w: dsl name %q vs row name %q (rename = new workflow)",
			ErrNameMismatch, wf.Name, cur.Name)
	}
	res, err := q.ExecContext(ctx, `
UPDATE workflow_definition
SET dsl_version = ?, description = ?, dsl = ?, dsl_hash = ?, updated_by = ?
WHERE id = ? AND status = ?`,
		wf.DSL, wf.Description, []byte(doc), hashDSL(doc), actor, id, statusDraft)
	if err != nil {
		return Definition{}, fmt.Errorf("api: update definition %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// changed-rows 语义(未设 clientFoundRows):赋值与现值全同的
		// 幂等保存(存储形态字节回放)affected=0。重读状态判真伪:
		// 仍 DRAFT = 幂等成功(返回现状);否则才是并发状态翻转。
		after, err := s.GetByID(ctx, q, id)
		if err != nil {
			return Definition{}, err
		}
		if after.Status != statusDraft {
			return Definition{}, fmt.Errorf("%w: definition %d no longer DRAFT", ErrConflict, id)
		}
		return after, nil
	}
	if err := s.finalizeHash(ctx, q, id); err != nil {
		return Definition{}, err
	}
	return s.GetByID(ctx, q, id)
}

// Publish 把草稿翻成 PUBLISHED:发布路径叠加工具白名单校验
// (allowlist 为名单内容;空调用 = 名单空集 = 禁用一切工具),版本号
// 取 MAX(version)+1。调用方必须经 WithTx 包裹(读号 + 翻转原子);
// 同名并发发布的间隙锁竞态由 uk_def_name_version 兜底 ⇒ 1062 ⇒
// ErrConflict(显式失败是并发闸,不是错误)。published_at 用库端
// NOW(3)(会话 time_zone '+00:00' ⇒ UTC,DDL 契约)。
func (s *Store) Publish(ctx context.Context, q DBTX, id uint64, actor string, allowlist ...string) (Definition, error) {
	// 锁定读:白名单校验的对象必须是即将被翻转的那份内容(getByIDForUpdate
	// 注释——快照读 + 只钉状态的 guarded UPDATE 会发布未校验的新内容)。
	cur, err := s.getByIDForUpdate(ctx, q, id)
	if err != nil {
		return Definition{}, err
	}
	if cur.Status != statusDraft {
		return Definition{}, fmt.Errorf("%w: definition %d is %s, not DRAFT", ErrConflict, id, cur.Status)
	}
	if _, err := dsl.Parse(cur.DSL, dsl.WithToolAllowlist(allowlist...)); err != nil {
		return Definition{}, asInvalidDSL(err)
	}
	var next sql.NullInt64
	if err := q.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) + 1 FROM workflow_definition WHERE name = ? AND version IS NOT NULL",
		cur.Name).Scan(&next); err != nil {
		return Definition{}, fmt.Errorf("api: next version of %q: %w", cur.Name, err)
	}
	res, err := q.ExecContext(ctx, `
UPDATE workflow_definition
SET version = ?, status = ?, published_by = ?, published_at = NOW(3), updated_by = ?
WHERE id = ? AND status = ?`,
		next.Int64, statusPublished, actor, actor, id, statusDraft)
	if err != nil {
		if isMySQLConflict(err) {
			return Definition{}, fmt.Errorf("%w: concurrent publish of %q v%d", ErrConflict, cur.Name, next.Int64)
		}
		return Definition{}, fmt.Errorf("api: publish definition %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Definition{}, fmt.Errorf("%w: definition %d no longer DRAFT", ErrConflict, id)
	}
	return s.GetByID(ctx, q, id)
}

// Archive 把 PUBLISHED 归档(禁新触发;单向不可逆,运行中实例不受影响)。
func (s *Store) Archive(ctx context.Context, q DBTX, name string, version uint32, actor string) (Definition, error) {
	cur, err := s.GetVersion(ctx, q, name, version)
	if err != nil {
		return Definition{}, err
	}
	if cur.Status != statusPublished {
		return Definition{}, fmt.Errorf("%w: %q v%d is %s, not PUBLISHED", ErrConflict, name, version, cur.Status)
	}
	res, err := q.ExecContext(ctx, `
UPDATE workflow_definition
SET status = ?, updated_by = ?
WHERE id = ? AND status = ?`,
		statusArchived, actor, cur.ID, statusPublished)
	if err != nil {
		return Definition{}, fmt.Errorf("api: archive %q v%d: %w", name, version, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Definition{}, fmt.Errorf("%w: %q v%d no longer PUBLISHED", ErrConflict, name, version)
	}
	return s.GetByID(ctx, q, cur.ID)
}

// rowScanner 收窄 *sql.Row / *sql.Rows 的公共扫描面。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanDefinition(r rowScanner) (Definition, error) {
	var (
		d           Definition
		version     sql.NullInt64
		dslBytes    []byte
		publishedBy sql.NullString
		publishedAt sql.NullTime
		createdAt   sql.NullTime
		updatedAt   sql.NullTime
	)
	if err := r.Scan(&d.ID, &d.Name, &version, &d.DSLVersion, &d.Status, &d.Description,
		&dslBytes, &d.DSLHash, &d.CreatedBy, &d.UpdatedBy, &publishedBy, &publishedAt,
		&createdAt, &updatedAt); err != nil {
		return Definition{}, err
	}
	if version.Valid {
		v := uint32(version.Int64)
		d.Version = &v
	}
	d.DSL = dslBytes
	if publishedBy.Valid {
		pb := publishedBy.String
		d.PublishedBy = &pb
	}
	if publishedAt.Valid {
		pa := publishedAt.Time
		d.PublishedAt = &pa
	}
	d.CreatedAt = createdAt.Time
	d.UpdatedAt = updatedAt.Time
	return d, nil
}

func collectDefinitions(rows *sql.Rows) ([]Definition, error) {
	var out []Definition
	for rows.Next() {
		d, err := scanDefinition(rows)
		if err != nil {
			return nil, fmt.Errorf("api: scan definition: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("api: iterate definitions: %w", err)
	}
	return out, nil
}

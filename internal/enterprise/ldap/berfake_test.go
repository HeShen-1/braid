package ldap

// 进程内最小 BER 目录 fake(T20.5 时域残差测试基建):127.0.0.1:0
// 真 TCP 监听,按消息序服务 bind → search → bind,支持按操作类型注入
// 差异化延迟与搜索命中数——时域差测试需要把「目录侧成本」变成可控
// 变量,真容器(osixia)不暴露延迟旋钮,slapo-delay 只延迟 search
// 不延迟 bind,均不可用(e2e 基建结论,ldap_e2e_test.go 同源)。
//
// 为什么手搓 BER 而不 import go-asn1-ber:该库现为 indirect 依赖
// (go.mod),测试 import 即翻 direct = 触碰铁律面(go.mod 变更的 PR
// 失去自动合并,AGENTS.md);本 fake 只需三种应答形状(BindResponse /
// SearchResultEntry / SearchResultDone),~百行内自足。客户端侧报文
// 只解析到 [messageID, protocolOp tag],bind 再多解析一层 DN 用于
// 区分服务 bind 与用户 bind——search filter 不解析(命中数由配置
// 直接给定,fake 不需要理解 filter)。
//
// 线协议事实(go-ldap v3.4.14 实证):Close() 不发 UnbindRequest,
// 直接断 TCP(fake 读到 EOF 即收线);BindRequest = [INTEGER 版本]
// [OCTET STRING DN][ctx0 密码]。明文 ldap:// 即可——StartTLS/ldaps
// 不在本测试面(D-t176 同判)。

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeDirOpt 是 fake 目录的行为配置:每类操作的处理延迟(模拟网络
// 往返 + 目录侧验证成本)与搜索命中列表。用户 bind 的应答码可配
// (0 = 成功 / 49 = invalidCredentials)——wrong_password 臂靠真 49
// 形状驱动,与生产路径同判据(IsErrorWithCode)。
type fakeDirOpt struct {
	svcDN          string        // 服务账号 DN;其余 DN 一律按用户 bind 处理
	svcBindDelay   time.Duration // 服务 bind 处理延迟(探测与登录连接共用)
	searchDelay    time.Duration // search 处理延迟(含零命中与命中两形态)
	userBindDelay  time.Duration // 用户 bind 处理延迟(模拟目录侧口令验证)
	userBindResult int           // 用户 bind 应答码:0 或 49
	entries        []string      // search 命中 DN 列表(空 = 零命中)
	// svcBindFailFrom 非零时,全局第 ≥ 该序号(1 起)的服务 bind 回 49
	// 而非 0——模拟「个别实现/策略拒同连接 rebind」目录,驱动
	// measureLevelPad 的 fail-loud 路径(对抗审计 MEDIUM② 修)。计数
	// 跨连接全局(探测连接 4 连 bind 形态下,1 = 全拒,2 = 首过后续拒)。
	svcBindFailFrom int
}

// fakeOp 是 fake 的记账条目:操作类型("bind"/"search")与 bind 的
// 目标 DN(search 为空)。自锁结构断言的判据面——unknown_user 路径
// 全程零用户 DN bind 恰一次服务 bind,直接读账本钉死。
type fakeOp struct {
	kind string
	dn   string
}

const (
	fakeOpBind   = "bind"
	fakeOpSearch = "search"
)

// fakeDirServer 是 fake 目录实例:单监听多连接,连接内按序处理消息
// (登录连接形态 = 服务 bind → search → 用户 bind,恰与生产同序)。
// 每连接设 10s 读写合一 deadline(SetDeadline 同界读写)——客户端
// 自带 5s 绝对界,超界残留连接由 deadline 兜底回收,测试失败时不泄
// 漏 goroutine。
type fakeDirServer struct {
	ln  net.Listener
	opt fakeDirOpt

	mu       sync.Mutex
	ops      []fakeOp
	svcBinds int // 服务 bind 全局序号(svcBindFailFrom 判据)
}

// newFakeDirServer 起监听并进入 accept 循环;生命周期挂 t.Cleanup。
func newFakeDirServer(t *testing.T, opt fakeDirOpt) *fakeDirServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake ldap listen: %v", err)
	}
	s := &fakeDirServer{ln: ln, opt: opt}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // listener closed (cleanup)
			}
			go s.serveConn(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

// Addr 返回可直接填入 LDAPConfig.Addr 的 ldap:// URL。
func (s *fakeDirServer) Addr() string { return "ldap://" + s.ln.Addr().String() }

// Ops 返回记账快照(构造期探测 + 各次 Authenticate 的全部目录交互)。
func (s *fakeDirServer) Ops() []fakeOp {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]fakeOp, len(s.ops))
	copy(out, s.ops)
	return out
}

func (s *fakeDirServer) record(kind, dn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, fakeOp{kind: kind, dn: dn})
}

// nextSvcBindOrdinal 取服务 bind 的全局 1 起序号(svcBindFailFrom 判据)。
func (s *fakeDirServer) nextSvcBindOrdinal() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.svcBinds++
	return s.svcBinds
}

// serveConn 单连接消息循环:读 BER 报文,按 op 类型注入延迟并应答。
func (s *fakeDirServer) serveConn(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(c)
	for {
		_, env, err := berReadPacket(r) // 顶层 LDAPMessage SEQUENCE
		if err != nil {
			return // EOF(客户端 Close 直断)或读超时
		}
		id, opTag, opContent, err := berParseEnvelope(env)
		if err != nil {
			return
		}
		switch opTag {
		case 0x60: // BindRequest [APPLICATION 0]
			dn := berParseBindDN(opContent)
			s.record(fakeOpBind, dn)
			if dn == s.opt.svcDN {
				code := 0
				if s.opt.svcBindFailFrom > 0 && s.nextSvcBindOrdinal() >= s.opt.svcBindFailFrom {
					code = 49
				}
				time.Sleep(s.opt.svcBindDelay)
				_, _ = c.Write(berBindResponse(id, code))
			} else {
				time.Sleep(s.opt.userBindDelay)
				_, _ = c.Write(berBindResponse(id, s.opt.userBindResult))
			}
		case 0x63: // SearchRequest [APPLICATION 3](RFC 4511:bind=0/unbind=2/search=3)
			s.record(fakeOpSearch, "")
			time.Sleep(s.opt.searchDelay)
			for _, dn := range s.opt.entries {
				_, _ = c.Write(berSearchResultEntry(id, dn))
			}
			_, _ = c.Write(berSearchResultDone(id))
		case 0x42: // UnbindRequest [APPLICATION 2] primitive(客户端不发,容忍即收线)
			return
		default:
			return // 未知 op:测试面外,收线让客户端响亮失败
		}
	}
}

// —— 最小 BER 编解码(仅 fake 所需形状;定长形式, indefinite 拒收)——

// berReadPacket 读一个 BER 报文(标识符 + 长度 + 内容)。long-form
// 长度 ≤ 8 字节,总长上限 1MiB(测试面 sanity,防坏长度吞内存)。
func berReadPacket(r io.Reader) (tag byte, content []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	tag = hdr[0]
	n := uint64(0)
	if hdr[1] < 0x80 {
		n = uint64(hdr[1])
	} else {
		k := int(hdr[1] & 0x7f)
		if k == 0 || k > 8 {
			return 0, nil, fmt.Errorf("fake ldap: indefinite/oversized BER length")
		}
		var lb [8]byte
		if _, err = io.ReadFull(r, lb[:k]); err != nil {
			return 0, nil, err
		}
		for _, b := range lb[:k] {
			n = n<<8 | uint64(b)
		}
	}
	if n > 1<<20 {
		return 0, nil, fmt.Errorf("fake ldap: BER packet too large (%d)", n)
	}
	content = make([]byte, n)
	if _, err = io.ReadFull(r, content); err != nil {
		return 0, nil, err
	}
	return tag, content, nil
}

// berAppendPacket 追加一个 BER 报文(long-form 长度按需)。
func berAppendPacket(dst []byte, tag byte, content []byte) []byte {
	dst = append(dst, tag)
	if n := len(content); n < 0x80 {
		dst = append(dst, byte(n))
	} else {
		var lb [8]byte
		i := 0
		for m := n; m > 0; m >>= 8 {
			lb[i] = byte(m)
			i++
		}
		dst = append(dst, 0x80|byte(i))
		for j := i - 1; j >= 0; j-- {
			dst = append(dst, lb[j])
		}
	}
	return append(dst, content...)
}

// berAppendUint 以无符号整数内容追加一个报文(INTEGER/ENUMERATED
// 共用;应答码与 messageID 均 < 128,单字节为主)。
func berAppendUint(dst []byte, tag byte, v uint64) []byte {
	var b [8]byte
	i := 0
	for m := v; ; {
		b[i] = byte(m)
		i++
		m >>= 8
		if m == 0 {
			break
		}
	}
	return berAppendPacket(dst, tag, b[:i])
}

// berMessage 组一个完整 LDAPMessage:SEQUENCE{ messageID, protocolOp }。
func berMessage(id uint64, opTag byte, opContent []byte) []byte {
	var env []byte
	env = berAppendUint(env, 0x02, id)
	env = berAppendPacket(env, opTag, opContent)
	return berAppendPacket(nil, 0x30, env)
}

// berBindResponse 组 BindResponse [APPLICATION 1] = LDAPResult:
// { resultCode, matchedDN(""), diagnosticMessage("") }。
func berBindResponse(id uint64, code int) []byte {
	var op []byte
	op = berAppendUint(op, 0x0A, uint64(code))
	op = berAppendPacket(op, 0x04, nil)
	op = berAppendPacket(op, 0x04, nil)
	return berMessage(id, 0x61, op)
}

// berSearchResultEntry 组 SearchResultEntry [APPLICATION 4]:
// { objectName(DN), attributes(空表——客户端只消费 DN) }。
func berSearchResultEntry(id uint64, dn string) []byte {
	var op []byte
	op = berAppendPacket(op, 0x04, []byte(dn))
	op = berAppendPacket(op, 0x30, nil)
	return berMessage(id, 0x64, op)
}

// berSearchResultDone 组 SearchResultDone [APPLICATION 5] =
// LDAPResult 成功形态。
func berSearchResultDone(id uint64) []byte {
	var op []byte
	op = berAppendUint(op, 0x0A, 0)
	op = berAppendPacket(op, 0x04, nil)
	op = berAppendPacket(op, 0x04, nil)
	return berMessage(id, 0x65, op)
}

// berParseEnvelope 拆顶层 SEQUENCE 内容:[messageID INTEGER,
// protocolOp 报文],返回 messageID、protocolOp 标签与其内容。
func berParseEnvelope(env []byte) (id uint64, opTag byte, opContent []byte, err error) {
	br := bytes.NewReader(env)
	idTag, idContent, err := berReadPacket(br)
	if err != nil || idTag != 0x02 || len(idContent) == 0 {
		return 0, 0, nil, fmt.Errorf("fake ldap: bad messageID TLV")
	}
	for _, b := range idContent {
		id = id<<8 | uint64(b)
	}
	opTag, opContent, err = berReadPacket(br)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("fake ldap: bad protocolOp TLV")
	}
	return id, opTag, opContent, nil
}

// berParseBindDN 从 BindRequest 内容取 DN:跳过版本 INTEGER,读
// 第二个 TLV(OCTET STRING 名字)。
func berParseBindDN(op []byte) string {
	br := bytes.NewReader(op)
	if _, _, err := berReadPacket(br); err != nil { // version
		return ""
	}
	_, dn, err := berReadPacket(br) // name
	if err != nil {
		return ""
	}
	return string(dn)
}

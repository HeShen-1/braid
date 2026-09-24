// Package notifytest 提供 T4.5 SMTP 通知的测试基建:包内 fake SMTP 服务器
// (最小对话桩)。internal/notify 与 internal/worker 的测试共用(redistest/
// sqltest 同款「测试基建独立包」先例)。
package notifytest

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
)

// TB 收窄 testing.TB 到本包所需面(镜像 sqltest.TB;避免测试文件外泄
// testing 依赖)。
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(f func())
}

// SMTP 是最小 SMTP 对话桩:接受 EHLO/MAIL/RCPT/DATA/QUIT;DATA 原文(逐行
// 含信头)按消息记录,供内容断言;未知命令一律 250 宽容(对话桩只关心捕获,
// 不做协议裁决)。扩展旋钮(对抗审计 MEDIUM⑦:生产默认路径的测试锚):
//
//   - AdvertiseTLS:E HLO 广告 STARTTLS 并支持升级(每次 Start 生成的自签
//     证书,IP SAN 127.0.0.1,MinVersion TLS1.2——与 sender 的客户端形态
//     对齐)。置真后 sender 的 StartTLS=true 路径可全握手送达;
//   - AdvertiseAuth:EHLO 广告 AUTH PLAIN 并接受之(AUTH PLAIN 行记录,
//     LastAuth 取回);未广告时收到 AUTH → 502(防测试误绿)。
//
// 两旋钮须在客户端连接前设置(读侧经 mu 同步)。
type SMTP struct {
	ln net.Listener

	AdvertiseTLS  bool
	AdvertiseAuth bool

	// QuitReply 覆盖 QUIT 应答(默认 "221 bye"):非 221 值模拟「DATA 已被
	// 接受后服务端异常关流」——sender 的 QUIT 失败非致命语义(MEDIUM②)的
	// 测试面。
	QuitReply string

	mu       sync.Mutex
	messages []string
	rcpts    [][]string
	auths    []string

	// cert/pool:Start 时一次性生成的自签证书及其信任池(CertPool 导出给
	// 测试注入 notify.Config.TLSRootCAs——客户端验证服务端证书用)。
	cert tls.Certificate
	pool *x509.CertPool
}

// Start 起一个监听 127.0.0.1 随机端口的 fake SMTP(t.Cleanup 自动关)。
func Start(t TB) *SMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("notifytest: listen: %v", err)
	}
	s := &SMTP{ln: ln}
	if s.cert, err = selfSignedCert(); err != nil {
		_ = ln.Close()
		t.Fatalf("notifytest: self-signed cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(s.cert.Certificate[0])
	if err != nil {
		_ = ln.Close()
		t.Fatalf("notifytest: parse leaf cert: %v", err)
	}
	s.pool = x509.NewCertPool()
	s.pool.AddCert(leaf)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go s.handle(conn)
		}
	}()
	return s
}

// CertPool 返回本实例自签证书的信任池(注入 notify.Config.TLSRootCAs,
// 客户端才能通过 STARTTLS 证书验证)。
func (s *SMTP) CertPool() *x509.CertPool {
	return s.pool
}

// Port 返回监听端口(拼 Config 用)。
func (s *SMTP) Port() int {
	return s.ln.Addr().(*net.TCPAddr).Port
}

// Count 返回已捕获的邮件数。
func (s *SMTP) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.messages)
}

// LastMessage 返回最近一封 DATA 原文(无消息时 fatal)。
func (s *SMTP) LastMessage(t TB) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.messages) == 0 {
		t.Fatalf("notifytest: no message captured")
	}
	return s.messages[len(s.messages)-1]
}

// LastRcpts 返回最近一封的信封收件人(去尖括号)。
func (s *SMTP) LastRcpts(t TB) []string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rcpts) == 0 {
		t.Fatalf("notifytest: no rcpt captured")
	}
	return s.rcpts[len(s.rcpts)-1]
}

// LastAuth 返回最近一次 AUTH 命令原文(未发生认证时 fatal)。
func (s *SMTP) LastAuth(t TB) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.auths) == 0 {
		t.Fatalf("notifytest: no auth captured")
	}
	return s.auths[len(s.auths)-1]
}

// selfSignedCert 生成一次性自签证书(IP SAN 127.0.0.1)。
func selfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func (s *SMTP) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	writeLine := func(str string) {
		_, _ = w.WriteString(str + "\r\n")
		_ = w.Flush()
	}
	writeLine("220 fake.test ESMTP")
	var (
		inData   bool
		msgLines []string
		rcpts    []string
	)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				inData = false
				s.mu.Lock()
				s.messages = append(s.messages, strings.Join(msgLines, "\r\n"))
				s.rcpts = append(s.rcpts, rcpts)
				s.mu.Unlock()
				writeLine("250 ok")
			} else {
				msgLines = append(msgLines, line)
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			s.mu.Lock()
			tlsAdv, authAdv := s.AdvertiseTLS, s.AdvertiseAuth
			s.mu.Unlock()
			writeLine("250-fake.test")
			writeLine("250-SIZE 10485760")
			if authAdv {
				writeLine("250-AUTH PLAIN")
			}
			if tlsAdv {
				writeLine("250-STARTTLS")
			}
			writeLine("250 OK")
		case line == "STARTTLS":
			s.mu.Lock()
			tlsAdv := s.AdvertiseTLS
			s.mu.Unlock()
			if !tlsAdv {
				writeLine("502 not advertised")
				continue
			}
			writeLine("220 2.0.0 Ready to start TLS")
			// RFC 3207:同连接就地升级;升级后客户端重新 EHLO,服务端不重发
			// greeting。读写缓冲在明文管道上重建。
			tlsConn := tls.Server(conn, &tls.Config{
				Certificates: []tls.Certificate{s.cert},
				MinVersion:   tls.VersionTLS12,
			})
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			r = bufio.NewReader(conn)
			w = bufio.NewWriter(conn)
			writeLine = func(str string) {
				_, _ = w.WriteString(str + "\r\n")
				_ = w.Flush()
			}
		case strings.HasPrefix(line, "AUTH PLAIN"):
			s.mu.Lock()
			authAdv := s.AdvertiseAuth
			s.auths = append(s.auths, line)
			s.mu.Unlock()
			if !authAdv {
				writeLine("502 not advertised")
				continue
			}
			writeLine("235 ok")
		case strings.HasPrefix(line, "MAIL FROM:"):
			rcpts, msgLines = nil, nil
			writeLine("250 ok")
		case strings.HasPrefix(line, "RCPT TO:"):
			rcpts = append(rcpts, strings.Trim(strings.TrimPrefix(line, "RCPT TO:"), " <>"))
			writeLine("250 ok")
		case line == "DATA":
			inData = true
			writeLine("354 end with .")
		case line == "QUIT":
			s.mu.Lock()
			reply := s.QuitReply
			s.mu.Unlock()
			if reply == "" {
				reply = "221 bye"
			}
			writeLine(reply)
			return
		default:
			writeLine("250 ok")
		}
	}
}

package notify

// Fanout 单测(T14.3,D-t143 决策 5):per-channel ErrAlreadyNotified 吸收、
// 失败裹通道名聚合、空集 nil、语义参数透传。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type stubNotifier struct {
	err   error
	calls int
}

func (s *stubNotifier) NotifyApproval(_ context.Context, _, _ uint64, _ string, _ int) error {
	s.calls++
	return s.err
}

func TestFanoutPerChannelIsolation(t *testing.T) {
	sentinel := errors.New("boom")

	t.Run("all ok", func(t *testing.T) {
		a, b := &stubNotifier{}, &stubNotifier{}
		f := NewFanout(Channel{Label: "smtp", N: a}, Channel{Label: "im", N: b})
		if err := f.NotifyApproval(context.Background(), 1, 2, "escalated", 1); err != nil {
			t.Fatalf("err: %v", err)
		}
		if a.calls != 1 || b.calls != 1 {
			t.Fatalf("calls: smtp=%d im=%d", a.calls, b.calls)
		}
	})

	t.Run("already-notified absorbed per channel", func(t *testing.T) {
		// SMTP 已送达(重投)+ IM 正常 ⇒ 整体 nil(不是 ErrAlreadyNotified:
		// 扇出面把它收敛为成功,worker 侧语义不变)。吸收不得短路后续通道
		//(对抗审计 MEDIUM 变异实证:continue→return nil 全绿)——b 必须仍
		// 被调用,否则双通道重投时 IM 被永久跳过。
		a, b := &stubNotifier{err: ErrAlreadyNotified}, &stubNotifier{}
		f := NewFanout(Channel{Label: "smtp", N: a}, Channel{Label: "im", N: b})
		if err := f.NotifyApproval(context.Background(), 1, 2, "escalated", 1); err != nil {
			t.Fatalf("err: %v", err)
		}
		if b.calls != 1 {
			t.Fatalf("absorb must not short-circuit later channels: im calls = %d", b.calls)
		}
	})

	t.Run("failure labeled, other channel unaffected", func(t *testing.T) {
		a, b := &stubNotifier{err: sentinel}, &stubNotifier{}
		f := NewFanout(Channel{Label: "smtp", N: a}, Channel{Label: "im", N: b})
		err := f.NotifyApproval(context.Background(), 1, 2, "escalated", 1)
		if err == nil || !strings.Contains(err.Error(), "smtp:") || !errors.Is(err, sentinel) {
			t.Fatalf("want labeled smtp error wrapping sentinel, got %v", err)
		}
	})

	t.Run("both fail joined", func(t *testing.T) {
		e1, e2 := errors.New("e1"), errors.New("e2")
		f := NewFanout(Channel{Label: "smtp", N: &stubNotifier{err: e1}}, Channel{Label: "im", N: &stubNotifier{err: e2}})
		err := f.NotifyApproval(context.Background(), 1, 2, "escalated", 1)
		if !errors.Is(err, e1) || !errors.Is(err, e2) {
			t.Fatalf("want joined e1+e2, got %v", err)
		}
	})

	t.Run("empty set yields nil fanout", func(t *testing.T) {
		if NewFanout() != nil {
			t.Fatal("want nil fanout for empty set")
		}
	})
}

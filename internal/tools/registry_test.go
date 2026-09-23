package tools

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"testing"
)

// echoTool 是最小进程内工具(测试用):inputs 原样回显。
type echoTool struct{ name string }

func (e echoTool) Name() string { return e.name }
func (e echoTool) Run(_ context.Context, call Call) (Result, error) {
	out, err := json.Marshal(map[string]any{"echo": call.Inputs})
	if err != nil {
		return Result{}, err
	}
	return Result{Output: out}, nil
}

func TestRegistryGet(t *testing.T) {
	r, err := NewRegistry(echoTool{"http"}, echoTool{"file_read"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if got, ok := r.Get("http"); !ok || got.Name() != "http" {
		t.Fatalf("Get(http) = %v, %v", got, ok)
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatal("Get(nope) found")
	}
}

func TestRegistryNames(t *testing.T) {
	r, err := NewRegistry(echoTool{"file_read"}, echoTool{"http"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got := r.Names()
	want := []string{"file_read", "http"} // 排序稳定:白名单换源与断言可比
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
}

func TestRegistryEmptyDeniesAll(t *testing.T) {
	// 空 registry:发布白名单换源后 = 禁用一切工具(T5.4 语义)。
	r, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if n := r.Names(); len(n) != 0 {
		t.Fatalf("empty registry Names() = %v", n)
	}
}

func TestRegistryDuplicateName(t *testing.T) {
	if _, err := NewRegistry(echoTool{"http"}, echoTool{"http"}); err == nil {
		t.Fatal("duplicate name accepted")
	}
}

func TestRegistryRegisterRuntime(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := r.Register(echoTool{"late"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.Register(echoTool{"late"}); err == nil {
		t.Fatal("duplicate Register accepted")
	}
	// 并发注册不同名 + 并发读:无竞态(-race 验证面)。
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) { defer wg.Done(); _ = r.Register(echoTool{string(rune('a' + i))}) }(i)
		go func() { defer wg.Done(); _ = r.Names(); _, _ = r.Get("late") }()
	}
	wg.Wait()
	if got := len(r.Names()); got != 9 {
		t.Fatalf("Names() len = %d, want 9", got)
	}
}

func TestNewRegistryRejectsInvalidTools(t *testing.T) {
	if _, err := NewRegistry(nil); err == nil {
		t.Fatal("nil tool accepted")
	}
	if _, err := NewRegistry(echoTool{""}); err == nil {
		t.Fatal("empty name accepted")
	}
}

func TestSortedNames(t *testing.T) {
	in := []string{"c", "a", "b"}
	sort.Strings(in)
	if in[0] != "a" {
		t.Fatal("sanity")
	}
}

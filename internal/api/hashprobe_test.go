package api

// MED① 契约边界(T3.6 对抗审计实证):HTTP JSON 编码器对嵌入的 dsl 文档
// 做 HTML 转义(标准库 json.Marshal 行为,`<`/`>`/`&` → `<` 等响应字
// 节形态),故「响应字节直接复算 dsl_hash」仅对不含这三个字符的文档成立
// (TestHTTPDetailDSLHashRoundTrip 钉该可复算子集);含敏感字符的文档,
// 消费方完整性核对走语义等价(本测试钉死)。dsl_hash 本身锚定服务端
// compact(存储字节) 形态,执行侧复算恒自洽(store_test 断言)。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"braid/internal/auth"
)

// dslDocEscaped 含全部 HTML 敏感字符(`<`/`>`/`&`,prompt 模板常见形态)。
func dslDocEscaped(name string) []byte {
	return fmt.Appendf(nil, `{"dsl":"1.0","name":%q,
"nodes":[{"id":"g","type":"llm","prompt":{"template":"输出 <doc> 与 a & b 形态。"},"outputs":{"text":"string"}}],
"outputs":{"greeting":"{{nodes.g.output.text}}"}}`, name)
}

func TestHTTPDetailDSLSemanticFidelity(t *testing.T) {
	r, _, sign := newTestServer(t)
	author := sign(auth.RoleAuthor)
	name := uniqName("esc")
	doc := dslDocEscaped(name)
	created := createViaAPI(t, r, author, doc)
	id := created["id"].(string)
	if code, _ := doJSON(t, r, http.MethodPost, "/api/v1/drafts/"+id+"/publish", author, nil); code != 200 {
		t.Fatalf("publish = %d, want 200", code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/definitions/"+name, nil)
	req.Header.Set("Authorization", "Bearer "+author)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("detail = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		DSL json.RawMessage `json:"dsl"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	assertSameJSON(t, resp.DSL, doc)
}

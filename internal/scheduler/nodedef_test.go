package scheduler

// NodeDefinition 集成测试(T3.6 执行器定义读取缝):经生产物化链路
// (Instantiate)落行后,按 node_execution id 反查钉住 DSL 节点(outputs
// 声明等执行期语义单源);未知行 → ErrNotFound(与其他只读缝同款)。

import (
	"context"
	"errors"
	"testing"

	"braid/internal/engine"
)

const llmDefDoc = `{"dsl":"1.0","name":"nodedef-t",
 "inputs":[{"name":"doc","type":"string"}],
 "nodes":[
 {"id":"sum","type":"llm","model":"glm-5.3","temperature":0.2,"max_tokens":128,
  "response_format":"json",
  "prompt":{"system":"你是助手","template":"总结以下内容",
            "user_data":[{"name":"doc","ref":"{{inputs.doc}}"}]},
  "outputs":{"summary":"string","count":"number"}}]}`

func TestNodeDefinition(t *testing.T) {
	s, d := newSched(t)
	_, execID := inst(t, s, d, llmDefDoc, `{"doc":"braid 是编排平台"}`)
	row := readNode(t, d, execID, "sum")

	n, err := s.NodeDefinition(context.Background(), row.id)
	if err != nil {
		t.Fatalf("NodeDefinition: %v", err)
	}
	if n.ID != "sum" || n.Type != "llm" || n.LLM == nil {
		t.Fatalf("node = %s/%s, want sum/llm with LLM spec", n.ID, n.Type)
	}
	if n.LLM.Model != "glm-5.3" || n.LLM.ResponseFormat != "json" {
		t.Fatalf("LLM spec = %+v", n.LLM)
	}
	if n.LLM.Outputs["summary"] != "string" || n.LLM.Outputs["count"] != "number" {
		t.Fatalf("outputs = %v(执行期抽取声明,必须回 DSL 取)", n.LLM.Outputs)
	}
	if len(n.LLM.Prompt.UserData) != 1 || n.LLM.Prompt.UserData[0].Ref != "{{inputs.doc}}" {
		t.Fatalf("user_data = %v", n.LLM.Prompt.UserData)
	}
}

func TestNodeDefinitionNotFound(t *testing.T) {
	s, _ := newSched(t)
	// Snowflake id 空间内的未存在行:ErrNotFound 而非裸 SQL 错。
	if _, err := s.NodeDefinition(context.Background(), 1<<62); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("NodeDefinition(unknown) = %v, want engine.ErrNotFound", err)
	}
}

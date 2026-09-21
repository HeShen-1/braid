// Package dsl 提供 braid 工作流 DSL(v1)的 JSON Schema 加载与校验。
// 结构合法性在这里;语义合法性(环、悬空引用、类型、祖先约束、inputs 只进 user_data)在 T1.2 解析器。
package dsl

import (
	"bytes"
	"embed"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schema.json
var schemaFS embed.FS

const schemaURL = "https://braid.dev/dsl/v1.json"

// Schema 是编译后的 DSL v1 JSON Schema,进程内编译一次。
var Schema = mustLoad()

func mustLoad() *jsonschema.Schema {
	raw, err := schemaFS.ReadFile("schema.json")
	if err != nil {
		panic(fmt.Sprintf("dsl: embedded schema.json missing: %v", err))
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		panic(fmt.Sprintf("dsl: embedded schema.json invalid: %v", err))
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaURL, doc); err != nil {
		panic(fmt.Sprintf("dsl: add schema resource: %v", err))
	}
	sch, err := c.Compile(schemaURL)
	if err != nil {
		panic(fmt.Sprintf("dsl: compile schema: %v", err))
	}
	return sch
}

// Validate 校验一份 DSL 文档(原始 JSON 字节)。
func Validate(data []byte) error {
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("dsl: invalid JSON: %w", err)
	}
	if err := Schema.Validate(v); err != nil {
		return fmt.Errorf("dsl: schema violation: %w", err)
	}
	return nil
}

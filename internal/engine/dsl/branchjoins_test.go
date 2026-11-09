package dsl

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestBranchJoinAdmission(t *testing.T) {
	tests := []struct {
		name   string
		edges  [][2]string
		branch bool
		want   string
	}{
		{"direct", [][2]string{{"gate", "a"}, {"gate", "b"}, {"a", "join"}, {"b", "join"}}, true, "join"},
		{"indirect", [][2]string{{"gate", "a"}, {"gate", "b"}, {"a", "x"}, {"x", "join"}, {"b", "join"}}, true, "join"},
		{"arm targets another arm", [][2]string{{"gate", "a"}, {"gate", "b"}, {"a", "b"}}, true, "b"},
		{"separate terminals", [][2]string{{"gate", "a"}, {"gate", "b"}, {"a", "x"}, {"b", "join"}}, true, ""},
		{"ordinary parallel join", [][2]string{{"gate", "a"}, {"gate", "b"}, {"a", "join"}, {"b", "join"}}, false, ""},
		{"diamond inside one arm", [][2]string{{"gate", "a"}, {"gate", "b"}, {"a", "x"}, {"a", "y"}, {"x", "join"}, {"y", "join"}}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodes := []map[string]any{}
			for _, id := range []string{"gate", "a", "b", "x", "y", "join"} {
				n := map[string]any{"id": id, "type": "approval", "approvers": []string{"alice"}}
				if id == "gate" && tt.branch {
					n = map[string]any{"id": id, "type": "branch", "cases": []any{map[string]any{"when": map[string]any{"left": "{{inputs.flag}}", "op": "eq", "right": true}, "next": "a"}}, "default": "b"}
				}
				nodes = append(nodes, n)
			}
			edges := []map[string]string{}
			for _, e := range tt.edges {
				edges = append(edges, map[string]string{"from": e[0], "to": e[1]})
			}
			raw, err := json.Marshal(map[string]any{"dsl": "1.0", "name": "join-test", "inputs": []any{map[string]any{"name": "flag", "type": "boolean"}}, "nodes": nodes, "edges": edges})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(raw); err != nil {
				t.Fatalf("historical parsing changed: %v", err)
			}
			_, err = Parse(raw, WithBranchJoinValidation())
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			issues := IssuesFrom(err)
			if len(issues) != 1 || issues[0].Code != IssueBranchJoin || issues[0].Node != tt.want || issues[0].Ref != "gate" {
				t.Fatalf("issues = %+v", issues)
			}
		})
	}
}

func TestBranchCasesSharingOneTarget(t *testing.T) {
	raw := fmt.Sprintf(branchTmpl, "gte", "60")
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	nodes := doc["nodes"].([]any)
	nodes[0].(map[string]any)["default"] = "hi"
	doc["edges"] = []any{map[string]any{"from": "decide", "to": "hi"}}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Parse(b, WithBranchJoinValidation()); err != nil {
		t.Fatal(err)
	}
}

func TestNestedBranchJoinAdmission(t *testing.T) {
	raw := []byte(`{"dsl":"1.0","name":"nested","inputs":[{"name":"flag","type":"boolean"}],"nodes":[
 {"id":"outer","type":"branch","cases":[{"when":{"left":"{{inputs.flag}}","op":"eq","right":true},"next":"inner"}],"default":"other"},
 {"id":"inner","type":"branch","cases":[{"when":{"left":"{{inputs.flag}}","op":"eq","right":true},"next":"a"}],"default":"b"},
 {"id":"a","type":"approval","approvers":["alice"]},
 {"id":"b","type":"approval","approvers":["alice"]},
 {"id":"other","type":"approval","approvers":["alice"]},
 {"id":"final","type":"approval","approvers":["alice"]}],
 "edges":[{"from":"outer","to":"inner"},{"from":"outer","to":"other"},{"from":"inner","to":"a"},{"from":"inner","to":"b"},{"from":"a","to":"final"},{"from":"b","to":"final"},{"from":"other","to":"final"}]}`)
	_, err := Parse(raw, WithBranchJoinValidation())
	issues := IssuesFrom(err)
	if len(issues) != 2 || issues[0].Node != "final" || issues[0].Ref != "outer" || issues[1].Node != "final" || issues[1].Ref != "inner" {
		t.Fatalf("nested issues = %+v", issues)
	}
}

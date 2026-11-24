package engine

import (
	"agentflow/internal/domain"
	"agentflow/internal/runtime"
	"encoding/json"
	"strings"
	"testing"
)

// An approval node's taskPrompt never reaches a role node, while the run
// input and upstream output always do, exactly once.
func TestRunContextReachesRolesAndNodeTaskPromptDoesNot(t *testing.T) {
	t.Parallel()
	wf := domain.WorkflowDefinition{Edges: []domain.WorkflowEdge{{Source: "source", Target: "report"}}}
	node := domain.WorkflowNode{ID: "report", TaskPrompt: "撰写中文简报"}
	run := domain.Run{Input: "验证图片、PPT 和网页", Nodes: map[string]*domain.NodeRun{"source": {Output: "已完成前置调研"}}}
	for _, role := range []*domain.AgentDefinition{nil, {Name: "分析师"}, {Prompt: "完成分析"}, {Prompt: "{{input}}\n{{upstream}}"}} {
		got := applyPromptPlaceholders(nil, wf, node, role, run)
		if strings.Contains(got, node.TaskPrompt) || strings.Contains(got, "本节点任务") {
			t.Fatalf("node taskPrompt must not reach a role node: %s", got)
		}
		for _, content := range []string{run.Input, "已完成前置调研"} {
			if strings.Count(got, content) != 1 {
				t.Fatalf("run context must appear exactly once (%q): %s", content, got)
			}
		}
	}
}

func TestWorkflowAgentInstructionsReachFrozenPrompt(t *testing.T) {
	t.Parallel()
	_, store := cvEEngine(t)
	e := New(store, runtime.NewManager(t.TempDir()))
	var original domain.WorkflowDefinition
	if err := json.Unmarshal([]byte(`{
		"id": "instruction-flow", "name": "Instruction flow", "sandboxProfileId": "local",
		"nodes": [
			{"id": "approval", "type": "approval", "taskPrompt": "Review the schema", "agentInstructions": "Wrong destination"},
			{"id": "work", "type": "agent", "agentId": "instruction-role", "agentInstructions": "Check the input schema", "taskPrompt": "Approval only"}
		],
		"edges": [{"source": "approval", "target": "work"}]
	}`), &original); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAgent(domain.AgentDefinition{ID: "instruction-role", Name: "Instruction role", Prompt: "Implement the workflow"}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutWorkflow(original); err != nil {
		t.Fatal(err)
	}
	run, err := e.Start(original.ID, "Run input")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, store, run.ID, "waiting")
	latest, ok := store.Workflow(original.ID)
	if !ok {
		t.Fatal("workflow missing from catalog")
	}
	latest.Name = "Edited workflow"
	var edited domain.WorkflowNode
	if err := json.Unmarshal([]byte(`{"id": "work", "type": "agent", "agentId": "instruction-role", "agentInstructions": "Check the output schema"}`), &edited); err != nil {
		t.Fatal(err)
	}
	latest.Nodes[1] = edited
	if err := store.PutWorkflow(latest); err != nil {
		t.Fatal(err)
	}
	frozen, ok := store.RunState(run.ID)
	if !ok {
		t.Fatal("frozen run missing")
	}
	snapshot := frozen.WorkflowSnapshot
	got := renderPromptWithStore(store, snapshot, snapshot.Nodes[1], frozen)
	for _, want := range []string{"Implement the workflow", "Check the input schema", "Run input"} {
		if strings.Count(got, want) != 1 {
			t.Fatalf("frozen agent prompt should contain %q once, got %q", want, got)
		}
	}
	for _, unwanted := range []string{"Approval only", "Check the output schema"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("frozen agent prompt contains %q: %q", unwanted, got)
		}
	}
	approval := domain.ApprovalWaitReason(frozen, "approval")
	if !strings.Contains(approval, "Review the schema") || strings.Contains(approval, "Wrong destination") {
		t.Fatalf("approval context = %q", approval)
	}
	if got := renderPromptWithStore(store, snapshot, snapshot.Nodes[0], frozen); strings.Contains(got, "Wrong destination") {
		t.Fatalf("rendered instructions for approval: %q", got)
	}
	if err := e.RefreshRunSOP(run.ID); err != nil {
		t.Fatal(err)
	}
	refreshed, _ := store.RunState(run.ID)
	snapshot = refreshed.WorkflowSnapshot
	got = renderPromptWithStore(store, snapshot, snapshot.Nodes[1], refreshed)
	if !strings.Contains(got, "Check the output schema") || strings.Contains(got, "Check the input schema") {
		t.Fatalf("refreshed agent prompt = %q", got)
	}
}

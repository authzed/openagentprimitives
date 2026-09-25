package meta_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
)

func findNewOpTool(t *testing.T) tool.Tool {
	t.Helper()
	for _, tl := range meta.Load() {
		if tl.Name() == "new_operation" {
			return tl
		}
	}
	t.Fatal("new_operation not registered")
	return nil
}

func TestNewOperationReturnsAnIDAndRegistersIt(t *testing.T) {
	tl := findNewOpTool(t)
	reg := operations.New(nil, nil)
	sess := &tool.SessionContext{Operations: reg}

	res, err := tl.Execute(context.Background(),
		json.RawMessage(`{"description":"fetch spicedb commits"}`), sess)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError; content=%s", res.Content)
	}
	if !res.Trusted {
		t.Fatal("new_operation is a framework meta tool and must opt out of content-guard inspection (Trusted must be true)")
	}
	var out struct {
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("response not JSON: %v\n%s", err, res.Content)
	}
	if !strings.HasPrefix(out.OperationID, "op-") {
		t.Fatalf("operation_id %q does not start with op-", out.OperationID)
	}
	op, ok := reg.Get(out.OperationID)
	if !ok {
		t.Fatal("returned operation_id is not in the registry")
	}
	if op.Description != "fetch spicedb commits" {
		t.Fatalf("Description = %q", op.Description)
	}
}

func TestNewOperationRejectsEmptyDescription(t *testing.T) {
	tl := findNewOpTool(t)
	sess := &tool.SessionContext{Operations: operations.New(nil, nil)}
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"description":""}`), sess)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("empty description should produce IsError tool result")
	}
}

func TestNewOperationFailsWithoutRegistry(t *testing.T) {
	tl := findNewOpTool(t)
	_, err := tl.Execute(context.Background(),
		json.RawMessage(`{"description":"x"}`), &tool.SessionContext{})
	if err == nil {
		t.Fatal("expected Go error when SessionContext.Operations is nil")
	}
}

func TestNewOperation_NoParent_TopLevel(t *testing.T) {
	ops := operations.New(nil, nil)
	sess := &tool.SessionContext{Operations: ops}
	tl := meta.LookupForTest("new_operation")
	require.NotNil(t, tl)

	res, err := tl.Execute(context.Background(),
		json.RawMessage(`{"description":"top"}`), sess)
	require.NoError(t, err)
	require.False(t, res.IsError)

	all := ops.All()
	require.Len(t, all, 1)
	require.Nil(t, all[0].Parent)
}

func TestNewOperation_ParentOperationID(t *testing.T) {
	ops := operations.New(nil, nil)
	sess := &tool.SessionContext{Operations: ops}
	tl := meta.LookupForTest("new_operation")

	parent := ops.Begin("parent")

	args := `{"description":"child","parent":{"operation_id":"` + parent.ID + `"}}`
	res, err := tl.Execute(context.Background(), json.RawMessage(args), sess)
	require.NoError(t, err)
	require.False(t, res.IsError)

	// Find the new (non-parent) op. ops.All() returns a slice in
	// randomized map order and a fresh slice on every call, so it must
	// be captured ONCE — indexing a second All() call by an index from
	// the first would non-deterministically miss the child.
	all := ops.All()
	var child *tool.Operation
	for i := range all {
		op := all[i]
		if op.ID != parent.ID {
			child = &op
		}
	}
	require.NotNil(t, child)
	require.NotNil(t, child.Parent)
	require.Equal(t, parent.ID, child.Parent.OperationID)
}

func TestNewOperation_ParentPlanItem(t *testing.T) {
	ops := operations.New(nil, nil)
	sess := &tool.SessionContext{Operations: ops}
	tl := meta.LookupForTest("new_operation")

	args := `{"description":"x","parent":{"plan":"main","item":"a"}}`
	res, err := tl.Execute(context.Background(), json.RawMessage(args), sess)
	require.NoError(t, err)
	require.False(t, res.IsError)

	all := ops.All()
	require.Len(t, all, 1)
	require.NotNil(t, all[0].Parent)
	require.NotNil(t, all[0].Parent.PlanItem)
	require.Equal(t, "main", all[0].Parent.PlanItem.Plan)
	require.Equal(t, "a", all[0].Parent.PlanItem.Item)
}

func TestNewOperation_ParentBothPopulatedRejected(t *testing.T) {
	ops := operations.New(nil, nil)
	sess := &tool.SessionContext{Operations: ops}
	tl := meta.LookupForTest("new_operation")

	args := `{"description":"x","parent":{"operation_id":"op-1","plan":"main","item":"a"}}`
	res, err := tl.Execute(context.Background(), json.RawMessage(args), sess)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, res.Content, "parent")
}

func TestNewOperation_ParentEmptyRejected(t *testing.T) {
	ops := operations.New(nil, nil)
	sess := &tool.SessionContext{Operations: ops}
	tl := meta.LookupForTest("new_operation")

	args := `{"description":"x","parent":{}}`
	res, err := tl.Execute(context.Background(), json.RawMessage(args), sess)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, res.Content, "parent")
}

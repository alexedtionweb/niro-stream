package dsl

import (
	"os"

	"github.com/alexedtionweb/niro-stream"
)

// ParseWorkflow unmarshals workflow definition JSON. No semantic validation; call ValidateWorkflow after.
func ParseWorkflow(data []byte) (*WorkflowDefinition, error) {
	wf, err := niro.UnmarshalTo[WorkflowDefinition](data)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeInvalidRequest, "dsl: parse workflow", err)
	}
	return &wf, nil
}

// ParseWorkflowFile reads and parses a workflow definition file (JSON).
func ParseWorkflowFile(path string) (*WorkflowDefinition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeInvalidRequest, "dsl: read workflow file", err)
	}
	return ParseWorkflow(data)
}

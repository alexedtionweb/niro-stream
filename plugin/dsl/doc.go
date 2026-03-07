// Package dsl provides an agent DSL plugin for niro-stream.
//
// It parses two separate definitions:
//   - Agent definition (one JSON): tools, schemas, and agents. Parse with ParseDSL,
//     validate with Validate, compile with Compile to obtain a Runner.
//   - Workflow definition (separate JSON): how agents are composed (fan, race, sequence).
//     Parse with ParseWorkflow, validate with ValidateWorkflow, compile to run with
//     orchestrate primitives.
//
// Install with:
//
//	go get github.com/alexedtionweb/niro-stream/plugin/dsl
//
// The Runner uses RunContext (Get/Set) for template expansion and expr conditions;
// set session, event, history and custom vars per invoke. Runner.Stream returns
// a niro.Stream for composition with orchestrate.Fan, Race, Sequence.
//
// Template strings: All description and prompt text is resolved as Go text/template
// with RunContext as data. Use ExecuteTemplate(tpl, data) or ExpandTemplate(tpl, runCtx)
// for custom expansion.
//
// Tools and niro: Tools are a single composable responsibility. Compile produces a shared
// tools.Toolset (from DSL tools + CompileOptions.ToolHandlers/ToolTypes). Per agent run,
// resolveAgentTools filters by when/unless, expands descriptions, and yields AgentTools
// (Toolset, ToolChoice, ToolStreamOptions). Optional WithToolsetTransform and WithToolOptions
// let you customize per agent (e.g. add HITL approver, override max rounds). The runner
// wires to niro as: req = toolset.Apply(req) then tools.NewToolingProvider(provider, toolset, opts).Generate(ctx, req).
// So the same tools.Toolset and ToolingProvider contract used outside the DSL is used inside it.
//
// Package layout: types/parse/validate (definition); compile (compileTools, compileAgents → NiroDefinition);
// runner (Stream/Run, filterTools, toolset expansion); cache (GetOrCompute); expr (EvalCondition, CompileRunContextExpr);
// template (ExecuteTemplate, getParsedTemplate); tooltype_*.go (http, handoff).
package dsl

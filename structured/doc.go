// Package structured provides typed decoding of JSON Schema-constrained LLM output.
//
// Use when the model is instructed to return JSON matching a schema and you want
// a typed Go struct instead of raw bytes.
//
// One-shot: [GenerateStructured] runs the request and unmarshals the final response into T.
//
// Streaming: [StreamStructured] parses partial JSON as it arrives and emits [StructuredEvent]
// with Partial and Final fields. Use for progressive UI or validation.
//
// Request must have ResponseFormat = "json_schema" and ResponseSchema set.
// See [WithSchema] and [WithSchemaAny] to configure the request.
package structured

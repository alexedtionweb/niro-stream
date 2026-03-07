package dsl

import (
	"os"

	"github.com/alexedtionweb/niro-stream"
)

// ParseDSL unmarshals agent definition JSON into DSLDefinition.
// No semantic validation is performed; call Validate after ParseDSL.
func ParseDSL(data []byte) (*DSLDefinition, error) {
	def, err := niro.UnmarshalTo[DSLDefinition](data)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeInvalidRequest, "dsl: parse", err)
	}
	return &def, nil
}

// ParseDSLFile reads and parses an agent definition file (JSON).
func ParseDSLFile(path string) (*DSLDefinition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, niro.WrapError(niro.ErrCodeInvalidRequest, "dsl: read file", err)
	}
	return ParseDSL(data)
}

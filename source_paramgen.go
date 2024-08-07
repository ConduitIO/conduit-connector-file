// Code generated by paramgen. DO NOT EDIT.
// Source: github.com/ConduitIO/conduit-commons/tree/main/paramgen

package file

import (
	"github.com/conduitio/conduit-commons/config"
)

const (
	SourceConfigPath = "path"
)

func (SourceConfig) Parameters() map[string]config.Parameter {
	return map[string]config.Parameter{
		SourceConfigPath: {
			Default:     "",
			Description: "Path is the file path used by the connector to read/write records.",
			Type:        config.ParameterTypeString,
			Validations: []config.Validation{
				config.ValidationRequired{},
			},
		},
	}
}

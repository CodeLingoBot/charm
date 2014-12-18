// Copyright 2011, 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package charm

import (
	"fmt"
	"io"
	"io/ioutil"
	"regexp"
	"strings"

	"github.com/juju/errors"
	"github.com/juju/gojsonschema"
	"gopkg.in/yaml.v1"
)

var prohibitedSchemaKeys = map[string]bool{"$ref": true, "$schema": true}

var actionNameRule = regexp.MustCompile("^[a-z](?:[a-z-]*[a-z])?$")

// Actions defines the available actions for the charm.  Additional params
// may be added as metadata at a future time (e.g. version.)
type Actions struct {
	ActionSpecs map[string]ActionSpec `yaml:"actions,omitempty" bson:",omitempty"`
}

// ActionSpec is a definition of the parameters and traits of an Action.
// The Params map is expected to conform to JSON-Schema Draft 4 as defined at
// http://json-schema.org/draft-04/schema# (see http://json-schema.org/latest/json-schema-core.html)
type ActionSpec struct {
	Description string
	Params      map[string]interface{}
}

func NewActions() *Actions {
	return &Actions{}
}

// ValidateParams tells us whether an unmarshaled JSON object conforms to the
// Params for the specific ActionSpec.
// Usage: ok, err := ch.Actions()["snapshot"].ValidateParams(jsonParams)
func (spec *ActionSpec) ValidateParams(params interface{}) (bool, error) {

	specSchemaDoc, err := gojsonschema.NewJsonSchemaDocument(spec.Params)
	if err != nil {
		return false, err
	}

	results := specSchemaDoc.Validate(params)
	if results.Valid() {
		return true, nil
	}

	var errorStrings []string
	for _, validationError := range results.Errors() {
		errorStrings = append(errorStrings, validationError.String())
	}
	return false, fmt.Errorf("JSON validation failed: %s", strings.Join(errorStrings, "; "))
}

// ReadActions builds an Actions spec from a charm's actions.yaml.
func ReadActionsYaml(r io.Reader) (*Actions, error) {
	data, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}

	result := &Actions{
		ActionSpecs: map[string]ActionSpec{},
	}

	var unmarshaledActions map[string]map[string]interface{}
	if err := yaml.Unmarshal(data, &unmarshaledActions); err != nil {
		return nil, err
	}

	for name, actionSpec := range unmarshaledActions {
		if valid := actionNameRule.MatchString(name); !valid {
			return nil, fmt.Errorf("bad action name %s", name)
		}

		desc := "No description"
		thisActionSchema := map[string]interface{}{
			"description": desc,
			"type":        "object",
			"title":       name,
			"properties":  map[string]interface{}{},
		}

		for key, value := range actionSpec {
			switch key {
			case "description":
				// These fields must be strings.
				typed, ok := value.(string)
				if !ok {
					return nil, errors.Errorf("value for schema key %q must be a string", key)
				}
				thisActionSchema[key] = typed
				desc = typed
			case "title":
				// These fields must be strings.
				typed, ok := value.(string)
				if !ok {
					return nil, errors.Errorf("value for schema key %q must be a string", key)
				}
				thisActionSchema[key] = typed
			case "required":
				typed, ok := value.([]interface{})
				if !ok {
					return nil, errors.Errorf("value for schema key %q must be a YAML list", key)
				}
				thisActionSchema[key] = typed
			case "params":
				// Clean any map[interface{}]interface{}s out so they don't
				// cause problems with BSON serialization later.
				cleansedParams, err := cleanse(value)
				if err != nil {
					return nil, err
				}

				// JSON-Schema must be a map
				typed, ok := cleansedParams.(map[string]interface{})
				if !ok {
					return nil, errors.New("params failed to parse as a map")
				}
				thisActionSchema["properties"] = typed
			default:
				// In case this has nested maps, we must clean them out.
				typed, err := cleanse(value)
				if err != nil {
					return nil, err
				}
				thisActionSchema[key] = typed
			}
		}

		// Make sure the new Params doc conforms to JSON-Schema
		// Draft 4 (http://json-schema.org/latest/json-schema-core.html)
		_, err = gojsonschema.NewJsonSchemaDocument(thisActionSchema)
		if err != nil {
			return nil, errors.Annotatef(err, "invalid params schema for action schema %s", name)
		}

		// Now assign the resulting schema to the final entry for the result.
		result.ActionSpecs[name] = ActionSpec{
			Description: desc,
			Params:      thisActionSchema,
		}
	}
	return result, nil
}

// cleanse rejects schemas containing references or maps keyed with non-
// strings, and coerces acceptable maps to contain only maps with string keys.
func cleanse(input interface{}) (interface{}, error) {
	switch typedInput := input.(type) {

	// In this case, recurse in.
	case map[string]interface{}:
		newMap := make(map[string]interface{})
		for key, value := range typedInput {

			if prohibitedSchemaKeys[key] {
				return nil, fmt.Errorf("schema key %q not compatible with this version of juju", key)
			}

			newValue, err := cleanse(value)
			if err != nil {
				return nil, err
			}
			newMap[key] = newValue
		}
		return newMap, nil

	// Coerce keys to strings and error out if there's a problem; then recurse.
	case map[interface{}]interface{}:
		newMap := make(map[string]interface{})
		for key, value := range typedInput {
			typedKey, ok := key.(string)
			if !ok {
				return nil, errors.New("map keyed with non-string value")
			}
			newMap[typedKey] = value
		}
		return cleanse(newMap)

	// Recurse
	case []interface{}:
		newSlice := make([]interface{}, 0)
		for _, sliceValue := range typedInput {
			newSliceValue, err := cleanse(sliceValue)
			if err != nil {
				return nil, errors.New("map keyed with non-string value")
			}
			newSlice = append(newSlice, newSliceValue)
		}
		return newSlice, nil

	// Other kinds of values are OK.
	default:
		return input, nil
	}
}

// recurseMapOnKeys returns the value of a map keyed recursively by the
// strings given in "keys".  Thus, recurseMapOnKeys({a,b}, {a:{b:{c:d}}})
// would return {c:d}.
func recurseMapOnKeys(keys []string, params map[string]interface{}) (interface{}, bool) {
	key, rest := keys[0], keys[1:]
	answer, ok := params[key]

	// If we're out of keys, we have our answer.
	if len(rest) == 0 {
		return answer, ok
	}

	// If we're not out of keys, but we tried a key that wasn't in the
	// map, there's no answer.
	if !ok {
		return nil, false
	}

	switch typed := answer.(type) {
	// If our value is a map[s]i{}, we can keep recursing.
	case map[string]interface{}:
		return recurseMapOnKeys(keys[1:], typed)
	// If it's a map[i{}]i{}, we need to check whether it's a map[s]i{}.
	case map[interface{}]interface{}:
		m := make(map[string]interface{})
		for k, v := range typed {
			if tK, ok := k.(string); ok {
				m[tK] = v
			} else {
				// If it's not, we don't have something we
				// can work with.
				return nil, false
			}
		}
		// If it is, recurse into it.
		return recurseMapOnKeys(keys[1:], m)

	// Otherwise, we're trying to recurse into something we don't know
	// how to deal with, so our answer is that we don't have an answer.
	default:
		return nil, false
	}
}

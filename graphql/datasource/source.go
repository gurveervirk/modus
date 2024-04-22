/*
 * Copyright 2024 Hypermode, Inc.
 */

package datasource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"hmruntime/functions"
	"hmruntime/logger"
	"hmruntime/utils"
	"hmruntime/wasmhost"

	"github.com/buger/jsonparser"
	"github.com/rs/xid"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/engine/resolve"
)

const DataSourceName = "HypermodeFunctionsDataSource"

type callInfo struct {
	Function   templateField  `json:"fn"`
	Parameters map[string]any `json:"data"`
}

type Source struct{}

func (s Source) Load(ctx context.Context, input []byte, writer io.Writer) error {

	// Parse the input to get the function call info
	callInfo, err := parseInput(input)
	if err != nil {
		return fmt.Errorf("error parsing input: %w", err)
	}

	// Load the data
	result, gqlErrors, err := s.callFunction(ctx, callInfo)
	if err != nil {
		logger.Err(ctx, err).Msg("Failed to call function.")
	}

	// Write the response
	return writeGraphQLResponse(writer, result, gqlErrors, err, callInfo)
}

func (s Source) callFunction(ctx context.Context, callInfo callInfo) (any, []resolve.GraphQLError, error) {

	// Get the function info
	info, ok := functions.Functions[callInfo.Function.Name]
	if !ok {
		return nil, nil, fmt.Errorf("no function registered named %s", callInfo.Function)
	}

	// Prepare the context that will be used throughout the function execution
	ctx = prepareContext(ctx, info)

	// Create output buffers for the function to write to
	bStdOut := &bytes.Buffer{}
	bStdErr := &bytes.Buffer{}

	// Get a module instance for this request.
	// Each request will get its own instance of the plugin module, so that we can run
	// multiple requests in parallel without risk of corrupting the module's memory.
	// This also protects against security risk, as each request will have its own
	// isolated memory space.  (One request cannot access another request's memory.)
	mod, err := wasmhost.GetModuleInstance(ctx, info.Plugin, bStdOut, bStdErr)
	if err != nil {
		return nil, nil, err
	}
	defer mod.Close(ctx)

	// Call the function
	result, err := functions.CallFunction(ctx, mod, info, callInfo.Parameters)

	// Transform lines in the output buffers to GraphQL gqlErrors
	gqlErrors := append(
		transformErrors(bStdOut, callInfo),
		transformErrors(bStdErr, callInfo)...,
	)

	return result, gqlErrors, err
}

func parseInput(input []byte) (callInfo, error) {
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()

	var ci callInfo
	err := dec.Decode(&ci)
	return ci, err
}

func prepareContext(ctx context.Context, info functions.FunctionInfo) context.Context {
	// TODO: We should return the execution id(s) in the response somehow.
	// There might be multiple execution ids if the request triggers multiple function calls.
	executionId := xid.New().String()
	ctx = context.WithValue(ctx, utils.ExecutionIdContextKey, executionId)
	ctx = context.WithValue(ctx, utils.PluginContextKey, info.Plugin)
	return ctx
}

func writeGraphQLResponse(writer io.Writer, result any, gqlErrors []resolve.GraphQLError, fnErr error, ci callInfo) error {

	// Include the function error (except any we've filtered out)
	if fnErr != nil && functions.ShouldReturnErrorToResponse(fnErr) {
		gqlErrors = append(gqlErrors, resolve.GraphQLError{
			Message: fnErr.Error(),
			Path:    []string{ci.Function.AliasOrName()},
			Extensions: map[string]interface{}{
				"level": "error",
			},
		})
	}

	// If there are GraphQL errors, marshal them to json
	var jsonErrors []byte
	if len(gqlErrors) > 0 {
		var err error
		jsonErrors, err = json.Marshal(gqlErrors)
		if err != nil {
			return err
		}

		// If there are no other results, return only the errors
		if result == nil {
			fmt.Fprintf(writer, `{"errors":%s}`, jsonErrors)
			return nil
		}
	}

	// Get the data as json from the result
	jsonData, err := json.Marshal(result)
	if err != nil {
		return err
	}

	// Transform the data
	jsonData, err = transformData(jsonData, ci.Function)
	if err != nil {
		return err
	}

	// Build and write the response, including errors if there are any
	if len(gqlErrors) > 0 {
		fmt.Fprintf(writer, `{"data":%s,"errors":%s}`, jsonData, jsonErrors)
	} else {
		fmt.Fprintf(writer, `{"data":%s}`, jsonData)
	}

	return nil
}

func transformData(data []byte, tf templateField) ([]byte, error) {
	val, err := transformValue(data, tf)
	if err != nil {
		return nil, err
	}

	out := []byte(`{}`)
	return jsonparser.Set(out, val, tf.AliasOrName())
}

func transformValue(data []byte, tf templateField) ([]byte, error) {
	if len(tf.Fields) == 0 || len(data) == 0 {
		return data, nil
	}

	buf := bytes.Buffer{}

	switch data[0] {
	case '{': // object
		buf.WriteByte('{')
		for i, f := range tf.Fields {
			val, dataType, _, err := jsonparser.Get(data, f.Name)
			if err != nil {
				return nil, err
			}
			if dataType == jsonparser.String {
				val, err = json.Marshal(string(val))
				if err != nil {
					return nil, err
				}
			}
			val, err = transformValue(val, f)
			if err != nil {
				return nil, err
			}
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteByte('"')
			buf.WriteString(f.AliasOrName())
			buf.WriteString(`":`)
			buf.Write(val)
		}
		buf.WriteByte('}')

	case '[': // array
		buf.WriteByte('[')
		_, err := jsonparser.ArrayEach(data, func(val []byte, _ jsonparser.ValueType, _ int, _ error) {
			if buf.Len() > 1 {
				buf.WriteByte(',')
			}
			val, err := transformValue(val, tf)
			if err != nil {
				return
			}
			buf.Write(val)
		})
		if err != nil {
			return nil, err
		}

		buf.WriteByte(']')

	default:
		return nil, fmt.Errorf("expected object or array")
	}

	return buf.Bytes(), nil
}

func transformErrors(buf *bytes.Buffer, ci callInfo) []resolve.GraphQLError {
	errors := make([]resolve.GraphQLError, 0)
	for _, s := range strings.Split(buf.String(), "\n") {
		if s != "" {
			errors = append(errors, transformError(s, ci))
		}
	}
	return errors
}

func transformError(msg string, ci callInfo) resolve.GraphQLError {
	level := ""
	a := strings.SplitAfterN(msg, ": ", 2)
	if len(a) == 2 {
		switch a[0] {
		case "Debug: ":
			level = "debug"
			msg = a[1]
		case "Info: ":
			level = "info"
			msg = a[1]
		case "Warning: ":
			level = "warning"
			msg = a[1]
		case "Error: ":
			level = "error"
			msg = a[1]
		case "abort: ":
			level = "fatal"
			msg = a[1]
		}
	}

	e := resolve.GraphQLError{
		Message: msg,
		Path:    []string{ci.Function.AliasOrName()},
	}

	if level != "" {
		e.Extensions = map[string]interface{}{
			"level": level,
		}
	}

	return e
}
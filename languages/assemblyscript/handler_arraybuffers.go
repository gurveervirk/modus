/*
 * Copyright 2024 Hypermode, Inc.
 */

package assemblyscript

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"hypruntime/langsupport"
	"hypruntime/utils"
)

func (p *planner) NewArrayBufferHandler(typ string, rt reflect.Type) (langsupport.TypeHandler, error) {
	handler := NewTypeHandler[arrayBufferHandler](p, typ)
	handler.info = langsupport.NewTypeHandlerInfo(typ, rt, 4, 1)
	handler.nullable = _typeInfo.IsNullable(typ)
	return handler, nil
}

type arrayBufferHandler struct {
	info     langsupport.TypeHandlerInfo
	nullable bool
}

func (h *arrayBufferHandler) Info() langsupport.TypeHandlerInfo {
	return h.info
}

func (h *arrayBufferHandler) Read(ctx context.Context, wa langsupport.WasmAdapter, offset uint32) (any, error) {
	if offset == 0 {
		return nil, fmt.Errorf("unexpected address 0 reading managed object of type %s", h.info.TypeName())
	}

	ptr, ok := wa.Memory().ReadUint32Le(offset)
	if !ok {
		return nil, errors.New("failed to read ArrayBuffer pointer")
	}

	return h.doReadBytes(wa, ptr)
}

func (h *arrayBufferHandler) Write(ctx context.Context, wa langsupport.WasmAdapter, offset uint32, obj any) (utils.Cleaner, error) {
	ptr, cln, err := h.doWriteBytes(ctx, wa, obj)
	if err != nil {
		return cln, err
	}

	if ok := wa.Memory().WriteUint32Le(offset, ptr); !ok {
		return cln, fmt.Errorf("failed to write ArrayBuffer pointer to WASM memory")
	}

	return cln, nil
}

func (h *arrayBufferHandler) Decode(ctx context.Context, wa langsupport.WasmAdapter, vals []uint64) (any, error) {
	if len(vals) != 1 {
		return nil, fmt.Errorf("expected 1 value when decoding an ArrayBuffer, got %d", len(vals))
	}

	return h.doReadBytes(wa, uint32(vals[0]))
}

func (h *arrayBufferHandler) Encode(ctx context.Context, wa langsupport.WasmAdapter, obj any) ([]uint64, utils.Cleaner, error) {
	ptr, cln, err := h.doWriteBytes(ctx, wa, obj)
	if err != nil {
		return nil, cln, err
	}

	return []uint64{uint64(ptr)}, cln, nil
}

func (h *arrayBufferHandler) doReadBytes(wa langsupport.WasmAdapter, offset uint32) ([]byte, error) {
	if offset == 0 {
		if h.nullable {
			return nil, nil
		} else {
			return nil, errors.New("unexpected null pointer for non-nullable ArrayBuffer")
		}
	}

	if id, ok := wa.Memory().ReadUint32Le(offset - 8); !ok {
		return nil, errors.New("failed to read class id of the WASM object")
	} else if id != 1 {
		return nil, errors.New("pointer is not to an ArrayBuffer")
	}

	size, ok := wa.Memory().ReadUint32Le(offset - 4)
	if !ok {
		return nil, errors.New("failed to read ArrayBuffer length")
	} else if size == 0 {
		return nil, nil
	}

	bytes, ok := wa.Memory().Read(offset, size)
	if !ok {
		return nil, fmt.Errorf("failed to read ArrayBuffer data from WASM memory (size: %d)", size)
	}

	return bytes, nil
}

func (h *arrayBufferHandler) doWriteBytes(ctx context.Context, wa langsupport.WasmAdapter, obj any) (uint32, utils.Cleaner, error) {
	if utils.HasNil(obj) {
		if h.nullable {
			return 0, nil, nil
		} else {
			return 0, nil, errors.New("unexpected nil value for non-nullable ArrayBuffer")
		}
	}

	var bytes []byte
	switch obj := obj.(type) {
	case []byte:
		bytes = obj
	case string:
		bytes = []byte(obj)
	default:
		return 0, nil, errors.New("input value cannot be used as an ArrayBuffer")
	}

	size := uint32(len(bytes))
	ptr, cln, err := wa.AllocateMemory(ctx, size)
	if err != nil {
		return 0, cln, err
	}

	if ok := wa.Memory().Write(ptr, bytes); !ok {
		return 0, cln, fmt.Errorf("failed to write ArrayBuffer data to WASM memory (size: %d)", size)
	}

	return ptr, cln, nil
}
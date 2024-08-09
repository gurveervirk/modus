/*
 * Copyright 2024 Hypermode, Inc.
 */

package assemblyscript

import (
	"context"
	"fmt"
	"reflect"

	"hmruntime/plugins/metadata"
	"hmruntime/utils"

	wasm "github.com/tetratelabs/wazero/api"
)

// Reference: https://github.com/AssemblyScript/assemblyscript/blob/main/std/assembly/array.ts

func (wa *wasmAdapter) readArray(ctx context.Context, mem wasm.Memory, typ *metadata.TypeDefinition, offset uint32) (data any, err error) {

	// buffer, ok := mem.ReadUint32Le(offset)
	// if !ok {
	// 	return nil, fmt.Errorf("failed to read array buffer pointer")
	// }

	dataStart, ok := mem.ReadUint32Le(offset + 4)
	if !ok {
		return nil, fmt.Errorf("failed to read array data start pointer")
	}

	// byteLength, ok := mem.ReadUint32Le(offset + 8)
	// if !ok {
	// 	return nil, fmt.Errorf("failed to read array bytes length")
	// }

	arrLen, ok := mem.ReadUint32Le(offset + 12)
	if !ok {
		return nil, fmt.Errorf("failed to read array length")
	}

	itemType := wa.typeInfo.GetArraySubtype(typ.Name)
	itemSize := wa.typeInfo.SizeOfType(itemType)

	goItemType, err := wa.typeInfo.getGoType(itemType)
	if err != nil {
		return nil, err
	}

	arr := reflect.MakeSlice(reflect.SliceOf(goItemType), int(arrLen), int(arrLen))
	for i := uint32(0); i < arrLen; i++ {
		val, err := wa.readField(ctx, mem, itemType, dataStart+(i*itemSize))
		if err != nil {
			return nil, err
		}
		arr.Index(int(i)).Set(reflect.ValueOf(val))
	}

	return arr.Interface(), nil
}

func (wa *wasmAdapter) writeArray(ctx context.Context, mod wasm.Module, typ *metadata.TypeDefinition, data any) (offset uint32, err error) {
	var arr []any
	arr, err = utils.ConvertToArray(data)
	if err != nil {
		return 0, err
	}

	var bufferOffset uint32
	var bufferSize uint32
	arrLen := uint32(len(arr))

	// unpin everything when done
	var pins = make([]uint32, 0, arrLen+1)
	defer func() {
		for _, ptr := range pins {
			err = wa.unpinWasmMemory(ctx, mod, ptr)
			if err != nil {
				break
			}
		}
	}()

	// write array buffer
	// note: empty array has no array buffer
	if arrLen > 0 {
		itemType := wa.typeInfo.GetArraySubtype(typ.Name)
		itemSize := wa.typeInfo.SizeOfType(itemType)
		bufferSize = itemSize * arrLen
		bufferOffset, err = wa.allocateWasmMemory(ctx, mod, bufferSize, 1)
		if err != nil {
			return 0, fmt.Errorf("failed to allocate memory for array buffer: %w", err)
		}

		// pin the array buffer so it can't get garbage collected
		// when we allocate the array object
		err = wa.pinWasmMemory(ctx, mod, bufferOffset)
		if err != nil {
			return 0, fmt.Errorf("failed to pin array buffer: %w", err)
		}
		pins = append(pins, bufferOffset)

		for i, v := range arr {
			itemOffset := bufferOffset + (itemSize * uint32(i))
			ptr, err := wa.writeField(ctx, mod, itemType, itemOffset, v)
			if err != nil {
				return 0, fmt.Errorf("failed to write array item: %w", err)
			}

			// If we allocated memory for the item, we need to pin it too.
			if ptr != 0 {
				err = wa.pinWasmMemory(ctx, mod, ptr)
				if err != nil {
					return 0, err
				}
				pins = append(pins, ptr)
			}
		}
	}

	// write array object
	const size = 16
	offset, err = wa.allocateWasmMemory(ctx, mod, size, typ.Id)
	if err != nil {
		return 0, err
	}

	mem := mod.Memory()
	ok := mem.WriteUint32Le(offset, bufferOffset)
	if !ok {
		return 0, fmt.Errorf("failed to write array buffer pointer")
	}

	ok = mem.WriteUint32Le(offset+4, bufferOffset)
	if !ok {
		return 0, fmt.Errorf("failed to write array data start pointer")
	}

	ok = mem.WriteUint32Le(offset+8, bufferSize)
	if !ok {
		return 0, fmt.Errorf("failed to write array bytes length")
	}

	ok = mem.WriteUint32Le(offset+12, arrLen)
	if !ok {
		return 0, fmt.Errorf("failed to write array length")
	}

	return offset, nil
}
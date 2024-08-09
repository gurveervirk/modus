/*
 * Copyright 2023 Hypermode, Inc.
 */

package assemblyscript

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"hmruntime/plugins/metadata"

	wasm "github.com/tetratelabs/wazero/api"
)

func (wa *wasmAdapter) readClass(ctx context.Context, mem wasm.Memory, typ *metadata.TypeDefinition, offset uint32) (data map[string]any, err error) {
	data = make(map[string]any)
	fieldOffset := uint32(0)
	for _, field := range typ.Fields {

		// align the field offset to the size of the field
		size := wa.typeInfo.SizeOfType(field.Type)
		mask := size - 1
		if fieldOffset&mask != 0 {
			fieldOffset = (fieldOffset | mask) + 1
		}

		// read the field value
		val, err := wa.readField(ctx, mem, field.Type, offset+fieldOffset)
		if err != nil {
			return nil, err
		}
		data[field.Name] = val

		// move to the next field
		fieldOffset += size
	}
	return data, nil
}

func (wa *wasmAdapter) readField(ctx context.Context, mem wasm.Memory, typ string, offset uint32) (data any, err error) {
	var result any
	var ok bool
	switch typ {
	case "bool":
		var val byte
		val, ok = mem.ReadByte(offset)
		result = val != 0

	case "u8":
		result, ok = mem.ReadByte(offset)

	case "u16":
		result, ok = mem.ReadUint16Le(offset)

	case "u32":
		result, ok = mem.ReadUint32Le(offset)

	case "u64":
		result, ok = mem.ReadUint64Le(offset)

	case "i8":
		var val byte
		val, ok = mem.ReadByte(offset)
		result = int8(val)

	case "i16":
		var val uint16
		val, ok = mem.ReadUint16Le(offset)
		result = int16(val)

	case "i32":
		var val uint32
		val, ok = mem.ReadUint32Le(offset)
		result = int32(val)

	case "i64":
		var val uint64
		val, ok = mem.ReadUint64Le(offset)
		result = int64(val)

	case "f32":
		result, ok = mem.ReadFloat32Le(offset)

	case "f64":
		result, ok = mem.ReadFloat64Le(offset)

	default:
		// Managed types have pointers to the actual data.
		p, ok := mem.ReadUint32Le(offset)
		if !ok {
			return nil, fmt.Errorf("error reading %s pointer from wasm memory", typ)
		}

		// Handle null values if the type is nullable
		if wa.typeInfo.IsNullable(typ) {
			if p == 0 {
				return nil, nil
			}
			typ = wa.typeInfo.GetUnderlyingType(typ)
		} else if p == 0 {
			return nil, fmt.Errorf("null pointer encountered for non-nullable type %s", typ)
		}

		// Read the actual data.
		return wa.readObject(ctx, mem, typ, p)
	}

	if !ok {
		return nil, fmt.Errorf("error reading %s from wasm memory", typ)
	}

	return result, nil
}

func (wa *wasmAdapter) writeClass(ctx context.Context, mod wasm.Module, typ *metadata.TypeDefinition, data any) (offset uint32, err error) {

	// unpin everything when done
	pins := make([]uint32, 0, len(typ.Fields)+1)
	defer func() {
		for _, ptr := range pins {
			err = wa.unpinWasmMemory(ctx, mod, ptr)
			if err != nil {
				break
			}
		}
	}()

	// calculate total size of all fields
	totalSize := uint32(0)
	for _, field := range typ.Fields {
		size := wa.typeInfo.SizeOfType(field.Type)
		mask := size - 1
		if totalSize&mask != 0 {
			totalSize = (totalSize | mask) + 1
		}
		totalSize += size
	}

	// Allocate memory for the object
	offset, err = wa.allocateWasmMemory(ctx, mod, totalSize, typ.Id)
	if err != nil {
		return 0, err
	}

	// we need to pin the object in memory so it doesn't get garbage collected
	// if we allocate more memory when writing a field before returning the object
	err = wa.pinWasmMemory(ctx, mod, offset)
	if err != nil {
		return 0, err
	}
	pins = append(pins, offset)

	// When reading from a struct, we need to use reflection to access the fields.
	// Reflect the data value just once to avoid the overhead of reflection for each field.
	var rv reflect.Value
	if _, ok := data.(map[string]any); !ok {
		rv = reflect.ValueOf(data)
	}

	// Loop over all fields in the class definition.
	fieldOffset := uint32(0)
	for _, field := range typ.Fields {

		// Read the field value from the data object.
		var val any
		switch data := data.(type) {
		case map[string]any:
			// When reading from a map, field matching should be case sensitive.
			// This allows the user to control the casing of the fields in their GraphQL
			// types, to match the casing of the fields in their AssemblyScript classes.
			val = data[field.Name]
		default:
			// When reading directly from a struct, field matching should be case insensitive.
			// This allows our host functions to use Go-defined structs with different casing
			// than the AssemblyScript class definition.
			f := rv.FieldByNameFunc(func(s string) bool { return strings.EqualFold(s, field.Name) })
			val = f.Interface()
		}

		// align the field offset to the size of the field
		size := wa.typeInfo.SizeOfType(field.Type)
		mask := size - 1
		if fieldOffset&mask != 0 {
			fieldOffset = (fieldOffset | mask) + 1
		}

		// Write the field value to WASM memory.
		ptr, err := wa.writeField(ctx, mod, field.Type, offset+fieldOffset, val)
		if err != nil {
			return 0, err
		}

		// If we allocated memory for the field, we need to pin it too.
		if ptr != 0 {
			err = wa.pinWasmMemory(ctx, mod, ptr)
			if err != nil {
				return 0, err
			}
			pins = append(pins, ptr)
		}

		// move to the next field
		fieldOffset += size
	}

	return offset, nil
}

func (wa *wasmAdapter) writeField(ctx context.Context, mod wasm.Module, typ string, offset uint32, val any) (ptr uint32, err error) {
	enc, err := wa.encodeValue(ctx, mod, typ, val)
	if err != nil {
		return 0, err
	}

	mem := mod.Memory()

	var ok bool
	switch typ {
	case "bool", "i8", "u8":
		ok = mem.WriteByte(offset, byte(enc))
	case "i16", "u16":
		ok = mem.WriteUint16Le(offset, uint16(enc))
	case "i32", "u32", "f32":
		ok = mem.WriteUint32Le(offset, uint32(enc))
	case "i64", "u64", "f64":
		ok = mem.WriteUint64Le(offset, enc)
	default: // managed types
		ptr = uint32(enc) // return pointer to the managed object
		ok = mem.WriteUint32Le(offset, uint32(enc))
	}

	if !ok {
		return ptr, fmt.Errorf("error writing %s to wasm memory", typ)
	}

	return ptr, nil
}
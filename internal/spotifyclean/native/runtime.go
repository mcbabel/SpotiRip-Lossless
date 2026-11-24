package native

/*
#cgo CFLAGS: -O3 -I${SRCDIR}
#cgo CXXFLAGS: -O3 -std=c++17 -I${SRCDIR}
#cgo !windows LDFLAGS: -static-libgcc -Wl,-Bstatic -lstdc++ -Wl,-Bdynamic -lm
#cgo windows LDFLAGS: -static-libgcc -Wl,-Bstatic -lstdc++ -lwinpthread -Wl,-Bdynamic -lm
#include "pcode_runtime.h"
*/
import "C"

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"fmt"
	"io"
	"unsafe"
)

//go:embed program.bin.gz
var compressedProgram []byte

//go:embed initial-memory.bin.gz
var compressedMemory []byte

type Runtime struct {
	ptr *C.clean_runtime
}

func decompress(data []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func New() (*Runtime, error) {
	program, err := decompress(compressedProgram)
	if err != nil {
		return nil, fmt.Errorf("operation program: %w", err)
	}
	memory, err := decompress(compressedMemory)
	if err != nil {
		return nil, fmt.Errorf("memory image: %w", err)
	}
	errorBuffer := make([]C.char, 512)
	ptr := C.clean_open(
		unsafe.Pointer(&program[0]), C.size_t(len(program)),
		unsafe.Pointer(&memory[0]), C.size_t(len(memory)),
		&errorBuffer[0], C.size_t(len(errorBuffer)),
	)
	if ptr == nil {
		return nil, fmt.Errorf("clean-room runtime: %s", C.GoString(&errorBuffer[0]))
	}
	return &Runtime{ptr: ptr}, nil
}

func (r *Runtime) failure(operation string) error {
	if r == nil || r.ptr == nil {
		return fmt.Errorf("%s: runtime is closed", operation)
	}
	return fmt.Errorf("%s: %s", operation, C.GoString(C.clean_error(r.ptr)))
}

func (r *Runtime) Derive(response []byte) ([]byte, error) {
	if len(response) != 24 {
		return nil, fmt.Errorf("expected a 24-byte PlayPlay response")
	}
	result := make([]byte, 28)
	if r == nil || r.ptr == nil {
		return nil, r.failure("PlayPlay")
	}
	if C.clean_derive(r.ptr, unsafe.Pointer(&response[0]), C.size_t(len(response)), (*C.uint8_t)(unsafe.Pointer(&result[0]))) != 0 {
		return nil, r.failure("PlayPlay")
	}
	return result, nil
}

func (r *Runtime) SetEntropy(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("empty entropy input")
	}
	if r == nil || r.ptr == nil {
		return r.failure("entropy")
	}
	if C.clean_set_entropy(r.ptr, unsafe.Pointer(&data[0]), C.size_t(len(data))) != 0 {
		return r.failure("entropy")
	}
	return nil
}

func (r *Runtime) InitCipher(key []byte) error {
	if len(key) != 28 {
		return fmt.Errorf("expected 28 bytes of cipher key material")
	}
	if r == nil || r.ptr == nil {
		return r.failure("cipher initialization")
	}
	if C.clean_init_cipher(r.ptr, (*C.uint8_t)(unsafe.Pointer(&key[0]))) != 0 {
		return r.failure("cipher initialization")
	}
	return nil
}

func (r *Runtime) CipherState() ([]byte, error) {
	if r == nil || r.ptr == nil {
		return nil, r.failure("reading cipher state")
	}
	state := make([]byte, 0x310)
	if C.clean_cipher_state(r.ptr, unsafe.Pointer(&state[0]), C.size_t(len(state))) != 0 {
		return nil, r.failure("reading cipher state")
	}
	return state, nil
}

func (r *Runtime) SetCipherState(state []byte) error {
	if len(state) != 0x310 {
		return fmt.Errorf("cipher context must be 784 bytes")
	}
	if r == nil || r.ptr == nil {
		return r.failure("setting cipher state")
	}
	if C.clean_set_cipher_state(r.ptr, unsafe.Pointer(&state[0]), C.size_t(len(state))) != 0 {
		return r.failure("setting cipher state")
	}
	return nil
}

func (r *Runtime) SeekCipher(block uint64) ([]byte, error) {
	if r == nil || r.ptr == nil {
		return nil, r.failure("cipher seek")
	}
	if C.clean_seek_cipher(r.ptr, C.uint64_t(block)) != 0 {
		return nil, r.failure("cipher seek")
	}
	return r.CipherState()
}

func (r *Runtime) TransformCipher() ([]byte, error) {
	if r == nil || r.ptr == nil {
		return nil, r.failure("cipher state transition")
	}
	if C.clean_transform_cipher(r.ptr) != 0 {
		return nil, r.failure("cipher state transition")
	}
	return r.CipherState()
}

func (r *Runtime) Close() {
	if r != nil && r.ptr != nil {
		C.clean_close(r.ptr)
		r.ptr = nil
	}
}

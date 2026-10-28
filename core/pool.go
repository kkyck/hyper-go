package main

// pool.go — sync.Pool for reusable objects (reduce GC pressure).
// For high-frequency operations, saves ~10-50µs per allocation.

import (
	"bytes"
	"strings"
	"sync"
)

var stringBuilderPool = sync.Pool{
	New: func() interface{} { return &strings.Builder{} },
}

func getStringBuilder() *strings.Builder {
	b := stringBuilderPool.Get().(*strings.Builder)
	b.Reset()
	return b
}

func putStringBuilder(b *strings.Builder) {
	if b != nil {
		stringBuilderPool.Put(b)
	}
}

var bytesBufferPool = sync.Pool{
	New: func() interface{} { return &bytes.Buffer{} },
}

func getBytesBuffer() *bytes.Buffer {
	b := bytesBufferPool.Get().(*bytes.Buffer)
	b.Reset()
	return b
}

func putBytesBuffer(b *bytes.Buffer) {
	if b != nil {
		bytesBufferPool.Put(b)
	}
}

package audit

import "sync"

type CaptureBuffer struct {
	limit     int64
	size      int64
	truncated bool
	data      []byte
	mu        sync.Mutex
}

func NewCaptureBuffer(limit int64) *CaptureBuffer {
	capacity := int64(4096)
	if limit > 0 {
		capacity = minInt64(limit, 4096)
	}
	return &CaptureBuffer{
		limit: limit,
		data:  make([]byte, 0, capacity),
	}
}

func (b *CaptureBuffer) Write(p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.size += int64(len(p))
	if b.limit <= 0 {
		b.data = append(b.data, p...)
		return
	}
	if int64(len(b.data)) >= b.limit {
		if len(p) > 0 {
			b.truncated = true
		}
		return
	}

	remaining := b.limit - int64(len(b.data))
	if int64(len(p)) > remaining {
		b.data = append(b.data, p[:remaining]...)
		b.truncated = true
		return
	}
	b.data = append(b.data, p...)
}

func (b *CaptureBuffer) Body(contentType string) Body {
	b.mu.Lock()
	defer b.mu.Unlock()

	return Body{
		ContentType: contentType,
		Content:     string(b.data),
		SizeBytes:   b.size,
		Truncated:   b.truncated,
	}
}

func BodyFromBytes(data []byte, contentType string, limit int64) Body {
	buffer := NewCaptureBuffer(limit)
	buffer.Write(data)
	return buffer.Body(contentType)
}

func minInt64(a int64, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

package blob

import (
	"context"
	"fmt"
	"net/url"
	"sync"
)

// Memory is the in-process store used when tests have no R2 account.
// PresignGet returns an unsigned local URL so callers can assert the key
// without treating it as a credential.
type Memory struct {
	mu      sync.Mutex
	objects map[string]Object
	Puts    int
	Deletes int
}

func NewMemory() *Memory {
	return &Memory{objects: map[string]Object{}}
}

func (m *Memory) Put(_ context.Context, object Object) error {
	if object.Key == "" || len(object.Body) == 0 {
		return fmt.Errorf("memory blob requires a key and a body")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := object
	copied.Body = append([]byte(nil), object.Body...)
	m.objects[object.Key] = copied
	m.Puts++
	return nil
}

func (m *Memory) PresignGet(_ context.Context, get Get) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[get.Key]; !ok {
		return "", fmt.Errorf("memory blob is missing")
	}
	if get.TTL <= 0 {
		return "", fmt.Errorf("memory blob ttl is required")
	}
	return "https://blob.invalid/get?key=" + url.QueryEscape(get.Key) + "&ttl=" + url.QueryEscape(get.TTL.String()), nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	m.Deletes++
	return nil
}

// Len reports how many objects are currently stored.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objects)
}

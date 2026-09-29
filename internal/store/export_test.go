package store

// Loads exposes how many times the cache actually read state.json.
func Loads(c *Cache) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loads
}

package vfscache

// ManagedBytes refreshes the managed cache's fetched data-byte accounting.
// Unlike Stats, it does not wait for the next periodic cleaner pass. Open and
// dirty data is included; filesystem allocation and metadata overhead is not.
func (c *Cache) ManagedBytes() int64 {
	if !c.managed.Load() {
		return 0
	}
	return c.updateUsed()
}

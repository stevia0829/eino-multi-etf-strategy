package datasource

import (
	"sync"
	"time"

	"github.com/multi-agents-etf-trade-strategy/types"
)

// CachedDataSource 包装任意 ETFDataSource，对所有 GetKLineAsOf 调用做内存缓存。
// 用于回测对比场景：第二个变体复用第一个变体已拉取的数据，保证基线完全一致。
//
// 缓存 key = code|asOf（不含 days），存储已拉取的最大 bar 数。
// 查找时：若缓存 bar 数 >= 请求 days，直接返回末尾 N 根（超集截取）；
// 否则穿透到底层源拉取完整数据并替换缓存。这样不管哪个变体先跑，
// 后跑的变体总能从缓存中获得一致的数据子集。
type CachedDataSource struct {
	inner ETFDataSource
	mu    sync.RWMutex
	cache map[string][]types.KLine
}

// NewCachedDataSource 创建缓存包装器。
func NewCachedDataSource(inner ETFDataSource) *CachedDataSource {
	return &CachedDataSource{
		inner: inner,
		cache: make(map[string][]types.KLine),
	}
}

// cacheKey 仅用 code + asOf 的日期部分做 key（不含 days）。
func cacheKey(code string, asOf time.Time) string {
	d := int64(0)
	if !asOf.IsZero() {
		d = asOf.Unix() / 86400
	}
	return code + "|" + itoa64(d)
}

// itoa64 快速 int64→字符串。
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// ListAllETFs 直接穿透到底层（ETF 列表本身不缓存，体积小且低频）。
func (c *CachedDataSource) ListAllETFs() ([]types.ETF, error) {
	return c.inner.ListAllETFs()
}

// GetKLine 直接穿透（不带 asOf 的便捷方法，回测不用这条路径）。
func (c *CachedDataSource) GetKLine(code string, days int) ([]types.KLine, error) {
	return c.inner.GetKLine(code, days)
}

// GetKLineAsOf 超集感知缓存：
//   - 命中（缓存 bar 数 >= days）：返回末尾 N 根的副本
//   - 未命中或缓存不足：穿透拉取，更新缓存为更大的数据集
func (c *CachedDataSource) GetKLineAsOf(code string, days int, asOf time.Time) ([]types.KLine, error) {
	key := cacheKey(code, asOf)

	// 先尝试读缓存
	c.mu.RLock()
	if cached, ok := c.cache[key]; ok && len(cached) >= days {
		c.mu.RUnlock()
		// 超集截取：返回末尾 days 根
		start := len(cached) - days
		out := make([]types.KLine, days)
		copy(out, cached[start:])
		return out, nil
	}
	c.mu.RUnlock()

	// 缓存未命中或 bar 数不够：穿透到底层源拉取
	data, err := c.inner.GetKLineAsOf(code, days, asOf)
	if err != nil {
		return nil, err
	}

	// 更新缓存：保留更大的数据集（已有 vs 新拉取）
	c.mu.Lock()
	if existing, ok := c.cache[key]; ok && len(existing) > len(data) {
		// 已有缓存更大，不覆盖（后续请求仍可从已有缓存截取）
	} else {
		c.cache[key] = data
	}
	c.mu.Unlock()

	// 返回副本
	out := make([]types.KLine, len(data))
	copy(out, data)
	return out, nil
}

// CacheStats 返回缓存条目数。
func (c *CachedDataSource) CacheStats() (entries int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache)
}

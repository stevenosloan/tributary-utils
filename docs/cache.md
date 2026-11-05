# cache

`cache.Store` is a small byte store with expiry. Clients that cache take one
(`modelflow.Options.Cache`, `weather.Options.Cache`), so a caller can keep results in process memory by
default, or share them across instances by supplying its own store.

```go
type Store interface {
	Get(ctx context.Context, key string) (val []byte, ok bool, err error)
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
}
```

## Provided

- `cache.NewMemory()` is a map with expiry. An expired value reads as a miss
  at once. `Get` removes an expired entry it finds, and `Set` removes the rest,
  sweeping at most once per minute. It copies byte slices on `Set` and on `Get`, so callers can't
  change what is stored. A `ttl <= 0` stores nothing and removes any value under the key.
- `cache.None` never stores: `Get` always misses and `Set` does nothing. Pass
  it to turn caching off.

## Writing a store

- Values are opaque bytes. Keys are strings, namespaced and versioned by the
  client (e.g. `modelflow:v3:https://api.water.noaa.gov:6495864:short_range`,
  `weather:v1:https://api.weather.gov:obs:KCNI`).
- The TTL comes with each write; the store drops the value when it lapses. A
  `ttl <= 0` stores nothing and removes any value already held under the key.
- Report a missing or expired value as `ok == false` with a nil error. Return
  an error only when the store itself failed.
- Clients treat any error as a miss and carry on, so a store that is down
  makes calls slower, not failed. Don't block for long: honour `ctx`.
- Be safe for concurrent use; one store serves many calls at once.

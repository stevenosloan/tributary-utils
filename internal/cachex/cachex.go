// Package cachex holds the key and JSON helpers shared by the packages in this
// module that cache through a cache.Store.
package cachex

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"github.com/stevenosloan/tributary-utils/cache"
)

// Prefix returns the start of every key a client stores under: the namespace,
// the version of the stored shape, and the base URL's scheme and host (both
// lowercased), followed by a colon, e.g. "weather:v1:https://api.weather.gov:".
// Including the endpoint lets clients on different endpoints share a Store.
func Prefix(namespace, version, base string) string {
	origin := strings.ToLower(base)
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		origin = strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
	}
	return namespace + ":" + version + ":" + origin + ":"
}

// Get decodes the JSON value under key into dst, reporting whether it did. A
// store error or an undecodable value is a miss.
func Get(ctx context.Context, s cache.Store, key string, dst any) bool {
	b, ok, err := s.Get(ctx, key)
	if err != nil || !ok {
		return false
	}
	return json.Unmarshal(b, dst) == nil
}

// Set stores v as JSON for ttl, ignoring any error.
func Set(ctx context.Context, s cache.Store, key string, v any, ttl time.Duration) {
	if b, err := json.Marshal(v); err == nil {
		_ = s.Set(ctx, key, b, ttl)
	}
}

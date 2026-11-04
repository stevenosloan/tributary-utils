# modelflow

`modelflow.Client.Flow(ctx, comid)` returns National Water Model (NWM)
streamflow for a river reach: the model's estimate of the last two days and its
forecast for the next 18 hours. The reach is an NHDPlusV2 reach identified by
COMID, the same COMID `gauges` returns, so it works anywhere on the network,
gauge or no gauge.

```go
c := modelflow.NewClient(nil)
res, err := c.Flow(ctx, 6495864)
if err != nil {
	return err // bad COMID, or ctx done: no call was made or finished
}
if res.AllFailed() {
	// Nothing usable. res.Reach has only the ID.
	for s, err := range res.Errors {
		if errors.Is(err, modelflow.ErrUnavailable) {
			log.Printf("%s: unknown reach or NWPS outage", s)
		} else {
			log.Printf("%s: %v", s, err)
		}
	}
	return errors.New("no model flow")
}
for s, err := range res.Errors {
	log.Printf("%s: %v", s, err) // some series failed, the rest are usable
}
if run := res.Run(modelflow.ShortRange); run != nil {
	for _, p := range run.Points {
		fmt.Println(p.Time, p.Flow, run.Unit)
	}
}
```

To start from a point, snap it first: `gauges.Client.Snap(ctx, lat, lon)`
returns the `COMID`.

## Source

The National Water Prediction Service (NWPS) API from NOAA,
`https://api.water.noaa.gov/nwps/v1` (`DefaultBaseURL`). `Flow` makes one
request per series, in parallel (at most 4 in flight):

| Op (`Error.Op`) | Request |
|-----------------|---------|
| the series name, e.g. `short_range` | `GET /reaches/{comid}/streamflow?series=short_range` |

Only the object for the requested series is filled in the response. Each
series object (`{}`, `[]` or `null` when empty) maps a member name to `{referenceTime, units, data}`.

## Series

| Series | Constant | Meaning | Live today |
|--------|----------|---------|------------|
| `analysis_assimilation` | `Analysis` | Recent model estimate, hourly | About 48 values, the last 2 days |
| `short_range` | `ShortRange` | Forecast, hourly, from a run about 2h old | 18 values |
| `medium_range` | `MediumRange` | Forecast days ahead, possibly several ensemble members | Empty |
| `medium_range_blend` | `MediumRangeBlend` | Blended days-ahead forecast | Empty |
| `long_range` | `LongRange` | Forecast weeks ahead, ensemble members | Empty |

`Options.Series` picks what `Flow` fetches. The default, `DefaultSeries()`, is
`Analysis` and `ShortRange`. Medium and long range are opt-in because NWPS
has served them empty for every reach checked; add them to `Options.Series`
when they are available. A value that isn't one of the constants is sent as
given, and NWPS's refusal is reported for that series in `Result.Errors`.

## What comes back

| Field | Meaning |
|-------|---------|
| `Result.Reach` | `ID` (COMID), `Name`, `Lat`, `Lon`, from the first requested series that came back. `Lat` and `Lon` are `*float64`, nil when NWPS gives no location. If every series failed, only `ID` is set (from the requested COMID) |
| `Result.Runs` | Runs per requested series. A series that came back empty has an empty slice, not a missing key. A series that wasn't requested or failed is absent |
| `Result.Errors` | Every requested series that failed, with its error, including when all did; `nil` if none did |
| `Run.Series`, `Run.Member` | The series, and the ensemble member. Analysis and short range have one, named `series`. Medium and long range may have several (`mean`, `member1`, ...); members are sorted by name |
| `Run.ReferenceTime` | When the model run started |
| `Run.Unit` | `ft^3/s` for what NWPS serves. `ft³/s`, `ft3/s`, `ft^3/s` and `cfs` are all written `ft^3/s`, as in `readings`; any other unit is left as given |
| `Run.Points` | `Time` and `Flow`, sorted by time. Points with a missing or non-finite flow or an unparseable time are dropped, and a member left with no points is left out |

`Result.Run(series)` returns the first run of a series, or nil if it failed,
wasn't requested or came back empty.

`Result.AllFailed()` is true when every requested series failed (`Errors` has
an entry for each and `Runs` is empty).

One series failing doesn't fail the others, and series failures are never
`Flow`'s own error, even when all of them fail: `Flow` returns `(res, nil)`
and `res.Errors` holds each one. `Flow` returns `(nil, err)` only when it
couldn't make the call: a `comid <= 0`, or a context already done before the
call or ended while the series were fetched. Both are an `*Error` with `Op`
`flow`; the context case wraps `ctx.Err()`, so `errors.Is(err,
context.Canceled)` and `context.DeadlineExceeded` work.

## Errors

`*Error` reports a failed request: `Op` is the series (`flow` for a bad COMID or a done context),
`Status` is the HTTP status (0 when no response arrived), and `Timeout()`
reports whether it timed out, on our side or as a 504. The default client
allows 30s per request.

`ErrUnavailable` is wrapped inside the `*Error` (check with `errors.Is`) for
any non-200 answer whose body has code 14 or the message "Data unavailable",
in practice a `503`. NWPS answers that way
both for a reach ID it doesn't know and during an outage, and the two can't be
told apart from the response. If it persists for one COMID while others work,
the COMID is probably not on the model's network.

## Retry

A request that fails with 502, 503 or 504, or times out, is retried once after
500ms (cut short if the context ends). That includes "Data unavailable",
since it can be a passing outage. An unknown COMID therefore costs one retry
(about half a second) before it fails. Other failures (400, 404, a bad body) are
not retried.

## Caching

`Flow` caches each series separately, so a call fetches only the series not
already held. The store is `Options.Cache`, a [`cache.Store`](cache.md):
`nil` (the default) gives each `Client` its own in-memory store, `cache.None`
turns caching off, and a shared or remote store can be supplied.

| | |
|-|-|
| Key | `modelflow:v2:<host>:<comid>:<series>`, the host of the base URL lowercased, so clients on different endpoints can share a store |
| Value | JSON of the reach and the series' runs |
| TTL | `Options.CacheTTL`, default 15 minutes |

An empty series is cached too, so an empty medium range isn't requested again
on every call. Failures are never cached. A store that errors, or holds a value
that doesn't decode, is treated as a miss, and a failed write is ignored:
`Flow` never fails because of the cache. Change the `v2` in the key if the
stored shape changes.

## Accuracy

These are model estimates, not measurements. The NWM is uneven on small
headwater streams, where the estimate can be far off in level or timing and
tends to miss local storms. The analysis is the model's best account of the
recent past, and at a gauged reach it is nudged toward the gauge's readings,
so it is closest there. Where a gauge exists, prefer its
[readings](readings.md) for what the flow is now, and use this for reaches
without one and for the short-range trend.

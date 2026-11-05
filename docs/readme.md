# utils docs

Public utility libraries for Tributary. Standard library only, no secrets or
private URLs (see the workspace readme).

## Layout

```
utils/
├── gauges/          # package gauges: lat/long -> nearby USGS gauges
│   └── render/      # package render: Result -> SVG drawing
├── readings/        # package readings: gauge site numbers -> latest and past values
├── modelflow/       # package modelflow: COMID -> National Water Model streamflow
├── weather/         # package weather: lat/long -> NWS observations and forecast
├── cache/           # package cache: Store interface, in-memory store
├── internal/        # httpx: HTTP helpers shared by the packages above
├── cmd/
│   ├── gauges/      # CLI: table/JSON output, -svg to draw
│   ├── modelflow/   # CLI: model flow for a COMID or a point
│   ├── flowcheck/   # CLI: record NWM forecasts, score them against gauges
│   └── weather/     # CLI: stations table, -forecast, -json
└── docs/            # you are here
```

Each library is a top-level package; each CLI lives in `cmd/<name>` and is a
thin wrapper over its package.

| Doc | Covers |
|-----|--------|
| [gauges.md](gauges.md) | How `gauges.Find` works, data sources, drawing, known limits |
| [readings.md](readings.md) | Latest and past gauge readings: source, what comes back, API key |
| [modelflow.md](modelflow.md) | National Water Model flow per reach: source, series, errors, caching, accuracy |
| [flowcheck.md](flowcheck.md) | Measuring how accurate NWM short-range forecasts are against gauges: what it shows, how to run it |
| [weather.md](weather.md) | NWS observations and forecast: User-Agent, nearby stations, precipitation totals, caching |
| [cache.md](cache.md) | The `Store` interface, the in-memory store, writing your own |

## Testing

```sh
go test ./...                                     # unit tests, fake servers
TRIBUTARY_LIVE=1 go test -run Live -v ./gauges    # hits the real NLDI
TRIBUTARY_LIVE=1 go test -run Live -v ./readings  # hits the real Water Data API (Latest and History)
TRIBUTARY_LIVE=1 go test -run Live -v ./modelflow # hits the real NWPS API
TRIBUTARY_LIVE=1 NWS_USER_AGENT='(app, contact)' go test -run Live -v ./weather # hits the real NWS API

go run ./cmd/gauges -links <lat> <lon>            # manual inspection
go run ./cmd/gauges -svg out.svg <lat> <lon>      # draw the result
go run ./cmd/modelflow <comid>                    # model flow for a reach
go run ./cmd/modelflow -at <lat> <lon>            # snap a point, then model flow
go run ./cmd/weather -forecast -ua '(app, contact)' <lat> <lon>  # conditions and forecast
go run ./cmd/flowcheck record -sites cmd/flowcheck/sites.txt -dir <dir>  # save current forecasts
go run ./cmd/flowcheck report -dir <dir>          # score saved forecasts against gauges
```

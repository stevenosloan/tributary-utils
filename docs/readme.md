# utils docs

Public utility libraries for Tributary. Standard library only, no secrets or
private URLs (see the workspace readme).

## Layout

```
utils/
├── gauges/          # package gauges: lat/long -> nearby USGS gauges
│   ├── gauges.go
│   ├── gauges_test.go
│   └── render/      # package render: Result -> SVG drawing
├── readings/        # package readings: gauge site numbers -> latest values
├── cmd/
│   └── gauges/      # CLI: table/JSON output, -svg to draw
└── docs/            # you are here
```

Each library is a top-level package; each CLI lives in `cmd/<name>` and is a
thin wrapper over its package.

| Doc | Covers |
|-----|--------|
| [gauges.md](gauges.md) | How `gauges.Find` works, data sources, drawing, known limits |
| [readings.md](readings.md) | Latest gauge readings: source, what comes back, API key |

## Testing

```sh
go test ./...                                    # unit tests, fake servers
TRIBUTARY_LIVE=1 go test -run Live -v ./gauges   # hits the real NLDI
TRIBUTARY_LIVE=1 go test -run Live -v ./readings # hits the real Water Data API
go run ./cmd/gauges -links <lat> <lon>         # manual inspection
go run ./cmd/gauges -svg out.svg <lat> <lon>   # draw the result
```

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
├── cmd/
│   └── gauges/      # CLI: table/JSON output, -svg to draw
└── docs/            # you are here
```

Each library is a top-level package; each CLI lives in `cmd/<name>` and is a
thin wrapper over its package.

| Doc | Covers |
|-----|--------|
| [gauges.md](gauges.md) | How `gauges.Find` works, data sources, drawing, known limits |

## Testing

```sh
go test ./...                                  # unit tests, fake NLDI server
TRIBUTARY_LIVE=1 go test -run Live -v ./gauges # hits the real NLDI
go run ./cmd/gauges -links <lat> <lon>         # manual inspection
go run ./cmd/gauges -svg out.svg <lat> <lon>   # draw the result
```

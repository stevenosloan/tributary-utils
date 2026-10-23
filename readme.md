# tributary-utils

Public Go utilities for Tributary. Standard library only.

## gauges

Given a lat/long, find nearby USGS stream gauges on the same river network, each
tagged upstream or downstream of the point with the distance along the river.
It uses the USGS NLDI (free, no API key).

![Example gauges result for a spot on Amicalola Creek, GA](docs/gauges-example.svg)

### Run it

```sh
go run ./cmd/gauges 34.49466 -84.27372                 # table
go run ./cmd/gauges -links 34.49466 -84.27372          # add waterdata.usgs.gov links
go run ./cmd/gauges -km 50 -json 34.49466 -84.27372    # wider cap, JSON output
go run ./cmd/gauges -svg out.svg 34.49466 -84.27372    # also draw the result
```

Arguments are `<lat> <lon>`. `-km` caps the distance searched along the river
(default 25).

### Use it

```go
c := gauges.NewClient(nil)
res, err := c.Find(ctx, 34.49466, -84.27372, 25)
for _, g := range res.Gauges {
	fmt.Println(g.Direction, g.SiteNo, g.NetworkKm, g.URL)
}
```

`c.Snap(ctx, lat, lon)` only finds the river reach the point lands on (one
request), with `SnapKm` to check it's the stream you meant.

### Docs

How it works, the algorithm, diagrams, and known limits: [docs/gauges.md](docs/gauges.md).

# readings

`readings.Client.Latest(ctx, siteNos)` returns the most recent value of every
continuous time series at a set of USGS gauges: discharge, gage height, water
temperature, and whatever else each gauge measures.

```go
c := readings.NewClient(nil) // or &readings.Options{APIKey: key}
got, err := c.Latest(ctx, []string{"01646500", "02390000"})
for _, r := range got["01646500"] {
	fmt.Println(r.Parameter, r.Sublocation, r.Value, r.Unit, r.Time, r.Discontinued)
}
```

Site numbers are the `gauges.Gauge.SiteNo` values (a `USGS-` prefix is also
accepted), so a `gauges.Find` result feeds straight in.

## Source

The USGS Water Data OGC API, `https://api.waterdata.usgs.gov/ogcapi/v1`
(`DefaultBaseURL`). It replaces the legacy `waterservices.usgs.gov` (NWIS)
services, which USGS is decommissioning in early 2027.

Each call to `Latest` makes two requests in parallel, one per collection,
covering every site at once:

| Op (`Error.Op`) | Request | Gives |
|-----------------|---------|-------|
| `latest-continuous` | `GET /collections/latest-continuous/items?monitoring_location_id=USGS-a,USGS-b` | Latest value, time, unit, approval and qualifiers for each series |
| `time-series-metadata` | `GET /collections/time-series-metadata/items?monitoring_location_id=...&computation_period_identifier=Points` | Parameter name, sublocation and status for each continuous series |

The two are joined on the series ID. Results are paged (`next` links are
followed).

### API key

Keyless requests work but get lower rate limits. Pass an
[api.data.gov](https://api.data.gov/signup/) key as `Options.APIKey`; it is
sent in the `X-Api-Key` header.

## What comes back

A gauge doesn't report a fixed set of parameters. The result has one
`Reading` per time series, so a gauge can have:

- **Several series for one parameter**, from different sensors. `Sublocation`
  tells them apart, e.g. `From multiparameter sonde` or
  `1.0 ft from riverbed (bottom)`.
- **Discontinued series.** The latest value of a retired sensor is still
  returned, possibly years old, with `Discontinued` set. USGS marks it in the
  series metadata or with a `DISCONTINUED` qualifier.
- **Old values that aren't marked discontinued.** A series can stop reporting
  without being retired. Compare `Time` to now to judge freshness.
- **Nothing at all.** Some gauges indexed on the river network have no
  continuous data. They are absent from the map, which isn't an error.

Readings are sorted by parameter code, then active before discontinued, then
sublocation. Values that are missing or equal to the `-999999` sentinel are
left out.

| Field | Meaning |
|-------|---------|
| `ParameterCode` | USGS parameter code, e.g. `00060` discharge, `00065` gage height, `00010` water temperature |
| `Parameter` | Name, e.g. `Discharge`. `""` if the series has no metadata |
| `Sublocation` | Which sensor, when a gauge has several. `""` if not given |
| `Unit` | e.g. `ft^3/s`, `ft`, `degC` |
| `Value`, `Time` | The latest value and when it was measured |
| `Approval` | `Provisional` (recent, may be revised) or `Approved` |
| `Qualifiers` | USGS qualifier codes, e.g. `ICE`, `EQUIP` |
| `Discontinued` | The gauge no longer reports this series |

## Errors

`*Error` reports a failed request: `Op` names the collection, `Status` is the
HTTP status (0 when no response arrived), and `Timeout()` reports whether it
timed out, on our side or as a 504. The default client allows 30s per
request. A 429 means the rate limit was hit; an API key raises it.

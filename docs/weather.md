# weather

`weather.Client.Current(ctx, lat, lon)` returns the latest conditions from the
weather stations nearest a point, and `Forecast(ctx, lat, lon)` returns the
forecast for it, both from the US National Weather Service (NWS).

```go
c, err := weather.NewClient(&weather.Options{UserAgent: "(example.com, contact@example.com)"})
if err != nil {
	log.Fatal(err)
}
obs, err := c.Current(ctx, 34.42, -84.22)
for _, o := range obs {
	fmt.Println(o.StationID, o.DistanceKm, o.TemperatureC, o.PressureTrend, o.PrecipLast24hMm)
}
fc, err := c.Forecast(ctx, 34.42, -84.22)
if errors.Is(err, weather.ErrNoCoverage) {
	// outside the US (some open-ocean points give a plain 500 instead)
}
```

## User-Agent

NWS is free and keyless but asks every caller to identify itself in the
`User-Agent` header, in the form `(app name, contact)`, and may block callers
that don't. So, unlike `readings.NewClient`, `weather.NewClient` takes a
required `Options.UserAgent` and returns an error when it is empty. The header
is sent on every request. Use real contact details in your own code.

## Source

The NWS API, `https://api.weather.gov` (`DefaultBaseURL`).

| Op (`Error.Op`) | Request | Gives |
|-----------------|---------|-------|
| `points` | `GET /points/{lat},{lon}` | The point's forecast URLs, station list URL and time zone |
| `stations` | `GET {observationStations}?limit=30` | Stations of the point's grid cell: id, name, location, elevation |
| `observations` | `GET /stations/{id}/observations?limit=500&start=<now-26h>` | Recent reports of one station, newest first (500 is NWS's maximum and covers 26 hours of 5-minute reports; further pages are not followed) |
| `forecast` | `GET {forecast}?units=si` | 12-hour periods, about 7 days |
| `forecast-hourly` | `GET {forecastHourly}?units=si` | Hourly periods, about 7 days |

URLs taken from a response (`forecast`, `forecastHourly`, `observationStations`)
are followed only if they have the same scheme and host as the base URL;
otherwise the call returns an `*Error`.

### Caching

Results are cached in `Options.Cache`, a [`cache.Store`](cache.md): `nil` (the
default) gives each `Client` its own in-memory store, `cache.None` turns caching
off, and a shared or remote store can be supplied. Values are JSON.

| What | Key (after the prefix) | TTL option, default |
|------|-------------------------|---------------------|
| Grid data of a point: forecast URLs, station list URL, time zone | `points:<lat,lon>` | `PointTTL`, 24 hours (`DefaultPointTTL`) |
| Stations of the point's grid cell | `stations:<stations URL>` | `PointTTL` |
| Recent reports of one station (what `Current` reads) | `obs:<stationID>` | `ObservationTTL`, 5 minutes (`DefaultObservationTTL`) |
| Daily forecast document of a grid cell | `forecast:<forecast URL>` | `ForecastTTL`, 15 minutes (`DefaultForecastTTL`) |
| Hourly forecast document of a grid cell | `forecast-hourly:<hourly URL>` | `ForecastTTL` |

The prefix is `weather:v1:<scheme>://<host>:` with the base URL's scheme and
host lowercased (for example `weather:v1:https://api.weather.gov:`), so clients
on different endpoints can share a store. `<lat,lon>` is the point rounded to 4
decimals, as in the `points` request. The station list and the forecasts are
keyed by the link the `points` response gave, so a point whose links change
fetches fresh ones, and nearby points in one grid cell share them. Observations
are keyed by station, so nearby points share those too.

Each TTL option defaults when zero. A negative value turns caching off for that
kind of data (for example `ObservationTTL: -1` always fetches observations
while points and forecasts stay cached).

What is cached is the decoded upstream data. Anything derived from it is worked
out on every call against the current time: whether a station's newest report
is under two hours old, the precipitation totals, the pressure trend, and which
hourly periods fall in the next 24 hours. A cached report therefore never makes
"the last hour" mean an hour ago; at worst it is up to `ObservationTTL` behind
what NWS holds.

Failures and empty results (no stations, a station with no reports) are never
cached, so a degraded answer doesn't stick for the TTL. A store that errors, or holds a value that doesn't
decode, is treated as a miss, and a failed write is ignored: a call never fails
because of the cache. Change the `v1` in the key if the stored shape changes.

## Current conditions

`Current` returns up to three stations, nearest first:

1. The station list is sorted by straight-line (haversine) distance from the
   point, and stations over 50 km away are dropped.
2. The nearest five are queried in parallel (at most four requests at once). A
   station is kept only if its newest report is under two hours old. If that
   leaves fewer than three, the next five are queried, and so on until three
   are found or the candidates run out.
3. A station whose request fails is skipped. The first error is returned only
   if no station could be read at all. No station in range gives an empty
   result, not an error.

### Why several stations

The nearest station often lacks measurements (commonly humidity, dewpoint or
pressure), so the caller can fill gaps from the next one. Take care when doing
so: the nearest stations can be 20 to 35 km away, and at a different elevation
than the point or each other (see `DistanceKm` and `ElevationM`), so
temperature and pressure differ by more than measurement error. Prefer one
station's values together when you can.

### Fields

Anything a station doesn't report is `nil`, never 0. A reported 0 (0 mm of
rain, 0 degC) is 0.

| Field | Meaning |
|-------|---------|
| `StationID`, `StationName`, `Lat`, `Lon`, `ElevationM` | The station |
| `DistanceKm` | Straight-line distance from the requested point |
| `Time`, `Description` | When the newest report was made, and NWS's text for it, e.g. `Mostly Cloudy` |
| `TemperatureC`, `DewpointC` | degC |
| `HumidityPct` | Relative humidity, percent |
| `WindSpeedKmh`, `WindGustKmh`, `WindDirectionDeg` | km/h, and degrees the wind blows from |
| `PressureHPa` | hPa; station pressure (`barometricPressure`), or sea level pressure if the station doesn't report that |
| `PressureTrend`, `PressureChangeHPa` | `rising`, `falling` or `steady` over 3 hours, and the change; `""` and nil when unknown |
| `PrecipLast1hMm`, `PrecipLast6hMm`, `PrecipLast24hMm` | Precipitation in mm; see below |
| `PrecipLast6hMissingHours`, `PrecipLast24hMissingHours` | Hours in each window whose precipitation was unknown, so the total may be low |

### Units

NWS tags each value with a unit code, and values are converted by it:
temperature to degC, speed to km/h (m/s is multiplied by 3.6), pressure to hPa
(Pa divided by 100), precipitation to mm (m multiplied by 1000), humidity in
percent, direction in degrees. A value with a unit code that isn't recognised
is treated as missing rather than guessed at.

### Precipitation totals

Only the newest report supplies the other fields, but precipitation is
assembled from the station's report history.

A station files `precipitationLastHour` on its routine hourly report and on any
special report. A special's value overlaps the routine reports around it, so
adding up every report would double count. Many stations also file 5-minute
reports that carry no precipitation at all. So a routine report is taken to be
one filed in the last 15 minutes of the hour (minute 45 to 59) that includes
`precipitationLastHour`, even as a null. When an hour has several, one with a raw
message beats one without (a 5-minute report can carry the key as a null), and
among equals the latest is used.

**Null means dry, sometimes.** A METAR omits its precipitation group when
nothing measurable fell, and NWS turns the omission into a null, so a dry hour
and an unknown hour look alike. The raw message tells them apart. A null counts
as 0 mm only if the raw message's remarks have an `AO2` token (`AO2A`, for an
augmented report, also counts), meaning a gauge that distinguishes
precipitation from none, and no `PNO` token (precipitation not available). A
null with an empty raw message, `AO1` or no `AO` marker, or `PNO` is unknown,
as is an hour with no routine report at all.

- `PrecipLast1hMm` is the newest routine report's hour: its value, 0 for a dry
  hour as above, or nil if unknown.
- `PrecipLast6hMm` and `PrecipLast24hMm` sum the known hours, one routine
  report per clock hour (UTC), ending at the newest routine report's hour.
  `PrecipLast6hMissingHours` and `PrecipLast24hMissingHours` count the unknown
  hours in each window. With any missing, the total may be low. With every hour
  unknown the total is nil and the count is the window length (6 or 24).
- If the newest routine report is more than two hours old, nothing is known:
  the totals are nil and every hour counts as missing.

Stations that don't report on the :45 to :59 schedule never produce totals.

### Pressure trend

The newest report's pressure is compared with that of the report closest to 3
hours earlier, within 30 minutes either way, using the same measurement
(station or sea level) for both. A change under 1.0 hPa in either direction is
`steady`; otherwise it is `rising` or `falling`. With no such earlier report
the trend is `""` and the change nil.

## Forecast

`Forecast` fetches both forecasts in parallel. The result has:

| Field | Meaning |
|-------|---------|
| `Periods` | The 12-hour periods NWS publishes, about 7 days |
| `Hourly` | Hourly periods covering the next 24 hours only |
| `Updated` | When NWS last updated the forecast |
| `TimeZone` | IANA name of the point's time zone |

A `Period` has `Name` (often empty for hourly), `Start`, `End`, `IsDaytime`,
`TemperatureC`, `PrecipChancePct` (nil when NWS gives none), `WindSpeed` and
`WindDirection` (NWS's text, e.g. `5 to 10 km/h` and `NW`), `Summary` (the short
forecast) and `Detail` (the detailed forecast; may be empty for hourly).
Temperatures are requested with `units=si`; a period reported in F anyway is
converted.

## Errors

`*Error` reports a failed request: `Op` names it (table above), `Status` is the
HTTP status (0 when no response arrived), and `Timeout()` reports whether it
timed out, on our side or as a 504. The default client allows 30s per request.

A 404 from `points`, which NWS gives for a point it doesn't cover outside the
US, also satisfies `errors.Is(err, weather.ErrNoCoverage)`. NWS does not answer
every uncovered point that way: for some open-ocean points it answers 500
"Unexpected Problem" (seen live at 30, -60). That comes back as an ordinary
`*Error` with `Status` 500, not `ErrNoCoverage`, so don't read a 500 from
`points` as an NWS outage without checking where the point is.

## Limits

- Observations are kept by NWS for about 7 days; the client only asks for 26
  hours.
- Forecasts are for NWS's 2.5 km grid cell, not the exact point.
- NWS applies undisclosed rate limits; a 429 or 503 means to back off.
- Coverage is the US and its territories.

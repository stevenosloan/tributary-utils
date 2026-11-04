# flowcheck

`cmd/flowcheck` measures how accurate National Water Model (NWM) short-range
forecasts (from [modelflow](modelflow.md)) are against USGS gauge discharge
(from [readings](readings.md)). The answer tells you how far to trust the model
at reaches that have no gauge.

It has two steps. `record` saves the current 18-hour forecast for each gauge in
a list; run it hourly. `report` pairs every saved forecast point with what the
gauge then measured and prints the errors by lead time.

## What the numbers mean

For each forecast point, the lead is the whole hours from the model run's
reference time to the point's valid time (1 to 18). The point is paired with
the gauge value nearest the valid time, within 15 minutes; points with no
reading yet (still in the future) or none that close are skipped.

| Metric | Meaning |
|--------|---------|
| median absolute error | median of `abs(forecast - observed) / observed`, in percent |
| median bias | median of `(forecast - observed) / observed`; positive means the model runs high |
| MAE | mean absolute error in the flow unit (`ft^3/s`) |
| N | pairs behind the numbers |

Pairs where the gauge reads zero or less are left out of the percent metrics
(they stay in MAE). Medians are used so a few bad pairs don't dominate; pooled
rows (overall, by label) weight each gauge by how many pairs it has.

### Read short leads with care

At a gauged reach the NWM assimilates the gauge: it nudges its estimate toward
the gauge's reading. The 1 to 3 hour leads of a gauged reach therefore flatter
the model. At an ungauged reach there is no such help, so the 12 to 18 hour
errors are the more honest guide to how far to trust the model there.

### Headwaters versus downstream

Headwater streams rise and clear rain quickly, so errors there are dominated by
*timing*: a rise forecast an hour or two early or late shows as a large percent
error even if the model got the size of the event right. Large rivers and
tailwaters change slowly, so errors there are about *magnitude*. Tailwaters
(rivers just below a dam) are driven by dam releases that the model may not
know about. That is why each gauge carries a label in the sites file and the
report groups by label: judge a headwater by its timing errors and a big river
by its bias, and don't average them together.

### Name mismatch

For each gauge the report compares the gauge's name (from NLDI) with the name of
its reach (from NWPS). If they share no significant word (case-insensitive,
ignoring words like "river", "creek" and "near"), the gauge is flagged
`MISMATCH`: it may be indexed to the wrong reach, in which case its errors say
little about the model. For example, the Beaver Kill at Cooks Falls is indexed
to a reach NWPS calls Cook Brook. The flag is a heuristic; check the pair by
eye.

## Running it

A useful run is hourly for about two weeks. That catches some rain events;
over a dry spell the forecasts mostly measure base flow, which is easy. The
model runs hourly and its output appears about two hours late, so each hourly
run usually saves one new forecast per gauge. Reports can be run at any time on
what has been recorded so far. Re-running `record` within the same model hour
is harmless: a forecast already saved is skipped.

```sh
go run ./cmd/flowcheck record -sites cmd/flowcheck/sites.txt -dir $HOME/flowcheck
go run ./cmd/flowcheck report -dir $HOME/flowcheck
go run ./cmd/flowcheck report -dir $HOME/flowcheck -json > report.json
```

To run it hourly, build once and schedule the binary.

```sh
go build -o $HOME/bin/flowcheck ./cmd/flowcheck
```

A crontab line (`crontab -e`; cron needs absolute paths, so write them out):

```
7 * * * * /home/me/bin/flowcheck record -sites /home/me/flowcheck/sites.txt -dir /home/me/flowcheck >> /home/me/flowcheck/record.log 2>&1
```

On macOS, a launchd agent in `~/Library/LaunchAgents` can do the same: set
`ProgramArguments` to the binary and the `record` arguments above, and
`StartInterval` to `3600`, then `launchctl load` it. It runs missed intervals
after the machine wakes, which cron does not.

## Sites file

Plain text, one gauge per line: `<siteNo> [label]`. The label is optional free
text; the suggested ones are `headwater`, `mid`, `large` and `tailwater`.
Anything after a `#` is a comment, and blank lines are ignored. A bad line (a
site number that isn't 8 to 15 digits, or a site listed twice) is an error that
names the line.

```
02390000 headwater   # Amicalola Creek near Dawsonville, GA
01646500 large       # Potomac River near Washington, DC (Little Falls)
```

`cmd/flowcheck/sites.txt` is a ready list of 14 gauges across the regions and
sizes. Labels are saved with each forecast, so the sites file used by `record`
decides them; `report -sites FILE` replaces them with the file's labels.

## `flowcheck record`

```
flowcheck record -sites FILE -dir DIR [-timeout 2m] [-nldi-url URL] [-nwps-url URL]
```

For each gauge, `record` finds its reach (the COMID) and name through NLDI,
fetches the short-range forecast with `modelflow`, and saves it. It always asks
NWPS for the latest run (the reference time comes from the response), and skips
saving if that run's file already exists. `sites.json` is rewritten only when
its content changed. Up to four
gauges are handled at once. It prints one line per gauge: `saved`,
`already had` or `error`. A gauge failing doesn't stop the others; the exit
status is non-zero only if every gauge failed.

| Flag | Meaning |
|------|---------|
| `-sites` | sites file (required) |
| `-dir` | data directory, created as needed (required) |
| `-timeout` | overall timeout, default 2m |
| `-nldi-url` | NLDI base URL, default `gauges.DefaultBaseURL` |
| `-nwps-url` | NWPS base URL, default `modelflow.DefaultBaseURL` |

## `flowcheck report`

```
flowcheck report -dir DIR [-sites FILE] [-json] [-timeout 2m] [-usgs-url URL] [-api-key KEY]
```

For each gauge, `report` fetches its discharge (parameter `00060`) from the
first forecast point's time (minus an hour) to the last plus 15 minutes, or now
if sooner. The whole window is fetched again on every run, which is fine for
weeks of data; a very long run may need caching (a follow-up). If the gauge
returns several series, the one with the most values is the primary: each
forecast point is paired with the primary's nearest value within 15 minutes,
and only if it has none, with the nearest value of another series. This keeps
data when USGS changes the series ID partway through the window. A gauge
whose data can't be fetched is listed as `no gauge data` and a warning goes to
stderr; the exit status is non-zero if no forecasts are saved, or if some gauge fetches
failed and none returned data. A gauge with nothing to compare yet (all its
points are in the future) isn't a success or a failure.

The text output has the overall errors by lead hour; errors by label in lead
buckets (1-3h, 4-6h, 7-12h, 13-18h), where gauges without a label group as
`(none)`; and one table with a row per gauge. Its columns are GAUGE, LABEL,
FORECASTS, FROM, TO, N, 1h, 6h, 12h, 18h (median absolute error at that lead),
ALL (median absolute error over all leads), BIAS (median bias over all leads)
and NAMES (`ok`, or the mismatch and `no gauge data` notes). With no pairs yet
it says so. `-json` writes everything: per lead for
the overall result, each label and each gauge.

| Flag | Meaning |
|------|---------|
| `-dir` | data directory (required) |
| `-sites` | sites file whose labels replace the saved ones |
| `-json` | JSON output with full detail |
| `-timeout` | overall timeout, default 2m |
| `-usgs-url` | USGS Water Data API base URL, default `readings.DefaultBaseURL` |
| `-api-key` | api.data.gov key; defaults to `$USGS_API_KEY` |

## Data layout

```
DIR/
├── sites.json                          # per gauge: COMID, gauge name, reach name
└── forecasts/
    └── 02390000/
        └── 20261103T1800Z.json         # one file per model run (reference time)
```

`sites.json` caches the NLDI lookup so later runs skip it; delete a gauge's
entry to look it up again. Each forecast file holds `site`, `label`, `comid`,
`reach_name`, `gauge_name`, `reference_time`, `unit` and `points` (each
`{time, flow}`). Files are written to a temporary file and renamed, so an
interrupted run leaves no partial forecast.

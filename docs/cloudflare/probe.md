# cloudflare/probe

A read-only check of a zone against what it declares. A program that only
applies what it declares never learns that a setting was changed by hand in
the dashboard; the probe reads the live zone and reports the difference. It
never writes, so it needs read rights only.

## What it checks

| Declared | Live read | Finding |
|---|---|---|
| `SSLMode` | `GET /zones/{id}/settings/ssl` | `ssl` |
| `MinTLSVersion` | `GET /zones/{id}/settings/min_tls_version` | `min_tls_version` |
| `TotalTLS` | `GET /zones/{id}/acm/total_tls` (enabled, and the certificate authority when declared) | `total_tls` |
| each `Names` entry | `GET /zones/{id}/dns_records?name=...&type=...` | `name` |

A name must exist; when `Target` is set its content must also match, ignoring
case and a trailing dot. What the declaration leaves empty is not checked, and
costs no request: an unset SSL mode, an unset minimum version, a nil
`TotalTLS`, and no names.

An answer the API refuses (a token without the right, an unknown zone) is an
**error**, never drift: a probe that cannot read has not found the zone in
order. Errors never carry the token.

## Use

```go
cfg, _ := config.Load(fsys, cat, projects)       // or config.Parse in a company program
d, err := probe.DeclaredFrom(cfg, "example.com") // the floor the registry sets
d.Names = probe.NamesFrom(providerHosts, targets) // provider-owned names and their records
report, err := probe.NewClient(token).Check(ctx, d)
for _, f := range report.Drift {
    log.Println(f) // "ssl: want strict, got full"
}
```

`DeclaredFrom` takes the SSL mode, minimum TLS version and Total TLS from the
registry, each only if the registry sets it. It refuses an unknown zone, a zone
managed for its security.txt only, and a zone without an id yet. The names are
not in the registry: the caller lists them (`NamesFrom` sorts and de-duplicates
hosts and gives each CNAME its expected target; a host without a target only has
to exist). For a company-owned zone these are the platform's published names,
`config.ProviderZone.Names()`.

## Credential

Read rights on the zone's settings, its DNS records and its certificates (Total
TLS). No preset kind grants exactly these, and the probe does not need the
`zone` kind's write token: use a purpose-made read-only token, so a probe that
runs on a schedule holds nothing it could change a zone with.

## What it does not do

It does not re-assert a lowered setting. Re-apply the stack that declares the
zone (`zonestack.Deploy`), which writes the declared value back. Per-host SSL
mode rules (`ssl_mode_rules`) are rulesets, not the zone setting, and are not
read here.

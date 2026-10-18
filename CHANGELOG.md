# Changelog

What changed for a consumer, per version, newest first. A version with no
heading here is a patch cut automatically for dependency bumps alone; its
GitHub Release lists them. The chart and the Go module are released
together at every version.

## v4.1.0

### Changed

- **`static-site`: the identity check hook also proves the AWS account** (owner decision D673 a). With `registry.ack.accountId` set (12 digits, quoted; the key `static-site-infra` already takes, so one set of values feeds both) the hook passes `-expect-account`: it fails unless the pod runs in that account, not only under the role. Unset, nothing changes: no flag is passed. From `service-lib` 1.54.0 and `identity-check` 0.34.0 (`-expect-account` needs 0.34.0 or later; the image default and its digest move). The chart now reads `registry.ack.accountId` besides `enabled`; a values file that pins `images.identity-check` below 0.34.0 and sets `accountId` fails every retry of the hook.

## v4.0.0

### Breaking

- **The Go module is `github.com/truvity/edge/v4`.** Rewrite `github.com/truvity/edge/v3` to `.../v4` in your imports and run `go mod tidy`. Every chart is published at 4.0.0 and renders exactly what the last v3 release rendered. See [docs/cloudflare/adoption.md](docs/cloudflare/adoption.md#v4-the-module-moves-to-edgev4-and-three-deprecated-things-go).
- **`cloudflare/presets`: the `edge` kind is removed** (`KindEdge`, deprecated since it was split into `zone` and `services`). `StandardRows(account, grants, wafZones, botZones)` returns the four rows status, r2-admin, zone and services; `SplitRows` is gone. A row of kind `edge` is refused as an unknown kind. `r2-admin` stays: it is still deprecated, but no narrower replacement exists yet.
- **`cloudflare/r2`: `Config.Token` is removed**, with `TokenConfig`, the `Permission*` constants and `Permissions()`, the `Token`, `TokenID`, `TokenValue`, `S3AccessKeyID` and `S3SecretAccessKey` fields of `R2`, the matching component outputs and the `token-<name>` child (deprecated since v2.6.0). The component creates the bucket and its lifecycle only. Mint a bucket's parent token with `account.NewChildToken` (or `NewChildTokenSet`) and `account.R2BucketParentPolicies(jurisdiction, bucket)`, which grant the same permission group on the same resource key.
- **`cloudflare/zone`, `cloudflare/zonespec`: `Compat` (`TopLevelAliases`) is removed**, and `zonestack` no longer sets it. The `firewall-custom-<name>`, `tls-client-auth-<name>` and `ssl-mode-<name>` children no longer carry an alias from the no-parent URN. Apply every zone stack with a v3 release first: a stack that was applied after it moved under the zone component previews with no create or delete on v4.

## v3.13.0

### Added

- **`cloudflare/config`: `managed_waf: {enabled: true}` derives the managed WAF's hosts: every proxied name, listed explicitly, next to where it is declared.** `Load` fills `Zone.WAFHosts` with every exact host the edge catalog serves through a tunnel in the zone (a `dns_elsewhere` name included) and every `proxied` entry of the zone's `records`, minus the names that opted out, sorted and de-duplicated, as `Zone.CacheHosts` is derived. The rule is one `http.host in {...}` expression, never a wildcard and never the whole zone. A name opts out where it is written, with a reason: a catalog domain takes `waf: off` and a required `waf_reason` (refused without `off`, and refused on an exposure that does not arrive by `cloudflare-tunnel`); a record takes `waf: false` and `waf_reason`; a generated project group takes `catalog.Endpoint.WAFOff` (the reason) or `names[host].{waf, waf_reason}` in `project_groups.overrides`. A proxied wildcard host (domain or record) in an enabled zone must opt out with a reason, or `Load` fails naming the host. `enabled` is exclusive with `hosts` and `whole_zone`. A name no catalog domain or record declares (a status page another stack creates) is added after `Load` with `Config.AddWAFHosts(zone, hosts...)`: exact lower-case names inside the zone, validated like the derived ones. A rule expression over Cloudflare's 4096-character limit is refused (`zonespec.MaxRuleExpression`). New library API: `Zone.ManagedWAFLibrary`, `zonespec.Record.WAF` and `WAFCovered`, `edgespec.DeriveInput.WAFZones`, `Group.WAFOff` and `Derived.WAFHosts`. Apply the zone stack before the services stack: a WAF entry for a name not created yet is harmless, the reverse leaves a gap. See [docs/cloudflare/config.md](docs/cloudflare/config.md).

### Changed

- **Breaking for a `cloudflare.yaml` whose `managed_waf.hosts` holds a wildcard (`*.x`): it now fails to load.** The managed WAF lists exact names only. Move to `managed_waf: {enabled: true}`, remove the host list, and give each wildcard catalog domain `waf: off` and a `waf_reason` (list the exact names it serves as their own domains). A config with exact `hosts` or `whole_zone` and no `enabled` loads and renders as before.

## v3.12.3

### Fixed

- **`static-site-infra`: the ACK `Policy` now adopts the managed policy already in AWS.** v3.12.2's `adoption-policy: adopt-or-create` was enough for the `Role` (ACK adopts a role by name) but not for the `Policy`: an IAM managed policy is identified by its ARN, so ACK still called `CreatePolicy` and got `EntityAlreadyExists`. The `Policy` now also carries `services.k8s.aws/adoption-fields: {"arn":"arn:aws:iam::<accountId>:policy/<name>"}`, built from `registry.ack.accountId` (set it, together with `region`, for adoption to work; without it only the `Role` is adopted). The `Role` and `PodIdentityAssociation` annotations are unchanged. See [docs/static-site.md](docs/static-site.md).

## v3.12.2

### Fixed

- **`static-site-infra`: the ACK `Policy` and `Role` carry `services.k8s.aws/adoption-policy: adopt-or-create`.** A `Policy` created while the project's ACK role was missing (AccessDenied) existed in AWS on the retry, so ACK refused the create for good (`ACK.Terminal`, "Resource already exists") and the `Role` and `PodIdentityAssociation` that reference it stayed blocked ("Reference resolution failed"). With the annotation, ACK (feature gate `ResourceAdoption=true`) adopts the object already in AWS. The `PodIdentityAssociation` is deliberately not adopted: AWS identifies it by cluster, namespace and ServiceAccount, and adopting it could let a legacy owner's deletion remove the association under the new object. See [docs/static-site.md](docs/static-site.md).
- **`edge-insights dismiss -id ... -format table` printed only the id: the SEVERITY, TYPE, CLASS and SUBJECT columns were empty.** The `-id` path built bare insights instead of looking them up. It now lists the scope first and prints the listed insights, so the table matches `list`; an id that the account or zone does not list is refused before anything is written. The counts and the filter path (`-class`, `-type`, `-severity`) were already right.

## v3.12.1

### Fixed

- **`static-site-infra`: the ACK `Policy`, `Role` and `PodIdentityAssociation` carry `argocd.argoproj.io/sync-options: Prune=false,Delete=false`, like the ServiceAccount.** During the handover from `static-site` 3.11 the live objects are still tracked by the site's Argo CD Application, which refreshes independently and may re-take tracking; its sync to 3.12.0, which no longer renders them, would prune them and ACK would delete the IAM Role. Argo CD reads the sync option from the live object when it prunes, and under server-side apply the previous owner cannot strip an annotation it never owned, so the guard holds whichever Application syncs. `Delete=false` also keeps the IAM Role if the `static-site-infra` Application is deleted, the same posture as the ServiceAccount. **Use this version or later for the handover**: install `static-site-infra` at 3.12.1 or later (it adds the guard to the live objects), then upgrade `static-site`; 3.12.0 does not protect the ACK objects.

## v3.12.0

### Changed

- **Breaking for installs with `registry.auth=ecr` and `registry.ack.enabled`: `static-site` no longer renders the pod's cloud identity; the new `static-site-infra` chart does, and `static-site` checks it before the Deployment starts.** EKS Pod Identity associations are eventually consistent: ACK reports one `Synced` seconds before the cluster's admission webhook can see it, and credentials are injected only when a pod is created, so a pod admitted in the gap runs for its whole life without credentials (seen on a staging and a production site). With `registry.auth=ecr` and `registry.ack.enabled` (derived, no new switch):
  - `static-site-infra` (new, `oci://ghcr.io/truvity/charts/static-site-infra`) renders the ACK `Policy` (wave -30), `Role` (-25) and `PodIdentityAssociation` (-20) that `static-site` v3 rendered, with the same names and specs, then the `ServiceAccount` (-15) with `argocd.argoproj.io/sync-options: Prune=false,Delete=false`: same name, labels and annotations as before, plus the wave and the guard. Its values use the keys `static-site` had: `registry.auth`, `registry.ack.*`, `serviceAccount.{create,name,annotations}`, `nameOverride`, `fullnameOverride`; new are `installName` (the application's release name, default the release's) and `networkPolicy.enabled` (egress for the check: DNS, `169.254.170.23:80`, HTTPS).
  - `static-site` renders no ACK objects and no ServiceAccount (it names `serviceAccount.name`, default the fullname) and renders the identity check of truvity/policy's `service-lib` (v1.51.0, a new chart dependency): a `pre-install,pre-upgrade` hook Job, `restartPolicy: Never`, running as that ServiceAccount, which passes only once a pod admitted now has the identity. Its image is `images.identity-check` (`ghcr.io/truvity/k8s/identity-check` 0.31.1, pinned by digest). The check pod carries labels of its own, so the site's Service, PodDisruptionBudget and NetworkPolicy do not select it. With `registry.auth` of `none` or `dockerconfig`, IRSA, or `registry.ack.enabled` off, the render is unchanged. The `registry.ack` keys other than `enabled` are still accepted by the schema and ignored. Decision: [0013](https://github.com/truvity/policy/blob/v1.51.0/docs/decisions/0013-pods-are-admitted-with-their-identity.md). See [docs/static-site.md](docs/static-site.md).

### Upgrade

This is a breaking change shipped in a minor by the owner's decision, because the Go module and the charts share one tag. A consumer that pins `static-site` by hand (for example a gitops `cfg/versions.yaml`) must add `static-site-infra` in the same change. For an install with `registry.auth=ecr` and `registry.ack.enabled`:

1. Install `static-site-infra` with the same release name (or `installName`) and the site's `registry`, `serviceAccount`, `nameOverride` and `fullnameOverride` values, **before** upgrading `static-site`: as its own Argo CD Application synced and healthy first (an app-of-apps wave, or a health gate between the two Applications), or `helm upgrade --install` first with plain Helm. The objects render as they are live, so nothing changes in the cluster; Argo CD takes them over, and the `Prune=false,Delete=false` guard on the ServiceAccount keeps either Application from deleting it while the ownership moves. Set `networkPolicy.enabled` where the namespace denies egress by default.
2. Upgrade `static-site` to this release. It stops rendering the ACK objects and the ServiceAccount, and runs the identity check before the Deployment.

Do not upgrade `static-site` first: it would prune the Policy, Role and PodIdentityAssociation and ACK would delete the IAM Role. For an install without `registry.ack.enabled` there is nothing to do.

### Added

- **`static-site-infra` chart**, released with every tag beside the others, at the same version; `hack/chart-deps.sh` resolves `static-site`'s registry dependency for lint, golden renders and packaging (`Chart.lock` is committed).

## v3.11.0

### Added

- **`static-site`: the chart renders the placeholders ExternalSecret.** `placeholders.externalSecret` (a reserved key, unset by default) renders an `external-secrets.io/v1` ExternalSecret named `placeholders.secretName` with `creationPolicy: Owner`: `storeRef` (`kind` `ClusterSecretStore` or `SecretStore`, `name`), `remoteKey`, `properties` (Secret key to remote property) and `refreshInterval` (default `1h`). Each key reads with `conversionStrategy: Default`, `decodingStrategy: None`, `metadataPolicy: None`, `nullBytePolicy: Ignore`. The ExternalSecret carries `argocd.argoproj.io/ignore-healthcheck: "true"` (`ignoreHealthcheck`, default `true`; `annotations` adds more) so an unwritten value does not hold the application Degraded. The keys of `properties` must equal `secretKeys` plus `optionalKeys`, and `secretName` is required; a mismatch fails the render. Additive: without `externalSecret` the render is unchanged. See [docs/static-site.md](docs/static-site.md).
- **`static-site`: ACK objects for the ECR identity, and the server's ECR actions as one list.** `registry.ack` (off by default; needs `registry.auth=ecr`) renders an ACK IAM `Policy`, an IAM `Role` trusting `pods.eks.amazonaws.com` (`sts:AssumeRole`, `sts:TagSession`) for the release namespace and the chart's ServiceAccount, and an EKS `PodIdentityAssociation` (`roleRef`), in sync waves -30, -25 and -20 so they precede the Deployment. Values: `ack.enabled`, `ack.clusterName` and `ack.repositoryArns` (required), `ack.rolePrefix` (the Role and Policy name; default `<clusterName>-<namespace>-<release>`), `ack.tags` (on the Role, with `cluster`), `ack.permissionsBoundary`, and `ack.accountId` with `ack.region` to pin the trust to the cluster. The Go package exports `staticsite.ECRAccountActions` and `staticsite.ECRRepositoryActions`; tests check them against the ECR seam, the registry requests of a pull and the chart's rendered Policy. IRSA stays `serviceAccount.annotations`. Additive: off, the render is unchanged. See [docs/static-site.md](docs/static-site.md).

## v3.10.0

### Added

- **`static-site`: the server authenticates to Amazon ECR itself.** For a registry host `<account>.dkr.ecr.<region>.amazonaws.com` (also `ecr-fips` and `.amazonaws.com.cn`) with no Docker config file configured, the server calls `ecr:GetAuthorizationToken` through the AWS SDK default credential chain (EKS Pod Identity, IRSA, environment), uses the `AWS:<password>` token as basic auth, caches it, replaces it at half of its 12 hours and on a 401. A configured Docker config file keeps precedence; otherwise the pull is anonymous. `STATIC_SITE_REGISTRY_AUTH` (`auto`, `dockerconfig`, `ecr`, `none`) forces one; the log names the method (`registry auth: ecr-sdk`), never a credential. The chart gains `registry.auth` (`dockerconfig`, the default and the previous behaviour, `ecr`, `none`): with `ecr` there is no pull-secret volume, the ServiceAccount token stays mounted for IRSA, and `serviceAccount.name`/`annotations` bind Pod Identity or IRSA (`site.pullSecretName` is refused). The Go module now depends on `aws-sdk-go-v2/config` and `service/ecr`. Additive: nothing changes for an existing install except that an ECR host with no pull secret now authenticates instead of failing anonymous. See [docs/static-site.md](docs/static-site.md).

## v3.9.0

### Added

- **`static-site`: placeholder values from a Secret.** `STATIC_SITE_PLACEHOLDERS_DIR` reads one file per name (a mounted Secret), declared by `STATIC_SITE_PLACEHOLDERS_KEYS` and `_OPTIONAL_KEYS`; `STATIC_SITE_PLACEHOLDERS_STRICT=true` makes a `{{ token }}` that is unknown, or declared without a value, fail the load instead of staying as written. The chart gains `placeholders.secretName`, `secretKeys` and `optionalKeys` (reserved keys of `placeholders`): the Secret is mounted read-only, the server runs strict, and the chart creates nothing. Names are logged, values never. Additive: without `secretName` the render is unchanged. See [docs/static-site.md](docs/static-site.md).

## v3.8.0

### Added

- **`cloudflare/presets`, `cloudflare/account`: the `zone` and `services` kinds for the per-company stack split.** `presets.KindZone` holds everything `edge` holds except Cloudflare Tunnel Write: zone, SSL, cache and config settings and DNS Write on each zone (new `account.ZonePolicies`, which `EdgePolicies` now builds on with an identical result), then `WAFZones`, `BotManagementZones`, `CloudConnectorZones`, `DNSOnlyZones` and Zone Settings Write on `SecurityTxtOnly` zones as `edge` does. `presets.KindServices` holds the rights of `status` (account tunnel write, DNS Write on each managed zone) under its own name and description. A test pins that `zone` and `services` together hold exactly `edge`'s groups, and that `services` equals `status`. `presets.SplitRows(account, grants, wafZones, botZones)` returns the rows of `StandardRows` plus `<account>-zone` and `<account>-services`; `StandardRows` and every existing preset output are unchanged. Additive. See [docs/cloudflare/presets.md](docs/cloudflare/presets.md).
- **`cloudflare/config`: per-company stack names.** `StackZone(company)`, `StackServices(company)`, `StackSTS(company)` and `StackName(kind, company)` return `cloudflare-<company>-zone|services|sts`, with the kinds `StackKindZone`, `StackKindServices` and `StackKindSTS`. The existing `(*Config).StackName(kind, account)` for per-account stacks is unchanged. See [docs/cloudflare/stacks.md](docs/cloudflare/stacks.md).
- **`cloudflare/awsapigw`: adopt an existing ACM certificate, its validation and its records without replacing them.** `CertificateArgs` gains `ValidationName` (the `acm.CertificateValidation`'s logical name, default `<name>-certificate-validation`), `Aliases` and `ValidationAliases` (the old identities of the certificate and its validation) and `ValidationRecord`, which has the package make the DNS-only Cloudflare validation record itself, with `Provider`, `ZoneID`, `ResourceName` (default `<name>-certificate-validation-record`), `Aliases` and `Comment`; `CreateValidationRecord` stays and exactly one of the two is required. `DNSArgs` gains `Comment`. `NoParentAliases(oldNames...)` builds the aliases for resources that sat at the top of another stack and were moved here with `pulumi state move`; a record that was under a component there uses `pulumi.Alias{Name, ParentURN}`. Defaults are unchanged: a stack that sets none of these registers what it did. Additive. See [docs/cloudflare/awsapigw.md](docs/cloudflare/awsapigw.md).

### Deprecated

- **`cloudflare/presets`: the `edge` and `r2-admin` kinds** are kept for one release and then removed. Use `zone` and `services` for the edge token; `r2-admin` has no replacement named yet. Nothing changes for a caller that keeps them.

## v3.7.0

### Added

- **`cloudflare/config`, `account`, `presets`, `tokensplit`, `zonestack`, `services`: a zone managed for its security.txt only (`manage: security_txt`).** A zone of the registry can carry `manage: security_txt` (default `full`, today's behaviour) to bring a hand-managed zone in so that `edge-securitytxt` renews its security.txt, and nothing else of it is managed. The zone must set `account`, `zone_id` and `security_txt`; every other field (`ssl_mode`, `records`, `cache`, `hsts`, `bot_management`, `trusted_clients`, `zone_id_pending`, ...) is refused at load. Every builder skips it: `zonestack.Deploy` and `services.Deploy` register nothing for it, `zonestack.ZoneArgs` returns `zonestack.ErrSecurityTxtOnly`, `Zone.Contributions`, `RecordsLibrary` and `ActiveOrigins` are empty, and the catalog derives no host, tunnel or record in it (`Config.ZoneOf` does not return it, so a catalog domain inside it is refused as in no zone). `securitytxt.Targets` includes it. For tokens: `presets.Zone.SecurityTxtOnly` and `tokensplit.Zone.SecurityTxtOnly` give the edge kind exactly one policy on the zone, `Zone Settings Write` (new `account.SecurityTxtPolicies`, `account.PermZoneSettingsWrite`), and none of DNS, SSL, settings, cache or config; the `dns` and `status` kinds do not cover it and a row's `WAFZones`, `BotManagementZones`, `CloudConnectorZones` and `DNSOnlyZones` refuse it. `tokensplit.MatchingZoneIDs` leaves such a zone out and `tokensplit.SecurityTxtZoneIDs` lists it. Every preset output is byte-identical when no zone uses the mode. Additive. See [docs/cloudflare/config.md](docs/cloudflare/config.md) and [docs/cloudflare/presets.md](docs/cloudflare/presets.md).

## v3.6.0

### Added


- **`cloudflare/account` and `cloudflare/presets`: `Bot Management Write` for the zones that set `bot_management`.** `account.BotManagementPolicies(zoneIDs...)` returns one `ZoneScope` policy per zone granting only `Bot Management Write` (`account.PermBotManagementWrite`); append it to `EdgePolicies` or `EdgePoliciesWithWAF`. `presets.Row.BotManagementZones` (edge only, each among the row's zones) adds it to the edge preset, the way `WAFZones` adds Zone WAF Write. `EdgePolicies`, `EdgePoliciesWithWAF` and `StandardRows` are unchanged, and a row that lists no zone builds the same token as before. Additive. See [docs/cloudflare/presets.md](docs/cloudflare/presets.md).
- **`cmd/static-site` and `charts/static-site` (new): one generic server for every static site, and its chart.** A site's CI pushes its build as an OCI artifact (`oras push`: a tar or gzip tar layer, or single files named by their title annotation) under `static/<site>`; the server pulls it by digest with oras-go, unpacks it into a memory-backed volume and serves it. Strong content-hash ETags with 304, `Cache-Control: public, max-age=31536000, immutable` for hashed file names (configurable pattern) and a short `max-age` plus `stale-while-revalidate` for everything else, so a CDN in front needs no purge. Serve-time placeholders (`{{ name }}`) in html, js, css and json files, only for allowlisted names and only with values from a safe character set, escaped for the file type, so one artifact serves every environment; the ETag is computed after substitution. Also: `index.html` for directories, optional SPA fallback, a configurable 404 page, no directory listing, traversal refused, GET and HEAD only, configurable security headers (nosniff, frame options and referrer policy by default), `/healthz`, `/readyz` and `/metrics` on a separate port, JSON logs and a graceful shutdown. The chart renders a Deployment, Service, PodDisruptionBudget and ConfigMap, with an optional ServiceMonitor and NetworkPolicy; `site.artifact.digest` is its own value so a promotion tool can write it alone. Released at the same version as the module and the other charts: the image is `ghcr.io/truvity/edge/static-site:<version>` and the chart's `appVersion` is its default tag. See [docs/static-site.md](docs/static-site.md).

- **`cloudflare/zone`, `zonespec`, `config`: manage the zone's AI bots rule and managed robots.txt.** `Args.BotManagement` (`botManagement`: `aiBots` of `block`, `disabled` or `only_on_ad_pages`, and `managedRobotsTxt`, a `*bool`) registers one `cloudflare.BotManagement` child, `bot-management-<name>`, with `zoneId`, `aiBotsProtection` and `isRobotsTxtManaged` only; a field left unset is not sent. Bot Fight Mode, Super Bot Fight Mode (`fight_mode`, `sbfm_*`), JavaScript detections and crawler protection are never set, so they stay as they are. In the registry a zone takes `bot_management: {ai_bots, managed_robots_txt}`. Validation refuses an unknown `aiBots` and an empty block. Absent means no resource: existing zones plan exactly what they did. The setting is a singleton and cannot be destroyed meaningfully: to undo, apply `aiBots: disabled` and `managedRobotsTxt: false`, then remove the block. The token needs `Bot Management Write` on the zone (`EdgePolicies` does not grant it). Additive. See [docs/cloudflare/reference.md](docs/cloudflare/reference.md).

### Fixed

- **`cloudflare/insights`: `Dismiss` accepts the ids Cloudflare uses for DNS insights.** Those ids embed the host name (`<zone tag>-www.example.com-<n>-cname_record_not_proxied`), and v3.5.0's id check (letters, digits, `-`, `_`) refused them, so `edge-insights dismiss -id` could not archive a DNS insight. Dots are now allowed; `.` and `..` alone are still refused, and the id is path-escaped as before.

## v3.5.0

### Added

- **`cloudflare/insights`: `Client.Dismiss` and `Client.DismissMatching`, and `edge-insights dismiss`.** `Dismiss(ctx, scope, issueID, dismiss)` archives (or restores) one Security Center insight through `PUT /accounts/{id}/security-center/insights/{issue_id}/dismiss` or the zone form, body `{"dismiss": true|false}`; the id is checked. `DismissMatching` lists by `Filter` and dismisses each match, and is a dry run unless `Client.Apply` is set. The command takes `-id` or `-class`/`-type`/`-severity`, `-apply` and `-undo`; a filter that matches nothing reports it and exits 0. See [docs/cloudflare/insights.md](docs/cloudflare/insights.md).
- **`cloudflare/presets`: the `insights-admin` kind (`KindInsightsAdmin`).** Account Security Center Insights Write and Read on the account, Zone Security Center Insights Write and Read on each zone (or the row's zones), default lifetime one hour, store none, nothing else writable. It is the credential `edge-insights dismiss` needs; the `insights` kind stays read only and unchanged. See [docs/cloudflare/presets.md](docs/cloudflare/presets.md).

### Changed

- **Behaviour change for consumers that set `trusted_clients`: a trusted client no longer skips the managed WAF.** The rule (same ref `trusted-clients`, same host and range expression) was `skip` with `ruleset: current`, phases `http_ratelimit`, `http_request_firewall_managed`, `http_request_sbfm` and products `bic`, `hot`, `rateLimit`, `securityLevel`, `uaBlock`, `waf`, `zoneLockdown`; Cloudflare Security Center flagged it as `skip_above_threshold`. It now skips only the bot and rate-limit checks: no `ruleset`, phases `http_ratelimit`, `http_request_sbfm`, products `bic`, `hot`, `rateLimit`, `securityLevel`, `uaBlock`. The managed rules, the legacy `waf` product, `zoneLockdown` and the rest of the custom rules now apply to those clients. The default description is `Trusted clients: skip bot and rate-limit checks` (was `Trusted clients: skip security features`); the next apply updates the one ruleset in place.
  - To opt back in, set `skip` (`TrustedClients.Skip`, `TrustedClientsCfg.Skip`) to a list of names; a list replaces the default. `zonespec.LegacyTrustedSkip()` is the old list in full (`current`, `http_ratelimit`, `http_request_firewall_managed`, `http_request_sbfm`, `bic`, `hot`, `rateLimit`, `securityLevel`, `uaBlock`, `waf`, `zoneLockdown`); to add back just the managed rules, list the default plus `http_request_firewall_managed`. Unknown or repeated names are refused at validation. With `skip` set the default description is `Trusted clients: skip selected security features`.
  - Also: an empty skip part (`ruleset`, `phases` or `products`) is now left out of the ruleset rule instead of sent empty.

## v3.4.0

### Added

- **`cloudflare/securitytxt` (new) and `cmd/edge-securitytxt`: a zone's security.txt, planned and applied.** `Targets(cfg, zone, account)` takes the `security_txt` blocks of a `cloudflare/config`, `Client.Plan` reads each zone's live document through `GET /zones/{zone_id}/security-center/securitytxt` and returns the difference, `Client.Apply` writes (`PUT`) the zones that differ and nothing else. RFC 9116 checks: `expires` required, in the future and less than a year ahead, a warning within 60 days. The command is a dry run unless `-apply`; the token comes from `CLOUDFLARE_API_TOKEN` (or `-token-env`) or `-token-cmd`, and is never printed. Needs the zone settings of the `edge` preset kind. See [docs/cloudflare/securitytxt.md](docs/cloudflare/securitytxt.md).
- **`cloudflare/insights` (new) and `cmd/edge-insights`: read-only checks for a WAF rollout.** `Client.List` lists Security Center insights of an account or a zone (filters: severity, type, class, dismissed); `Client.Events` sums Security Events per host, rule and action over a window from the GraphQL `firewallEventsAdaptiveGroups` dataset, so false positives show per host before the managed WAF widens. JSON or table output. Needs the `insights` preset kind. See [docs/cloudflare/insights.md](docs/cloudflare/insights.md).

### Fixed

- **`cloudflare/presets`: the `insights` kind uses the permission-group names an account actually lists.** The account group is `Account Security Center Insights Read` (the public permission reference spells it without "Read", and Cloudflare's lookup by exact name refused it), and each zone also gets `Zone Security Center Insights Read`, so zone-scoped insights can be listed. A prototype built from v3.3.0's `insights` kind could not be created; no other kind changes.

## v3.3.0

### Added

- **`cloudflare/presets` (new): the sluis Cloudflare presets, built from plain rows.** `Row{Name, Kind, Account, Zones, WAFZones, CloudConnectorZones, DNSOnlyZones, Bucket, Jurisdiction, Grants, Lifetime, Rotation, Store}` and a `Registry{Accounts, Zones}` give `Build`, `BuildReady` (leaves out an account with a pending zone), `ForAccount`, `Named`, `AccountOf` and `StandardRows` (`<account>-edge`, `-status`, `-r2-admin`); the kind (`edge`, `status`, `dns`, `insights` (read only: Account Security Center Insights, and Bot Management Read, Analytics Read and Zone WAF Read per zone), `r2-admin`, `bucket-rw`, `bucket-write`) decides the permission groups, their scopes and their order. `Validate` reports every problem; `Preset.Documents()` renders the policies as Cloudflare receives them and `Preset.Request()` is the `presettoken.Request`. No file, no environment, no resource. A golden test over a synthetic registry and six rows pins every group, scope and order a kind produces. See [docs/cloudflare/presets.md](docs/cloudflare/presets.md).
- **`presettoken.Estate`: what an estate decides once, and one account's `Config` from it.** `Estate{SluisctlMin, BinaryPrefixes, Lifetime, GrantHint, Runbook, BreakGlass EstateBreakGlass}`, `Estate.Config(account, item, issuer, getenv)` (the account's root item, or the `ItemEnv` variable over it, completes the break-glass block; none known: off is the zero block, on is an error naming what to set) and `Estate.Credential(...)`, the credential of one preset's `Request`. They replace an estate's own `TokenConfig` and `ProviderToken`.
- **`prototypes.DeployTable`, `prototypes.FromPresets` and `Args.Options`.** `DeployTable(ctx, Table)` is the prototypes stack of one account over the presets table: it refuses an account whose root item the estate does not name, then calls `Deploy`, whose Pulumi names (`prototype-<preset>`), token names and policy order are unchanged. `Args.Options` (and `Table.Options`) adds resource options per preset, such as an alias. Additive: no exported symbol is removed or changed.

### Documentation

- `PublicPathsCache` in [docs/cloudflare/rules.md](docs/cloudflare/rules.md): when a signing key in a cached key set is removed after a compromise, purge those paths, because a removed key is otherwise served until the edge TTL expires.

## v3.2.0

### Added

- **`cloudflare/awsapigw` (new): the AWS API Gateway front door, moved from `truvity/sluis` `deploy/pulumi/edge/cloudflare`.** `awsapigw.New(ctx, name, *Args)` creates the regional TLS 1.2 custom domain with mutual TLS on a versioned truststore (Cloudflare's origin-pull CA, Authenticated Origin Pulls), its API mapping, the optional DNS-validated ACM certificate, the truststore bucket (`Truststore.NewBucket`) or an attested existing one (`Truststore.Existing`), and an optional Cloudflare CNAME (`DNS`), always proxied (`DNS.Unproxied` is refused: a DNS-only record cannot present the client certificate). `FrontDoor{Name, Component, APIID, StageName}` replaces sluis's type; `ProtectedPrefix` is the truststore guard; `Args.Validate()` reports every problem; `Args.ComponentType` (default `truvity:aws:ApiGatewayDomain`, `awsapigw.ComponentTypeSluis` for the sluis type) keeps a migrating stack's URNs. The child logical names are those of sluis. It imports no sluis package. The root module now requires `github.com/pulumi/pulumi-aws/sdk/v7` (v7.50.0), and a renovate rule groups its bumps monthly without automerge. Additive: no exported symbol is removed or changed and every existing golden is byte-identical. See [docs/cloudflare/awsapigw.md](docs/cloudflare/awsapigw.md).
- **`cloudflare/zonestack`, `cloudflare/services`, `cloudflare/instancetunnel` (new): the stack entry points.** `zonestack.Deploy` is the zone base of one company (a provider per account, the zone component per zone, built from `cloudflare/config`, its rules as the builders give them (`config.Zone.Contributions`)); `services.Deploy` is its tunnels (per cluster and account, ingress from the registry's derived tunnels, CNAMEs for `DNSNames()` only so `dns_elsewhere` is honoured, `origin_cnames` applied, exports `tunnelIds`); `instancetunnel.Deploy` is the tunnel of one instance. They register the names, parents, providers and inputs the single `cloudflare/edge.Deploy` stack and an estate's glue register, proved by a golden test that runs both on the config fixture, so splitting such a stack by `pulumi state move` replaces nothing; the rulesets whose rules the estate registered in an older form (no `ref`, another SSL mode description) take one in-place update, listed in the docs. Also additive: `edge.DeployTunnel` (the tunnel half of `edge.Deploy`, which now calls it), `config.CompanyOf` and `config.AccountsOf`. Additive: no exported symbol is removed or changed and every existing golden is byte-identical. See [docs/cloudflare/stacks.md](docs/cloudflare/stacks.md).
- **`cloudflare/config` (new): the Cloudflare config model, with no Pulumi or Cloudflare SDK.** The typed form of an estate's `cloudflare.yaml`: `Config{Accounts, Zones, Fleets, R2, Root}`, `Account` (with an optional `Company` label), `Zone` (SSL mode, TLS floor, Total TLS, HTTPS and HSTS, cache, public cache, trusted clients, rate limits, managed WAF, records, origin CNAMEs, SSL mode rules, security.txt, `ZoneIDPending`), the derived `Tunnel` (`Name`, `Release`, `SecretName`, `Zones`, `DNSNames`), `Parse(data)`, `Load(fsys, cat, projects)`, `Validate`, `Derive`d tunnels and cache hosts, `ForAccount`, `StackName` / `StackAccount`, `ValidateForeignHosts` and the converters to the zone component's inputs. Decoding is strict. `Zone.Contributions()` returns the zone's `public_cache`, `ssl_mode_rules`, `rate_limits`, `trusted_clients` and `managed_waf` blocks as `[]rules.Contribution`. The OpenBao and SSM naming of tunnel tokens is not here: it names an estate's layout. See [docs/cloudflare/config.md](docs/cloudflare/config.md).
- **`catalog.Domain.DNSElsewhere` (`dns_elsewhere`): a tunnel name whose DNS record something else writes.** The name keeps its ingress rule and the tunnel creates no CNAME for it: `edgespec.Group.DNSElsewhere` in, `edgespec.Tunnel.DNSElsewhere` and `Tunnel.DNSNames()` out, and `cloudflare/edge` registers DNS for the latter only (a tunnel with none gets no DNS block). Refused on an exposure that does not arrive by `cloudflare-tunnel`. Additive: no exported symbol is removed or changed.

### Changed

- **`rules.RateLimits` names each rule's ref by a slug of its row name.** The ref was `ratelimit-` plus the row name verbatim (spaces and capitals included); it is now `ratelimit-` plus the name lowercased with every run of characters outside `[a-z0-9]` made one `-` and the ends trimmed (`Browser telemetry ingest` gives `ratelimit-browser-telemetry-ingest`). Two rows that make the same ref, and a name with no letter or digit, are refused, naming the rows. The ref was not yet consumed by any estate.

## v3.1.0

### Added

- **`cloudflare/cloudflared` (new): the one cloudflared version pin.** `Version` (`2026.10.0`), `ImageRepository`, `ImageDigest` (the multi-architecture index), `SHA256AMD64` / `SHA256ARM64` (the release assets `cloudflared-linux-<arch>`), `BinaryURL(arch)` and `BinarySHA256(arch)`. Standard library only. `hack/cloudflared-pin.sh <version>` rewrites the pin and the chart value from the upstream release. See [docs/cloudflare/cloudflared.md](docs/cloudflare/cloudflared.md).
- **`cloudflare/instanceconnector` (new): cloudflared as a tunnel connector on a Linux instance.** `Bootstrap(Args) (Fragment, error)` renders, in separate pieces so a caller can start late, an idempotent checksum-verified install of the pinned binary, the systemd unit, a step that loads the tunnel token from SSM into a 0600 env file under `/run` without printing it, and the start. Validates architecture, paths, parameter name and region. Standard library plus `cloudflare/cloudflared`. See [docs/cloudflare/cloudflared.md](docs/cloudflare/cloudflared.md).
- **`cloudflare/zone` assembles its rulesets from `cloudflare/rules` contributions, and gains Authenticated Origin Pulls.** `zone.Args` (that is, `zonespec.Args`) gains `Contributions []rules.Contribution`, `AuthenticatedOriginPulls *bool` (zone setting `tls_client_auth`, child `tls-client-auth-<name>`), `ImportRulesets map[rules.Phase]string` (adopt a phase's existing entry point ruleset by id) and `Compat{TopLevelAliases}` (aliases `firewall-custom-`, `tls-client-auth-` and `ssl-mode-` children to a no-parent URN). The `cache`, `trustedClients`, `managedWaf` and `rateLimits` blocks are now contributions too, and every ruleset is registered from `rules.Assemble` under the existing child names (`ssl-mode-<name>` for the config phase). Additive: no exported symbol is removed or changed and a zone without the new fields registers byte-identical inputs. The rule shapes move to the new standard-library-only package `cloudflare/rulespec` and `rules` aliases them (`rules.Rule` is `rulespec.Rule`). See [docs/cloudflare/rules.md](docs/cloudflare/rules.md).

### Changed

- **The `cloudflared` chart runs cloudflared 2026.10.0 (was 2026.8.3).** `image.tag` moves from `2026.8.3` to `2026.10.0`; the rendered Deployment's `image:` line is the only change in every golden render. One version now serves the whole estate, pinned in `cloudflare/cloudflared` (below) and kept equal to the chart value by `hack/cloudflared-pin.sh check` in `just lint` and a Go test. A consumer sees the new image tag in its zero-diff gate when it moves to the next chart version, and rolls its pods once; a consumer that sets `image.tag` itself is unaffected.

## v3.0.1

- **`cloudflare/rules` (new): rule contributions and the one-ruleset-per-phase assembler.** A zone holds one ruleset per phase, so features that each want rules in a phase cannot each create it. `rules.Contribution` states the rules one source wants (`Source`, `Phase`, `Order`, `Rules`, and in the cache phase a `Claim`), and `rules.Assemble` merges them into one ordered `[]Rule` per `Phase`, refusing conflicting writers (a `Ref` written twice in a phase, a `Claim` outside the cache phase). In the cache phase it adds the catch-all bypass at Order 500, whose expression folds over the claims (`true`, `not <a>`, `(not <a>) and not <b>`), the same string a zone's library bypass narrowed by one public-documents rule has today. Builders: `CacheHosts`, `PublicPathsCache` (the sluis edge package's `PublicCacheRule`, moved unchanged, returning a `CacheRule` with the same fields), `StaticSiteCache`, `RateLimits`, `TrustedClients`, `ManagedWAF`, `SSLMode`. The package links the standard library and `cloudflare/zonespec` only. Additive: no existing package changes. See [docs/cloudflare/rules.md](docs/cloudflare/rules.md).

## v3.0.0

**Breaking: `truvity/cloudflare` becomes `truvity/edge` and absorbs `truvity/gateway`.** One repository, one Go module and one version for everything: the Cloudflare half (Pulumi Go packages and the `cloudflared` chart) and the Envoy half (the `gateway-fleet`, `gateway-groups`, `gateway-policies`, `gateway-projects` and `gateway-routes` charts and the `catalog` package). The full history of `truvity/gateway` is imported (each imported commit carries an `Origin: truvity/gateway@<sha>` trailer); its tags are not, and its changelog up to v1.19.0 is [docs/gateway/CHANGELOG-history.md](docs/gateway/CHANGELOG-history.md). The headings below this one are the former `truvity/cloudflare` releases.

- **Breaking: the Go module is `github.com/truvity/edge/v3`, and `pkg/` is gone.** Import paths change, nothing else in the Go API does:

  | Before | After |
  |---|---|
  | `github.com/truvity/cloudflare/v2/pkg/<name>` | `github.com/truvity/edge/v3/cloudflare/<name>` |
  | `github.com/truvity/gateway/catalog` | `github.com/truvity/edge/v3/catalog` |

  `<name>` is one of `account`, `cfnames`, `edge`, `edgespec`, `presettoken`, `prototypes`, `r2`, `tokensplit`, `tunnel`, `zone`, `zonespec`. Run `go get github.com/truvity/edge/v3@v3.0.0`, rewrite the imports, `go mod tidy`. GitHub redirects the old repository name, but the Go proxy serves the old module path only at the versions already published.
- **Chart versions jump to 3.0.0 with identical content.** Every chart is published at v3.0.0 under its existing OCI name (`oci://ghcr.io/truvity/charts/<chart>`); the rendered output of each chart is byte-identical to the last release of its former repository (`cloudflared` v2.22.0, `gateway-*` v1.19.0). Chart `home` and `sources` now point at `https://github.com/truvity/edge`. A consumer changes the pinned version and nothing else, and its zero-diff gate stays empty.
- **One release, six charts.** A tag publishes `cloudflared`, `gateway-fleet`, `gateway-groups`, `gateway-policies`, `gateway-projects` and `gateway-routes` together, and the same tag is the Go module's version.
- **Documentation is split by half:** `docs/cloudflare/` and `docs/gateway/`, with an index in `docs/README.md`. The repository's CI, Justfile, release and leak-canary configuration are the former `truvity/cloudflare` ones, now covering both halves; `catalog/` is linted by golangci-lint, which `truvity/gateway` did not run.

## v2.22.0

- **`pkg/presettoken` (new): the client of a sluis Cloudflare preset, extracted from an estate's Pulumi code and estate-neutral.** `presettoken.Token(ctx, Config, Request)` (or `Source.Token`, which takes test seams) returns a `Credential` for one preset: an API token, or an R2 access-key pair when `Request.R2`. It execs `sluisctl cloudflare token|r2 <preset> --lifetime ... --no-cache --issuer <url>`, refuses a `sluisctl` that does not resolve under one of `Config.BinaryPrefixes` or is older than `Config.SluisctlMin`, strips every inherited `SLUISCTL_*` and `OP_*` (except `OP_SESSION_*`), proxy and CA-override variable (the issuer therefore travels as `--issuer`, never as `SLUISCTL_ISSUER`), caps every stderr and timeout, refuses an answer that outlives the lifetime asked or expires within `Config.MinRemaining`, and verifies a new API token against `api.cloudflare.com` (fixed host, system roots, no proxy, no redirect) before handing it out; an R2 pair is left to settle. With `Config.BreakGlass` configured and its variable set to `1`, the credential is minted from the account root instead (read from 1Password into memory, never printed) as a child with `Request.Policies`, for an hour at most, and verified as that child. Config is validated before anything runs: binary prefixes must be clean, specific directories (expected `/nix/store`), the lifetime is capped at `MaxLifetime` (1h), the break-glass variable name must contain `BREAKGLASS`, and account ids and preset names are pattern-checked. Nothing defaults to an estate: the issuer, the version floor, the binary prefixes, the break-glass variable, 1Password account, vault, item and field, the child name prefix, the account id and the policies are all inputs. Additive.
- **`pkg/prototypes` (new): the disabled prototype tokens a sluis preset is cloned from.** `prototypes.Deploy(ctx, Args)` declares one protected, `disabled` `cloudflare.AccountToken` per `Preset` (a name and `[]account.ChildTokenPolicy`) as `prototype-<name>`, named `Args.NamePrefix + name`, and exports `sluisPrototypeIds` and `sluisPrototypeStatus_<name>`. It uses the default Cloudflare provider and refuses to run outside break-glass, without `CLOUDFLARE_API_TOKEN`, with a second Cloudflare credential in the environment or a credential or endpoint (`cloudflare:apiToken`, `apiKey`, `apiUserServiceKey`, `baseUrl`) in the stack config or a `CLOUDFLARE_BASE_URL` in the environment, so the root never enters the state. `Args.Moved` gates prototypes that are being moved in from another stack's state. A policy holding Account API Tokens is refused. Additive.
- **`pkg/account`: `ResourceKey` and `LookupPermissionGroupID` are exported.** `ResourceKey(scope, accountID)` is the resource key `NewChildToken` writes for a scope, for callers that build a policy outside Pulumi; `LookupPermissionGroupID` is the account-scoped, exact-match permission group lookup `NewChildToken` uses. Additive.

## v2.21.0

- **`pkg/zone` and `pkg/zonespec`: Always Use HTTPS, HSTS and the Cloudflare Managed Ruleset.** `Args.AlwaysUseHTTPS` (`alwaysUseHttps`, a `*bool`) manages the zone setting `always_use_https` as `setting-<name>-always-use-https`. `Args.HSTS` (`hsts`: `enabled`, `maxAge`, `includeSubdomains`, `nosniff`, `preload`) manages `security_header` as `setting-<name>-security-header`, written with the API's snake_case keys (pulumi/pulumi-cloudflare#1579); validation refuses a `maxAge` outside 0 to 31536000 and `preload` without `enabled`, `includeSubdomains`, a one-year `maxAge` and `alwaysUseHttps: true`. `Args.ManagedWAF` (`managedWaf`: `zone`, `hosts`, `wholeZone`, `description`) owns the zone's `http_request_firewall_managed` entry point ruleset, `firewall-managed-<name>`: one execute rule for the Cloudflare Managed Ruleset (`zone.CloudflareManagedRulesetID`) with its default actions, on the sorted host list (`zonespec.ManagedWAFExpression`) or on the whole zone; an empty host list is refused unless `wholeZone` is set, and so are hosts outside `zone`, duplicates, and `hosts` with `wholeZone`. The ruleset needs `Zone WAF Write` (`account.EdgePoliciesWithWAF`). Unset fields manage nothing, so an existing zone plans exactly what it did. Additive.

## v2.20.0

- **A tunnel may serve names in several zones of its account.** `pkg/tunnel`: `DNS.Zones` (`zones`, a list of `DNSZone{ZoneID, Name}`) is the alternative to `DNS.ZoneID`: each name's CNAME is created in the zone whose name is its longest suffix, and `DNS.ZoneIDOf` returns it. Validation refuses `zoneId` and `zones` together, a zone listed twice, a name in no listed zone, a name listed twice, and `certificates` with `zones` but no `certificates.hosts`. `pkg/edgespec` (and `pkg/edge`): `Derive` no longer refuses a tunnel whose names span zones; `Tunnel.Zones` lists every zone its hosts lie in (sorted; `Zone` stays the first host's). `pkg/edge`: `TunnelArgsInZones` maps a tunnel with its zone ids; `Deploy` uses it and refuses a tunnel zone that is undeclared or of another account, and a host in none of the tunnel's zones (empty `Tunnel.Zones` means `{Zone}`). Resource names do not change (`cname-<tunnel>-<slug>`, `ingress-<tunnel>`), and a single-zone tunnel registers exactly what it did: golden tests pin it byte for byte. A consumer that copies `edgespec.Tunnel` into `edge.Tunnel` field by field must copy `Zones` too. Additive.

## v2.19.0

- **Breaking: the R2 credentials broker is removed.** `cmd/r2broker`, `charts/r2-broker`, the `r2broker` image and archives, the audit catalogue (`catalogue/r2broker.yaml`, `R2BrokerCatalogueFS` in the module root) and the broker's `internal/` packages are gone, and so are the `audit-catalogue` recipe and CI job. A release no longer publishes `ghcr.io/truvity/cloudflare/r2broker`, `oci://ghcr.io/truvity/charts/r2-broker` or `r2broker_*` archives; releases up to v2.18.0 keep theirs. Short-lived R2 credentials now come from the Cloudflare module of sluis (truvity/sluis, `sluisctl cloudflare r2 <preset>`), which clones a disabled prototype token into expiring ones. Before upgrading: uninstall any `r2-broker` release, and move every `r2broker credentials` or `sluisctl r2` caller to a sluis R2 preset. The Go packages (`pkg/account`, `pkg/zone`, `pkg/tunnel`, `pkg/edge`, `pkg/tokensplit`, `pkg/r2`, `pkg/cfnames`, `pkg/edgespec`, `pkg/zonespec`) and the `cloudflared` chart do not change.

## v2.18.0

- **`pkg/zone` and `pkg/zonespec`: `Records`, a zone's own DNS records.** `Args.Records{Zone, Entries}` declares A, AAAA and CNAME records (name, content, proxied, TTL, comment) for names served by something other than a tunnel, such as a load balancer or a CDN; each becomes a `cloudflare.DnsRecord` named `record-<name>-<host-slug>-<type>[-<content-slug>]` (`zone.RecordName`). Only the listed records are managed: a zone's hand-made or imported records stay as they are. Validation refuses names outside the zone, content that does not fit its type, a proxied record with a TTL, duplicates and a CNAME sharing its name. Additive.

## v2.17.1

- Dependency updates.

## v2.17.0

- **`pkg/edgespec` and `pkg/zonespec` (new), and `cfnames.R2Permissions`: the pure side of the edge without the SDKs.** `pkg/edgespec` holds `Derive`, `ValidateRouting`, the tunnel naming rules, `ZoneOf`, `OrderHosts` and `Covers` (with the `Tunnel` type); `pkg/zonespec` holds a zone's declaration (`Args`, `Cache`, `TrustedClients`, `RateLimits` and their blocks), its `Validate` and the filter expressions rendered from it; `pkg/cfnames` gains `R2Permissions`. Each imports the standard library only, so an estate's config layer that only derives and validates no longer links Pulumi, `pulumi-cloudflare` and the rest of the SDKs. `pkg/edge`, `pkg/zone`, `pkg/tunnel` and `pkg/r2` re-export them as aliases and thin wrappers: no behavior, name or resource change. A test fails if `pkg/edgespec` or `pkg/zonespec` ever gains a non-standard-library import. Additive.

## v2.16.0

- **`pkg/account`: `EdgePolicies` (and so `EdgePoliciesWithWAF` and `pkg/tokensplit`'s `edge` child) grants `Config Settings Write` on each zone.** It is the dashboard's "Config Rules: Edit": the zone's `http_config_settings` phase, where Configuration Rules live (a per-hostname SSL mode). Without it an edge token cannot read that phase's entrypoint ruleset (403), so an edge stack can neither declare nor adopt a configuration rule. **Widens the edge grant**: an estate's next root apply updates each `edge` child token's policies in place (the token's value does not change). `pkg/tokensplit`: `PermConfigSettingsWrite`.

## v2.15.0

- **`pkg/tokensplit`: `Inputs.WithoutR2Admin`.** An account that owns no R2 buckets gets no `r2-admin` child (no token, no written key); its `r2-parent-<bucket>` children still follow `Buckets`. Unset, `Deploy` mints exactly what it did. Additive.

## v2.14.0

- **`pkg/edge`: `Derive`, `ValidateRouting` and the tunnel naming rules.** `Derive` takes the exposures that arrive by a Cloudflare tunnel (their accounts and their groups of hosts), the zones with their accounts and each account's legacy-names flag, and returns one `Tunnel` per cluster and account with its hosts in ingress order (`tunnel.OrderHosts`), plus the cache hosts of each zone that declares a cache policy (every exact name of a business project's group; wildcards and mixed groups are left out). It refuses a host outside every zone, a host in a zone of an account its exposure does not list, an exposure with no accounts and a tunnel whose names span two zones. `ValidateRouting` refuses a hostname declared twice across tunnels and a rule shadowed by an earlier one. `TunnelName`, `ReleaseName`, `SecretName` and `ZoneOf` are the naming and zone-lookup rules; `ReleaseBase` is the release stem. **`pkg/tunnel`: `Covers`** is cloudflared's rule-matching test. Additive.

## v2.13.0

- **`pkg/tunnel`: `OrderHosts`.** Puts a tunnel's hostnames in the ingress order cloudflared needs: every exact host first, then wildcards deeper-first, each part in DNS canonical order. The result never trips `Args.Validate`'s shadow check. **`pkg/r2`: `Permissions`** lists the levels `TokenConfig.Permission` accepts. Additive.

## v2.12.0

- **`pkg/edge`: the edge stack factory.** One `edge.Args` (accounts with their already-resolved tokens, zones in `pkg/zone`'s own shape, tunnels, the in-cluster origin) deploys a provider per account, each zone's settings and one tunnel per cluster and account, and exports `tunnelIds`. `Args.Sink` receives each tunnel's secret token, so where it lives stays the caller's decision. `AccountOptions`, `ZoneOptions` and `TunnelOptions` add resource options, typically aliases, so an estate adopting the factory over resources it already has gets an empty first preview. Moved out of an estate's deploy glue; no resource name changes.
- **`pkg/tokensplit`: the root of a token split.** `Deploy` mints `edge`, `status`, `r2-admin` and one `r2-parent-<bucket>` child per enabled bucket from the root token and hands each to a `Writer`; `ParseRootToken`, `ParseChild` and `SoleAccount` are the read side's refusals (whitespace, missing fields, a child of another account, more than one account). Zone ids are matched by account and the Zone WAF grant is an input.
- **`pkg/r2`: `DeployStack`.** Every enabled bucket of one account as that account's bucket-admin child; it mints no tokens.

## v2.11.1

- Dependency updates.

## v2.11.0

- **`r2broker` and `charts/r2-broker`: OpenTelemetry.** `r2broker serve` now
  pushes metrics and traces over OTLP/HTTP, configured by OpenTelemetry's own
  environment only (`OTEL_EXPORTER_OTLP_ENDPOINT` and friends) and only when a
  collector is named: without one nothing is installed and nothing is exported.
  It publishes `r2broker_grants` (steady), `r2broker_credentials_minted_total`
  (by `path`), `r2broker_credentials_failed_total` (by `outcome`),
  `r2broker_credentials_duration_seconds`, `r2broker_mint_api_fallbacks_total`
  and a server span per credential request. The chart gains
  `telemetry.otlp` (`endpoint`, `protocol`, `extraEnv`), which renders
  `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_PROTOCOL` and
  `OTEL_SERVICE_NAME=r2-broker`; it refuses a non-http(s) endpoint and any
  `extraEnv` key that is not `OTEL_*`. Empty renders nothing, so an existing
  release renders byte for byte what it did. See `docs/reference.md`,
  Telemetry.

## v2.10.1

- Dependency updates.

## v2.10.0

- **`zone`: `RateLimits`, the zone's rate limiting rules, validated against
  the plan.** A new optional `Args.RateLimits` (`rateLimits`: `plan` and
  `rules`) manages the zone's whole `http_ratelimit` entry point ruleset as
  one `cloudflare.Ruleset` child, `ratelimit-<name>`. Each rule is a name,
  `endpoints` (host and path or path-prefix pairs, ORed into one expression
  with every value restricted to a character set that needs no escaping) or
  a hand-written `expression`, `characteristics` (default `ip.src`;
  `cf.colo.id` is added for you), `period`, `requests`, `mitigationTimeout`
  and `action`. `plan` is `free`, `pro` or `business` and the limits are
  constants per plan: rule count (Pro: 2), periods (Pro: 10 or 60), block
  durations, counting keys, and the fields Pro lacks (no request method, no
  separate counting expression) are refused before the apply, as is the
  Enterprise-only `log` action. Several endpoints in one rule share one
  counter per address per data center, which is how a small quota covers
  several endpoints; see `docs/reference.md`. Nil manages nothing, so
  existing zones are unchanged; a declared zone owns the whole phase, and
  the token needs `Zone WAF Write` (`EdgePoliciesWithWAF`).

## v2.9.0

- **`account`: `EdgePoliciesWithWAF`, the edge preset with an opt-in WAF
  grant.** `EdgePoliciesWithWAF(wafZoneIDs, zoneIDs...)` returns exactly
  what `EdgePolicies(zoneIDs...)` returns, plus one zone-scoped policy per
  zone in `wafZoneIDs` granting `Zone WAF Write`, the permission
  `zone.Args.TrustedClients` needs. `EdgePolicies` itself is unchanged, and
  an empty `wafZoneIDs` gives identical output. See `docs/reference.md`.

## v2.8.0

- **`zone`: `TrustedClients`, one skip rule for known client ranges.** A new
  optional `Args.TrustedClients` (`trustedClients`: `zone`, `hosts`,
  `ranges`, `description`) creates one zone Ruleset child,
  `firewall-custom-<name>`, in the `http_request_firewall_custom` phase with
  one `skip` rule: a request to one of the hosts from an address in one of
  the ranges skips the remaining custom rules, managed rules, rate
  limiting, Super Bot Fight Mode and the legacy security products. Hosts
  must be lower-case exact names inside the zone; ranges must be CIDR
  blocks with no host bits, no shorter than /8 (IPv4) or /16 (IPv6). Both
  lists empty manages no ruleset. The token needs `Zone WAF Write` on the
  zone, which `EdgePolicies` does not grant. See `docs/reference.md`.

## v2.7.2

- **`r2broker`: locally signed credentials are ones R2 accepts.** The local
  minter had guessed the JWT's claims, because Cloudflare had not published
  them. It now follows Cloudflare's published client-side signing example
  ([Authenticate against R2 with temporary credentials](https://developers.cloudflare.com/r2/examples/authenticate-r2-temp-credentials/)).
  The header gains `typ: JWT`. The claims are `sub` (account id), `iss`
  (parent access key id), `aud` (the S3 endpoint host), `iat`, `exp`,
  `bucket`, `scope`, and `paths.prefixPaths` when the grant has prefixes;
  they used to be `accountId`, `permission`, `prefixes` and
  `parentAccessKeyId`. The HMAC key is the parent secret access key as
  text (hex SHA-256 of the token value), where it used to be the digest's
  raw bytes. Up to v2.7.1, R2 refused every locally minted credential with
  `400 InvalidArgument: X-Amz-Security-Token`, and the broker could not
  see it, because local signing never fails and the API fallback runs only
  on a signing error.
- **`r2broker`: `minting.mode: api` sends `parentAccessKeyId`**, which the
  temporary-credentials endpoint requires.

## v2.7.1

- OpenTelemetry exporters bumped to the current stable line (`otlploggrpc` v0.22.0, `otlptrace` v1.46.0); govulncheck reports no reachable vulnerability.
- `doc.go` lists every package, command and chart, including `pkg/r2`, `pkg/cfnames`, `cmd/r2broker` and `charts/r2-broker`; README follows the component contract's heading order with `Consumers` and `Neighbours` (gateway and tailscale as the other layers of the exposure path) and pins the install example.

## v2.7.0

- **`charts/r2-broker`: `account.id` and `account.parentTokenId` can each
  come from the same Secret as the parent token's value, not just a
  plain value.** New `account.idSecretKey` / `account.parentTokenIdSecretKey`
  values (default `""`) name a key in `account.parentTokenSecretName`;
  when set, the chart mounts that key read-only and points
  `internal/config.Account`'s new `idFile` / `parentTokenIdFile` fields at
  it instead of `id` / `parentTokenId`. The plain values stay the default
  and fully supported — this is additive. `values.schema.json` refuses a
  values file that sets both or neither of a pair. A file's contents are
  trimmed of exactly one trailing newline; any other whitespace fails the
  load. See
  [docs/reference.md#chartsr2-broker-values](docs/reference.md#chartsr2-broker-values).
- **A new SDK-free package, `pkg/cfnames`, holds the R2 jurisdiction list
  and the R2 bucket-name syntax check.** `pkg/account`'s
  `ValidR2Jurisdiction` and `ValidR2BucketName` now delegate to it;
  behaviour is unchanged. Unlike `pkg/account` and `pkg/r2`, `pkg/cfnames`
  imports nothing outside the standard library (enforced by its own
  test), so a downstream config layer that only needs to validate a
  jurisdiction or a bucket name no longer has to pull in the Pulumi/
  Cloudflare SDK, or keep its own copy of the jurisdiction list, to do it.

## v2.6.0

- **`pkg/account` gains `NewChildTokenSet`: the "one root token mints
  every least-privilege child" pattern, reusable instead of hand-written
  once per estate.** It mints one child token per entry of a
  `map[string]ChildTokenConfig`, in sorted key order, and returns them
  keyed by the same name. It is a plain iteration convenience over
  `NewChildToken`: every minted resource's Pulumi name, type and provider
  come entirely from the map's own keys, exactly as they would from the
  equivalent hand-written `NewChildToken` calls, so migrating several such
  calls onto one `NewChildTokenSet` call is a zero-diff change in Pulumi's
  state as long as the map's keys are the names those calls already used.
  Three optional presets — `EdgePolicies(zoneIDs...)`, `R2AdminPolicies()`
  and `R2BucketParentPolicies(jurisdiction, bucket)` — build the
  `[]ChildTokenPolicy` for a common child role from Cloudflare's own live
  permission-group names. See
  [docs/reference.md#newchildtokenset](docs/reference.md#newchildtokenset)
  and the new [docs/layout.md](docs/layout.md), a worked root/edge/r2
  split using this and `NewChildToken` together.
- **`pkg/r2`'s `Config.Token` (and `TokenConfig`) is deprecated.** Mint a
  bucket's parent token separately instead, with `NewChildToken`/
  `NewChildTokenSet` and the new `R2BucketParentPolicies`, then call `New`
  with `Token.Enabled: false` — see
  [docs/reference.md#token-is-deprecated](docs/reference.md#token-is-deprecated).
  `Config.Token` is unchanged and keeps working; this is a documentation
  change only, not a removal.
- **Docs: two stale "(dashboard: … — Edit)" cross-references, in `pkg/r2`'s
  package doc and `docs/reference.md`, are corrected.** Cloudflare's own
  `getAccountApiTokenPermissionGroupsList` names every group this library
  grants with "Write"/"Read" (`Cloudflare Tunnel Write`, `DNS Write`, `SSL
  and Certificates Write`, `Zone Settings Write`, `Cache Settings Write`,
  `Workers R2 Storage Write`, `Workers R2 Storage Bucket Item Write`);
  there is no "Cache Rules" group. No permission-group name this library
  actually looks up changed — only the doc text describing it.

## v2.5.0

- **`r2broker`: the R2 temporary-credentials broker's service, CLI and
  chart.** One binary, two modes, sharing the `internal/mint`,
  `internal/decide` and `internal/verify` groundwork v2.3.0 shipped:
  `r2broker serve` runs `POST /v1/credentials` (a bearer OIDC token in, a
  group -> grant decision, a minted credential out) and `r2broker
  credentials` either calls a running service or mints in-process from a
  local config, printing an AWS `credential_process` document either way.
  A file-locked, per-scope client-side cache keeps several concurrent
  callers in one build to one mint. `charts/r2-broker` deploys the
  service: `grants: []` is the default, and renders a broker that
  verifies tokens and refuses every request rather than a load error.
  `internal/config.Account` gained `parentTokenId` (required): the
  parent token's own id, needed to mint locally, that the original
  config schema had no field for. See
  [docs/reference.md](docs/reference.md#cmdr2broker-and-chartsr2-broker).
- **The broker writes its own audit trail.** Its own catalogue
  (`catalogue/r2broker.yaml`, source `r2broker`, independent of
  access-roster's `roster.*`): `r2broker.credential.minted` and
  `r2broker.credential.refused`, never the credential itself.
  `internal/decide.Decision` gained `Group` (the row that resolved the
  decision), so a minted record can say which group authorized it.
  `r2broker serve` gains `--audit-receiver-url` / `--audit-token-file`
  (or `$R2BROKER_AUDIT_RECEIVER_URL` / `$R2BROKER_AUDIT_TOKEN_FILE`); with
  neither set, every record is still validated against the catalogue and
  logged, kept nowhere else. `charts/r2-broker` gains `audit.receiverUrl`
  and, when it is set, mounts a projected ServiceAccount token (audience
  `audit`) — the broker needs no audit credential of its own.
  `just audit-catalogue` (`audit validate` + `audit check-emitters`) runs
  in CI.

## v2.4.0

- **`pkg/account` gains `NewChildToken`: a generic, least-privilege
  child-token helper.** It mints one account-owned Cloudflare API token
  (`cloudflare.AccountToken`) scoped to a caller-chosen set of policies —
  each a permission-group list plus a `Scope` (`WholeAccountScope`,
  `ZoneScope{ZoneID}` or `R2BucketScope{Jurisdiction, Bucket}`) — under the
  root, account-owned token the estate's Pulumi program runs as. It is the
  generalised form of the token `pkg/r2` has minted for its own bucket
  since v2.2.0: the same by-name, account-scoped permission-group lookup
  (explicit provider carried into the invoke, never the default one), the
  same `Rotation`-forces-`REPLACE` mechanism, and the same
  refuse-before-registering validation, now available to any caller that
  needs a token scoped to a zone or a whole account, not only to an R2
  bucket. A `PermissionGroups` entry naming `Account API Tokens Read` or
  `Account API Tokens Write` is refused outright: only the root token that
  mints child tokens may ever hold either. See
  [docs/reference.md#newchildtoken](docs/reference.md#newchildtoken) and
  [docs/safety.md#pkgaccount](docs/safety.md#pkgaccount).
- **`pkg/r2` now mints its bucket token through `account.NewChildToken`.**
  `Config`, `New`'s signature and every `R2` output are unchanged; the
  underlying `cloudflare.AccountToken` keeps the exact same child name and
  type in Pulumi's state (`NewChildToken`'s result is deliberately not a
  component resource, precisely so this move is invisible to an existing
  deployment — see [docs/reference.md#newchildtoken](docs/reference.md#newchildtoken)).
  No consumer of `pkg/r2` needs to change anything.

## v2.3.1

- **Fix: `pkg/r2`'s permission-group lookup no longer double-encodes the
  Name filter.** It pre-encoded spaces as `%20` before calling
  `getAccountApiTokenPermissionGroupsList`, but the provider URL-encodes
  that argument itself — the request arrived as
  `name=Workers%2520R2%2520Storage%2520Bucket%2520Item%2520Write`,
  Cloudflare filtered by that literal string, and the lookup found
  nothing even when the token had the permission. The name is now passed
  plain. The lookup also now refuses rather than guesses when the
  (undocumented-as-exact) filter returns zero or more than one entry
  whose name matches exactly.

## v2.3.0

- **Internal groundwork for an R2 temporary-credentials broker**, under
  `internal/`: OIDC verification behind a small `Verifier` interface
  (`github.com/coreos/go-oidc/v3`), a group-only config schema
  (`group → bucket/prefixes/permission`, no claim-matching fields), a
  `Minter` that signs temporary credentials locally per Cloudflare's
  documented HS256 mechanism with an API-mode fallback, and pure
  group-to-grant decision logic (a request selects by bucket/prefixes/
  permission, never by naming a group or a row directly). No service,
  CLI, chart, or release artifact ships yet — this change has no
  consumer-visible effect.
- **Fix: `pkg/r2`'s permission-group lookup, and `pkg/tunnel`'s token
  lookup, now carry the account's explicit Cloudflare provider.** Neither
  invoke had one: a resource inherits its parent component's provider
  automatically, but a plain invoke does not, so both fell back to the
  DEFAULT Cloudflare provider regardless of what `opts` gave `New`. An
  estate that disables that default provider saw the whole apply fail at
  preview the moment an R2 bucket's token was enabled, before touching a
  single resource. `account.InvokeOptionsFromResourceOptions(opts...)`
  (and the new `Account.Invoke()`, `Use()`'s invoke-side counterpart) now
  threads the same explicit provider the sibling resources use into both
  invokes. A caller who already passes `acct.Use()` needs no code change.
  See [safety.md](docs/safety.md#an-invoke-does-not-inherit-a-provider-the-way-a-resource-does).

## v2.2.0

- **`pkg/r2`**: a Pulumi component for one R2 bucket, an opt-in expiry
  lifecycle, and an account-owned API token scoped to exactly that bucket,
  plus the S3-compatible credential pair (access key id, secret access
  key) Cloudflare derives from it. The permission group named in the
  token's policy is looked up by name, through the account-scoped
  permission-group list, at apply time — never a hard-coded id.
  `Token.Rotation` forces the token to be replaced — a fresh id and value
  — when changed to any new value, never an in-place rename. No consumer
  currently references it; additive.

## v2.1.0

- **`pkg/zone` gains `cache`: which hostnames a zone may cache at all.**
  Without it a zone caches by file extension — a response under a `.js`
  or `.css` name is stored at the edge whether or not its origin asked,
  and where the origin sent no `Cache-Control` the zone gives the browser
  four hours. The block renders the `http_request_cache_settings` phase
  as two rules whose expressions partition the zone, so exactly one
  matches any request: a listed host is cached only as far as its own
  `Cache-Control` goes, and everything else is bypassed.
  `respectOriginBrowserTtl` sets the zone's Browser Cache TTL to "Respect
  Existing Headers", which is what stops the four hours being added to
  responses no rule caches. Additive: a zone without the block keeps the
  caching it has. New children: `cache-rules-<name>` and
  `setting-<name>-browser-cache-ttl`. A zone has one ruleset per phase,
  so adopting a zone that already has cache rules means importing or
  deleting them first — see [reference.md](docs/reference.md#pkgzone).

## v2.0.1

- **Use this release, not v2.0.0, for Go.** The module path is now
  `github.com/truvity/cloudflare/v2`; v2.0.0's `go.mod` still declared the
  v1 path, and Go refuses a major-version tag whose module path disagrees.
  The chart is unchanged apart from the version.

## v2.0.0

- **Accounts and zones are data.** The new `pkg/account` binds one API
  token to one account and hands back the resource option
  (`acct.Use()`, an explicit provider) that binds a zone, tunnel or
  record to that account, where the provider otherwise arrives
  ambiently. It is additive: `tunnel.Args` still carries the account id
  as a string, and `pkg/tunnel`'s only change in this release is the
  ingress-order check below.
- **`pkg/zone`**: the three zone settings an estate decides — origin SSL
  mode, minimum TLS version and Total TLS — each opt-in; a zone that would
  manage nothing is refused.
- **Breaking: `charts/cloudflared` installs once per account.** The
  Deployment, PodDisruptionBudget and ServiceAccount are named after the
  release, and the pod selector gains `app.kubernetes.io/instance`. A
  selector is immutable, so an existing install is deleted and recreated
  rather than upgraded in place; with the release named `cloudflared`
  every object keeps its name and only the selector changes.
  `serviceAccount.name` now defaults to empty, meaning "named after the
  release".
- **`tunnel.Args.Validate` refuses an ingress list in which an earlier
  rule already catches a later one**: a wildcard before an exact host
  under it, a broad wildcard before a deeper one, a duplicate hostname, a
  catch-all before anything else. cloudflared uses the first matching
  rule's hostname as the origin SNI, so a shadowed host answers 502 with a
  configuration that reads correctly.
- Dependency updates that clear GO-2026-6443 and GO-2026-6348 (gRPC,
  transitive).

## v1.2.0

- **`pkg/tunnel`**: a Pulumi component that provisions a remotely managed
  tunnel from one plain-data struct — the tunnel, its ordered ingress
  rules with the catch-all appended, proxied CNAMEs, and an opt-in
  Advanced Certificate block. The tunnel secret is a caller input and the
  token a secret output; child resource names are a documented contract.

## v1.1.0

- **`values.schema.json`**: an unknown key fails the render.

## v1.0.0

- First release: the `cloudflared` chart and the Go module skeleton.

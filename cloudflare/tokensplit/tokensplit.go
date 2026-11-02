// Package tokensplit is the root stack of a Cloudflare token split: the one
// stack that holds the account-owned token able to mint other tokens, and
// the least-privilege children every other Cloudflare stack runs as.
//
// The split exists so that the stack that touches a resource is never the
// stack that mints the credential scoped to it: a bug in one cannot widen
// what the other reaches. The root mints, per account,
//
//   - "edge": whole-account tunnel write, plus DNS, SSL, zone settings,
//     cache settings and config settings write on the account's zones (and
//     Zone WAF Write on one named zone / Dynamic URL Redirects Write on
//     Inputs.RedirectZones, when asked), for the stack that owns tunnels, DNS
//     and zone settings;
//   - "status": whole-account tunnel write plus DNS write on the zones,
//     for a status stack that needs no SSL, settings or cache;
//   - "r2-admin": bucket create and configure, for the stack that owns R2
//     buckets;
//   - "r2-parent-<name>": one bucket-scoped parent credential per enabled
//     bucket, read by a credential broker as an S3 pair.
//
// No child ever holds Account API Tokens: cloudflare/account's NewChildToken
// refuses that outright, so the invariant does not rely on this package
// getting every policy right.
//
// Where a secret lives is the caller's: Deploy hands each child's value to
// a [Writer], and ReadChild and ParseRootToken are the matching read side
// with the refusals every consumer needs.
package tokensplit

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/edge/v4/cloudflare/account"
)

// The Cloudflare permission-group names the children are granted, each
// resolved by NAME against the account's own list at apply time
// (account.NewChildToken), never a hard-coded id. The API's names say
// "Write" and "Read" where the dashboard says "Edit".
const (
	// PermCloudflareTunnelWrite is account-scope tunnel write.
	PermCloudflareTunnelWrite = "Cloudflare Tunnel Write"
	// PermDNSWrite is zone-scope DNS record write.
	PermDNSWrite = "DNS Write"
	// PermSSLAndCertificatesWrite is the zone-scope grade (not the
	// account-scope one of the same family).
	PermSSLAndCertificatesWrite = "SSL and Certificates Write"
	// PermZoneSettingsWrite is zone-scope settings write.
	PermZoneSettingsWrite = "Zone Settings Write"
	// PermCacheSettingsWrite is the narrowest grant the rulesets API accepts
	// for a zone's cache-settings phase.
	PermCacheSettingsWrite = "Cache Settings Write"
	// PermConfigSettingsWrite is the zone's Configuration Rules (the
	// http_config_settings phase); the dashboard calls it "Config Rules: Edit".
	PermConfigSettingsWrite = "Config Settings Write"
	// PermDynamicRedirectsWrite is the zone's Single Redirects (the
	// http_request_dynamic_redirect phase); the dashboard calls it
	// "Single Redirect > Edit".
	PermDynamicRedirectsWrite = "Dynamic URL Redirects Write"
	// PermWorkersR2StorageWrite creates and administers R2 buckets.
	PermWorkersR2StorageWrite = "Workers R2 Storage Write"
	// PermWorkersR2StorageBucketItemWrite is the bucket-scope grade: object
	// read, write and list on one named bucket.
	PermWorkersR2StorageBucketItemWrite = "Workers R2 Storage Bucket Item Write"
)

// Child names, the keys of the minted set and of the secrets written.
const (
	// ChildEdge is the tunnel, DNS and zone-settings child.
	ChildEdge = "edge"
	// ChildStatus is the tunnel-write and DNS-only child.
	ChildStatus = "status"
	// ChildR2Admin is the bucket-admin child.
	ChildR2Admin = "r2-admin"
	// ChildR2Parent prefixes the per-bucket parent children.
	ChildR2Parent = "r2-parent-"
)

const (
	// rootProviderName names the provider and the child set. It never
	// changes: a resource's Pulumi name comes from the child keys alone.
	rootProviderName = "root"
	// tokenNamePrefix starts every child token's Cloudflare-visible name.
	tokenNamePrefix = "cloudflare-"
)

type (
	// Writer stores one child's values where the consumers read them.
	Writer interface {
		// Put writes the properties as one secret at key in namespace.
		Put(ctx *pulumi.Context, namespace, key string, properties map[string]pulumi.StringInput, opts ...pulumi.ResourceOption) error
	}

	// Zone is one Cloudflare zone the account may own.
	Zone struct {
		// Name is the zone's own name, matched against Inputs.WAFZone.
		Name string
		// ID is the zone id.
		ID string
		// AccountID is the account that owns the zone; only zones of the
		// root token's own account are granted.
		AccountID string
		// SecurityTxtOnly marks a zone managed for its security.txt alone
		// (config manage: security_txt). MatchingZoneIDs leaves it out, so
		// the edge and status children get none of their usual groups on
		// it; the edge child gets Zone Settings Write on it, the one group
		// security.txt needs (SecurityTxtZoneIDs).
		SecurityTxtOnly bool
	}

	// Bucket is one enabled R2 bucket.
	Bucket struct {
		// Name keys the parent credential's secret and Pulumi name, so
		// renaming the bucket on Cloudflare's side never moves the secret.
		Name string
		// Bucket is the Cloudflare bucket name.
		Bucket string
		// Jurisdiction segments the bucket's resource key exactly as it
		// segments the bucket; empty is the default jurisdiction. Read from
		// the same declaration the bucket is created with.
		Jurisdiction string
	}

	// Rotations are the per-kind rotation labels. Changing one replaces
	// that kind's tokens.
	Rotations struct {
		Edge, Status, R2Admin, R2Parent string
	}

	// Inputs is everything Deploy needs, already resolved.
	Inputs struct {
		// RootToken and AccountID are the root token's own values, read by
		// the caller before the program runs (they configure a provider;
		// they are not an Output).
		RootToken string
		AccountID string
		Zones     []Zone
		Buckets   []Bucket
		Rotation  Rotations
		// WAFZone names the zone (Zone.Name) the edge child additionally
		// gets Zone WAF Write on. Empty, or a zone the edge child cannot
		// reach, grants none.
		WAFZone string
		// RedirectZones names the zones (Zone.Name) the edge child
		// additionally gets Dynamic URL Redirects Write on. Each must be a
		// managed (not security.txt-only) zone of AccountID. Empty grants
		// none; keep a zone here until its last redirect ruleset has been
		// deleted, because the same grant authorizes the delete.
		RedirectZones []string
		// WithoutR2Admin mints no r2-admin child: an account that owns no
		// R2 buckets has no stack to administer them, so nothing should
		// hold that power. Its r2-parent children follow Buckets as before.
		WithoutR2Admin bool

		// Writer, Namespace and KeyPrefix say where the children go: each
		// child is written at Namespace, KeyPrefix+"/"+child. The root's
		// consumers read the same keys through ReadChild.
		Writer    Writer
		Namespace string
		KeyPrefix string
	}
)

// Deploy mints every child from an already-resolved root token and writes
// each into the Writer. Pulumi names are the child keys; rotating a kind
// is its Rotation label.
func Deploy(ctx *pulumi.Context, logger *slog.Logger, in Inputs) error {
	rootAcct, err := account.New(ctx, rootProviderName, account.Args{AccountID: in.AccountID}, pulumi.String(in.RootToken))
	if err != nil {
		return fmt.Errorf("root account provider: %w", err)
	}

	zoneIDs := MatchingZoneIDs(in.Zones, in.AccountID)
	txtIDs := SecurityTxtZoneIDs(in.Zones, in.AccountID)

	if len(zoneIDs)+len(txtIDs) == 0 {
		return fmt.Errorf(
			"no zone belongs to account %s (the root token's own account): nothing to scope edge/status to", in.AccountID)
	}

	redirectIDs, err := RedirectZoneIDs(in.Zones, in.AccountID, zoneIDs, in.RedirectZones)
	if err != nil {
		return fmt.Errorf("redirect zones: %w", err)
	}

	cfgs := map[string]account.ChildTokenConfig{
		ChildEdge: {
			Name:     tokenNamePrefix + ChildEdge,
			Rotation: in.Rotation.Edge,
			Policies: append(
				EdgePoliciesWithRedirects(in.Zones, zoneIDs, in.WAFZone, redirectIDs),
				account.SecurityTxtPolicies(txtIDs...)...),
		},
		ChildStatus: {
			Name:     tokenNamePrefix + ChildStatus,
			Rotation: in.Rotation.Status,
			Policies: StatusPolicies(zoneIDs),
		},
	}

	accountChildren := []string{ChildEdge, ChildStatus}

	if !in.WithoutR2Admin {
		cfgs[ChildR2Admin] = account.ChildTokenConfig{
			Name:     tokenNamePrefix + ChildR2Admin,
			Rotation: in.Rotation.R2Admin,
			Policies: account.R2AdminPolicies(),
		}
		accountChildren = append(accountChildren, ChildR2Admin)
	}

	for _, b := range in.Buckets {
		cfgs[ChildR2Parent+b.Name] = account.ChildTokenConfig{
			Name:     tokenNamePrefix + ChildR2Parent + b.Name,
			Rotation: in.Rotation.R2Parent,
			Policies: account.R2BucketParentPolicies(b.Jurisdiction, b.Bucket),
		}
	}

	children, err := account.NewChildTokenSet(ctx, rootProviderName, rootAcct, cfgs)
	if err != nil {
		return fmt.Errorf("mint children: %w", err)
	}

	for _, name := range accountChildren {
		if err := in.put(ctx, name, map[string]pulumi.StringInput{
			"api-token":  children[name].Value,
			"account-id": pulumi.String(in.AccountID),
		}); err != nil {
			return err
		}
	}

	// An r2-parent child is read back as an S3 credential pair: the access
	// key id is the token's own id and the secret access key is the SHA-256
	// of its value, derived at the point of use and deliberately not stored.
	for _, b := range in.Buckets {
		name := ChildR2Parent + b.Name
		if err := in.put(ctx, name, map[string]pulumi.StringInput{
			"api-token":  children[name].Value,
			"account-id": pulumi.String(in.AccountID),
			"token-id":   children[name].ID,
		}); err != nil {
			return err
		}
	}

	logger.InfoContext(ctx.Context(), "cloudflare-root children minted",
		slog.String("account", in.AccountID),
		slog.Int("zones", len(zoneIDs)),
		slog.Int("r2_parents", len(in.Buckets)),
	)

	return nil
}

func (in Inputs) put(ctx *pulumi.Context, child string, properties map[string]pulumi.StringInput) error {
	key := in.KeyPrefix + "/" + child
	if err := in.Writer.Put(ctx, in.Namespace, key, properties); err != nil {
		return fmt.Errorf("write %s: %w", key, err)
	}

	return nil
}

// MatchingZoneIDs is every zone id whose account is the root token's own,
// sorted by zone name for a deterministic policy order. A further zone under
// the same account picks up the grants with no code change. A zone managed for
// its security.txt only is left out (SecurityTxtZoneIDs).
func MatchingZoneIDs(zones []Zone, accountID string) []string {
	return zoneIDs(zones, accountID, false)
}

// SecurityTxtZoneIDs is the ids of the zones of the root token's account that
// are managed for their security.txt only, sorted by zone name.
func SecurityTxtZoneIDs(zones []Zone, accountID string) []string {
	return zoneIDs(zones, accountID, true)
}

func zoneIDs(zones []Zone, accountID string, securityTxtOnly bool) []string {
	sorted := slices.Clone(zones)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	out := make([]string, 0, len(sorted))

	for _, z := range sorted {
		if z.AccountID == accountID && z.SecurityTxtOnly == securityTxtOnly {
			out = append(out, z.ID)
		}
	}

	return out
}

// EdgePolicies is the library's edge grant plus Zone WAF Write on the one
// zone named wafZone. The WAF zone is passed only when it is among the edge
// child's own zone ids: the library does not cross-check.
func EdgePolicies(zones []Zone, zoneIDs []string, wafZone string) []account.ChildTokenPolicy {
	var wafZoneIDs []string

	for _, z := range zones {
		if wafZone != "" && z.Name == wafZone && slices.Contains(zoneIDs, z.ID) {
			wafZoneIDs = []string{z.ID}
		}
	}

	return account.EdgePoliciesWithWAF(wafZoneIDs, zoneIDs...)
}

// EdgePoliciesWithRedirects is EdgePolicies plus one Dynamic URL Redirects
// Write policy per entry of redirectZoneIDs. redirectZoneIDs must already be
// managed zone ids (RedirectZoneIDs maps names to them); EdgePolicies itself
// never grants the redirect group.
func EdgePoliciesWithRedirects(zones []Zone, zoneIDs []string, wafZone string, redirectZoneIDs []string) []account.ChildTokenPolicy {
	policies := EdgePolicies(zones, zoneIDs, wafZone)

	return append(policies, account.RedirectPolicies(redirectZoneIDs...)...)
}

// RedirectZoneIDs maps explicit redirect zone names to the ids of the edge
// child's managed zones. A name that is not a zone of accountID, is
// security.txt-only, or is not one of zoneIDs is refused, and so is a
// repeated name. The returned ids keep the input order, which is the policy
// order. Empty names return nil.
func RedirectZoneIDs(zones []Zone, accountID string, zoneIDs []string, names []string) ([]string, error) {
	out := make([]string, 0, len(names))
	seen := map[string]bool{}

	for _, name := range names {
		if seen[name] {
			return nil, fmt.Errorf("zone %q is listed twice", name)
		}

		seen[name] = true

		found := false

		for _, z := range zones {
			if z.Name != name {
				continue
			}

			found = true

			if z.AccountID == "" || z.AccountID != accountID {
				return nil, fmt.Errorf("zone %q is not in the account this split serves", name)
			}

			if z.SecurityTxtOnly {
				return nil, fmt.Errorf("zone %q is managed for its security.txt only and cannot manage redirects", name)
			}

			if !slices.Contains(zoneIDs, z.ID) {
				return nil, fmt.Errorf("zone %q is not a managed zone of the edge child", name)
			}

			out = append(out, z.ID)
		}

		if !found {
			return nil, fmt.Errorf("zone %q is in none of the declared zones", name)
		}
	}

	return out, nil
}

// StatusPolicies is the status child's own grant: the account-wide tunnel
// write of edge, but DNS only on each zone, with no SSL, settings or cache.
// The library ships no preset for it: how much of edge's grant a backup
// token should get is an estate's trade-off, not a shape to name once.
func StatusPolicies(zoneIDs []string) []account.ChildTokenPolicy {
	policies := []account.ChildTokenPolicy{
		{PermissionGroups: []string{PermCloudflareTunnelWrite}, Scope: account.WholeAccountScope{}},
	}

	for _, zoneID := range zoneIDs {
		policies = append(policies, account.ChildTokenPolicy{
			PermissionGroups: []string{PermDNSWrite},
			Scope:            account.ZoneScope{ZoneID: zoneID},
		})
	}

	return policies
}

// SoleAccount returns the only account name. A child covers exactly one
// account (the root token's own), so a consumer refuses outright when a
// registry declares another: a second account needs its own root scoping,
// not a second consumer of one child.
func SoleAccount(accounts []string, child string) (string, error) {
	if len(accounts) != 1 {
		return "", fmt.Errorf(
			"the registry declares %d accounts; the single %q child covers exactly one: extend the root stack before adding a second",
			len(accounts), child)
	}

	return accounts[0], nil
}

type (
	// Reader reads one secret as the operator running the stack.
	Reader interface {
		// Read returns the secret's properties, or nil when nothing is
		// there yet.
		Read(ctx context.Context, key string) (map[string]string, error)
	}

	// Source names where a token is read, for the refusals' messages.
	Source struct {
		// Where describes the store (for example "ns kv/path"); Hint is
		// what to do when nothing is there yet.
		Where, Hint string
	}
)

// ParseRootToken maps the root token's secret onto its two values. A hand
// written value with a trailing newline (a secret read without
// no-newline) must fail loudly, naming the field, never be silently
// trimmed: a value that would need trimming is a value somebody meant to
// write differently. The value itself is never part of an error.
func ParseRootToken(data map[string]string, src Source) (apiToken, accountID string, err error) {
	if data == nil {
		return "", "", fmt.Errorf("%s holds no value yet: %s", src.Where, src.Hint)
	}

	apiToken, accountID = data["api-token"], data["account-id"]
	if apiToken == "" || accountID == "" {
		return "", "", fmt.Errorf("%s is missing api-token or account-id", src.Where)
	}

	for field, value := range map[string]string{"api-token": apiToken, "account-id": accountID} {
		if err := refuseWhitespace(src.Where, field, value); err != nil {
			return "", "", err
		}
	}

	return apiToken, accountID, nil
}

// ParseChild is ParseRootToken for a child, plus the cross-check that the
// child's account id is the registry's own for the account, so a stale or
// misrouted child can never be mistaken for the right one.
func ParseChild(data map[string]string, src Source, accountName, wantAccountID string) (string, error) {
	apiToken, accountID, err := ParseRootToken(data, src)
	if err != nil {
		return "", err
	}

	if accountID != wantAccountID {
		return "", fmt.Errorf("%s: account-id %s does not match the registry's account %s (%s)",
			src.Where, accountID, accountName, wantAccountID)
	}

	return apiToken, nil
}

func refuseWhitespace(where, field, value string) error {
	if strings.IndexFunc(value, unicode.IsSpace) == -1 {
		return nil
	}

	return fmt.Errorf("%s: field %q holds whitespace (a trailing newline from a read without no-newline?): "+
		"refusing rather than silently trimming it", where, field)
}

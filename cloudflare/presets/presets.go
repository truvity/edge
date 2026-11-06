// Package presets is the table of sluis Cloudflare presets, built from plain
// data rows: what an estate states (a preset's name, kind, account, zones,
// grants and lifetime) is a [Row], and what follows from the kind (the
// permission groups, the scope of each, their order) is computed here.
//
// A preset is the unit sluis mints a Cloudflare credential for. Its rights are
// the policies of a DISABLED prototype token (cloudflare/prototypes declares
// it) and, in break-glass, of a child minted by cloudflare/presettoken. Both
// read the one [Preset] this package builds, so the rights a prototype holds
// and the rights break-glass mints can never differ.
//
// KINDS. A kind names a set of rights, not a stack:
//
//	zone      the per-company zone stack: DNS, SSL, zone settings, cache
//	          settings and config settings write on each zone; then Zone WAF
//	          Write on Row.WAFZones, Bot Management Write on
//	          Row.BotManagementZones, Dynamic URL Redirects Write on
//	          Row.RedirectZones, Cloud Connector Write on
//	          Row.CloudConnectorZones, DNS Write on Row.DNSOnlyZones and the
//	          security.txt-only zones' Zone Settings Write. No account-wide
//	          grant at all, no R2. (The `edge` kind, this plus the account's
//	          tunnel write in one token, was removed in v4.0.0: use zone and
//	          services.)
//	services  the per-company services stack: tunnel write on the account;
//	          DNS write on each zone. The same rights as status.
//	status    tunnel write on the account; DNS write on each zone.
//	dns       DNS write on each zone, nothing else.
//	insights  read only: Account Security Center Insights Read on the account;
//	          Zone Security Center Insights Read, Bot Management Read, Analytics
//	          Read and Zone WAF Read on each zone.
//	insights-admin  Account Security Center Insights Write and Read on the
//	          account; Zone Security Center Insights Write and Read on each
//	          zone. The one kind that can dismiss insights; nothing else writable.
//	r2-admin  DEPRECATED, kept until a narrower replacement exists. Workers R2
//	          Storage Write on the account (every bucket).
//	bucket-rw     R2 object read and write on ONE bucket (an R2 credential).
//	bucket-write  R2 object write on ONE bucket (an R2 credential).
//
// A zone-scoped kind with no Row.Zones covers every zone of its account, in
// zone-name order, so a further zone picks the grant up with no row change.
// No kind grants Account API Tokens, Billing, Account Settings, Memberships or
// Access identity providers: sluis refuses such a prototype outright, and
// [Row] has no way to ask for one.
//
// The package reads no file and no environment, and registers no resource. An
// estate's registry (accounts and zones) and rows are plain values; a zone
// without an id yet (zone_id_pending) is refused where its account's presets
// are asked for.
package presets

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/truvity/edge/v4/cloudflare/account"
	"github.com/truvity/edge/v4/cloudflare/presettoken"
	"github.com/truvity/edge/v4/cloudflare/tokensplit"
)

// The kinds.
const (
	// KindZone is the zone stack's token: the zones' write groups, no
	// account-wide grant.
	KindZone = "zone"
	// KindServices is the services stack's token: account Tunnel Write plus
	// DNS Write on the managed zones, the rights of KindStatus.
	KindServices = "services"
	KindStatus   = "status"
	KindDNS      = "dns"
	KindInsights = "insights"
	// KindInsightsAdmin is the write-capable counterpart of KindInsights: it
	// can dismiss Security Center insights and reads nothing else.
	KindInsightsAdmin = "insights-admin"
	// KindR2Admin is DEPRECATED, kept until a narrower replacement exists.
	KindR2Admin     = "r2-admin"
	KindBucketRW    = "bucket-rw"
	KindBucketWrite = "bucket-write"
)

// The permission groups a kind adds to tokensplit's own.
const (
	permCloudConnectorWrite = "Cloud Connector Write"
	// The insights kind's read-only groups, by the exact names an account's own
	// permission-group list returns (GET /accounts/{id}/tokens/permission_groups,
	// read 2026-10-15). The public permission reference spells the account group
	// "Account Security Center Insights", without "Read"; the API does not, and
	// the apply-time lookup resolves by exact name.
	permAccountSecurityInsights = "Account Security Center Insights Read"
	permZoneSecurityInsights    = "Zone Security Center Insights Read"
	permBotManagementRead       = "Bot Management Read"
	permAnalyticsRead           = "Analytics Read"
	permZoneWAFRead             = "Zone WAF Read"
	// The insights-admin groups, by the exact names an account's own list
	// returns (read 2026-10-15). Read is listed beside Write because dismissing
	// starts from a listing and this package does not assume Write implies Read.
	permAccountSecurityInsightsWrite = "Account Security Center Insights Write"
	permZoneSecurityInsightsWrite    = "Zone Security Center Insights Write"
	permBucketItemRead               = "Workers R2 Storage Bucket Item Read"
	permBucketItemWrite              = tokensplit.PermWorkersR2StorageBucketItemWrite
)

// Store is where sluis keeps a preset's current credential.
type Store string

const (
	// StoreNone is on demand only: never rotated into a store.
	StoreNone Store = "none"
	// StoreExternal is rotated into the estate's external store.
	StoreExternal Store = "external"
)

// DefaultLifetime is a preset's lifetime when Row.Lifetime is zero.
const DefaultLifetime = time.Hour

var (
	// Kinds are the kinds this package builds.
	Kinds = []string{KindZone, KindServices, KindStatus, KindDNS, KindInsights, KindInsightsAdmin, KindR2Admin, KindBucketRW, KindBucketWrite}

	// grantPattern is a policy group's name: the estate's own, a short label.
	grantPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]*$`)
)

type (
	// Account is one Cloudflare account of the registry.
	Account struct {
		Name string
		// ID is the 32-character lowercase hex account id.
		ID string
	}

	// Zone is one zone of the registry.
	Zone struct {
		Name string
		// ID is the zone id; empty while the zone is pending (zone_id_pending).
		ID      string
		Account string
		// SecurityTxtOnly marks a zone managed for its security.txt alone
		// (config manage: security_txt). The edge kind grants it Zone
		// Settings Write and nothing else; the dns and status kinds never
		// cover it; the insights kinds cover it as any zone.
		SecurityTxtOnly bool
		// Owner is who owns the zone: "platform", or a company label (a
		// lowercase name). The registry's zone owner (cloudflare/config
		// Zone.Owner); empty says nothing about ownership. Only a row with
		// an Owner reads it.
		Owner string
	}

	// Registry is the accounts and zones the rows refer to.
	Registry struct {
		Accounts []Account
		Zones    []Zone
	}

	// Row is one preset as an estate states it.
	Row struct {
		// Owner scopes the row to one owner's zones: "platform" or a company
		// label. Empty (the default) is a row of the account, as before.
		// An owner row is the zone kind only, lists Zones explicitly (never
		// "every zone of the account"), every listed zone has this owner in
		// the registry (an unowned zone is refused, so is another owner's),
		// and it has no DNSOnlyZones or CloudConnectorZones, which name zones
		// outside Zones. It grants exactly what the same row without an Owner
		// grants; the owner is what the guard that decides who may ask for
		// the preset reads (Preset.Owner).
		Owner string
		// Name is the sluis preset name: <account>-<kind> by convention
		// ([PresetName]).
		Name string
		// Kind is one of [Kinds].
		Kind string
		// Account is a registry account name.
		Account string
		// Description is shown wherever the preset is listed. Empty: the
		// kind's own sentence ([Description]).
		Description string
		// Zones are zone names of Account, for the kinds that are zone-scoped.
		// Empty: every zone of the account, by name.
		Zones []string
		// WAFZones (edge only) get Zone WAF Write; each must be among the
		// zones of the row.
		WAFZones []string
		// BotManagementZones (edge only) get Bot Management Write, the grant
		// a zone that sets bot_management needs; each must be among the
		// zones of the row. StandardRows leaves it empty: set it on the row.
		BotManagementZones []string
		// RedirectZones (edge only) get Dynamic URL Redirects Write, the
		// grant a zone that declares redirects needs; each must be among
		// the zones of the row and be a managed zone of the account.
		// StandardRows leaves it empty: set it on the row. Keep a zone here
		// until its last redirect ruleset has been deleted: the same grant
		// authorizes the delete.
		RedirectZones []string
		// CloudConnectorZones (edge only) get Cloud Connector Write; each must
		// be a zone of Account. They need not be among the row's zones.
		CloudConnectorZones []string
		// DNSOnlyZones (edge only) get DNS Write alone; each must be a zone of
		// Account that Zones does not already list.
		DNSOnlyZones []string
		// Bucket and Jurisdiction (bucket kinds only) name the one R2 bucket.
		Bucket, Jurisdiction string
		// Grants are the policy groups that may ask for the preset.
		Grants []string
		// Lifetime of a minted credential; zero is [DefaultLifetime].
		Lifetime time.Duration
		// Rotation of the stored credential: set exactly when Store is
		// [StoreExternal], and shorter than Lifetime.
		Rotation time.Duration
		// Store is [StoreNone] when empty.
		Store Store
	}

	// Preset is a row with its policies built.
	Preset struct {
		// Owner is the row's Owner: whose zones the preset is limited to,
		// or empty for a preset of the account.
		Owner      string
		Name, Kind string
		// Account is the account name, AccountID its id.
		Account, AccountID string
		Description        string
		// Policies are what every minted credential may do, and what
		// break-glass mints the same credential with. In a fixed order.
		Policies []account.ChildTokenPolicy
		// R2 presets hand out an S3 pair, not an API token.
		R2       bool
		Lifetime time.Duration
		Rotation time.Duration
		Store    Store
		Grants   []string
	}

	// PolicyDocument is one policy as Cloudflare receives it: the permission
	// group names and the resource keys with their grant.
	PolicyDocument struct {
		Groups    []string          `json:"groups"`
		Resources map[string]string `json:"resources"`
	}
)

// PresetName is the conventional name of the preset of one kind in one account.
func PresetName(accountName, kind string) string { return accountName + "-" + kind }

// Description is the generic sentence a kind's preset has when its row gives
// none. An estate that wants its own wording sets Row.Description.
func Description(accountName, kind string) string {
	switch kind {
	case KindZone:
		return "Pulumi: zone settings, SSL, cache, WAF, bot management and zone records of the " + accountName + " account; no tunnels, no R2"
	case KindServices:
		return "Pulumi: tunnels and their DNS records of the " + accountName + " account"
	case KindStatus:
		return "Pulumi: tunnel and DNS of the " + accountName + " account's status service"
	case KindDNS:
		return "Pulumi: DNS records in the " + accountName + " account"
	case KindInsights:
		return "Read-only security insights, bot management, analytics and WAF settings of the " + accountName + " account"
	case KindInsightsAdmin:
		return "Security insights of the " + accountName + " account, read and dismiss; nothing else"
	case KindR2Admin:
		return "Pulumi: R2 buckets of the " + accountName + " account"
	case KindBucketRW:
		return "R2 objects of one bucket in the " + accountName + " account, read and write"
	case KindBucketWrite:
		return "R2 objects of one bucket in the " + accountName + " account, write"
	}

	return ""
}

// StandardRows are the four rows an account has, in this order and named
// <account>-<kind>: status, r2-admin, zone (Zone WAF Write on wafZones, Bot
// Management Write on botZones, and no redirect grant) and services. grants
// and the other fields of a row are the estate's: set them on the returned
// rows, including Row.RedirectZones for a zone that manages redirects.
func StandardRows(accountName string, grants, wafZones, botZones []string) []Row {
	return []Row{
		{Name: PresetName(accountName, KindStatus), Kind: KindStatus, Account: accountName, Grants: slices.Clone(grants)},
		{Name: PresetName(accountName, KindR2Admin), Kind: KindR2Admin, Account: accountName, Grants: slices.Clone(grants)},
		{
			Name: PresetName(accountName, KindZone), Kind: KindZone, Account: accountName,
			WAFZones: slices.Clone(wafZones), BotManagementZones: slices.Clone(botZones), Grants: slices.Clone(grants),
		},
		{Name: PresetName(accountName, KindServices), Kind: KindServices, Account: accountName, Grants: slices.Clone(grants)},
	}
}

// PendingZone is an error naming a zone of the account that has no id yet, or
// nil. A preset's rights need the id, so every preset of such an account waits
// for it.
func (r Registry) PendingZone(accountName string) error {
	for _, z := range r.zonesOf(accountName) {
		if z.ID == "" {
			return fmt.Errorf("cloudflare presets: zones[%s] of account %s has no zone_id (zone_id_pending): its presets need it", z.Name, accountName)
		}
	}

	return nil
}

// zonesOf are the zones of an account sorted by name.
func (r Registry) zonesOf(accountName string) []Zone {
	var out []Zone

	for _, z := range r.Zones {
		if z.Account == accountName {
			out = append(out, z)
		}
	}

	slices.SortFunc(out, func(a, b Zone) int { return cmp.Compare(a.Name, b.Name) })

	return out
}

func (r Registry) account(name string) (Account, bool) {
	for _, a := range r.Accounts {
		if a.Name == name {
			return a, true
		}
	}

	return Account{}, false
}

// Validate reports every problem with the registry and the rows, without
// building anything. Build calls it.
func Validate(reg Registry, rows []Row) error {
	var errs []error

	seenAccount := map[string]bool{}

	for _, a := range reg.Accounts {
		switch {
		case a.Name == "":
			errs = append(errs, errors.New("presets: an account has no name"))
		case seenAccount[a.Name]:
			errs = append(errs, fmt.Errorf("presets: account %q is declared twice", a.Name))
		case !presettoken.ValidAccountID(a.ID):
			errs = append(errs, fmt.Errorf("presets: account %q: id %q is not a 32-character lowercase hex string", a.Name, a.ID))
		}

		seenAccount[a.Name] = true
	}

	seenZone := map[string]bool{}

	for _, z := range reg.Zones {
		switch {
		case z.Name == "":
			errs = append(errs, errors.New("presets: a zone has no name"))
		case seenZone[z.Name]:
			errs = append(errs, fmt.Errorf("presets: zone %q is declared twice", z.Name))
		case !seenAccount[z.Account]:
			errs = append(errs, fmt.Errorf("presets: zone %q: no account %q", z.Name, z.Account))
		}

		seenZone[z.Name] = true
	}

	names := map[string]bool{}

	for i := range rows {
		row := &rows[i]
		label := fmt.Sprintf("row %d (%q)", i, row.Name)

		if names[row.Name] {
			errs = append(errs, fmt.Errorf("presets: %s: the name is used twice", label))
		}

		names[row.Name] = true

		for _, err := range validateRow(reg, row) {
			errs = append(errs, fmt.Errorf("presets: %s: %w", label, err))
		}
	}

	return errors.Join(errs...)
}

func validateRow(reg Registry, row *Row) []error {
	var errs []error

	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if !presettoken.ValidPresetName(row.Name) {
		bad("the name must be lowercase letters, digits and hyphens, 1-63 characters, starting with a letter or digit")
	}

	if !slices.Contains(Kinds, row.Kind) {
		bad("kind %q is not one of %v", row.Kind, Kinds)
	}

	if _, ok := reg.account(row.Account); !ok {
		bad("no account %q in the registry", row.Account)

		return errs
	}

	zoned := slices.Contains([]string{KindZone, KindServices, KindStatus, KindDNS, KindInsights, KindInsightsAdmin}, row.Kind)
	bucketed := row.Kind == KindBucketRW || row.Kind == KindBucketWrite

	inAccount := map[string]bool{}
	for _, z := range reg.zonesOf(row.Account) {
		inAccount[z.Name] = true
	}

	checkList := func(field string, names []string, allowed func(string) bool, why string) {
		seen := map[string]bool{}

		for _, n := range names {
			switch {
			case !inAccount[n]:
				bad("%s: zone %q is not a zone of account %s", field, n, row.Account)
			case seen[n]:
				bad("%s: zone %q is listed twice", field, n)
			case !allowed(n):
				bad("%s: zone %q %s", field, n, why)
			}

			seen[n] = true
		}
	}

	all := func(string) bool { return true }

	if !zoned && len(row.Zones) > 0 {
		bad("Zones is for the edge, zone, services, status, dns, insights and insights-admin kinds, not %s", row.Kind)
	}

	if zoned {
		checkList("Zones", row.Zones, all, "")

		if len(row.Zones) == 0 && len(inAccount) == 0 {
			bad("account %s has no zone", row.Account)
		}
	}

	if row.Kind == KindDNS || row.Kind == KindStatus || row.Kind == KindServices {
		// These kinds write DNS, which a security.txt-only zone does not manage.
		managed := 0

		for _, z := range reg.zonesOf(row.Account) {
			if !z.SecurityTxtOnly {
				managed++
			}
		}

		for _, n := range row.Zones {
			if zoneNamed(reg, n).SecurityTxtOnly {
				bad("Zones: zone %q is managed for its security.txt only, and the %s kind writes DNS", n, row.Kind)
			}
		}

		if len(row.Zones) == 0 && managed == 0 {
			bad("account %s has no zone the %s kind may cover (every zone is managed for its security.txt only)", row.Account, row.Kind)
		}
	}

	zoneKind := row.Kind == KindZone

	if !zoneKind && (len(row.WAFZones) > 0 || len(row.BotManagementZones) > 0 || len(row.RedirectZones) > 0 ||
		len(row.CloudConnectorZones) > 0 || len(row.DNSOnlyZones) > 0) {
		bad("WAFZones, BotManagementZones, RedirectZones, CloudConnectorZones and DNSOnlyZones are for the zone kind, not %s", row.Kind)
	}

	if zoneKind {
		covered := func(n string) bool { return len(row.Zones) == 0 || slices.Contains(row.Zones, n) }
		notCovered := func(n string) bool { return !covered(n) }

		checkList("WAFZones", row.WAFZones, covered, "is not among the zones of the row")
		checkList("BotManagementZones", row.BotManagementZones, covered, "is not among the zones of the row")
		checkList("RedirectZones", row.RedirectZones, covered, "is not among the zones of the row")
		checkList("CloudConnectorZones", row.CloudConnectorZones, all, "")
		checkList("DNSOnlyZones", row.DNSOnlyZones, notCovered, "is already among the zones of the row")

		managedOnly := func(list string, names []string) {
			for _, n := range names {
				if zoneNamed(reg, n).SecurityTxtOnly {
					bad("%s: zone %q is managed for its security.txt only, which needs Zone Settings Write and nothing else", list, n)
				}
			}
		}

		managedOnly("WAFZones", row.WAFZones)
		managedOnly("BotManagementZones", row.BotManagementZones)
		managedOnly("RedirectZones", row.RedirectZones)
		managedOnly("CloudConnectorZones", row.CloudConnectorZones)
		managedOnly("DNSOnlyZones", row.DNSOnlyZones)
	}

	errs = append(errs, validateOwner(reg, row, zoneKind)...)

	switch {
	case bucketed && !account.ValidR2BucketName(row.Bucket):
		bad("Bucket %q is not an R2 bucket name", row.Bucket)
	case bucketed && !account.ValidR2Jurisdiction(row.Jurisdiction):
		bad("Jurisdiction %q is not default, eu, fedramp or us", row.Jurisdiction)
	case !bucketed && (row.Bucket != "" || row.Jurisdiction != ""):
		bad("Bucket and Jurisdiction are for the bucket kinds, not %s", row.Kind)
	}

	for _, g := range row.Grants {
		if !grantPattern.MatchString(g) {
			bad("grant %q is not a policy group name", g)
		}
	}

	if row.Lifetime < 0 {
		bad("Lifetime %s is negative", row.Lifetime)
	}

	switch row.Store {
	case "", StoreNone:
		if row.Rotation != 0 {
			bad("Rotation is set on a store %q preset: sluis refuses a rotation on it", StoreNone)
		}
	case StoreExternal:
		if row.Rotation <= 0 || row.Rotation >= row.lifetime() {
			bad("Rotation %s must be positive and under Lifetime %s for a store %q preset", row.Rotation, row.lifetime(), StoreExternal)
		}
	default:
		bad("store %q is not %q or %q", row.Store, StoreNone, StoreExternal)
	}

	return errs
}

// ownerPattern is the shape of a row's Owner and a zone's: "platform" or a
// company label.
var ownerPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// validateOwner holds an owner row to what makes it the owner's: the zone
// kind, explicit zones, every one of them the owner's, and no zone outside
// that list.
func validateOwner(reg Registry, row *Row, zoneKind bool) []error {
	if row.Owner == "" {
		return nil
	}

	var errs []error

	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if !ownerPattern.MatchString(row.Owner) {
		bad("Owner %q is not \"platform\" or a company label (lowercase letters, digits and hyphens, starting with a letter)", row.Owner)
	}

	if !zoneKind {
		bad("Owner is for the zone kind, not %s: only the zone stack's rights are scoped to a list of zones", row.Kind)
	}

	if len(row.Zones) == 0 {
		bad("Owner needs Zones: an owner row names its zones, it never means every zone of the account")
	}

	for _, n := range row.Zones {
		z := zoneNamed(reg, n)

		switch {
		case z.Name == "":
			// reported by the Zones check
		case z.Owner == "":
			bad("Zones: zone %q has no owner in the registry, so it cannot be in a row owned by %q", n, row.Owner)
		case z.Owner != row.Owner:
			bad("Zones: zone %q is owned by %q, not by %q", n, z.Owner, row.Owner)
		}
	}

	if len(row.DNSOnlyZones) > 0 || len(row.CloudConnectorZones) > 0 {
		bad("Owner rows take no DNSOnlyZones or CloudConnectorZones: they name zones outside Zones, and an owner row's rights stay inside its list")
	}

	return errs
}

func (r *Row) lifetime() time.Duration {
	if r.Lifetime > 0 {
		return r.Lifetime
	}

	return DefaultLifetime
}

// Build is the presets of the rows, in row order. It fails on any problem with
// the registry or a row, and on an account with a pending zone.
func Build(reg Registry, rows []Row) ([]Preset, error) {
	if err := Validate(reg, rows); err != nil {
		return nil, err
	}

	out := make([]Preset, 0, len(rows))

	for i := range rows {
		p, err := build(reg, &rows[i])
		if err != nil {
			return nil, err
		}

		out = append(out, p)
	}

	return out, nil
}

// BuildReady is Build for the rows of every account that has no pending zone;
// an account with one contributes no preset (its prototypes would be narrower
// than its children, and every other account is unaffected). [ForAccount]
// names the problem when that account's own presets are asked for.
func BuildReady(reg Registry, rows []Row) ([]Preset, error) {
	if err := Validate(reg, rows); err != nil {
		return nil, err
	}

	var out []Preset

	for i := range rows {
		if reg.PendingZone(rows[i].Account) != nil {
			continue
		}

		p, err := build(reg, &rows[i])
		if err != nil {
			return nil, err
		}

		out = append(out, p)
	}

	return out, nil
}

// ForAccount is the presets of one account's rows. It fails only for that
// account's own problems, never another account's.
func ForAccount(reg Registry, rows []Row, accountName string) ([]Preset, error) {
	if _, ok := reg.account(accountName); !ok {
		return nil, fmt.Errorf("cloudflare presets: no account %q", accountName)
	}

	if err := reg.PendingZone(accountName); err != nil {
		return nil, err
	}

	var mine []Row

	for i := range rows {
		if rows[i].Account == accountName {
			mine = append(mine, rows[i])
		}
	}

	return Build(reg, mine)
}

// Named is one preset of the table.
func Named(table []Preset, name string) (Preset, error) {
	for i := range table {
		if table[i].Name == name {
			return table[i], nil
		}
	}

	return Preset{}, fmt.Errorf("cloudflare: no preset %q", name)
}

// AccountOf is the account whose name starts the preset's (<account>-<kind>),
// the longest such name, or "" when none does.
func AccountOf(reg Registry, preset string) string {
	best := ""

	for _, a := range reg.Accounts {
		if strings.HasPrefix(preset, a.Name+"-") && len(a.Name) > len(best) {
			best = a.Name
		}
	}

	return best
}

func build(reg Registry, row *Row) (Preset, error) {
	acct, _ := reg.account(row.Account)

	if err := reg.PendingZone(row.Account); err != nil {
		return Preset{}, err
	}

	zones := reg.zonesOf(row.Account)
	if len(row.Zones) > 0 {
		zones = zones[:0]

		for _, n := range row.Zones {
			zones = append(zones, zoneNamed(reg, n))
		}
	}

	// A zone managed for its security.txt only gets Zone Settings Write on the
	// edge kind and no DNS, status or other zone grant; the insights kinds see
	// it like any zone.
	var managed, txtOnly []Zone

	for _, z := range zones {
		if z.SecurityTxtOnly {
			txtOnly = append(txtOnly, z)
		} else {
			managed = append(managed, z)
		}
	}

	ids := idsOf(zones)
	managedIDs := idsOf(managed)

	p := Preset{
		Name: row.Name, Kind: row.Kind, Account: row.Account, AccountID: acct.ID, Owner: row.Owner,
		Description: row.Description, Lifetime: row.lifetime(), Rotation: row.Rotation, Store: row.Store,
		Grants: slices.Clone(row.Grants),
	}

	if p.Description == "" {
		p.Description = Description(row.Account, row.Kind)
	}

	if p.Store == "" {
		p.Store = StoreNone
	}

	switch row.Kind {
	case KindZone:
		p.Policies = account.ZonePolicies(managedIDs...)

		for _, n := range row.WAFZones {
			p.Policies = append(p.Policies, account.ChildTokenPolicy{
				PermissionGroups: []string{"Zone WAF Write"}, Scope: account.ZoneScope{ZoneID: zoneNamed(reg, n).ID},
			})
		}

		var bots []string
		for _, n := range row.BotManagementZones {
			bots = append(bots, zoneNamed(reg, n).ID)
		}

		p.Policies = append(p.Policies, account.BotManagementPolicies(bots...)...)

		var redirects []string
		for _, n := range row.RedirectZones {
			redirects = append(redirects, zoneNamed(reg, n).ID)
		}

		p.Policies = append(p.Policies, account.RedirectPolicies(redirects...)...)

		for _, n := range row.CloudConnectorZones {
			p.Policies = append(p.Policies, account.ChildTokenPolicy{
				PermissionGroups: []string{permCloudConnectorWrite}, Scope: account.ZoneScope{ZoneID: zoneNamed(reg, n).ID},
			})
		}

		for _, n := range row.DNSOnlyZones {
			p.Policies = append(p.Policies, account.ChildTokenPolicy{
				PermissionGroups: []string{tokensplit.PermDNSWrite}, Scope: account.ZoneScope{ZoneID: zoneNamed(reg, n).ID},
			})
		}

		p.Policies = append(p.Policies, account.SecurityTxtPolicies(idsOf(txtOnly)...)...)
	case KindStatus, KindServices:
		p.Policies = tokensplit.StatusPolicies(managedIDs)
	case KindDNS:
		for _, id := range managedIDs {
			p.Policies = append(p.Policies, account.ChildTokenPolicy{
				PermissionGroups: []string{tokensplit.PermDNSWrite}, Scope: account.ZoneScope{ZoneID: id},
			})
		}
	case KindInsights:
		p.Policies = []account.ChildTokenPolicy{{PermissionGroups: []string{permAccountSecurityInsights}, Scope: account.WholeAccountScope{}}}
		for _, id := range ids {
			p.Policies = append(p.Policies, account.ChildTokenPolicy{
				PermissionGroups: []string{permZoneSecurityInsights, permBotManagementRead, permAnalyticsRead, permZoneWAFRead}, Scope: account.ZoneScope{ZoneID: id},
			})
		}
	case KindInsightsAdmin:
		p.Policies = []account.ChildTokenPolicy{{
			PermissionGroups: []string{permAccountSecurityInsightsWrite, permAccountSecurityInsights}, Scope: account.WholeAccountScope{},
		}}
		for _, id := range ids {
			p.Policies = append(p.Policies, account.ChildTokenPolicy{
				PermissionGroups: []string{permZoneSecurityInsightsWrite, permZoneSecurityInsights}, Scope: account.ZoneScope{ZoneID: id},
			})
		}
	case KindR2Admin:
		p.Policies = account.R2AdminPolicies()
	case KindBucketRW:
		p.R2 = true
		p.Policies = []account.ChildTokenPolicy{{
			PermissionGroups: []string{permBucketItemRead, permBucketItemWrite},
			Scope:            account.R2BucketScope{Jurisdiction: row.Jurisdiction, Bucket: row.Bucket},
		}}
	case KindBucketWrite:
		p.R2 = true
		p.Policies = []account.ChildTokenPolicy{{
			PermissionGroups: []string{permBucketItemWrite},
			Scope:            account.R2BucketScope{Jurisdiction: row.Jurisdiction, Bucket: row.Bucket},
		}}
	}

	check := account.ChildTokenConfig{Name: "prototype-" + p.Name, Policies: p.Policies}
	if err := check.Validate(); err != nil {
		return Preset{}, fmt.Errorf("cloudflare preset %s: %w", p.Name, err)
	}

	return p, nil
}

func zoneNamed(reg Registry, name string) Zone {
	for _, z := range reg.Zones {
		if z.Name == name {
			return z
		}
	}

	return Zone{}
}

func idsOf(zones []Zone) []string {
	out := make([]string, 0, len(zones))
	for _, z := range zones {
		out = append(out, z.ID)
	}

	return out
}

// Request is what presettoken needs to mint the preset's credential:
// break-glass mints the same rights without sluis.
func (p Preset) Request() presettoken.Request {
	return presettoken.Request{Preset: p.Name, R2: p.R2, AccountID: p.AccountID, Policies: p.Policies}
}

// Documents are the policies as Cloudflare receives them, in order: the same
// resource keys a prototype token and a break-glass child are written with.
func (p Preset) Documents() ([]PolicyDocument, error) {
	out := make([]PolicyDocument, 0, len(p.Policies))

	for i, pol := range p.Policies {
		key, err := account.ResourceKey(pol.Scope, p.AccountID)
		if err != nil {
			return nil, fmt.Errorf("cloudflare preset %s: policy %d: %w", p.Name, i, err)
		}

		out = append(out, PolicyDocument{Groups: slices.Clone(pol.PermissionGroups), Resources: map[string]string{key: "*"}})
	}

	return out, nil
}

package config

import (
	"fmt"
	"reflect"
	"strings"
)

// The values of Zone.Manage.
const (
	// ManageFull is the default (an empty manage): the registry manages
	// everything the zone's other fields declare.
	ManageFull = "full"
	// ManageSecurityTxt brings a zone that is managed by hand into the
	// registry for its security.txt only, so a tool that renews it
	// (cloudflare/securitytxt) reaches the zone. Nothing else of the zone is
	// managed: no settings, rulesets, DNS records, tunnel routes, origin pulls,
	// cache rule or bot management, and the edge preset grants its token
	// Zone Settings Write on the zone and nothing more.
	ManageSecurityTxt = "security_txt"
)

// SecurityTxtOnly reports whether the zone is managed for its security.txt
// only (manage: security_txt). Every builder skips such a zone.
func (z Zone) SecurityTxtOnly() bool { return z.Manage == ManageSecurityTxt }

// securityTxtOnlyFields are the yaml keys a security_txt-only zone may set.
var securityTxtOnlyFields = map[string]bool{
	"account": true, "owner": true, "zone_id": true, "manage": true, "security_txt": true,
}

// validateManage checks Zone.Manage: a known value, and for security_txt the
// whole shape of the mode. Every field a zone can set and the mode does not
// allow is refused by its yaml key, so a field added to Zone later is refused
// there without a change here.
func (z Zone) validateManage(where string) error {
	switch z.Manage {
	case "", ManageFull:
		return nil
	case ManageSecurityTxt:
	default:
		return fmt.Errorf("%s.manage: %q is not allowed (valid: %s, %s)", where, z.Manage, ManageFull, ManageSecurityTxt)
	}

	if z.Account == "" {
		return fmt.Errorf("%s.account is required", where)
	}

	if z.ZoneID == "" {
		return fmt.Errorf("%s.zone_id is required for manage: %s (the zone's id cannot be pending)", where, ManageSecurityTxt)
	}

	if z.SecurityTxt == nil {
		return fmt.Errorf("%s.security_txt is required for manage: %s", where, ManageSecurityTxt)
	}

	rv := reflect.ValueOf(z)
	rt := rv.Type()

	for i := range rt.NumField() {
		key, _, _ := strings.Cut(rt.Field(i).Tag.Get("yaml"), ",")
		if key == "" || key == "-" || securityTxtOnlyFields[key] || rv.Field(i).IsZero() {
			continue
		}

		return fmt.Errorf("%s.%s: refused with manage: %s, which manages the zone's security.txt and nothing else", where, key, ManageSecurityTxt)
	}

	return nil
}

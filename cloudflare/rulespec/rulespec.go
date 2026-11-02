// Package rulespec holds the provider-neutral shapes of a Cloudflare ruleset
// rule and of a [Contribution], the rules one source wants in one phase.
//
// It exists so that cloudflare/zonespec (the declaration a consumer
// unmarshals) can carry contributions without importing cloudflare/rules,
// which imports zonespec. Callers use the aliases in cloudflare/rules
// (rules.Contribution, rules.Rule, ...): they are the same types. The
// package links the standard library only.
package rulespec

// Phase is a Cloudflare ruleset phase: a zone holds one entry point ruleset
// per phase.
type Phase string

// TTL is a cache TTL: a mode and, for the overriding modes, the seconds.
type TTL struct {
	Mode string
	// Default is the TTL in seconds (the API's "default"); 0 where the
	// mode takes none.
	Default int
}

// CacheParams are the action parameters of set_cache_settings.
type CacheParams struct {
	// Cache is whether matching responses are cached at all.
	Cache bool
	// EdgeTTL and BrowserTTL are unset when nil.
	EdgeTTL    *TTL
	BrowserTTL *TTL
	// RespectStrongETags honours the origin's strong ETag when set.
	RespectStrongETags bool
}

// ConfigParams are the action parameters of set_config.
type ConfigParams struct {
	// SSL is the SSL mode: off, flexible, full or strict.
	SSL string
}

// SkipParams are the action parameters of skip.
type SkipParams struct {
	// Ruleset is "current" to skip the rest of the rule's own ruleset.
	Ruleset  string
	Phases   []string
	Products []string
}

// ExecuteParams are the action parameters of execute.
type ExecuteParams struct {
	// ID of the ruleset executed.
	ID string
}

// RedirectParams are the action parameters of redirect (a Single Redirect
// in the http_request_dynamic_redirect phase).
type RedirectParams struct {
	// TargetURL is the Rules-language expression the redirect targets, for
	// example concat("https://example.com", http.request.uri.path).
	TargetURL string
	// StatusCode is the redirect status: 301, 302, 307 or 308.
	StatusCode int
	// PreserveQueryString keeps the request's query string.
	PreserveQueryString bool
}

// Params are the action parameters of a rule. At most one member is set,
// the one its Action takes.
type Params struct {
	Cache    *CacheParams
	Config   *ConfigParams
	Skip     *SkipParams
	Execute  *ExecuteParams
	Redirect *RedirectParams
}

// RateLimit is the rate limiting block of a rule in the http_ratelimit phase.
type RateLimit struct {
	Characteristics    []string
	Period             int
	Requests           int
	MitigationTimeout  int
	CountingExpression string
}

// Rule is one Cloudflare ruleset rule in provider-neutral terms.
type Rule struct {
	// Ref identifies the rule within its phase; unique per phase.
	Ref         string
	Description string
	Action      string
	Expression  string
	// Disabled turns the rule off; the zero value is an enabled rule.
	Disabled  bool
	Params    Params
	RateLimit *RateLimit
	// Logging is whether a match is logged (skip rules).
	Logging bool
}

// Contribution is the rules one source wants in one phase.
type Contribution struct {
	// Source names the contributor; with Order it fixes the sort order.
	Source string
	Phase  Phase
	// Order is ascending; ties are broken by Source.
	Order int
	Rules []Rule
	// Claim is the parenthesised expression this contribution serves. Only
	// the cache phase takes one; the bypass rule excludes it.
	Claim string
}

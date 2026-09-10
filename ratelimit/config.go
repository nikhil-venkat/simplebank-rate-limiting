package ratelimit

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Supported algorithms.
const (
	AlgorithmTokenBucket = "token_bucket"
	AlgorithmFixedWindow = "fixed_window"
)

// Supported enforcement modes.
const (
	// EnforcementAllClients limits every client, using Default for any IP
	// that no rule matches.
	EnforcementAllClients = "all_clients"
	// EnforcementListedOnly limits only the IPs listed in Rules. Everyone
	// else passes untouched.
	EnforcementListedOnly = "listed_only"
)

const defaultMaxTrackedIPs = 100_000

// Rule is one rate limit budget: Requests allowed per Per, with room to burst
// up to Burst back to back. Both algorithms read the same three fields so that
// switching algorithms never means rewriting the rule list.
type Rule struct {
	IP       string        `mapstructure:"ip"`
	Requests int           `mapstructure:"requests"`
	Per      time.Duration `mapstructure:"per"`
	Burst    int           `mapstructure:"burst"`

	network *net.IPNet
	prefix  int
}

// Name identifies the rule in logs and in the Decision it produces.
func (r Rule) Name() string {
	if r.IP == "" {
		return "default"
	}
	return r.IP
}

func (r Rule) ratePerSecond() float64 {
	return float64(r.Requests) / r.Per.Seconds()
}

func (r *Rule) validate(label string) error {
	if r.Requests <= 0 {
		return fmt.Errorf("%s: requests must be greater than 0, got %d", label, r.Requests)
	}
	if r.Per <= 0 {
		return fmt.Errorf("%s: per must be a positive duration, got %s", label, r.Per)
	}
	if r.Burst < 0 {
		return fmt.Errorf("%s: burst cannot be negative, got %d", label, r.Burst)
	}
	if r.Burst == 0 {
		// An unset burst means "no allowance beyond the steady rate".
		r.Burst = r.Requests
	}
	return nil
}

// Config is the parsed contents of the rate limit config file.
type Config struct {
	Enabled       bool     `mapstructure:"enabled"`
	Algorithm     string   `mapstructure:"algorithm"`
	Enforcement   string   `mapstructure:"enforcement"`
	Default       Rule     `mapstructure:"default"`
	Exempt        []string `mapstructure:"exempt"`
	Rules         []Rule   `mapstructure:"rules"`
	MaxTrackedIPs int      `mapstructure:"max_tracked_ips"`

	exempt []*net.IPNet
}

// LoadConfig reads and validates a rate limit config file.
//
// A malformed file is a hard error. Booting with a config the operator thinks
// is protecting the service, but which silently parsed to nothing, is worse
// than refusing to boot.
func LoadConfig(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("cannot read rate limit config %q: %w", path, err)
	}

	config := &Config{}
	if err := v.Unmarshal(config); err != nil {
		return nil, fmt.Errorf("cannot parse rate limit config %q: %w", path, err)
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid rate limit config %q: %w", path, err)
	}

	return config, nil
}

// Validate fills in defaults and parses every network in the config. It is
// exported so that tests can build a Config literal and put it through the
// same checks a file goes through.
func (c *Config) Validate() error {
	if c.Algorithm == "" {
		c.Algorithm = AlgorithmTokenBucket
	}
	if c.Algorithm != AlgorithmTokenBucket && c.Algorithm != AlgorithmFixedWindow {
		return fmt.Errorf("unknown algorithm %q, want %q or %q", c.Algorithm, AlgorithmTokenBucket, AlgorithmFixedWindow)
	}

	if c.Enforcement == "" {
		c.Enforcement = EnforcementAllClients
	}
	if c.Enforcement != EnforcementAllClients && c.Enforcement != EnforcementListedOnly {
		return fmt.Errorf("unknown enforcement %q, want %q or %q", c.Enforcement, EnforcementAllClients, EnforcementListedOnly)
	}

	if c.MaxTrackedIPs == 0 {
		c.MaxTrackedIPs = defaultMaxTrackedIPs
	}
	if c.MaxTrackedIPs < 0 {
		return fmt.Errorf("max_tracked_ips cannot be negative, got %d", c.MaxTrackedIPs)
	}

	c.exempt = nil
	for _, entry := range c.Exempt {
		network, err := parseNetwork(entry)
		if err != nil {
			return fmt.Errorf("exempt: %w", err)
		}
		c.exempt = append(c.exempt, network)
	}

	for i := range c.Rules {
		rule := &c.Rules[i]
		if strings.TrimSpace(rule.IP) == "" {
			return fmt.Errorf("rules[%d]: ip is required", i)
		}

		network, err := parseNetwork(rule.IP)
		if err != nil {
			return fmt.Errorf("rules[%d]: %w", i, err)
		}
		rule.network = network
		rule.prefix, _ = network.Mask.Size()

		if err := rule.validate(fmt.Sprintf("rules[%d] (%s)", i, rule.IP)); err != nil {
			return err
		}
	}

	// Most specific prefix wins, regardless of the order an operator happened
	// to write the rules in. sort.SliceStable keeps file order as the
	// tie-breaker for rules with equal prefix length.
	sort.SliceStable(c.Rules, func(i, j int) bool {
		return c.Rules[i].prefix > c.Rules[j].prefix
	})

	// The default rule is only consulted when we limit unlisted clients.
	if c.Enforcement == EnforcementAllClients {
		if err := c.Default.validate("default"); err != nil {
			return err
		}
	}

	return nil
}

// match reports the budget that applies to ip.
//
// Evaluation order is exempt, then rules, then the default. limited is false
// when the client should not be rate limited at all.
func (c *Config) match(ip net.IP) (rule Rule, exempt bool, limited bool) {
	for _, network := range c.exempt {
		if network.Contains(ip) {
			return Rule{}, true, false
		}
	}

	for _, candidate := range c.Rules {
		if candidate.network.Contains(ip) {
			return candidate, false, true
		}
	}

	if c.Enforcement == EnforcementListedOnly {
		return Rule{}, false, false
	}

	return c.Default, false, true
}

// parseNetwork accepts either a CIDR block or a bare address, which is treated
// as a single-host /32 or /128.
func parseNetwork(entry string) (*net.IPNet, error) {
	entry = strings.TrimSpace(entry)

	if strings.Contains(entry, "/") {
		_, network, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("%q is not a valid CIDR block", entry)
		}
		return network, nil
	}

	ip := net.ParseIP(entry)
	if ip == nil {
		return nil, fmt.Errorf("%q is not a valid IP address or CIDR block", entry)
	}

	if v4 := ip.To4(); v4 != nil {
		return &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)}, nil
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}, nil
}

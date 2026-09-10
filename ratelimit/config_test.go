package ratelimit

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoadConfigFromFile(t *testing.T) {
	t.Parallel()

	config, err := LoadConfig(filepath.Join("testdata", "ratelimit.yaml"))
	require.NoError(t, err)

	require.True(t, config.Enabled)
	require.Equal(t, AlgorithmTokenBucket, config.Algorithm)
	require.Equal(t, EnforcementAllClients, config.Enforcement)
	require.Equal(t, 5, config.Default.Requests)
	require.Equal(t, time.Second, config.Default.Per)
	require.Equal(t, 5, config.Default.Burst)
	require.Equal(t, 1000, config.MaxTrackedIPs)
	require.Len(t, config.Rules, 2)
}

func TestLoadConfigMissingFile(t *testing.T) {
	t.Parallel()

	_, err := LoadConfig(filepath.Join("testdata", "does-not-exist.yaml"))
	require.Error(t, err)
}

func TestMostSpecificRuleWins(t *testing.T) {
	t.Parallel()

	// The fixture lists the /24 before the /32 on purpose. Matching must not
	// depend on the order an operator happened to type the rules in.
	config, err := LoadConfig(filepath.Join("testdata", "ratelimit.yaml"))
	require.NoError(t, err)

	rule, exempt, limited := config.match(net.ParseIP("198.51.100.7").To4())
	require.False(t, exempt)
	require.True(t, limited)
	require.Equal(t, "198.51.100.7", rule.IP)
	require.Equal(t, 50, rule.Requests)
}

func TestCIDRRuleMatchesMembers(t *testing.T) {
	t.Parallel()

	config, err := LoadConfig(filepath.Join("testdata", "ratelimit.yaml"))
	require.NoError(t, err)

	rule, _, limited := config.match(net.ParseIP("198.51.100.9").To4())
	require.True(t, limited)
	require.Equal(t, "198.51.100.0/24", rule.IP)
	require.Equal(t, 2, rule.Requests)
}

func TestUnmatchedIPFallsBackToDefault(t *testing.T) {
	t.Parallel()

	config, err := LoadConfig(filepath.Join("testdata", "ratelimit.yaml"))
	require.NoError(t, err)

	rule, exempt, limited := config.match(net.ParseIP("203.0.113.5").To4())
	require.False(t, exempt)
	require.True(t, limited)
	require.Equal(t, "default", rule.Name())
	require.Equal(t, 5, rule.Requests)
}

func TestExemptMatching(t *testing.T) {
	t.Parallel()

	config, err := LoadConfig(filepath.Join("testdata", "ratelimit.yaml"))
	require.NoError(t, err)

	for _, addr := range []string{"10.1.2.3", "192.0.2.1"} {
		_, exempt, limited := config.match(net.ParseIP(addr).To4())
		require.True(t, exempt, "%s should be exempt", addr)
		require.False(t, limited)
	}
}

func TestBurstDefaultsToRequests(t *testing.T) {
	t.Parallel()

	config := &Config{Enabled: true, Default: Rule{Requests: 7, Per: time.Second}}
	require.NoError(t, config.Validate())
	require.Equal(t, 7, config.Default.Burst, "an unset burst means no allowance beyond the steady rate")
}

func TestValidateAppliesDefaults(t *testing.T) {
	t.Parallel()

	config := &Config{Enabled: true, Default: Rule{Requests: 1, Per: time.Second, Burst: 1}}
	require.NoError(t, config.Validate())

	require.Equal(t, AlgorithmTokenBucket, config.Algorithm)
	require.Equal(t, EnforcementAllClients, config.Enforcement)
	require.Equal(t, defaultMaxTrackedIPs, config.MaxTrackedIPs)
}

func TestInvalidConfigFailsFast(t *testing.T) {
	t.Parallel()

	// Every one of these must be a hard error. Booting with rules that
	// silently parsed to nothing, while the operator believes the service is
	// protected, is worse than refusing to boot.
	testCases := []struct {
		name   string
		config *Config
	}{
		{
			name:   "unknown algorithm",
			config: &Config{Algorithm: "leaky_bucket", Default: Rule{Requests: 1, Per: time.Second}},
		},
		{
			name:   "unknown enforcement",
			config: &Config{Enforcement: "sometimes", Default: Rule{Requests: 1, Per: time.Second}},
		},
		{
			name:   "zero requests",
			config: &Config{Default: Rule{Requests: 0, Per: time.Second}},
		},
		{
			name:   "negative requests",
			config: &Config{Default: Rule{Requests: -1, Per: time.Second}},
		},
		{
			name:   "zero window",
			config: &Config{Default: Rule{Requests: 1, Per: 0}},
		},
		{
			name:   "negative burst",
			config: &Config{Default: Rule{Requests: 1, Per: time.Second, Burst: -1}},
		},
		{
			name:   "negative max tracked ips",
			config: &Config{MaxTrackedIPs: -1, Default: Rule{Requests: 1, Per: time.Second}},
		},
		{
			name: "malformed cidr in rules",
			config: &Config{
				Default: Rule{Requests: 1, Per: time.Second},
				Rules:   []Rule{{IP: "203.0.113.0/99", Requests: 1, Per: time.Second}},
			},
		},
		{
			name: "malformed ip in exempt",
			config: &Config{
				Default: Rule{Requests: 1, Per: time.Second},
				Exempt:  []string{"not-an-ip"},
			},
		},
		{
			name: "rule without ip",
			config: &Config{
				Default: Rule{Requests: 1, Per: time.Second},
				Rules:   []Rule{{Requests: 1, Per: time.Second}},
			},
		},
		{
			name: "rule with invalid budget",
			config: &Config{
				Default: Rule{Requests: 1, Per: time.Second},
				Rules:   []Rule{{IP: "203.0.113.1", Requests: 0, Per: time.Second}},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, tc.config.Validate())
		})
	}
}

func TestMalformedYAMLFailsFast(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "ratelimit.yaml")
	require.NoError(t, os.WriteFile(path, []byte("enabled: true\n  bad indent: ["), 0o600))

	_, err := LoadConfig(path)
	require.Error(t, err)
}

func TestShippedConfigIsValid(t *testing.T) {
	t.Parallel()

	// The file that actually ships must load. This catches a typo in
	// ratelimit.yaml at test time rather than at container start.
	config, err := LoadConfig(filepath.Join("..", "ratelimit.yaml"))
	require.NoError(t, err)
	require.True(t, config.Enabled)
	require.Greater(t, config.Default.Requests, 0)

	// Under docker compose the API sees the bridge gateway address, which
	// lives in 172.16.0.0/12. Exempting that range would silently exempt all
	// local traffic and make the limiter look broken during validation.
	_, exempt, _ := config.match(net.ParseIP("172.17.0.1").To4())
	require.False(t, exempt, "the docker bridge range must not be exempt by default")
}

func TestNormalizeIP(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		addr string
		want string
	}{
		{"1.2.3.4:5678", "1.2.3.4"},
		{"1.2.3.4", "1.2.3.4"},
		{"[::1]:5678", "::1"},
		{"::1", "::1"},
		{"[::ffff:127.0.0.1]:80", "127.0.0.1"},
		{"::ffff:127.0.0.1", "127.0.0.1"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"garbage", ""},
		{"", ""},
	}

	for _, tc := range testCases {
		t.Run(tc.addr, func(t *testing.T) {
			_, key := NormalizeIP(tc.addr)
			require.Equal(t, tc.want, key)
		})
	}
}

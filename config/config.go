// Package config provides application configuration management for ToughRADIUS.
//
// This package handles loading configuration from YAML files and environment variables,
// with support for both PostgreSQL and SQLite databases. Configuration can be loaded
// from multiple locations with the following priority:
//
//  1. Custom file specified via -c flag
//  2. ./toughradius.yml in current directory
//  3. /etc/toughradius.yml system-wide config
//  4. DefaultAppConfig embedded defaults
//
// Environment variables override YAML settings using the TOUGHRADIUS_ prefix.
// For example, TOUGHRADIUS_WEB_PORT overrides the web.port setting.
//
// Example usage:
//
//	cfg := config.LoadConfig("toughradius.yml")
//	fmt.Printf("Web server will listen on %s:%d\n", cfg.Web.Host, cfg.Web.Port)
//	fmt.Printf("Database type: %s\n", cfg.Database.Type)
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/talkincode/toughradius/v9/pkg/common"
	"gopkg.in/yaml.v3"
)

// DefaultWebSecret is the development placeholder used when web.secret is not
// overridden by a config file or TOUGHRADIUS_WEB_SECRET.
const DefaultWebSecret = "9b6de5cc-0731-1203-xxtt-0f568ac9da37" //nolint:gosec // Development placeholder; production must override it.

// DBConfig holds database connection settings for ToughRADIUS.
//
// Supports two database backends:
//   - postgres: Full-featured PostgreSQL (production recommended)
//   - sqlite: Embedded database for development/testing (no CGO required)
//
// Connection pooling is configured via MaxConn and IdleConn to optimize
// performance under high concurrent load from RADIUS requests.
//
// When Type is "postgres", Host/Port/User/Passwd are required.
// When Type is "sqlite", only Name is used (database file path).
//
// Environment variable overrides:
//   - TOUGHRADIUS_DB_TYPE
//   - TOUGHRADIUS_DB_HOST
//   - TOUGHRADIUS_DB_PORT
//   - TOUGHRADIUS_DB_NAME
//   - TOUGHRADIUS_DB_USER
//   - TOUGHRADIUS_DB_PWD
//   - TOUGHRADIUS_DB_DEBUG
type DBConfig struct {
	Type     string `yaml:"type"`      // Database type: postgres or sqlite
	Host     string `yaml:"host"`      // PostgreSQL host address
	Port     int    `yaml:"port"`      // PostgreSQL port
	Name     string `yaml:"name"`      // Database name or SQLite file path
	User     string `yaml:"user"`      // PostgreSQL username
	Passwd   string `yaml:"passwd"`    // PostgreSQL password
	MaxConn  int    `yaml:"max_conn"`  // Maximum connections
	IdleConn int    `yaml:"idle_conn"` // Idle connections
	Debug    bool   `yaml:"debug"`     // Debug mode
}

// SysConfig holds system-level settings for the ToughRADIUS application.
//
// These settings control global application behavior including:
//   - Application identifier for logging and metrics
//   - Timezone for timestamp handling
//   - Working directory for data/log/certificate storage
//   - Debug mode for verbose logging
//
// The Workdir is critical as it determines the base path for all runtime
// data including logs/, data/, private/, public/, and backup/ directories.
//
// Environment variable overrides:
//   - TOUGHRADIUS_SYSTEM_WORKER_DIR
//   - TOUGHRADIUS_SYSTEM_DEBUG
type SysConfig struct {
	Appid    string `yaml:"appid"`
	Location string `yaml:"location"`
	Workdir  string `yaml:"workdir"`
	Debug    bool   `yaml:"debug"`
}

// WebConfig holds HTTP/HTTPS server settings for the management interface.
//
// The web server provides the React Admin UI and RESTful API endpoints
// for managing RADIUS users, devices, and system configuration.
//
// Secret is used for JWT token signing and session management. It should
// be a cryptographically secure random string in production environments.
//
// TlsEnabled controls whether the built-in HTTPS listener is started. A nil
// value preserves the legacy default and enables the listener.
//
// TlsPort is the built-in HTTPS listener port when TlsEnabled is true and valid
// certificates are present.
//
// Environment variable overrides:
//   - TOUGHRADIUS_WEB_HOST
//   - TOUGHRADIUS_WEB_PORT
//   - TOUGHRADIUS_WEB_TLS_ENABLED
//   - TOUGHRADIUS_WEB_TLS_PORT
//   - TOUGHRADIUS_WEB_SECRET
type WebConfig struct {
	Host       string `yaml:"host"`
	Port       int    `yaml:"port"`
	TlsEnabled *bool  `yaml:"tls_enabled"`
	TlsPort    int    `yaml:"tls_port"`
	Secret     string `yaml:"secret"`
}

// RadiusdConfig holds RADIUS protocol service settings.
//
// Configures three RADIUS services that run concurrently via errgroup:
//   - Authentication service (UDP, default port 1812, RFC 2865)
//   - Accounting service (UDP, default port 1813, RFC 2866)
//   - RadSec service (TCP/TLS, default port 2083, RFC 6614)
//
// RadSec (RADIUS over TLS) provides encrypted transport for RADIUS traffic.
// Certificate paths can be absolute or relative to System.Workdir.
// RadsecWorker controls the goroutine pool size for concurrent TLS connections.
//
// Enabled allows disabling the RADIUS services while keeping the web interface
// running for configuration management.
//
// Environment variable overrides:
//   - TOUGHRADIUS_RADIUS_ENABLED
//   - TOUGHRADIUS_RADIUS_HOST
//   - TOUGHRADIUS_RADIUS_AUTHPORT
//   - TOUGHRADIUS_RADIUS_ACCTPORT
//   - TOUGHRADIUS_RADIUS_RADSEC_PORT
//   - TOUGHRADIUS_RADIUS_RADSEC_WORKER
//   - TOUGHRADIUS_RADIUS_RADSEC_CA_CERT
//   - TOUGHRADIUS_RADIUS_RADSEC_CERT
//   - TOUGHRADIUS_RADIUS_RADSEC_KEY
//   - TOUGHRADIUS_RADIUS_DEBUG
//
// Proxy settings control the RADIUS proxy/replication subsystem:
//   - ProxyEnabled gates upstream forwarding and request replication
//   - DynAuthEnabled gates the inbound RFC 5176 dynamic-authorization server
//     (upstream-initiated CoA/Disconnect relayed to the NAS)
//   - DynAuthPort is the dynamic-authorization listener port (default 3799)
//   - DynAuthSecret is the fallback shared secret for dynamic-authorization
//     clients that are not registered as proxy upstreams
type RadiusdConfig struct {
	Enabled      bool   `yaml:"enabled" json:"enabled"`
	Host         string `yaml:"host" json:"host"`
	AuthPort     int    `yaml:"auth_port" json:"auth_port"`
	AcctPort     int    `yaml:"acct_port" json:"acct_port"`
	RadsecPort   int    `yaml:"radsec_port" json:"radsec_port"`
	RadsecWorker int    `yaml:"radsec_worker" json:"radsec_worker"`
	RadsecCaCert string `yaml:"radsec_ca_cert" json:"radsec_ca_cert"` // RadSec CA certificate path
	RadsecCert   string `yaml:"radsec_cert" json:"radsec_cert"`       // RadSec server certificate path
	RadsecKey    string `yaml:"radsec_key" json:"radsec_key"`         // RadSec server private key path

	ProxyEnabled   bool   `yaml:"proxy_enabled" json:"proxy_enabled"`     // enable upstream proxy/replicate forwarding
	ProxyTimeout   int    `yaml:"proxy_timeout" json:"proxy_timeout"`     // upstream exchange timeout in seconds (default 5)
	ProxyRetry     int    `yaml:"proxy_retry" json:"proxy_retry"`         // upstream retransmission count (default 2)
	DynAuthEnabled bool   `yaml:"dynauth_enabled" json:"dynauth_enabled"` // enable inbound CoA/Disconnect listener
	DynAuthPort    int    `yaml:"dynauth_port" json:"dynauth_port"`       // inbound dynamic-authorization port (default 3799)
	DynAuthSecret  string `yaml:"dynauth_secret" json:"dynauth_secret"`   // fallback dynamic-authorization shared secret

	Debug bool `yaml:"debug" json:"debug"`
}

// PortalConfig controls the Huawei/CMCC Portal V2 UDP protocol server.
//
// Portal is protocol-only: the RADIUS Access-Request is sent by the NAS to
// this instance's 1812 listener; no login page is hosted here. Register
// Portal-capable NAS devices through /api/v1/portal-nases (addr + secret).
//
// Environment variables:
//   - TOUGHRADIUS_PORTAL_ENABLED
//   - TOUGHRADIUS_PORTAL_BIND_HOST / _BIND_PORT / _NAS_PORT
//   - TOUGHRADIUS_PORTAL_TIMEOUT / _RETRY / _SESSION_TTL
type PortalConfig struct {
	Enabled    bool   `yaml:"enabled" json:"enabled"`
	BindHost   string `yaml:"bind_host" json:"bind_host"`
	BindPort   int    `yaml:"bind_port" json:"bind_port"`     // server listener, default 50100
	NASPort    int    `yaml:"nas_port" json:"nas_port"`       // NAS Portal port, default 2000
	Timeout    int    `yaml:"timeout" json:"timeout"`         // seconds, default 5
	Retry      int    `yaml:"retry" json:"retry"`             // retransmissions, default 2
	SessionTTL int    `yaml:"session_ttl" json:"session_ttl"` // seconds, default 600
}

// CaptiveConfig configures HTTP 302 redirection and the built-in login page.
// The upstream project forbids hosting a login page by default; enabling this
// section is an explicit second-development opt-in.
type CaptiveConfig struct {
	Enabled       bool     `yaml:"enabled" json:"enabled"`
	BindAddr      string   `yaml:"bind_addr" json:"bind_addr"`
	LoginPath     string   `yaml:"login_path" json:"login_path"`
	PortalURL     string   `yaml:"portal_url" json:"portal_url"`
	AllowedHosts  []string `yaml:"allowed_hosts" json:"allowed_hosts"`
	WhitelistCIDR []string `yaml:"whitelist_cidr" json:"whitelist_cidr"`
	AuthorizedTTL int      `yaml:"authorized_ttl" json:"authorized_ttl"`
}

// DHCPConfig configures the RFC 2131 DHCPv4 server and Portal URL delivery.
type DHCPConfig struct {
	Enabled   bool   `yaml:"enabled" json:"enabled"`
	BindAddr  string `yaml:"bind_addr" json:"bind_addr"`
	ServerIP  string `yaml:"server_ip" json:"server_ip"`
	Network   string `yaml:"network" json:"network"`
	Gateway   string `yaml:"gateway" json:"gateway"`
	DNS       string `yaml:"dns" json:"dns"`
	Start     string `yaml:"start" json:"start"`
	End       string `yaml:"end" json:"end"`
	Lease     int    `yaml:"lease" json:"lease"`
	PortalURL string `yaml:"portal_url" json:"portal_url"`
}

// DNSRedirectConfig configures RFC 1035 DNS redirection (deployment practice).
type DNSRedirectConfig struct {
	Enabled   bool     `yaml:"enabled" json:"enabled"`
	BindAddr  string   `yaml:"bind_addr" json:"bind_addr"`
	PortalIP  string   `yaml:"portal_ip" json:"portal_ip"`
	Upstream  string   `yaml:"upstream" json:"upstream"`
	Whitelist []string `yaml:"whitelist" json:"whitelist"`
	TTL       int      `yaml:"ttl" json:"ttl"`
}

// IPv6Config configures the DHCPv6 (RFC 3315/8415) server for IPv6 tracing.
type IPv6Config struct {
	Enabled   bool   `yaml:"enabled" json:"enabled"`
	BindAddr  string `yaml:"bind_addr" json:"bind_addr"`
	Prefix    string `yaml:"prefix" json:"prefix"`
	DNS       string `yaml:"dns" json:"dns"`
	PortalURL string `yaml:"portal_url" json:"portal_url"`
	Lease     int    `yaml:"lease" json:"lease"`
}

// OpenAPIConfig configures the northbound REST API security layer.
// Credentials maps AppKey to AppSecret; an empty map disables all open API
// requests (fail-closed). Secrets are never returned by any API.
type OpenAPIConfig struct {
	Enabled        bool              `yaml:"enabled" json:"enabled"`
	Credentials    map[string]string `yaml:"credentials" json:"credentials"`
	QPS            int               `yaml:"qps" json:"qps"`
	NonceTTL       int               `yaml:"nonce_ttl" json:"nonce_ttl"`
	IdempotencyTTL int               `yaml:"idempotency_ttl" json:"idempotency_ttl"`
}

// LogConfig holds logging settings using zap structured logger.
//
// Mode can be "development" or "production":
//   - development: Human-readable console output with stack traces
//   - production: JSON-formatted output optimized for log aggregation
//
// FileEnable controls whether logs are written to Filename in addition
// to console output. File logging is recommended for production environments.
//
// Environment variable overrides:
//   - TOUGHRADIUS_LOGGER_MODE
//   - TOUGHRADIUS_LOGGER_FILE_ENABLE
type LogConfig struct {
	Mode       string `yaml:"mode"`
	FileEnable bool   `yaml:"file_enable"`
	Filename   string `yaml:"filename"`
}

// AppConfig is the root configuration structure for ToughRADIUS.
//
// It aggregates all subsystem configurations and provides helper methods
// for accessing runtime directories and certificate paths.
//
// Configuration loading priority:
//  1. YAML file values
//  2. Environment variable overrides (TOUGHRADIUS_* prefix)
//  3. DefaultAppConfig fallback values
//
// The configuration is loaded once at application startup via LoadConfig()
// and accessed globally through app.GApp().
type AppConfig struct {
	System      SysConfig         `yaml:"system" json:"system"`
	Web         WebConfig         `yaml:"web" json:"web"`
	Database    DBConfig          `yaml:"database" json:"database"`
	Radiusd     RadiusdConfig     `yaml:"radiusd" json:"radiusd"`
	Portal      PortalConfig      `yaml:"portal" json:"portal"`
	Captive     CaptiveConfig     `yaml:"captive" json:"captive"`
	DHCP        DHCPConfig        `yaml:"dhcp" json:"dhcp"`
	DNSRedirect DNSRedirectConfig `yaml:"dns_redirect" json:"dns_redirect"`
	IPv6        IPv6Config        `yaml:"ipv6" json:"ipv6"`
	OpenAPI     OpenAPIConfig     `yaml:"openapi" json:"openapi"`
	Logger      LogConfig         `yaml:"logger" json:"logger"`
}

// GetLogDir returns the full path to the logs directory.
//
// This directory stores application logs when Logger.FileEnable is true.
// The directory is automatically created by initDirs() with 0750 permissions.
//
// Returns:
//   - string: Absolute path to {Workdir}/logs
func (c *AppConfig) GetLogDir() string {
	return filepath.Join(c.System.Workdir, "logs")
}

// GetPublicDir returns the full path to the public directory.
//
// This directory stores publicly accessible static files served by the web server.
// The directory is automatically created by initDirs() with 0750 permissions.
//
// Returns:
//   - string: Absolute path to {Workdir}/public
func (c *AppConfig) GetPublicDir() string {
	return filepath.Join(c.System.Workdir, "public")
}

// GetPrivateDir returns the full path to the private directory.
//
// This directory stores sensitive files such as TLS certificates and private keys
// for RadSec. The directory is created with 0700 permissions to restrict access.
//
// Returns:
//   - string: Absolute path to {Workdir}/private
func (c *AppConfig) GetPrivateDir() string {
	return filepath.Join(c.System.Workdir, "private")
}

// GetDataDir returns the full path to the data directory.
//
// This directory stores application runtime data including metrics and the
// SQLite database file (when Database.Type is "sqlite").
// The directory is automatically created by initDirs() with 0750 permissions.
//
// Returns:
//   - string: Absolute path to {Workdir}/data
func (c *AppConfig) GetDataDir() string {
	return filepath.Join(c.System.Workdir, "data")
}

// GetBackupDir returns the full path to the backup directory.
//
// This directory stores database backups and exported configuration files.
// The directory is automatically created by initDirs() with 0750 permissions.
//
// Returns:
//   - string: Absolute path to {Workdir}/backup
func (c *AppConfig) GetBackupDir() string {
	return filepath.Join(c.System.Workdir, "backup")
}

// GetRadsecCaCertPath returns the full path to the RadSec CA certificate.
//
// This CA certificate is used to verify client certificates in RadSec TLS connections.
// If the configured path is relative, it is resolved against System.Workdir.
// Absolute paths are returned unchanged.
//
// Default configuration uses "private/ca.crt" relative to Workdir.
//
// Returns:
//   - string: Absolute path to the CA certificate file
//
// See also: RFC 6614 for RadSec specification
func (c *AppConfig) GetRadsecCaCertPath() string {
	if filepath.IsAbs(c.Radiusd.RadsecCaCert) {
		return c.Radiusd.RadsecCaCert
	}
	return filepath.Join(c.System.Workdir, c.Radiusd.RadsecCaCert)
}

// GetRadsecCertPath returns the full path to the RadSec server certificate.
//
// This certificate is presented to clients during RadSec TLS handshakes.
// If the configured path is relative, it is resolved against System.Workdir.
// Absolute paths are returned unchanged.
//
// Default configuration uses "private/radsec.tls.crt" relative to Workdir.
//
// Returns:
//   - string: Absolute path to the server certificate file
//
// See also: RFC 6614 for RadSec specification
func (c *AppConfig) GetRadsecCertPath() string {
	if filepath.IsAbs(c.Radiusd.RadsecCert) {
		return c.Radiusd.RadsecCert
	}
	return filepath.Join(c.System.Workdir, c.Radiusd.RadsecCert)
}

// GetRadsecKeyPath returns the full path to the RadSec server private key.
//
// This private key corresponds to the server certificate and is used for
// TLS encryption in RadSec connections. The key file should be protected
// with restrictive file permissions (0600 recommended).
//
// If the configured path is relative, it is resolved against System.Workdir.
// Absolute paths are returned unchanged.
//
// Default configuration uses "private/radsec.tls.key" relative to Workdir.
//
// Returns:
//   - string: Absolute path to the server private key file
//
// See also: RFC 6614 for RadSec specification
func (c *AppConfig) GetRadsecKeyPath() string {
	if filepath.IsAbs(c.Radiusd.RadsecKey) {
		return c.Radiusd.RadsecKey
	}
	return filepath.Join(c.System.Workdir, c.Radiusd.RadsecKey)
}

// initDirs creates the required runtime directory structure.
//
// Called automatically by LoadConfig() to ensure all necessary directories
// exist before services start. Creates the following directory tree under
// System.Workdir:
//
//   - logs/          (0750) - Application logs
//   - public/        (0750) - Static web assets
//   - data/          (0750) - Runtime data and SQLite database
//   - data/metrics/  (0750) - Prometheus metrics storage
//   - private/       (0700) - TLS certificates and private keys
//   - backup/        (0750) - Database and config backups
//
// Errors during directory creation are intentionally ignored as the directories
// may already exist. Subsequent file operations will fail with clear errors
// if directory creation actually failed due to permission issues.
//
// Side effects:
//   - Creates directories on filesystem
//   - Applies explicit directory permissions where supported by the OS
func (c *AppConfig) initDirs() {
	_ = ensureDir(filepath.Join(c.System.Workdir, "logs"), 0750)            //nolint:errcheck
	_ = ensureDir(filepath.Join(c.System.Workdir, "public"), 0750)          //nolint:errcheck
	_ = ensureDir(filepath.Join(c.System.Workdir, "data"), 0750)            //nolint:errcheck
	_ = ensureDir(filepath.Join(c.System.Workdir, "data", "metrics"), 0750) //nolint:errcheck
	_ = ensureDir(filepath.Join(c.System.Workdir, "private"), 0700)         //nolint:errcheck
	_ = ensureDir(filepath.Join(c.System.Workdir, "backup"), 0750)          //nolint:errcheck
}

func ensureDir(dir string, mode os.FileMode) error {
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	return os.Chmod(dir, mode)
}

// setEnvValue sets a string configuration value from an environment variable.
//
// If the environment variable is empty or unset, the original value is preserved.
// This allows YAML configuration to provide defaults that can be overridden.
//
// Parameters:
//   - name: Environment variable name (e.g., "TOUGHRADIUS_WEB_HOST")
//   - val: Pointer to configuration field to update
//
// Side effects:
//   - Modifies *val if environment variable is set and non-empty
func setEnvValue(name string, val *string) {
	var evalue = os.Getenv(name)
	if evalue != "" {
		*val = evalue
	}
}

// setEnvBoolValue sets a boolean configuration value from an environment variable.
//
// Recognizes the following truthy values (case-sensitive): "true", "1", "on".
// All other non-empty values are treated as false.
//
// Parameters:
//   - name: Environment variable name (e.g., "TOUGHRADIUS_RADIUS_DEBUG")
//   - val: Pointer to configuration field to update
//
// Side effects:
//   - Modifies *val if environment variable is set and non-empty
func setEnvBoolValue(name string, val *bool) {
	var evalue = os.Getenv(name)
	if evalue != "" {
		*val = evalue == "true" || evalue == "1" || evalue == "on"
	}
}

// setEnvOptionalBoolValue sets an optional boolean configuration value from an
// environment variable.
//
// Recognizes the following truthy values (case-sensitive): "true", "1", "on".
// All other non-empty values are treated as false.
//
// Parameters:
//   - name: Environment variable name (e.g., "TOUGHRADIUS_WEB_TLS_ENABLED")
//   - val: Pointer to an optional configuration field to update
//
// Side effects:
//   - Modifies *val if environment variable is set and non-empty
func setEnvOptionalBoolValue(name string, val **bool) {
	var evalue = os.Getenv(name)
	if evalue == "" {
		return
	}

	parsed := evalue == "true" || evalue == "1" || evalue == "on"
	*val = &parsed
}

// setEnvInt64Value sets an int64 configuration value from an environment variable.
//
// Parses the environment variable as a base-10 integer. If parsing fails,
// the original value is preserved and no error is reported.
//
// Parameters:
//   - name: Environment variable name (e.g., "TOUGHRADIUS_DB_MAX_CONN")
//   - val: Pointer to configuration field to update
//
// Side effects:
//   - Modifies *val if environment variable is set and parseable as int64
func setEnvInt64Value(name string, val *int64) {
	var evalue = os.Getenv(name)
	if evalue == "" {
		return
	}

	p, err := strconv.ParseInt(evalue, 10, 64)
	if err == nil {
		*val = p
	}
}

// setEnvIntValue sets an int configuration value from an environment variable.
//
// Parses the environment variable as a base-10 integer. If parsing fails,
// the original value is preserved and no error is reported.
//
// Parameters:
//   - name: Environment variable name (e.g., "TOUGHRADIUS_WEB_PORT")
//   - val: Pointer to configuration field to update
//
// Side effects:
//   - Modifies *val if environment variable is set and parseable as int
func setEnvIntValue(name string, val *int) {
	var evalue = os.Getenv(name)
	if evalue == "" {
		return
	}

	p, err := strconv.Atoi(evalue)
	if err == nil {
		*val = p
	}
}

// DefaultAppConfig provides fallback configuration values.
//
// Used when no configuration file is found or as a base for merging
// with YAML and environment variable settings.
//
// Default configuration features:
//   - SQLite database for zero-dependency development
//   - Debug mode enabled for verbose logging
//   - Standard RADIUS ports (1812 auth, 1813 acct, 2083 radsec)
//   - Web interface on port 1816
//   - All services bound to 0.0.0.0 (all interfaces)
//
// Production deployments should override:
//   - Database.Type to "postgres" for better performance
//   - Web.Secret to a cryptographically random value
//   - System.Debug to false
//   - Logger.Mode to "production"
//   - TOUGHRADIUS_ADMIN_PASSWORD for a chosen first-start admin password
//     (otherwise a random password is written to {workdir}/private/admin-bootstrap-password)
var DefaultAppConfig = &AppConfig{
	System: SysConfig{
		Appid:    "ToughRADIUS",
		Location: "Asia/Shanghai",
		Workdir:  "/var/toughradius",
		Debug:    true,
	},
	Web: WebConfig{ //nolint:gosec // Default secret is a placeholder for development/testing only
		Host:       "0.0.0.0",
		Port:       1816,
		TlsEnabled: boolPtr(true),
		TlsPort:    1817,
		Secret:     DefaultWebSecret,
	},
	Database: DBConfig{
		Type:     "sqlite",    // Default to SQLite for development and testing
		Host:     "127.0.0.1", // PostgreSQL configuration (used when type is postgres)
		Port:     5432,
		Name:     "toughradius.db", // SQLite: database filename; PostgreSQL: database name
		User:     "postgres",
		Passwd:   "myroot",
		MaxConn:  100,
		IdleConn: 10,
		Debug:    false,
	},
	Radiusd: RadiusdConfig{
		Enabled:      true,
		Host:         "0.0.0.0",
		AuthPort:     1812,
		AcctPort:     1813,
		RadsecPort:   2083,
		RadsecWorker: 100,
		RadsecCaCert: "private/ca.crt",
		RadsecCert:   "private/radsec.tls.crt",
		RadsecKey:    "private/radsec.tls.key",
		ProxyEnabled: false,
		ProxyTimeout: 5,
		ProxyRetry:   2,

		DynAuthEnabled: false,
		DynAuthPort:    3799,
		DynAuthSecret:  "",
		Debug:          true,
	},
	Portal: PortalConfig{
		Enabled:    false,
		BindHost:   "0.0.0.0",
		BindPort:   50100,
		NASPort:    2000,
		Timeout:    5,
		Retry:      2,
		SessionTTL: 600,
	},
	Captive: CaptiveConfig{
		Enabled: false, BindAddr: ":80", LoginPath: "/portal/login", AuthorizedTTL: 86400,
	},
	DHCP:        DHCPConfig{Enabled: false, BindAddr: ":67", Lease: 3600},
	DNSRedirect: DNSRedirectConfig{Enabled: false, BindAddr: ":53", TTL: 0},
	IPv6:        IPv6Config{Enabled: false, BindAddr: ":547", Lease: 7200},
	OpenAPI: OpenAPIConfig{
		Enabled:        false,
		Credentials:    map[string]string{},
		QPS:            20,
		NonceTTL:       300,
		IdempotencyTTL: 86400,
	},
	Logger: LogConfig{
		Mode:       "development",
		FileEnable: true,
		Filename:   "/var/toughradius/toughradius.log",
	},
}

func boolPtr(v bool) *bool {
	return &v
}

// LoadConfig loads application configuration from YAML file and environment variables.
//
// Configuration loading follows this search order:
//  1. Use cfile parameter if provided and exists
//  2. Try ./toughradius.yml in current directory (development mode)
//  3. Try /etc/toughradius.yml system-wide location (production mode)
//  4. Fall back to DefaultAppConfig if no file found
//
// After loading YAML, all TOUGHRADIUS_* environment variables are applied
// as overrides. This allows containerized deployments to customize settings
// without modifying configuration files.
//
// The function creates required runtime directories (logs/, data/, etc.) and
// panics if YAML parsing fails, ensuring invalid configurations are caught
// at startup rather than during runtime.
//
// Parameters:
//   - cfile: Path to configuration file (empty string triggers search)
//
// Returns:
//   - *AppConfig: Loaded and merged configuration
//
// Panics:
//   - If YAML file exists but cannot be read or parsed
//
// Side effects:
//   - Reads from filesystem
//   - Reads environment variables
//   - Creates runtime directories via initDirs()
//
// Example:
//
//	// Load from default locations
//	cfg := LoadConfig("")
//
//	// Load from specific file
//	cfg := LoadConfig("/opt/toughradius/config.yml")
func LoadConfig(cfile string) *AppConfig {
	// In development environment, first check if custom config file exists in current directory
	if cfile == "" {
		cfile = "toughradius.yml"
	}
	if !common.FileExists(cfile) {
		cfile = "/etc/toughradius.yml"
	}
	cfg := new(AppConfig)
	if common.FileExists(cfile) {
		data := common.Must2(os.ReadFile(cfile))        //nolint:gosec // G304: config file path is intentionally variable
		common.Must(yaml.Unmarshal(data.([]byte), cfg)) //nolint:errcheck // type assertion is safe after Must2
	} else {
		cfg = DefaultAppConfig
	}

	cfg.initDirs()

	setEnvValue("TOUGHRADIUS_SYSTEM_WORKER_DIR", &cfg.System.Workdir)
	setEnvBoolValue("TOUGHRADIUS_SYSTEM_DEBUG", &cfg.System.Debug)

	// WEB
	setEnvValue("TOUGHRADIUS_WEB_HOST", &cfg.Web.Host)
	setEnvValue("TOUGHRADIUS_WEB_SECRET", &cfg.Web.Secret)
	setEnvIntValue("TOUGHRADIUS_WEB_PORT", &cfg.Web.Port)
	setEnvOptionalBoolValue("TOUGHRADIUS_WEB_TLS_ENABLED", &cfg.Web.TlsEnabled)
	if cfg.Web.TlsEnabled == nil {
		cfg.Web.TlsEnabled = boolPtr(true)
	}
	setEnvIntValue("TOUGHRADIUS_WEB_TLS_PORT", &cfg.Web.TlsPort)

	// DB
	setEnvValue("TOUGHRADIUS_DB_TYPE", &cfg.Database.Type)
	setEnvValue("TOUGHRADIUS_DB_HOST", &cfg.Database.Host)
	setEnvValue("TOUGHRADIUS_DB_NAME", &cfg.Database.Name)
	setEnvValue("TOUGHRADIUS_DB_USER", &cfg.Database.User)
	setEnvValue("TOUGHRADIUS_DB_PWD", &cfg.Database.Passwd)
	setEnvIntValue("TOUGHRADIUS_DB_PORT", &cfg.Database.Port)
	setEnvBoolValue("TOUGHRADIUS_DB_DEBUG", &cfg.Database.Debug)

	// toughradius
	setEnvValue("TOUGHRADIUS_RADIUS_HOST", &cfg.Radiusd.Host)
	setEnvIntValue("TOUGHRADIUS_RADIUS_AUTHPORT", &cfg.Radiusd.AuthPort)
	setEnvIntValue("TOUGHRADIUS_RADIUS_ACCTPORT", &cfg.Radiusd.AcctPort)
	setEnvIntValue("TOUGHRADIUS_RADIUS_RADSEC_PORT", &cfg.Radiusd.RadsecPort)
	setEnvIntValue("TOUGHRADIUS_RADIUS_RADSEC_WORKER", &cfg.Radiusd.RadsecWorker)
	setEnvValue("TOUGHRADIUS_RADIUS_RADSEC_CA_CERT", &cfg.Radiusd.RadsecCaCert)
	setEnvValue("TOUGHRADIUS_RADIUS_RADSEC_CERT", &cfg.Radiusd.RadsecCert)
	setEnvValue("TOUGHRADIUS_RADIUS_RADSEC_KEY", &cfg.Radiusd.RadsecKey)
	setEnvBoolValue("TOUGHRADIUS_RADIUS_DEBUG", &cfg.Radiusd.Debug)
	setEnvBoolValue("TOUGHRADIUS_RADIUS_ENABLED", &cfg.Radiusd.Enabled)

	// Proxy subsystem
	setEnvBoolValue("TOUGHRADIUS_RADIUS_PROXY_ENABLED", &cfg.Radiusd.ProxyEnabled)
	setEnvIntValue("TOUGHRADIUS_RADIUS_PROXY_TIMEOUT", &cfg.Radiusd.ProxyTimeout)
	setEnvIntValue("TOUGHRADIUS_RADIUS_PROXY_RETRY", &cfg.Radiusd.ProxyRetry)
	setEnvBoolValue("TOUGHRADIUS_RADIUS_DYNAUTH_ENABLED", &cfg.Radiusd.DynAuthEnabled)
	setEnvIntValue("TOUGHRADIUS_RADIUS_DYNAUTH_PORT", &cfg.Radiusd.DynAuthPort)
	setEnvValue("TOUGHRADIUS_RADIUS_DYNAUTH_SECRET", &cfg.Radiusd.DynAuthSecret)

	// Portal protocol subsystem
	setEnvBoolValue("TOUGHRADIUS_PORTAL_ENABLED", &cfg.Portal.Enabled)
	setEnvValue("TOUGHRADIUS_PORTAL_BIND_HOST", &cfg.Portal.BindHost)
	setEnvIntValue("TOUGHRADIUS_PORTAL_BIND_PORT", &cfg.Portal.BindPort)
	setEnvIntValue("TOUGHRADIUS_PORTAL_NAS_PORT", &cfg.Portal.NASPort)
	setEnvIntValue("TOUGHRADIUS_PORTAL_TIMEOUT", &cfg.Portal.Timeout)
	setEnvIntValue("TOUGHRADIUS_PORTAL_RETRY", &cfg.Portal.Retry)
	setEnvIntValue("TOUGHRADIUS_PORTAL_SESSION_TTL", &cfg.Portal.SessionTTL)

	setEnvBoolValue("TOUGHRADIUS_CAPTIVE_ENABLED", &cfg.Captive.Enabled)
	setEnvValue("TOUGHRADIUS_CAPTIVE_BIND_ADDR", &cfg.Captive.BindAddr)
	setEnvValue("TOUGHRADIUS_CAPTIVE_LOGIN_PATH", &cfg.Captive.LoginPath)
	setEnvValue("TOUGHRADIUS_CAPTIVE_PORTAL_URL", &cfg.Captive.PortalURL)
	if raw := os.Getenv("TOUGHRADIUS_CAPTIVE_WHITELIST"); raw != "" {
		cfg.Captive.WhitelistCIDR = strings.Split(raw, ",")
	}
	setEnvIntValue("TOUGHRADIUS_CAPTIVE_AUTHORIZED_TTL", &cfg.Captive.AuthorizedTTL)

	setEnvBoolValue("TOUGHRADIUS_DHCP_ENABLED", &cfg.DHCP.Enabled)
	setEnvValue("TOUGHRADIUS_DHCP_BIND_ADDR", &cfg.DHCP.BindAddr)
	setEnvValue("TOUGHRADIUS_DHCP_SERVER_IP", &cfg.DHCP.ServerIP)
	setEnvValue("TOUGHRADIUS_DHCP_NETWORK", &cfg.DHCP.Network)
	setEnvValue("TOUGHRADIUS_DHCP_GATEWAY", &cfg.DHCP.Gateway)
	setEnvValue("TOUGHRADIUS_DHCP_DNS", &cfg.DHCP.DNS)
	setEnvValue("TOUGHRADIUS_DHCP_START", &cfg.DHCP.Start)
	setEnvValue("TOUGHRADIUS_DHCP_END", &cfg.DHCP.End)
	setEnvValue("TOUGHRADIUS_DHCP_PORTAL_URL", &cfg.DHCP.PortalURL)
	setEnvIntValue("TOUGHRADIUS_DHCP_LEASE", &cfg.DHCP.Lease)

	setEnvBoolValue("TOUGHRADIUS_DNSREDIRECT_ENABLED", &cfg.DNSRedirect.Enabled)
	setEnvValue("TOUGHRADIUS_DNSREDIRECT_BIND_ADDR", &cfg.DNSRedirect.BindAddr)
	setEnvValue("TOUGHRADIUS_DNSREDIRECT_PORTAL_IP", &cfg.DNSRedirect.PortalIP)
	setEnvValue("TOUGHRADIUS_DNSREDIRECT_UPSTREAM", &cfg.DNSRedirect.Upstream)
	if raw := os.Getenv("TOUGHRADIUS_DNSREDIRECT_WHITELIST"); raw != "" {
		cfg.DNSRedirect.Whitelist = strings.Split(raw, ",")
	}
	setEnvIntValue("TOUGHRADIUS_DNSREDIRECT_TTL", &cfg.DNSRedirect.TTL)

	setEnvBoolValue("TOUGHRADIUS_IPV6_ENABLED", &cfg.IPv6.Enabled)
	setEnvValue("TOUGHRADIUS_IPV6_BIND_ADDR", &cfg.IPv6.BindAddr)
	setEnvValue("TOUGHRADIUS_IPV6_PREFIX", &cfg.IPv6.Prefix)
	setEnvValue("TOUGHRADIUS_IPV6_DNS", &cfg.IPv6.DNS)
	setEnvValue("TOUGHRADIUS_IPV6_PORTAL_URL", &cfg.IPv6.PortalURL)
	setEnvIntValue("TOUGHRADIUS_IPV6_LEASE", &cfg.IPv6.Lease)

	setEnvBoolValue("TOUGHRADIUS_OPENAPI_ENABLED", &cfg.OpenAPI.Enabled)
	if raw := os.Getenv("TOUGHRADIUS_OPENAPI_CREDENTIALS"); raw != "" {
		cfg.OpenAPI.Credentials = parseAppCredentials(raw)
	}
	setEnvIntValue("TOUGHRADIUS_OPENAPI_QPS", &cfg.OpenAPI.QPS)
	setEnvIntValue("TOUGHRADIUS_OPENAPI_NONCE_TTL", &cfg.OpenAPI.NonceTTL)
	setEnvIntValue("TOUGHRADIUS_OPENAPI_IDEMPOTENCY_TTL", &cfg.OpenAPI.IdempotencyTTL)

	setEnvValue("TOUGHRADIUS_LOGGER_MODE", &cfg.Logger.Mode)
	setEnvBoolValue("TOUGHRADIUS_LOGGER_FILE_ENABLE", &cfg.Logger.FileEnable)

	return cfg
}

// parseAppCredentials parses "key1:secret1,key2:secret2" into an AppKey map.
func parseAppCredentials(raw string) map[string]string {
	result := make(map[string]string)
	for _, entry := range strings.Split(raw, ",") {
		parts := strings.SplitN(strings.TrimSpace(entry), ":", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			result[parts[0]] = parts[1]
		}
	}
	return result
}

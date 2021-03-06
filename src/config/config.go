// Package config is used to records the service configurations
package config

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/spf13/viper"

	"github.com/skycoin/skycoin/src/visor"
	"github.com/skycoin/skycoin/src/wallet"

	"github.com/skycoin/teller/src/util/mathutil"
)

const (
	// BuyMethodDirect is used when buying directly from the local hot wallet
	BuyMethodDirect = "direct"
	// BuyMethodPassthrough is used when coins are first bought from an exchange before sending from the local hot wallet
	BuyMethodPassthrough = "passthrough"

	// CoinTypeBTC is BTC coin type
	CoinTypeBTC = "BTC"
	// CoinTypeETH is ETH coin type
	CoinTypeETH = "ETH"
	// CoinTypeSKY is SKY coin type
	CoinTypeSKY = "SKY"
)

var (
	// ErrInvalidBuyMethod is returned if BindAddress is called with an invalid buy method
	ErrInvalidBuyMethod = errors.New("Invalid buy method")

	// ErrUnsupportedCoinType unsupported coin type
	ErrUnsupportedCoinType = errors.New("unsupported coin type")

	// CoinTypes is a list of supported coin types
	CoinTypes = []string{
		CoinTypeBTC,
		CoinTypeETH,
		CoinTypeSKY,
	}
)

// ValidateCoinType returns an error if a coin type string is invalid
func ValidateCoinType(coinType string) error {
	for _, k := range CoinTypes {
		if k == coinType {
			return nil
		}
	}
	return ErrUnsupportedCoinType
}

// ValidateBuyMethod returns an error if a buy method string is invalid
func ValidateBuyMethod(m string) error {
	switch m {
	case BuyMethodDirect, BuyMethodPassthrough:
		return nil
	default:
		return ErrInvalidBuyMethod
	}
}

// Config represents the configuration root
type Config struct {
	// Enable debug logging
	Debug bool `mapstructure:"debug"`
	// Where log is saved
	LogFilename string `mapstructure:"logfile"`
	// Where database is saved, inside the ~/.teller-skycoin data directory
	DBFilename  string `mapstructure:"dbfile"`
	PidFilename string `mapstructure:"pidfile"`

	// GitCommit is set after loading using ldflags, not parsed from a config file
	GitCommit string `mapstructure:"-"`
	// StartTime is set after loading, not parsed from a config file
	StartTime time.Time `mapstructure:"-"`

	// Path of BTC addresses JSON file
	BtcAddresses string `mapstructure:"btc_addresses"`
	// Path of ETH addresses JSON file
	EthAddresses string `mapstructure:"eth_addresses"`
	// Path of SKY addresses JSON file
	SkyAddresses string `mapstructure:"sky_addresses"`

	Teller Teller `mapstructure:"teller"`

	SkyRPC SkyRPC `mapstructure:"sky_rpc"`
	BtcRPC BtcRPC `mapstructure:"btc_rpc"`
	EthRPC EthRPC `mapstructure:"eth_rpc"`

	BtcScanner   BtcScanner   `mapstructure:"btc_scanner"`
	EthScanner   EthScanner   `mapstructure:"eth_scanner"`
	SkyScanner   SkyScanner   `mapstructure:"sky_scanner"`
	SkyExchanger SkyExchanger `mapstructure:"sky_exchanger"`

	Web Web `mapstructure:"web"`

	AdminPanel AdminPanel `mapstructure:"admin_panel"`

	Dummy Dummy `mapstructure:"dummy"`
}

// Teller config for teller
type Teller struct {
	// Max number of btc addresses a skycoin address can bind
	MaxBoundAddresses int `mapstructure:"max_bound_addrs"`
	// Allow address binding
	BindEnabled bool `mapstructure:"bind_enabled"`
}

// SkyRPC config for Skycoin daemon node RPC
type SkyRPC struct {
	Address string `mapstructure:"address"`
}

// BtcRPC config for btcrpc
type BtcRPC struct {
	Server string `mapstructure:"server"`
	User   string `mapstructure:"user"`
	Pass   string `mapstructure:"pass"`
	Cert   string `mapstructure:"cert"`
}

// EthRPC config for ethrpc
type EthRPC struct {
	Server string `mapstructure:"server"`
	Port   string `mapstructure:"port"`
}

// BtcScanner config for BTC scanner
type BtcScanner struct {
	// How often to try to scan for blocks
	ScanPeriod            time.Duration `mapstructure:"scan_period"`
	InitialScanHeight     int64         `mapstructure:"initial_scan_height"`
	ConfirmationsRequired int64         `mapstructure:"confirmations_required"`
	Enabled               bool          `mapstructure:"enabled"`
}

// EthScanner config for ETH scanner
type EthScanner struct {
	// How often to try to scan for blocks
	ScanPeriod            time.Duration `mapstructure:"scan_period"`
	InitialScanHeight     int64         `mapstructure:"initial_scan_height"`
	ConfirmationsRequired int64         `mapstructure:"confirmations_required"`
	Enabled               bool          `mapstructure:"enabled"`
}

// SkyScanner config for SKY Scanner
type SkyScanner struct {
	// How often to try to scan for blocks
	ScanPeriod            time.Duration `mapstructure:"scan_period"`
	InitialScanHeight     int64         `mapstructure:"initial_scan_height"`
	ConfirmationsRequired int64         `mapstructure:"confirmations_required"`
	Enabled               bool          `mapstrucutre:"enabled"`
}

// SkyExchanger config for skycoin sender
type SkyExchanger struct {
	// SKY/BTC exchange rate. Can be an int, float or rational fraction string
	SkyBtcExchangeRate string `mapstructure:"sky_btc_exchange_rate"`
	SkyEthExchangeRate string `mapstructure:"sky_eth_exchange_rate"`
	SkySkyExchangeRate string `mapstructure:"sky_sky_exchange_rate"`
	// Number of decimal places to truncate SKY to
	MaxDecimals int `mapstructure:"max_decimals"`
	// How long to wait before rechecking transaction confirmations
	TxConfirmationCheckWait time.Duration `mapstructure:"tx_confirmation_check_wait"`
	// Path of hot Skycoin wallet file on disk
	Wallet string `mapstructure:"wallet"`
	// Allow sending of coins (deposits will still be received and recorded)
	SendEnabled bool `mapstructure:"send_enabled"`
	// Method of purchasing coins ("direct buy" or "passthrough"
	BuyMethod string `mapstructure:"buy_method"`
	// C2CX configuration
	C2CX C2CX `mapstructure:"c2cx"`
}

// C2CX config for the C2CX implementation from skycoin/exchange-api
type C2CX struct {
	Key                string          `mapstructure:"key"`
	Secret             string          `mapstructure:"secret"`
	RequestFailureWait time.Duration   `mapstructure:"request_failure_wait"`
	RatelimitWait      time.Duration   `mapstructure:"ratelimit_wait"`
	CheckOrderWait     time.Duration   `mapstructure:"check_order_wait"`
	BtcMinimumVolume   decimal.Decimal `mapstructure:"btc_minimum_volume"`
}

// Validate validates the SkyExchanger config
func (c SkyExchanger) Validate() error {
	if errs := c.validate(); len(errs) != 0 {
		return errs[0]
	}

	if errs := c.validateWallet(); len(errs) != 0 {
		return errs[0]
	}

	return nil
}

func (c SkyExchanger) validate() []error {
	var errs []error

	if _, err := mathutil.ParseRate(c.SkyBtcExchangeRate); err != nil {
		errs = append(errs, fmt.Errorf("sky_exchanger.sky_btc_exchange_rate invalid: %v", err))
	}

	if _, err := mathutil.ParseRate(c.SkyEthExchangeRate); err != nil {
		errs = append(errs, fmt.Errorf("sky_exchanger.sky_eth_exchange_rate invalid: %v", err))
	}

	if _, err := mathutil.ParseRate(c.SkySkyExchangeRate); err != nil {
		errs = append(errs, fmt.Errorf("sky_exchanger.sky_sky_exchange_rate invalid: %v", err))
	}

	if c.MaxDecimals < 0 {
		errs = append(errs, errors.New("sky_exchanger.max_decimals can't be negative"))
	}

	if uint64(c.MaxDecimals) > visor.MaxDropletPrecision {
		errs = append(errs, fmt.Errorf("sky_exchanger.max_decimals is larger than visor.MaxDropletPrecision=%d", visor.MaxDropletPrecision))
	}

	if err := ValidateBuyMethod(c.BuyMethod); err != nil {
		errs = append(errs, fmt.Errorf("sky_exchanger.buy_method must be \"%s\" or \"%s\"", BuyMethodDirect, BuyMethodPassthrough))
	}

	if c.BuyMethod == BuyMethodPassthrough {
		if c.C2CX.Key == "" {
			errs = append(errs, errors.New("c2cx.key must be set for buy_method passthrough"))
		}

		if c.C2CX.Secret == "" {
			errs = append(errs, errors.New("c2cx.secret must be set for buy_method passthrough"))
		}
	}

	return errs
}

func (c SkyExchanger) validateWallet() []error {
	var errs []error

	if c.Wallet == "" {
		errs = append(errs, errors.New("sky_exchanger.wallet missing"))
	}

	if _, err := os.Stat(c.Wallet); os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("sky_exchanger.wallet file %s does not exist", c.Wallet))
	}

	w, err := wallet.Load(c.Wallet)
	if err != nil {
		errs = append(errs, fmt.Errorf("sky_exchanger.wallet file %s failed to load: %v", c.Wallet, err))
	} else if err := w.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("sky_exchanger.wallet file %s is invalid: %v", c.Wallet, err))
	}

	return errs
}

// Web config for the teller HTTP interface
type Web struct {
	HTTPAddr         string        `mapstructure:"http_addr"`
	HTTPSAddr        string        `mapstructure:"https_addr"`
	StaticDir        string        `mapstructure:"static_dir"`
	AutoTLSHost      string        `mapstructure:"auto_tls_host"`
	TLSCert          string        `mapstructure:"tls_cert"`
	TLSKey           string        `mapstructure:"tls_key"`
	ThrottleMax      int64         `mapstructure:"throttle_max"`
	ThrottleDuration time.Duration `mapstructure:"throttle_duration"`
	BehindProxy      bool          `mapstructure:"behind_proxy"`
	CORSAllowed      []string      `mapstructure:"cors_allowed"`
}

// Validate validates Web config
func (c Web) Validate() error {
	if c.HTTPAddr == "" && c.HTTPSAddr == "" {
		return errors.New("at least one of web.http_addr, web.https_addr must be set")
	}

	if c.HTTPSAddr != "" && c.AutoTLSHost == "" && (c.TLSCert == "" || c.TLSKey == "") {
		return errors.New("when using web.https_addr, either web.auto_tls_host or both web.tls_cert and web.tls_key must be set")
	}

	if (c.TLSCert == "" && c.TLSKey != "") || (c.TLSCert != "" && c.TLSKey == "") {
		return errors.New("web.tls_cert and web.tls_key must be set or unset together")
	}

	if c.AutoTLSHost != "" && (c.TLSKey != "" || c.TLSCert != "") {
		return errors.New("either use web.auto_tls_host or both web.tls_key and web.tls_cert")
	}

	if c.HTTPSAddr == "" && (c.AutoTLSHost != "" || c.TLSKey != "" || c.TLSCert != "") {
		return errors.New("web.auto_tls_host or web.tls_key or web.tls_cert is set but web.https_addr is not enabled")
	}

	return nil
}

// AdminPanel config for the admin panel AdminPanel
type AdminPanel struct {
	Host string `mapstructure:"host"`
}

// Dummy config for the fake sender and scanner
type Dummy struct {
	Scanner  bool   `mapstructure:"scanner"`
	Sender   bool   `mapstructure:"sender"`
	HTTPAddr string `mapstructure:"http_addr"`
}

// IsScannerEnabled returns whether or not a scanner is enabled for a given coin type
func (c Config) IsScannerEnabled(coinType string) (bool, error) {
	// TODO -- adjust this after adding multicoin dummy scanner support
	// This check makes an assumption about cmd/teller/teller.go's initialization
	// of the scanners, which ignores the individial scanner.Enabled setting
	// if Dummy.Scanner is enabled
	if c.Dummy.Scanner {
		return false, nil
	}

	switch coinType {
	case CoinTypeBTC:
		return c.BtcScanner.Enabled, nil
	case CoinTypeETH:
		return c.EthScanner.Enabled, nil
	case CoinTypeSKY:
		return c.SkyScanner.Enabled, nil
	default:
		return false, ErrUnsupportedCoinType
	}
}

// Redacted returns a copy of the config with sensitive information redacted
func (c Config) Redacted() Config {
	redacted := "<redacted>"

	if c.BtcRPC.User != "" {
		c.BtcRPC.User = redacted
	}

	if c.BtcRPC.Pass != "" {
		c.BtcRPC.Pass = redacted
	}

	if c.SkyExchanger.C2CX.Key != "" {
		c.SkyExchanger.C2CX.Key = redacted
	}

	if c.SkyExchanger.C2CX.Secret != "" {
		c.SkyExchanger.C2CX.Secret = redacted
	}

	return c
}

// Validate validates the config
func (c Config) Validate() error {
	var errs []string
	oops := func(err string) {
		errs = append(errs, err)
	}

	if c.BtcAddresses == "" {
		oops("btc_addresses missing")
	}
	if _, err := os.Stat(c.BtcAddresses); os.IsNotExist(err) {
		oops("btc_addresses file does not exist")
	}
	if c.EthAddresses == "" {
		oops("eth_addresses missing")
	}
	if _, err := os.Stat(c.EthAddresses); os.IsNotExist(err) {
		oops("eth_addresses file does not exist")
	}

	if !c.Dummy.Sender {
		if c.SkyRPC.Address == "" {
			oops("sky_rpc.address missing")
		}

		// test if skycoin node rpc service is reachable
		conn, err := net.Dial("tcp", c.SkyRPC.Address)
		if err != nil {
			oops(fmt.Sprintf("sky_rpc.address connect failed: %v", err))
		} else {
			if err := conn.Close(); err != nil {
				log.Printf("Failed to close test connection to sky_rpc.address: %v", err)
			}
		}
	}

	if !c.Dummy.Scanner {
		if c.BtcScanner.Enabled {
			if c.BtcRPC.Server == "" {
				oops("btc_rpc.server missing")
			}

			if c.BtcRPC.User == "" {
				oops("btc_rpc.user missing")
			}
			if c.BtcRPC.Pass == "" {
				oops("btc_rpc.pass missing")
			}
			if c.BtcRPC.Cert == "" {
				oops("btc_rpc.cert missing")
			}

			if _, err := os.Stat(c.BtcRPC.Cert); os.IsNotExist(err) {
				oops("btc_rpc.cert file does not exist")
			}
		}
		if c.EthScanner.Enabled {
			if c.EthRPC.Server == "" {
				oops("eth_rpc.server missing")
			}
			if c.EthRPC.Port == "" {
				oops("eth_rpc.port missing")
			}
		}

		if c.SkyScanner.Enabled {
			if c.SkyRPC.Address == "" {
				oops("sky_rpc.address missing")
			}
		}
	}

	if c.BtcScanner.ConfirmationsRequired < 0 {
		oops("btc_scanner.confirmations_required must be >= 0")
	}
	if c.BtcScanner.InitialScanHeight < 0 {
		oops("btc_scanner.initial_scan_height must be >= 0")
	}
	if c.EthScanner.ConfirmationsRequired < 0 {
		oops("eth_scanner.confirmations_required must be >= 0")
	}
	if c.EthScanner.InitialScanHeight < 0 {
		oops("eth_scanner.initial_scan_height must be >= 0")
	}
	if c.SkyScanner.InitialScanHeight < 0 {
		oops("sky_scanner.initial_scan_height must be >= 0")
	}

	if c.SkyExchanger.BuyMethod == BuyMethodPassthrough {
		if c.EthScanner.Enabled {
			oops("eth_scanner must be disabled for buy_method passthrough")
		}
		if c.SkyScanner.Enabled {
			oops("sky_scanner must be disabled for buy_method passthrough")
		}
	}

	exchangeErrs := c.SkyExchanger.validate()
	for _, err := range exchangeErrs {
		oops(err.Error())
	}

	if !c.Dummy.Sender {
		exchangeErrs := c.SkyExchanger.validateWallet()
		for _, err := range exchangeErrs {
			oops(err.Error())
		}
	}

	if err := c.Web.Validate(); err != nil {
		oops(err.Error())
	}

	if len(errs) == 0 {
		return nil
	}

	return errors.New(strings.Join(errs, "\n"))
}

func setDefaults() {
	// Top-level args
	viper.SetDefault("debug", true)
	viper.SetDefault("logfile", "./teller.log")
	viper.SetDefault("dbfile", "teller.db")

	// Teller
	viper.SetDefault("teller.max_bound_btc_addrs", 5)
	viper.SetDefault("teller.bind_enabled", true)

	// SkyRPC
	viper.SetDefault("sky_rpc.address", "127.0.0.1:6430")

	// BtcRPC
	viper.SetDefault("btc_rpc.server", "127.0.0.1:8334")

	// BtcScanner
	viper.SetDefault("btc_scanner.enabled", true)
	viper.SetDefault("btc_scanner.scan_period", time.Second*20)
	viper.SetDefault("btc_scanner.initial_scan_height", int64(492478))
	viper.SetDefault("btc_scanner.confirmations_required", int64(1))

	// EthScanner
	viper.SetDefault("eth_scanner.enabled", false)
	viper.SetDefault("eth_scanner.scan_period", time.Second*5)
	viper.SetDefault("eth_scanner.initial_scan_height", int64(4654259))
	viper.SetDefault("eth_scanner.confirmations_required", int64(1))

	// SkyScanner
	viper.SetDefault("sky_scanner.enabled", false)
	viper.SetDefault("sky_scanner.scan_period", time.Second*5)
	viper.SetDefault("sky_scanner.initial_scan_height", int64(17000))
	viper.SetDefault("sky_scanner.confirmations_required", int64(0))

	// SkyExchanger
	viper.SetDefault("sky_exchanger.tx_confirmation_check_wait", time.Second*5)
	viper.SetDefault("sky_exchanger.max_decimals", 3)
	viper.SetDefault("sky_exchanger.buy_method", BuyMethodDirect)

	// C2CX
	btcMinimumVolume, err := decimal.NewFromString("0.005")
	if err != nil {
		panic(err)
	}
	viper.SetDefault("sky_exchanger.c2cx.btc_minimum_volume", btcMinimumVolume)
	viper.SetDefault("sky_exchanger.c2cx.request_failure_wait", time.Second*10)
	viper.SetDefault("sky_exchanger.c2cx.ratelimit_wait", time.Second*30)
	viper.SetDefault("sky_exchanger.c2cx.check_order_wait", time.Second*2)

	// Web
	viper.SetDefault("web.send_enabled", true)
	viper.SetDefault("web.http_addr", "127.0.0.1:7071")
	viper.SetDefault("web.static_dir", "./web/build")
	viper.SetDefault("web.throttle_max", int64(60))
	viper.SetDefault("web.throttle_duration", time.Minute)
	viper.SetDefault("web.cors_allowed", []string{})

	// AdminPanel
	viper.SetDefault("admin_panel.host", "127.0.0.1:7711")

	// DummySender
	viper.SetDefault("dummy.http_addr", "127.0.0.1:4121")
	viper.SetDefault("dummy.scanner", false)
	viper.SetDefault("dummy.sender", false)
}

// Load loads the configuration from "./$configName.*" where "*" is a
// JSON, toml or yaml file (toml preferred).
func Load(configName, appDir string) (Config, error) {
	if strings.HasSuffix(configName, ".toml") {
		configName = configName[:len(configName)-len(".toml")]
	}

	viper.SetConfigName(configName)
	viper.SetConfigType("toml")
	viper.AddConfigPath(appDir)
	viper.AddConfigPath(".")

	setDefaults()

	cfg := Config{}

	if err := viper.ReadInConfig(); err != nil {
		return cfg, err
	}

	if err := viper.Unmarshal(&cfg); err != nil {
		return cfg, err
	}

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}

	return cfg, nil
}

func init() {
	// Verify that the IsScannerEnabled switch handles all defined coin types
	var c Config
	for _, k := range CoinTypes {
		enabled, err := c.IsScannerEnabled(k)
		if err != nil {
			panic(err)
		}
		if enabled {
			panic(fmt.Sprintf("scanner for coin type %s is inexplicably enabled during empty config initialization", k))
		}
	}
}

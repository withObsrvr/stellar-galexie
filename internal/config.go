package galexie

import (
	"context"
	_ "embed"
	"fmt"
	"os"

	"github.com/stellar/go/historyarchive"
	"github.com/stellar/go/network"
	"github.com/stellar/go/support/log"
	ledgerbackend "github.com/withObsrvr/stellar-ledgerbackend"

	"github.com/pelletier/go-toml"

	"github.com/stellar/go/support/errors"
	"github.com/stellar/go/support/ordered"
	"github.com/stellar/go/support/storage"
	datastore "github.com/withObsrvr/stellar-datastore"
)

const (
	Pubnet    = "pubnet"
	Testnet   = "testnet"
	UserAgent = "galexie"
)

type Mode int

const (
	_        Mode = iota
	ScanFill Mode = iota
	Append
)

func (mode Mode) Name() string {
	switch mode {
	case ScanFill:
		return "Scan and Fill"
	case Append:
		return "Append"
	}
	return "none"
}

type RuntimeSettings struct {
	StartLedger    uint32
	EndLedger      uint32
	ConfigFilePath string
	Mode           Mode
	Ctx            context.Context
}

type StellarCoreConfig struct {
	Network               string   `toml:"network"`
	NetworkPassphrase     string   `toml:"network_passphrase"`
	HistoryArchiveUrls    []string `toml:"history_archive_urls"`
	StellarCoreBinaryPath string   `toml:"stellar_core_binary_path"`
	CaptiveCoreTomlPath   string   `toml:"captive_core_toml_path"`
	CheckpointFrequency   uint32   `toml:"checkpoint_frequency"`
	StoragePath           string   `toml:"storage_path"`
}

type DataStoreConfig struct {
	Type   string            `toml:"type"`
	Params map[string]string `toml:"params"`
	Schema DataStoreSchema   `toml:"schema"`
}

type DataStoreSchema struct {
	LedgersPerFile    uint32 `toml:"ledgers_per_file"`
	FilesPerPartition uint32 `toml:"files_per_partition"`
}

// Validate checks if the datastore configuration is valid
func (c *DataStoreConfig) Validate() error {
	if c.Type == "" {
		return errors.New("datastore type is required")
	}

	switch c.Type {
	case "GCS":
		if _, ok := c.Params["destination_bucket_path"]; !ok {
			return errors.New("destination_bucket_path is required for GCS")
		}
	case "S3":
		if _, ok := c.Params["bucket_name"]; !ok {
			return errors.New("bucket_name is required for S3")
		}
	case "FS":
		if _, ok := c.Params["base_path"]; !ok {
			return errors.New("base_path is required for FS")
		}
	default:
		return fmt.Errorf("unsupported datastore type: %s", c.Type)
	}

	if c.Schema.LedgersPerFile == 0 {
		return errors.New("ledgers_per_file must be greater than 0")
	}

	if c.Schema.FilesPerPartition == 0 {
		return errors.New("files_per_partition must be greater than 0")
	}

	return nil
}

// GetDataStore creates a new datastore instance based on the configuration
func (c *DataStoreConfig) GetDataStore(ctx context.Context) (datastore.DataStore, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}

	return datastore.NewDataStore(ctx, datastore.DataStoreConfig{
		Type:   c.Type,
		Params: c.Params,
		Schema: datastore.DataStoreSchema{
			LedgersPerFile:    c.Schema.LedgersPerFile,
			FilesPerPartition: c.Schema.FilesPerPartition,
		},
	})
}

type Config struct {
	AdminPort int `toml:"admin_port"`

	DataStoreConfig   datastore.DataStoreConfig `toml:"datastore_config"`
	StellarCoreConfig StellarCoreConfig         `toml:"stellar_core_config"`
	UserAgent         string                    `toml:"user_agent"`

	StartLedger uint32
	EndLedger   uint32
	Mode        Mode

	CoreVersion               string
	SerializedCaptiveCoreToml []byte
	CoreBuildVersionFn        ledgerbackend.CoreBuildVersionFunc
}

// This will generate the config based on settings
//
// settings              - requested settings
//
// return                - *Config or an error if any range validation failed.
func NewConfig(settings RuntimeSettings, getCoreVersionFn ledgerbackend.CoreBuildVersionFunc) (*Config, error) {
	config := &Config{}

	config.StartLedger = uint32(settings.StartLedger)
	config.EndLedger = uint32(settings.EndLedger)
	config.Mode = settings.Mode
	config.CoreBuildVersionFn = ledgerbackend.CoreBuildVersion
	if getCoreVersionFn != nil {
		config.CoreBuildVersionFn = getCoreVersionFn
	}

	logger.Infof("Requested export mode of %v with start=%d, end=%d", settings.Mode.Name(), config.StartLedger, config.EndLedger)

	var err error
	if err = config.processToml(settings.ConfigFilePath); err != nil {
		return nil, err
	}
	logger.Infof("Network Config Archive URLs: %v", config.StellarCoreConfig.HistoryArchiveUrls)
	logger.Infof("Network Config Archive Passphrase: %v", config.StellarCoreConfig.NetworkPassphrase)
	logger.Infof("Network Config Stellar Core Binary Path: %v", config.StellarCoreConfig.StellarCoreBinaryPath)
	logger.Infof("Network Config Stellar Core Toml Config: %v", string(config.SerializedCaptiveCoreToml))

	return config, nil
}

func (config *Config) Resumable() bool {
	return config.Mode == Append
}

// Validates requested ledger range, and will automatically adjust it
// to be ledgers-per-file boundary aligned
func (config *Config) ValidateAndSetLedgerRange(ctx context.Context, archive historyarchive.ArchiveInterface) error {

	if config.StartLedger < 2 {
		return errors.New("invalid start value, must be greater than one.")
	}

	if config.Mode == ScanFill && config.EndLedger == 0 {
		return errors.New("invalid end value, unbounded mode not supported, end must be greater than start.")
	}

	if config.EndLedger != 0 && config.EndLedger <= config.StartLedger {
		return errors.New("invalid end value, must be greater than start")
	}

	latestNetworkLedger, err := archive.GetLatestLedgerSequence()
	latestNetworkLedger = latestNetworkLedger + (archive.GetCheckpointManager().GetCheckpointFrequency() * 2)

	if err != nil {
		return errors.Wrap(err, "Failed to retrieve the latest ledger sequence from history archives.")
	}
	logger.Infof("Latest ledger sequence was detected as %d", latestNetworkLedger)

	if config.StartLedger > latestNetworkLedger {
		return errors.Errorf("start %d exceeds latest network ledger %d",
			config.StartLedger, latestNetworkLedger)
	}

	if config.EndLedger > latestNetworkLedger {
		return errors.Errorf("end %d exceeds latest network ledger %d",
			config.EndLedger, latestNetworkLedger)
	}

	config.adjustLedgerRange()
	return nil
}

func (config *Config) GenerateHistoryArchive(ctx context.Context, entry *log.Entry) (historyarchive.ArchiveInterface, error) {
	return historyarchive.NewArchivePool(config.StellarCoreConfig.HistoryArchiveUrls, historyarchive.ArchiveOptions{
		ConnectOptions: storage.ConnectOptions{
			UserAgent: config.UserAgent,
			Context:   ctx,
		},
		Logger: logger,
	})
}

// coreBinDefaultPath - a default value to use for core binary path on system.
//
//	this will be used if StellarCoreConfig.StellarCoreBinaryPath is not specified
func (config *Config) GenerateCaptiveCoreConfig(coreBinFromPath string) (ledgerbackend.CaptiveCoreConfig, error) {
	var err error

	if config.StellarCoreConfig.StellarCoreBinaryPath == "" && coreBinFromPath == "" {
		return ledgerbackend.CaptiveCoreConfig{}, errors.New("Invalid captive core config, no stellar-core binary path was provided.")
	}

	if config.StellarCoreConfig.StellarCoreBinaryPath == "" {
		config.StellarCoreConfig.StellarCoreBinaryPath = coreBinFromPath
	}

	if err = config.setCoreVersionInfo(); err != nil {
		return ledgerbackend.CaptiveCoreConfig{}, fmt.Errorf("failed to set stellar-core version info: %w", err)
	}

	params := ledgerbackend.CaptiveCoreTomlParams{
		NetworkPassphrase:  config.StellarCoreConfig.NetworkPassphrase,
		HistoryArchiveURLs: config.StellarCoreConfig.HistoryArchiveUrls,
		UseDB:              true,
	}

	captiveCoreToml, err := ledgerbackend.NewCaptiveCoreTomlFromData(config.SerializedCaptiveCoreToml, params)
	if err != nil {
		return ledgerbackend.CaptiveCoreConfig{}, errors.Wrap(err, "Failed to create captive-core toml")
	}

	checkpointFrequency := historyarchive.DefaultCheckpointFrequency
	if config.StellarCoreConfig.CheckpointFrequency > 0 {
		checkpointFrequency = config.StellarCoreConfig.CheckpointFrequency
	}
	return ledgerbackend.CaptiveCoreConfig{
		BinaryPath:          config.StellarCoreConfig.StellarCoreBinaryPath,
		NetworkPassphrase:   params.NetworkPassphrase,
		HistoryArchiveURLs:  params.HistoryArchiveURLs,
		CheckpointFrequency: checkpointFrequency,
		Log:                 logger.WithField("subservice", "stellar-core"),
		Toml:                captiveCoreToml,
		UserAgent:           config.UserAgent,
		UseDB:               true,
		StoragePath:         config.StellarCoreConfig.StoragePath,
	}, nil
}

func (c *Config) setCoreVersionInfo() (err error) {
	c.CoreVersion, err = c.CoreBuildVersionFn(c.StellarCoreConfig.StellarCoreBinaryPath)
	if err != nil {
		return fmt.Errorf("failed to set stellar-core version: %w", err)
	}
	logger.Infof("stellar-core version: %s", c.CoreVersion)
	return nil
}

func (config *Config) processToml(tomlPath string) error {
	// Load config TOML file
	cfg, err := toml.LoadFile(tomlPath)
	if err != nil {
		return errors.Wrapf(err, "config file %v was not found", tomlPath)
	}

	// Unmarshal TOML data into the Config struct
	if err = cfg.Unmarshal(config); err != nil {
		return errors.Wrap(err, "Error unmarshalling TOML config.")
	}

	if config.UserAgent == "" {
		config.UserAgent = UserAgent
	}

	if config.StellarCoreConfig.Network == "" && (len(config.StellarCoreConfig.HistoryArchiveUrls) == 0 || config.StellarCoreConfig.NetworkPassphrase == "" || config.StellarCoreConfig.CaptiveCoreTomlPath == "") {
		return errors.New("Invalid captive core config, the 'network' parameter must be set to pubnet or testnet or " +
			"'stellar_core_config.history_archive_urls' and 'stellar_core_config.network_passphrase' and 'stellar_core_config.captive_core_toml_path' must be set.")
	}

	// network config values are an overlay, with network preconfigured values being first if network is present
	// and then toml settings specific for passphrase, archiveurls, core toml file can override lastly.
	var networkPassPhrase string
	var networkArchiveUrls []string
	switch config.StellarCoreConfig.Network {
	case "":

	case Pubnet:
		networkPassPhrase = network.PublicNetworkPassphrase
		networkArchiveUrls = network.PublicNetworkhistoryArchiveURLs
		config.SerializedCaptiveCoreToml = ledgerbackend.PubnetDefaultConfig

	case Testnet:
		networkPassPhrase = network.TestNetworkPassphrase
		networkArchiveUrls = network.TestNetworkhistoryArchiveURLs
		config.SerializedCaptiveCoreToml = ledgerbackend.TestnetDefaultConfig

	default:
		return errors.New("invalid captive core config, " +
			"preconfigured_network must be set to 'pubnet' or 'testnet' or network_passphrase, history_archive_urls," +
			" and captive_core_toml_path must be set")
	}

	if config.StellarCoreConfig.NetworkPassphrase == "" {
		config.StellarCoreConfig.NetworkPassphrase = networkPassPhrase
	}

	if len(config.StellarCoreConfig.HistoryArchiveUrls) < 1 {
		config.StellarCoreConfig.HistoryArchiveUrls = networkArchiveUrls
	}

	if config.StellarCoreConfig.CaptiveCoreTomlPath != "" {
		if config.SerializedCaptiveCoreToml, err = os.ReadFile(config.StellarCoreConfig.CaptiveCoreTomlPath); err != nil {
			return errors.Wrap(err, "Failed to load captive-core-toml-path file")
		}
	}

	return nil
}

func (config *Config) adjustLedgerRange() {
	// Check if either the start or end ledger does not fall on the "LedgersPerFile" boundary
	// and adjust the start and end ledger accordingly.
	// Align the start ledger to the nearest "LedgersPerFile" boundary.
	config.StartLedger = config.DataStoreConfig.Schema.GetSequenceNumberStartBoundary(config.StartLedger)

	// Ensure that the adjusted start ledger is at least 2.
	config.StartLedger = ordered.Max(2, config.StartLedger)

	// Align the end ledger (for bounded cases) to the nearest "LedgersPerFile" boundary.
	if config.EndLedger != 0 {
		config.EndLedger = config.DataStoreConfig.Schema.GetSequenceNumberEndBoundary(config.EndLedger)
	}

	logger.Infof("Computed effective export boundary ledger range: start=%d, end=%d", config.StartLedger, config.EndLedger)
}

// ToParams converts the config to a map of parameters
func (c *DataStoreConfig) ToParams() map[string]string {
	return c.Params
}

package run

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/freetsdb/freetsdb/cluster"
	"github.com/freetsdb/freetsdb/monitor"
	"github.com/freetsdb/freetsdb/services/collectd"
	"github.com/freetsdb/freetsdb/services/continuous_querier"
	"github.com/freetsdb/freetsdb/services/graphite"
	"github.com/freetsdb/freetsdb/services/hh"
	"github.com/freetsdb/freetsdb/services/httpd"
	"github.com/freetsdb/freetsdb/services/meta"
	"github.com/freetsdb/freetsdb/services/opentsdb"
	"github.com/freetsdb/freetsdb/services/precreator"
	"github.com/freetsdb/freetsdb/services/retention"
	"github.com/freetsdb/freetsdb/services/subscriber"
	"github.com/freetsdb/freetsdb/services/udp"
	"github.com/freetsdb/freetsdb/tsdb"
)

const (
	// DefaultBindAddress is the default address for raft, cluster, snapshot, etc..
	DefaultBindAddress = ":8088"

	// DefaultHostname is the default hostname used if we are unable to determine
	// the hostname from the system
	DefaultHostname = "localhost"
)

// Config represents the configuration format for the freetsd binary.
type Config struct {
	Meta       *meta.Config      `toml:"meta"`
	Data       tsdb.Config       `toml:"data"`
	Cluster    cluster.Config    `toml:"cluster"`
	Retention  retention.Config  `toml:"retention"`
	Precreator precreator.Config `toml:"shard-precreation"`

	Monitor    monitor.Config    `toml:"monitor"`
	Subscriber subscriber.Config `toml:"subscriber"`
	HTTPD      httpd.Config      `toml:"http"`
	Graphites  []graphite.Config `toml:"graphite"`
	Collectd   collectd.Config   `toml:"collectd"`
	OpenTSDB   opentsdb.Config   `toml:"opentsdb"`
	UDPs       []udp.Config      `toml:"udp"`

	ContinuousQuery continuous_querier.Config `toml:"continuous_queries"`
	HintedHandoff   hh.Config                 `toml:"hinted-handoff"`

	// Server reporting
	ReportingDisabled bool `toml:"reporting-disabled"`

	// BindAddress is the address that all TCP services use (Raft, Snapshot, Cluster, etc.)
	BindAddress string `toml:"bind-address"`

	// Hostname is the hostname portion to use when registering local
	// addresses.  This hostname must be resolvable from other nodes.
	Hostname string `toml:"hostname"`

	Join string `toml:"join"`
}

// NewConfig returns an instance of Config with reasonable defaults.
func NewConfig() *Config {
	c := &Config{}
	c.Meta = meta.NewConfig()
	c.Data = tsdb.NewConfig()
	c.Cluster = cluster.NewConfig()
	c.Precreator = precreator.NewConfig()

	c.Monitor = monitor.NewConfig()
	c.Subscriber = subscriber.NewConfig()
	c.HTTPD = httpd.NewConfig()
	c.Collectd = collectd.NewConfig()
	c.OpenTSDB = opentsdb.NewConfig()

	c.ContinuousQuery = continuous_querier.NewConfig()
	c.Retention = retention.NewConfig()
	c.HintedHandoff = hh.NewConfig()
	c.BindAddress = DefaultBindAddress

	// All ARRAY attributes have to be init after toml decode
	// See: https://github.com/BurntSushi/toml/pull/68
	// Those attributes will be initialized in Config.InitTableAttrs method
	// Concerned Attributes:
	//  * `c.Graphites`
	//  * `c.UDPs`

	return c
}

// InitTableAttrs initialises all ARRAY attributes if empty
func (c *Config) InitTableAttrs() {
	if len(c.UDPs) == 0 {
		c.UDPs = []udp.Config{udp.NewConfig()}
	}
	if len(c.Graphites) == 0 {
		c.Graphites = []graphite.Config{graphite.NewConfig()}
	}
}

// NewDemoConfig returns the config that runs when no config is specified.
func NewDemoConfig() (*Config, error) {
	c := NewConfig()
	c.InitTableAttrs()

	var homeDir string
	// By default, store meta and data files in current users home directory
	u, err := user.Current()
	if err == nil {
		homeDir = u.HomeDir
	} else if os.Getenv("HOME") != "" {
		homeDir = os.Getenv("HOME")
	} else {
		return nil, fmt.Errorf("failed to determine current user for storage")
	}

	c.Meta.Dir = filepath.Join(homeDir, ".freetsdb/meta")
	c.Data.Dir = filepath.Join(homeDir, ".freetsdb/data")
	c.HintedHandoff.Dir = filepath.Join(homeDir, ".freetsdb/hh")
	c.Data.WALDir = filepath.Join(homeDir, ".freetsdb/wal")

	c.HintedHandoff.Enabled = true

	return c, nil
}

// Validate returns an error if the config is invalid.
func (c *Config) Validate() error {
	if !c.Meta.Enabled && !c.Data.Enabled {
		return errors.New("either Meta, Data, or both must be enabled")
	}

	if c.Meta.Enabled {
		if err := c.Meta.Validate(); err != nil {
			return err
		}

		// If the config is for a meta-only node, we can't store monitor stats
		// locally.
		if c.Monitor.StoreEnabled && !c.Data.Enabled {
			return fmt.Errorf("monitor storage can not be enabled on meta only nodes")
		}
	}

	if c.Data.Enabled {
		if err := c.Data.Validate(); err != nil {
			return err
		}

		if err := c.HintedHandoff.Validate(); err != nil {
			return err
		}
		for _, g := range c.Graphites {
			if err := g.Validate(); err != nil {
				return fmt.Errorf("invalid graphite config: %v", err)
			}
		}
	}

	return nil
}

// ApplyEnvOverrides apply the environment configuration on top of the config.
func (c *Config) ApplyEnvOverrides() error {
	return c.applyEnvOverrides("INFLUXDB", reflect.ValueOf(c))
}

func (c *Config) applyEnvOverrides(prefix string, spec reflect.Value) error {
	// If we have a pointer, dereference it
	s := spec
	if spec.Kind() == reflect.Ptr {
		s = spec.Elem()
	}

	// Make sure we have struct
	if s.Kind() != reflect.Struct {
		return nil
	}

	typeOfSpec := s.Type()
	for i := 0; i < s.NumField(); i++ {
		f := s.Field(i)
		// Get the toml tag to determine what env var name to use
		configName := typeOfSpec.Field(i).Tag.Get("toml")
		// Replace hyphens with underscores to avoid issues with shells
		configName = strings.Replace(configName, "-", "_", -1)
		fieldKey := typeOfSpec.Field(i).Name

		// Skip any fields that we cannot set
		if f.CanSet() || f.Kind() == reflect.Slice {

			// Use the upper-case prefix and toml name for the env var
			key := strings.ToUpper(configName)
			if prefix != "" {
				key = strings.ToUpper(fmt.Sprintf("%s_%s", prefix, configName))
			}
			value := os.Getenv(key)

			// If the type is s slice, apply to each using the index as a suffix
			// e.g. GRAPHITE_0
			if f.Kind() == reflect.Slice || f.Kind() == reflect.Array {
				for i := 0; i < f.Len(); i++ {
					if err := c.applyEnvOverrides(fmt.Sprintf("%s_%d", key, i), f.Index(i)); err != nil {
						return err
					}
				}
				continue
			}

			// If it's a sub-config, recursively apply
			if f.Kind() == reflect.Struct || f.Kind() == reflect.Ptr {
				if err := c.applyEnvOverrides(key, f); err != nil {
					return err
				}
				continue
			}

			// Skip any fields we don't have a value to set
			if value == "" {
				continue
			}

			switch f.Kind() {
			case reflect.String:
				f.SetString(value)
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:

				var intValue int64

				// Handle toml.Duration
				if f.Type().Name() == "Duration" {
					dur, err := time.ParseDuration(value)
					if err != nil {
						return fmt.Errorf("failed to apply %v to %v using type %v and value '%v'", key, fieldKey, f.Type().String(), value)
					}
					intValue = dur.Nanoseconds()
				} else {
					var err error
					intValue, err = strconv.ParseInt(value, 0, f.Type().Bits())
					if err != nil {
						return fmt.Errorf("failed to apply %v to %v using type %v and value '%v'", key, fieldKey, f.Type().String(), value)
					}
				}

				f.SetInt(intValue)
			case reflect.Bool:
				boolValue, err := strconv.ParseBool(value)
				if err != nil {
					return fmt.Errorf("failed to apply %v to %v using type %v and value '%v'", key, fieldKey, f.Type().String(), value)

				}
				f.SetBool(boolValue)
			case reflect.Float32, reflect.Float64:
				floatValue, err := strconv.ParseFloat(value, f.Type().Bits())
				if err != nil {
					return fmt.Errorf("failed to apply %v to %v using type %v and value '%v'", key, fieldKey, f.Type().String(), value)

				}
				f.SetFloat(floatValue)
			default:
				if err := c.applyEnvOverrides(key, f); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

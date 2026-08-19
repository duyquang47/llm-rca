// Package config parses the YAML that drives a service-chain binary
// instance. One binary, one config file per deployment — see CLAUDE.md §4.1.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Cache struct {
	Addr string        `yaml:"addr"`
	Key  string        `yaml:"key"`
	TTL  time.Duration `yaml:"ttl"`
}

type DB struct {
	DSN      string `yaml:"dsn"`
	Query    string `yaml:"query"`
	PoolSize int    `yaml:"pool_size"`
	NPlusOne bool   `yaml:"n_plus_one"`
}

type Call struct {
	Name    string        `yaml:"name"`
	URL     string        `yaml:"url"`
	Timeout time.Duration `yaml:"timeout"`
	Retry   int           `yaml:"retry"`
}

type Publish struct {
	Addr   string `yaml:"addr"`
	Stream string `yaml:"stream"`
}

type Route struct {
	Path    string   `yaml:"path"`
	Cache   *Cache   `yaml:"cache"`
	DB      *DB      `yaml:"db"`
	Calls   []Call   `yaml:"calls"`
	Publish *Publish `yaml:"publish"`
}

// Consume configures notify-worker's background stream-consumer loop —
// the one deployment in the chain that reacts to a Redis Stream instead
// of an inbound HTTP route (see CLAUDE.md §2.1 dependency diagram; the
// route-based Config/Handler in §4.1/§4.2 doesn't cover this shape).
type Consume struct {
	Addr     string `yaml:"addr"`
	Stream   string `yaml:"stream"`
	Group    string `yaml:"group"`
	Consumer string `yaml:"consumer"`
}

type Config struct {
	ServiceName string   `yaml:"service_name"`
	Listen      string   `yaml:"listen"`
	Routes      []Route  `yaml:"routes"`
	Consume     *Consume `yaml:"consume"`
}

func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.ServiceName == "" {
		return Config{}, fmt.Errorf("config %s: service_name is required", path)
	}
	if cfg.Listen == "" {
		return Config{}, fmt.Errorf("config %s: listen is required", path)
	}
	return cfg, nil
}

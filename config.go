package main

import (
	"errors"
	"time"

	"gopkg.in/yaml.v3"
)

const pluginID = "quota-reset-router"
const pluginVersion = "0.3.0"

type config struct {
	Mode            string        `yaml:"mode" json:"mode"`
	SelectionPolicy string        `yaml:"selection_policy" json:"selection_policy"`
	PollInterval    time.Duration `yaml:"poll_interval" json:"-"`
	MaxAge          time.Duration `yaml:"max_age" json:"-"`
	RequestTimeout  time.Duration `yaml:"request_timeout" json:"-"`
}

func decodeConfig(raw []byte) (config, error) {
	cfg := config{Mode: "shadow", SelectionPolicy: "quota_balanced", PollInterval: 5 * time.Minute, MaxAge: 10 * time.Minute, RequestTimeout: 10 * time.Second}
	if len(raw) != 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return config{}, errors.New("invalid plugin configuration")
		}
	}
	if cfg.Mode != "shadow" && cfg.Mode != "active" {
		return config{}, errors.New("mode must be shadow or active")
	}
	if cfg.SelectionPolicy != "quota_balanced" && cfg.SelectionPolicy != "weekly_reset_first" {
		return config{}, errors.New("selection_policy must be quota_balanced or weekly_reset_first")
	}
	if cfg.PollInterval < time.Minute || cfg.PollInterval > time.Hour {
		return config{}, errors.New("poll_interval must be between 1m and 1h")
	}
	if cfg.MaxAge < cfg.PollInterval || cfg.MaxAge > time.Hour {
		return config{}, errors.New("max_age must be between poll_interval and 1h")
	}
	if cfg.RequestTimeout < time.Second || cfg.RequestTimeout > 30*time.Second {
		return config{}, errors.New("request_timeout must be between 1s and 30s")
	}
	return cfg, nil
}

package connector

import (
	_ "embed"
	"time"

	up "go.mau.fi/util/configupgrade"
)

//go:embed example-config.yaml
var ExampleConfig string

type Config struct {
	EduPageURL   string        `yaml:"edupage_url"`
	PollInterval time.Duration `yaml:"poll_interval"`
	RecentDays   int           `yaml:"recent_days"`
	InitialDays  int           `yaml:"initial_days"`
	SettleDays   int           `yaml:"settle_days"`
	Arrivals     bool          `yaml:"arrivals"`
	Meals        bool          `yaml:"meals"`
	MaxFileSize  int64         `yaml:"max_file_size"`
}

func upgradeConfig(helper up.Helper) {
	helper.Copy(up.Str, "edupage_url")
	helper.Copy(up.Str|up.Int, "poll_interval")
	helper.Copy(up.Int, "recent_days")
	helper.Copy(up.Int, "initial_days")
	helper.Copy(up.Int, "settle_days")
	helper.Copy(up.Bool, "arrivals")
	helper.Copy(up.Bool, "meals")
	helper.Copy(up.Int, "max_file_size")
}

func (ec *EduPageConnector) GetConfig() (string, any, up.Upgrader) {
	return ExampleConfig, &ec.Config, &up.StructUpgrader{
		SimpleUpgrader: up.SimpleUpgrader(upgradeConfig),
		Base:           ExampleConfig,
	}
}

// Mirrors the goserving smartcacheserving/config/config.go shape:
// a layered Configuration struct, a yaml-tagged OriginConfig with
// a Renderer field, and a trivial getter returning the
// cmserving block. The YAML literal `/scrr.php` lives in
// setup/production.yaml.
package config

type Configuration struct {
	Origins OriginConfigurations `yaml:"origins"`
}

type OriginConfigurations struct {
	CmServing OriginConfig `yaml:"cmserving"`
}

type OriginConfig struct {
	Protocol string `yaml:"protocol"`
	Host     string `yaml:"host"`
	Port     string `yaml:"port"`
	Path     string `yaml:"path"`
	Renderer string `yaml:"renderer"`
}

var selected Configuration

func GetOriginConfig() OriginConfig {
	return selected.Origins.CmServing
}

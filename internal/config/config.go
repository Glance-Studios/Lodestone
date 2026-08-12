package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	Addr    string // iface address to listen on
	Port    int    // TCP port
	DataDir string // dir for the ledger and cached data
	Token   string // bearer token for protected endpoints; empty = fail closed

	// Deploy target. Deploying is disabled unless all of these are set, so the
	// agent is useful as an upload+ledger service before a cluster exists.
	BaseImage  string // image to append artifacts onto
	Repo       string // registry path to push built images to
	DestPath   string // where the artifact lands inside the image
	Namespace  string // Kubernetes namespace of the target Deployment
	Deployment string // name of the target Deployment
	Container  string // container within the Deployment to update
	Kubeconfig string // explicit kubeconfig path; empty = in-cluster or default rules

	// Health gate. Empty means the rollout succeeds once Kubernetes reports it
	// settled, with no application-level check.
	HealthURL  string // HTTP GET must return 2xx
	HealthAddr string // TCP connect must succeed ("host:port")
}

// DeployEnabled reports whether enough is configured to package and deploy.
func (c Config) DeployEnabled() bool {
	return c.BaseImage != "" && c.Repo != "" && c.Namespace != "" &&
		c.Deployment != "" && c.Container != ""
}

const (
	defaultAddr     = "0.0.0.0"
	defaultPort     = 8080
	defaultDataDir  = "/var/lib/lodestone"
	defaultDestPath = "/plugins/app.jar"
)

// Load builds a Config from the environment
func Load() (Config, error) {
	cfg := Config{
		Addr:    envOr("LODESTONE_ADDR", defaultAddr),
		Port:    defaultPort,
		DataDir: envOr("LODESTONE_DATA_DIR", defaultDataDir),
		Token:   os.Getenv("LODESTONE_TOKEN"),

		BaseImage:  os.Getenv("LODESTONE_BASE_IMAGE"),
		Repo:       os.Getenv("LODESTONE_REPO"),
		DestPath:   envOr("LODESTONE_DEST_PATH", defaultDestPath),
		Namespace:  os.Getenv("LODESTONE_NAMESPACE"),
		Deployment: os.Getenv("LODESTONE_DEPLOYMENT"),
		Container:  os.Getenv("LODESTONE_CONTAINER"),
		Kubeconfig: os.Getenv("LODESTONE_KUBECONFIG"),
		HealthURL:  os.Getenv("LODESTONE_HEALTH_URL"),
		HealthAddr: os.Getenv("LODESTONE_HEALTH_ADDR"),
	}

	if s := os.Getenv("LODESTONE_PORT"); s != "" {
		port, err := strconv.Atoi(s)
		if err != nil {
			return Config{}, fmt.Errorf("LODESTONE_PORT %q: %w", s, err)
		}
		cfg.Port = port
	}

	if cfg.Port < 1 || cfg.Port > 65535 {
		return Config{}, fmt.Errorf("port %d out of range 1-65535", cfg.Port)
	}

	return cfg, nil
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

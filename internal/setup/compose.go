package setup

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/jt-helsinki/ideal-robot/internal/output"
	"github.com/jt-helsinki/ideal-robot/internal/paths"
)

// This file renders a docker-compose.yaml for the service tier as a DEBUG ARTIFACT. It
// is NOT the launcher — `ai setup`'s per-container reconcile (ensure*) remains the source
// of truth and the thing that actually runs the stack (it keeps the surgical idempotent
// reconcile + docker/podman parity + the keys-never-on-disk env passthrough). This file
// mirrors that topology into a single declarative artifact so a developer can bring the
// SAME stack up under compose's tooling — `docker compose -f ~/.ai-platform/
// docker-compose.yaml up -d`, then `logs -f <svc>` / `ps` / `restart <svc>` — for easier
// debugging. It is generated from the SAME consts/helpers the reconcile uses (container
// names, `containerImage`, `platformNetwork`, the volume/config paths, `ollamaEnvPairs`),
// so it stays in sync. Secrets stay OFF disk: UI_PASSWORD / LITELLM_MASTER_KEY /
// LITELLM_SALT_KEY are emitted in compose's PASSTHROUGH form (bare NAME, no value), so
// `docker compose up` reads them from the environment (e.g. ~/.ai-platform/.ai-platform.env)
// exactly as the reconcile's `-e NAME` passthrough does.
//
// Because the containers use fixed container_names + a name-pinned aip-net, bringing this
// up requires the platform's own services to be DOWN first (`ai services stop`) to avoid
// name/port/network clashes — it is an alternative launcher for debugging, not concurrent.

// composeFile is the minimal docker-compose schema this renderer emits.
type composeFile struct {
	Services map[string]*composeService `yaml:"services"`
	Networks map[string]composeNetwork  `yaml:"networks"`
}

type composeService struct {
	Image         string   `yaml:"image"`
	ContainerName string   `yaml:"container_name"`
	Networks      []string `yaml:"networks,omitempty"`
	Ports         []string `yaml:"ports,omitempty"`
	Volumes       []string `yaml:"volumes,omitempty"`
	Environment   []string `yaml:"environment,omitempty"`
	Command       []string `yaml:"command,omitempty"`
	DependsOn     []string `yaml:"depends_on,omitempty"`
	Restart       string   `yaml:"restart,omitempty"`
}

type composeNetwork struct {
	Name string `yaml:"name"`
}

// ComposeFilePath is where `ai services compose` writes the artifact.
func ComposeFilePath() (string, error) {
	root, err := paths.PlatformDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "docker-compose.yaml"), nil
}

// WriteComposeFile renders the service-tier compose (role-resolved bind) and writes it to
// ComposeFilePath, returning that path. Best-effort artifact: it overwrites in place.
func WriteComposeFile() (string, error) {
	path, err := ComposeFilePath()
	if err != nil {
		return "", err
	}
	content, err := ServicesComposeYAML("")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", output.Errorf(output.ExitRuntimeFailure, "create platform dir: %s", err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return "", output.Errorf(output.ExitRuntimeFailure, "write %s: %s", path, err)
	}
	return path, nil
}

// ServicesComposeYAML renders the service-tier docker-compose.yaml. bindHost governs the
// nginx gateway publish (127.0.0.1 standalone / 0.0.0.0 server), matching ensureProxy.
func ServicesComposeYAML(bindHost string) ([]byte, error) {
	volumesDir, err := paths.VolumesDir()
	if err != nil {
		return nil, err
	}
	configDir, err := paths.ConfigDir()
	if err != nil {
		return nil, err
	}
	if bindHost == "" {
		bindHost = currentBindHost() // role-based: 0.0.0.0 for server, else 127.0.0.1
	}
	const net = platformNetwork
	litellmConfig := filepath.Join(configDir, "litellm", "config.yaml")
	corefile := filepath.Join(configDir, "dns", "Corefile")
	nginxConf := filepath.Join(configDir, "proxy", "nginx.conf")

	file := composeFile{
		Networks: map[string]composeNetwork{net: {Name: net}},
		Services: map[string]*composeService{
			dnsContainer: {
				Image: containerImage("dns"), ContainerName: dnsContainer, Networks: []string{net}, Restart: "unless-stopped",
				Ports:   []string{"127.0.0.1:" + dnsHostPort + ":53/udp", "127.0.0.1:" + dnsHostPort + ":53/tcp"},
				Volumes: []string{corefile + ":/Corefile"},
				Command: []string{"-conf", "/Corefile"},
			},
			ollamaContainer: {
				Image: containerImage("ollama"), ContainerName: ollamaContainer, Networks: []string{net}, Restart: "unless-stopped",
				Volumes:     []string{filepath.Join(volumesDir, ollamaModelsVolume) + ":" + ollamaModelsGuest},
				Environment: ollamaEnvPairs(),
			},
			presidioAnalyzerContainer: {
				Image: containerImage("presidio-analyzer"), ContainerName: presidioAnalyzerContainer, Networks: []string{net}, Restart: "unless-stopped",
			},
			presidioAnonymizerContainer: {
				Image: containerImage("presidio-anonymizer"), ContainerName: presidioAnonymizerContainer, Networks: []string{net}, Restart: "unless-stopped",
			},
			valkeyContainer: {
				Image: containerImage("valkey"), ContainerName: valkeyContainer, Networks: []string{net}, Restart: "unless-stopped",
			},
			valkeyAdminContainer: {
				Image: containerImage("valkey-admin"), ContainerName: valkeyAdminContainer, Networks: []string{net}, Restart: "unless-stopped",
				Environment: []string{"DEPLOYMENT_MODE=Web", "VALKEY_HOST=" + valkeyContainer, "VALKEY_PORT=6379", "VALKEY_ENDPOINT_TYPE=node", "VALKEY_TLS=false"},
				DependsOn:   []string{valkeyContainer},
			},
			litellmDBContainer: {
				Image: containerImage("litellm-db"), ContainerName: litellmDBContainer, Networks: []string{net}, Restart: "unless-stopped",
				Ports:       []string{"127.0.0.1:" + litellmDBHostPort + ":5432"},
				Volumes:     []string{filepath.Join(volumesDir, litellmDBVolume) + ":/var/lib/postgresql"},
				Environment: []string{"POSTGRES_USER=" + litellmDBUser, "POSTGRES_DB=" + litellmDBName, "POSTGRES_HOST_AUTH_METHOD=trust"},
			},
			litellmContainer: {
				Image: containerImage("litellm"), ContainerName: litellmContainer, Networks: []string{net}, Restart: "unless-stopped",
				Volumes: []string{litellmConfig + ":/app/config.yaml"},
				// Secrets are PASSTHROUGH (bare names, no values) — read from the environment,
				// never written into this file. UI_USERNAME + the wiring URLs are not secret.
				Environment: []string{
					"UI_USERNAME=" + litellmUIUsername,
					"UI_PASSWORD",
					"LITELLM_MASTER_KEY",
					"LITELLM_SALT_KEY",
					"DATABASE_URL=" + litellmDatabaseURL,
					"PRESIDIO_ANALYZER_API_BASE=" + presidioAnalyzerURL,
					"PRESIDIO_ANONYMIZER_API_BASE=" + presidioAnonymizerURL,
				},
				Command:   []string{"--config", "/app/config.yaml", "--port", "4000"},
				DependsOn: []string{litellmDBContainer, presidioAnalyzerContainer, presidioAnonymizerContainer, valkeyContainer},
			},
			headroomContainer: {
				Image: containerImage("headroom"), ContainerName: headroomContainer, Networks: []string{net}, Restart: "unless-stopped",
				Environment: []string{"OPENAI_TARGET_API_URL=" + headroomTargetURL, "HEADROOM_NO_CCR_INJECT_TOOL=1"},
				DependsOn:   []string{litellmContainer},
			},
			proxyContainer: {
				Image: containerImage("proxy"), ContainerName: proxyContainer, Networks: []string{net}, Restart: "unless-stopped",
				Ports:     []string{bindHost + ":" + proxyHostPort + ":80"},
				Volumes:   []string{nginxConf + ":/etc/nginx/nginx.conf:ro"},
				DependsOn: []string{headroomContainer},
			},
		},
	}

	var buffer []byte
	buffer, err = yaml.Marshal(file)
	if err != nil {
		return nil, output.Errorf(output.ExitRuntimeFailure, "render compose: %s", err)
	}
	header := "# docker-compose.yaml for the AI Development Platform service tier — a DEBUG\n" +
		"# ARTIFACT generated by `ai services compose`, NOT the launcher (`ai setup` runs the\n" +
		"# stack via its per-container reconcile). Bring the SAME stack up under compose for\n" +
		"# debugging with:  ai services stop  &&  docker compose -f " + mustComposePath() + " up -d\n" +
		"# then `docker compose logs -f <service>` / `ps` / `restart <service>`. Secrets\n" +
		"# (UI_PASSWORD, LITELLM_MASTER_KEY, LITELLM_SALT_KEY) are passthrough — export them\n" +
		"# (or source ~/.ai-platform/.ai-platform.env) before `up`. Regenerate after changes.\n\n"
	return append([]byte(header), buffer...), nil
}

// mustComposePath returns the compose path for the header hint, or a plain filename when
// the platform dir can't be resolved (best-effort documentation string only).
func mustComposePath() string {
	if path, err := ComposeFilePath(); err == nil {
		return path
	}
	return "docker-compose.yaml"
}

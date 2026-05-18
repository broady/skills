# Configuration

What belongs in config, how to load it, how to validate it.

## Contents

- [What belongs in config](#what-belongs-in-config)
- [Default approach](#default-approach)
- [The Secret type](#the-secret-type)
- [Validation](#validation)
- [When to deviate](#when-to-deviate)
- [Config file vs env vars](#config-file-vs-env-vars)
- [Per-environment variation](#per-environment-variation)

---

## What belongs in config

> If this value is the same in dev, staging, and prod -- it is not config.
> It is an engineering decision. Put it in code.

Operational tuning knobs start in code and graduate to config when proven
necessary.

**Always config** (differs per deployment):

| Value | Why |
|---|---|
| Listen address | Different per environment, port conflicts |
| Database URL | Always different; is a secret |
| Downstream service URLs | Different per environment |
| Feature flags | Rolled out incrementally |
| Log level | Debug in dev, info in prod |

**Sometimes config** (graduate when there's a concrete reason):

| Value | Graduate when... |
|---|---|
| DB pool max connections | Different machine sizes or scaling tiers |
| Worker concurrency | CPU-bound work on varied hardware |
| Shutdown timeout | Different orchestrators have different drain windows |

**Almost never config** (engineering decisions):

| Value | Why it stays in code |
|---|---|
| HTTP `ReadHeaderTimeout` | Correctness: 5s is right for your protocol |
| HTTP `WriteTimeout` | Correctness: based on your slowest endpoint |
| HTTP `IdleTimeout` | Correctness: matches your load balancer |
| Retry backoff intervals | Chosen to match downstream SLOs |
| TLS handshake timeout | Physical constraint, rarely varies |
| Max header bytes | Security boundary, not a knob |
| `MaxConnLifetime` | Correctness: prevents stale connections |

If you find yourself wanting to change `ReadHeaderTimeout` between staging and
prod, you probably have a bug. Fix the bug, don't add a config value.

---

## Default approach

One `Config` struct. One `LoadConfig()`. Explicit `Validate()`.

- **Kong** owns the CLI envelope: `--config`, `--log-level`, subcommands.
- **`LoadConfig()`** owns service configuration: defaults, optional file, env/secret overlay, validation.
- **Constructors** receive the values they need, not the whole config struct.

Source precedence:

```text
1. Code defaults (in struct literal or DefaultConfig())
2. Optional config file (--config flag)
3. Environment variables / secret files
4. Validation
5. Construct dependencies with resolved values
```

For the complete working implementation of this pattern (Config struct, loadConfig,
env helpers, Kong CLI, and server lifecycle), see
[server/scaffold.md](server/scaffold.md).

---

## The Secret type

Secrets are not strings. They must not appear in logs, error messages, or
debug endpoints.

```go
type Secret struct {
    value string
}

func (s Secret) Value() string    { return s.value }
func (s Secret) String() string   { if s.value == "" { return "" }; return "<redacted>" }
func (s Secret) MarshalText() ([]byte, error) { return []byte("<redacted>"), nil }

func (s *Secret) UnmarshalText(text []byte) error {
    s.value = string(text)
    return nil
}
```

Loading convention:

```text
APP_DB_URL=postgres://...            # direct value (acceptable for dev)
APP_DB_URL_FILE=/run/secrets/db_url  # path to secret file (preferred for prod)
```

If both are set, fail. Ambiguous secret precedence is a bug.

```go
func envSecret(name string, dst *Secret) error {
    val, hasVal := os.LookupEnv(name)
    path, hasFile := os.LookupEnv(name + "_FILE")

    if hasVal && hasFile {
        return fmt.Errorf("%s and %s_FILE are mutually exclusive", name, name)
    }
    if hasFile {
        b, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("read %s_FILE: %w", name, err)
        }
        dst.value = strings.TrimRight(string(b), "\r\n")
        return nil
    }
    if hasVal {
        dst.value = val
    }
    return nil
}
```

---

## Validation

Validate things the **deployment operator** controls. Do not validate hardcoded
engineering defaults -- if `WriteTimeout` is always `30s` in your code, there is
nothing to validate.

```go
func (c Config) Validate() error {
    var errs []error
    if c.DatabaseURL.Value() == "" {
        errs = append(errs, errors.New("database_url is required"))
    }
    if c.PaymentsURL == "" {
        errs = append(errs, errors.New("payments_url is required"))
    }
    if c.MaxWorkers < 1 {
        errs = append(errs, errors.New("max_workers must be >= 1"))
    }
    return errors.Join(errs...)
}
```

---

## When to deviate

### Graduating a value to config

When you have a concrete operational reason:

```go
// Before: engineering decision
db.SetMaxOpenConns(16)

// After: graduated because we autoscale from 1 to 32 cores
// and pool size should track deployment tier.
db.SetMaxOpenConns(cfg.DBMaxConns)
```

Add it to the struct, add a default, add validation, add a single env var.
Do not pre-emptively graduate values "in case someone needs to tune it."

### Large services (5+ downstreams)

A config file provides structure; env vars override individual values:

```yaml
# config.yaml -- reviewed and deployed per environment
addr: ":8080"
payments_url: "https://payments.internal"
inventory_url: "https://inventory.internal"
shipping_url: "https://shipping.internal"
```

### Env-only (small services, tools)

For tiny services, use `caarlos0/env` or `sethvargo/go-envconfig` with struct
tags. Still call `Validate()` afterward.

### CLI-first programs

When flags ARE the product interface (not service config), let Kong own more:

```go
type CLI struct {
    Output string `short:"o" default:"-" help:"Output file."`
    Format string `enum:"json,csv" default:"json" help:"Output format."`
    Run    RunCmd `cmd:"" help:"Execute the export."`
}
```

---

## Config file vs env vars

| Use env vars when... | Use a config file when... |
|---|---|
| Small, flat config (3-5 values) | Structured, nested, 10+ values |
| Pure Twelve-Factor deployment | Team-reviewed deployment manifests |
| Single deployment target | Multiple named environments with shared structure |
| Quick iteration in dev | Operational documentation in version control |

The hybrid: **file for structure, env for deployment-specific overrides and
secrets.**

---

## Per-environment variation

Same binary. Same `Config` type. Different deployment inputs.

```text
dev:     ADDR=:8080  DATABASE_URL=postgres://localhost/app  PAYMENTS_URL=http://localhost:9090
staging: ADDR=:8080  DATABASE_URL_FILE=/run/secrets/db      PAYMENTS_URL=https://payments.staging.internal
prod:    ADDR=:8080  DATABASE_URL_FILE=/run/secrets/db      PAYMENTS_URL=https://payments.internal
```

Never:

```go
if os.Getenv("ENV") == "prod" {
    // ...
}
```

The application does not know or care which environment it is in. It receives
addresses, credentials, and feature flags -- that is all.

---

## Config Hot-Reload

### Hot-Reload Patterns
For services that need config changes without restart:

**Checksum-based change detection** — SHA-256 the config file plus all referenced files. Only reload when checksum changes:
```go
func configChanged(paths []string, lastChecksum [32]byte) (bool, [32]byte, error) {
	h := sha256.New()
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return false, [32]byte{}, fmt.Errorf("read config dependency %s: %w", p, err)
		}
		h.Write([]byte(p))
		h.Write(data)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum != lastChecksum, sum, nil
}
```

**Reloader chain** — Register named reloaders. Execute all sequentially. Partial failure doesn't abort:
```go
type reloader struct {
	name     string
	reloader func(*Config) error
}

func reloadConfig(reloaders []reloader, cfg *Config) error {
	var errs []error
	for _, r := range reloaders {
		if err := r.reloader(cfg); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.name, err))
		}
	}
	return errors.Join(errs...)
}
```

**Per-tenant runtime overrides** (Loki/Temporal pattern) — Default limits from flags; per-tenant overrides from a hot-reloaded YAML file. The overrides struct falls back to defaults. See also [backpressure.md](backpressure.md) for the atomic-pointer variant used in rate limiters.
```go
func (o *Overrides) MaxRate(tenantID string) float64 {
	if t := o.tenantLimits(tenantID); t != nil && t.MaxRate > 0 {
		return t.MaxRate
	}
	return o.defaults.MaxRate
}
```

**Rules:**
- Serialize concurrent reloads (mutex or channel)
- Deep-copy config at subscriber boundaries
- Log reload success/failure with metrics (config_reload_success gauge)
- Never silently ignore reload errors — all reloaders execute, all errors are reported

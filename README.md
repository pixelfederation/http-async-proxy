# http-async-proxy
pod local async http proxy with buffer

# Options

| Option          | Long Flag         | Short Flag | Environment Variable | Description                                    | Default Value      |
|-----------------|-------------------|------------|----------------------|------------------------------------------------|--------------------|
| Queue Size      | `--queue_size`    | `-q`       | `QUEUE_SIZE`         | Buffer queue size/number of buffered requests  | `100`              |
| Worker Count    | `--worker_count`  | `-w`       | `WORKER_COUNT`       | Number of worker goroutines to spawn           | `5`                |
| Listen Address  | `--listen`        | `-l`       | `LISTEN_ADDRESS`     | Listen bind address                            | `:8080`            |
| Metrics Address | `--metrics_address` | `-m`      | `METRICS_ADDRESS`    | Metrics bind address                           | `:9091`            |
| Config Path     | `--config_path`   | `-c`       | `CONFIG_PATH`        | Path to backend map file in YAML format        | `/etc/backends.yaml` |
| Terminate File  | `--terminate_file_path` | `-t` | `TERMINATE_FILE_PATH` | Shut down once this file exists               | (disabled)         |

# Backends

`delay` and `timeout` are in seconds and may be fractional. `timeout` limits connecting;
the whole request may take one second more.

# Shutdown

On SIGTERM, SIGINT or the terminate file the proxy stops accepting requests, delivers
everything still queued and exits 0. It sets no time limit of its own: run as a native
sidecar, it gets SIGTERM after the app container has stopped and is killed at the end of
the pod's grace period.

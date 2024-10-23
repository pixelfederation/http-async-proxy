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


#TODO 
- pre-stop script to image, sig term handling

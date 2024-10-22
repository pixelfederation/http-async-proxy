# http-async-proxy
pod local async http proxy with buffer

env configuraion variables with defaults:
QUEUE_SIZE: 100
WORKER_COUNT: 5
LISTEN_ADDRESS: 8080
METRICS_PORT: 9091
CONFIG_PATH: /etc/backends.yaml

## Configuration file

```
proxy-vector.com:
  - backend: http://127.0.0.1:8081
    retries: 2
    delay: 0.1
    timeout: 0.5
  - backend: http://127.0.0.1:8082
    retries: 1
    delay: 1
    timeout: 2
proxy-something.com:
  - backend: http://something.com
    retries: 3
    delay: 1
    timeout: 1
```

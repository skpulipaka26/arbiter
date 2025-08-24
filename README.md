# Arbiter

Arbiter is a high-performance priority-based request gateway designed for sidecar deployment with resource-intensive services like vLLM. It manages request flow based on configurable priorities and processing modes, ensuring critical requests are processed first while maintaining system stability under load.

## Why Arbiter?

Modern ML/LLM services face a fundamental challenge: managing varying request priorities while maximizing throughput. A simple FIFO queue leads to head-of-line blocking where urgent requests wait behind bulk operations. Rate limiting drops requests indiscriminately. Load balancers distribute traffic but don't understand priority.

Arbiter solves this by implementing intelligent request management between clients and your service. Deployed as a sidecar, it maintains a priority queue that ensures high-priority requests are processed first, while using sophisticated batching strategies to maximize throughput for bulk operations.

## Key Features

### Smart Priority Management
Arbiter uses a binary min-heap priority queue with O(log n) operations, ensuring efficient handling even with thousands of queued requests. Requests are classified into three priority levels based on the `Priority` header, with FIFO ordering within each level.

### Intelligent Batching
In batch mode, Arbiter doesn't just wait for timeouts. It continuously monitors request priorities and makes intelligent flush decisions:
- High-priority request arrives → flush within 10ms
- Medium-priority in queue → flush at 50% capacity or 50ms
- Low-priority only → wait for full batch to maximize throughput

The system uses adaptive polling that speeds up to 5ms under load and backs off to 50ms when idle, with hysteresis to prevent oscillation.

### Efficient Resource Usage
- **Fire-and-forget architecture**: Request processing isn't blocked by slow upstream responses
- **Zero-copy streaming**: Responses flow directly from upstream to client without buffering
- **Lazy queue maintenance**: Stale requests are removed only when needed, not via periodic scanning
- **Minimal memory footprint**: Only request metadata is queued, not request bodies

### Graceful Degradation
Under load, Arbiter implements graduated load shedding:
- Queue exceeds `low_priority_shed_at` → reject low-priority requests
- Queue exceeds `medium_priority_shed_at` → only accept high-priority
- Queue full → reject all requests with 429 status

Requests that exceed `request_max_age` are automatically dropped to prevent processing stale work.

## Architecture

Arbiter runs as a companion service alongside your application:

```
Client → Arbiter (port 8080) → Your Service (localhost:8000)
```

When a request arrives:
1. Priority is extracted from the request header
2. Request is enqueued based on priority
3. Processor dequeues based on configured mode (individual/batch)
4. Request is proxied to upstream with path and headers preserved
5. Response streams directly back to client

The reverse proxy preserves all request characteristics, making Arbiter transparent to both clients and upstream services.

## Configuration

Arbiter uses a simple YAML configuration:

### Individual Mode (for vLLM/LLM servers)
```yaml
port: 8080

upstream:
  url: "http://localhost:8000"  # Your service endpoint
  mode: "individual"
  max_concurrent: 10            # Parallel requests to upstream
  queue:
    max_size: 100
    low_priority_shed_at: 30
    medium_priority_shed_at: 60
    request_max_age: 60s
```

### Batch Mode (for ML inference)
```yaml
port: 8080

upstream:
  url: "http://localhost:8000"
  mode: "batch"
  batch_size: 10              # Maximum requests per batch
  batch_timeout: 5s            # Safety timeout (rarely triggered)
  queue:
    max_size: 1000
    low_priority_shed_at: 500
    medium_priority_shed_at: 800
    request_max_age: 30s
```

### Configuration Validation

Arbiter validates all settings at startup:
- Mode must be "individual" or "batch"
- Batch mode requires batch_size and batch_timeout
- Queue thresholds must be logical (low < medium < max)
- Port must be valid (1-65535)

## Deployment

### Docker Compose
```yaml
services:
  vllm:
    image: vllm/vllm-openai:latest
    command: --model meta-llama/Llama-2-7b-hf
    
  arbiter:
    image: your-registry/arbiter:latest
    ports:
      - "8080:8080"
    volumes:
      - ./config.yaml:/config.yaml
    command: ["/arbiter", "-config", "/config.yaml"]
    depends_on:
      - vllm
```

### Kubernetes Sidecar
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vllm-with-arbiter
spec:
  template:
    spec:
      containers:
        - name: vllm
          image: vllm/vllm-openai:latest
          ports:
            - containerPort: 8000  # Not exposed to service
            
        - name: arbiter
          image: your-registry/arbiter:latest
          ports:
            - containerPort: 8080  # Service targets this port
          volumeMounts:
            - name: config
              mountPath: /config
          command: ["/arbiter", "-config", "/config/config.yaml"]
          
      volumes:
        - name: config
          configMap:
            name: arbiter-config
---
apiVersion: v1
kind: Service
metadata:
  name: vllm-service
spec:
  selector:
    app: vllm-with-arbiter
  ports:
    - port: 80
      targetPort: 8080  # Points to Arbiter, not vLLM
```

### Gradual Rollout

For safe deployment, use traffic splitting:

```yaml
# Using Gateway API
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
spec:
  rules:
    - backendRefs:
        - name: vllm-direct
          port: 8000
          weight: 50
        - name: vllm-with-arbiter
          port: 8080
          weight: 50
```

## Usage

### Sending Requests

Include a Priority header with your requests:

```bash
# High priority inference
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Priority: high" \
  -H "Content-Type: application/json" \
  -d '{"model": "llama-2", "messages": [{"role": "user", "content": "Critical question"}]}'

# Bulk processing
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Priority: low" \
  -H "Content-Type: application/json" \
  -d '{"model": "llama-2", "messages": [{"role": "user", "content": "Batch processing"}]}'
```

Priority mappings:
- `high`, `urgent`, `critical` → High priority
- `medium`, `normal`, `standard` → Medium priority  
- `low`, `background`, `batch` → Low priority
- No header → Low priority (default)

### Monitoring

Arbiter exposes metrics at `/metrics` (Prometheus format) and health at `/health`:

```bash
# Check health
curl http://localhost:8080/health

# View Prometheus metrics
curl http://localhost:8080/metrics
```

Metrics include:
- Request counts by priority
- Queue depths
- Processing latencies
- Shed counts

## Building from Source

```bash
# Clone the repository
git clone https://github.com/yourusername/arbiter
cd arbiter

# Build the binary
go build -o arbiter ./cmd/arbiter

# Run with configuration
./arbiter -config config.yaml
```

## Testing

The project includes utilities for testing priority behavior:

```bash
# Test priority ordering with LLM
go run cmd/llmtest/main.go

# Load testing
go run cmd/loadtest/main.go
```

## Use Cases

### vLLM/LLM Serving
Ensure production inference requests aren't blocked by development/testing traffic. Configure with individual mode and appropriate concurrency limits based on your GPU capacity.

### ML Batch Inference
Maximize GPU utilization by batching requests intelligently. High-priority requests still get processed quickly while background jobs are batched efficiently.

### Rate-Limited APIs
Respect upstream rate limits while ensuring critical requests get through. The queue and priority system naturally implements sophisticated rate limiting.

### Mixed Workloads
Handle both real-time and batch workloads on the same infrastructure. Priority-based scheduling ensures SLAs are met for important requests.

## License

MIT
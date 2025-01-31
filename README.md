# Kitsune - A Lightweight In-Memory Cache Server in Go

![Kitsune Logo](./cover.webp)

Kitsune is a simple, in-memory caching server written in Go. It supports multiple buckets (namespaces), basic LRU eviction to enforce a maximum cache size, per-entry TTL (time-to-live), and a straightforward HTTP API for storing and retrieving values.

## Table of Contents

1. [Features](#features)  
2. [Installation](#installation)  
3. [Configuration](#configuration)  
   - [Command-Line Flags](#command-line-flags)
4. [HTTP Endpoints](#http-endpoints)
5. [Usage Examples](#usage-examples)
6. [Running the Server](#running-the-server)
7. [Testing and Benchmarks](#testing-and-benchmarks)
8. [Contributing](#contributing)
9. [License](#license)

---

## Features

- **Multiple Buckets**: Organize keys into separate buckets (namespaces).
- **LRU-Based Eviction**: Automatic eviction of the least-recently-used entry when total cache size exceeds the defined maximum.
- **Configurable TTL**: All items can have a default time-to-live.
- **Cleanup Interval**: Expired items are periodically removed in the background.
- **HTTP API**: Simple endpoints to GET, PUT, and DELETE cached items.
- **Default Bucket**: Convenient single-bucket usage when you don't need multiple namespaces.
- **Thread-Safe**: Built with concurrency in mind, safe to use in multi-threaded environments.
- **Memory Pooling**: Uses sync.Pool to reduce GC pressure and improve performance.
- **Lazy Expiration**: Expired items are removed both periodically and upon access.

---

## Installation

1. **Clone the repository**:
   ```bash
   git clone https://github.com/seashell86/kitsune.git
   cd kitsune
   ```

2. **Build**:
   ```bash
   go build -o kitsune main.go
   ```

3. **Run**:
   ```bash
   ./kitsune
   ```
   (See [Running the Server](#running-the-server) below for details on configuration.)

---

## Configuration

You can configure Kitsune using command-line flags. Below are the available configuration options.

### Command-Line Flags

| Flag                   | Default        | Description                                   |
|------------------------|----------------|-----------------------------------------------|
| `--host`               | `0.0.0.0`      | Host IP to bind.                              |
| `--port`               | `42069`        | Port to listen on.                            |
| `--max-entry-size`     | `9.22 * 10^18` | Maximum size of a single cache entry (bytes). |
| `--max-size`           | `9.22 * 10^18` | Maximum total size of the cache (bytes).      |
| `--ttl`                | `3600`         | Default TTL for entries (in seconds).         |
| `--cleanup-interval`   | `300`          | Cleanup interval in seconds.                  |
| `--default-keyspace`   | `__root__`     | Default bucket/namespace name.                |

> **Note**: If both an environment variable and a command-line flag are provided for a setting, **the flag value** is used.

---

## HTTP Endpoints

### Health Check

- **`GET /`**
  - **Response**: `{"status": "healthy"}`

### Default Keyspace Endpoints

- **`GET /keys/{key}`**  
  Retrieve the value of `{key}` in the default bucket.

- **`PUT /keys/{key}`**  
  Set the value of `{key}` in the default bucket.  
  - **Request Body** (JSON):
    ```json
    {
      "value": "some string value"
    }
    ```
  - **Response**: `200 OK` on success.

- **`DELETE /keys/{key}`**  
  Delete the specified key from the default bucket.

### Bucket Endpoints

- **`GET /buckets/{bucket}`**  
  Returns the number of keys in the specified `{bucket}` as `{"count": <number>}`.

- **`DELETE /buckets/{bucket}`**  
  Clear all keys from the specified `{bucket}`.

- **`GET /buckets/{bucket}/{key}`**  
  Retrieve the value of `{key}` from the specified `{bucket}`.

- **`PUT /buckets/{bucket}/{key}`**  
  Set the value of `{key}` in the specified `{bucket}`.  
  - **Request Body** (JSON):
    ```json
    {
      "value": "some string value"
    }
    ```
  - **Response**: `200 OK` on success.

- **`DELETE /buckets/{bucket}/{key}`**  
  Delete the specified key from the specified bucket.

- **`DELETE /buckets`**  
  Clear **all** buckets and keys in the entire cache.

---

## Usage Examples

1. **Set and Get from the Default Keyspace**:
   ```bash
   # Set a key in the default keyspace
   curl -X PUT -H "Content-Type: application/json" \
        -d '{"value":"hello world"}' \
        http://localhost:42069/keys/mykey
   
   # Get the key from the default keyspace
   curl http://localhost:42069/keys/mykey
   # -> {"value":"hello world"}
   ```

2. **Use a Custom Bucket**:
   ```bash
   # Set a key in a custom bucket named "mybucket"
   curl -X PUT -H "Content-Type: application/json" \
        -d '{"value":"bucket data"}' \
        http://localhost:42069/buckets/mybucket/specialkey

   # Retrieve the key from "mybucket"
   curl http://localhost:42069/buckets/mybucket/specialkey
   # -> {"value":"bucket data"}

   # Get the total count of items in "mybucket"
   curl http://localhost:42069/buckets/mybucket
   # -> {"count":1}

   # Delete a single key in "mybucket"
   curl -X DELETE http://localhost:42069/buckets/mybucket/specialkey

   # Clear the entire "mybucket"
   curl -X DELETE http://localhost:42069/buckets/mybucket
   ```

---

## Running the Server

By default, the server listens on port `42069`. To run the server with the defaults:

```bash
go run main.go
```

Or, build and run the generated binary:

```bash
go build -o kitsune main.go
./kitsune
```

You can override default settings using flags:

```bash
./kitsune --host 127.0.0.1 --port 8080 --ttl 120 --cleanup-interval 30
```

---

## Testing and Benchmarks

The project includes comprehensive tests and benchmarks in `kitsune_test.go`. The test suite covers:

- Basic cache operations (Set/Get/Delete)
- TTL and expiration behavior
- HTTP API endpoints
- Concurrent access patterns
- Edge cases and error conditions

Benchmarks measure performance of:

- Single-operation performance (Set/Get/Delete)
- Parallel operations
- High-contention scenarios
- Memory pressure and eviction
- Multi-bucket usage

Run tests with:
```bash
go test -v
```

Run benchmarks with:
```bash
go test -bench=. -benchmem
```

Sample benchmark results might look like:
```
BenchmarkSet-8                     1000000    1234 ns/op    123 B/op    2 allocs/op
BenchmarkGet-8                     2000000     567 ns/op     32 B/op    1 allocs/op
BenchmarkParallelSetGet-8          500000    2345 ns/op    234 B/op    3 allocs/op
// ... etc
```

The `-benchmem` flag shows memory allocation statistics to help with optimization:
- ns/op: Nanoseconds per operation
- B/op: Bytes allocated per operation
- allocs/op: Number of heap allocations per operation

---

## Contributing

Contributions, bug reports, and feature requests are welcome. Feel free to open an issue or submit a pull request.

1. **Fork** the repo on GitHub.
2. **Clone** your fork locally.
3. **Create** a new feature branch.
4. **Commit** your changes and push your branch.
5. **Submit** a pull request.

---

## License

This project is licensed under the [MIT License](https://opensource.org/licenses/MIT). Feel free to use, modify, and distribute as you see fit.
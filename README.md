# TREM - Transactional Racing Executor Monkey

A high-performance HTTP/1.1 and HTTP/2 race condition testing tool designed for exploiting timing vulnerabilities in web applications. TREM enables security researchers to test race conditions in authentication flows, rate limiters, WAFs, and transactional systems with extreme precision and minimal computational overhead.

## Overview

Race conditions occur when multiple operations access shared resources without proper synchronization, leading to unexpected behavior. TREM exploits these vulnerabilities by orchestrating multiple concurrent HTTP request chains with nanosecond-level timing precision.

### Attack Scenarios

**Classic Race Conditions (Sync Mode)**
Multiple threads establish connections, synchronize at a barrier, then fire requests simultaneously. Exploits TOCTOU (Time-of-Check to Time-of-Use) vulnerabilities in:
- Double-spend in financial transactions
- Coupon/voucher redemption bypass
- Inventory manipulation (overselling)
- Privilege escalation via concurrent role assignment

**Rate Limiter & WAF Bypass**
Abuse the timing window between request arrival and counter increment. When N requests arrive within the same processing window, the rate limiter may count them as fewer attempts than actual, allowing:
- Bypass of login attempt limits
- Circumvention of API throttling
- Evasion of brute-force protections

**Password Spray & Brute-Force Racing**
Via FIFO-based value distribution, TREM injects credentials at wire speed while exploiting race windows in authentication systems. A WAF configured for "5 attempts per minute" may allow 50+ attempts if requests arrive faster than the counter synchronization interval.

### Computational Efficiency

TREM is engineered for minimal overhead and maximum throughput:

| Component | Strategy | Complexity |
|-----------|----------|------------|
| Value Distribution | Lock-free reads via `atomic.Pointer` | O(1) |
| Thread Communication | Buffered channels (1000 capacity) | O(1) amortized |
| FIFO Reader | Non-blocking I/O with 100ms timeout | O(n) per batch |
| Batch Processing | 4KB buffer / 64 lines per dispatch | O(n) |
| Key Extraction | Regex with linear dedup | O(n) |
| Cartesian Product | In-place combination generation | O(k^m) for k values, m keys |
| Connection Pooling | Keep-alive with TLS session reuse | O(1) per request |

The architecture eliminates mutex contention through copy-on-write semantics and atomic operations, enabling thousands of concurrent requests with sub-millisecond coordination overhead.

## Build

```bash
# Clone
git clone https://github.com/otavioarj/TREM.git
cd TREM
go mod tidy

# Debug build
go build

# Release build (smaller binary)
go build -ldflags="-s -w"
```

## Usage

```bash
./trem -l "req1.txt,req2.txt,...,reqN.txt" -re patterns.txt [options]
```

## Demo
![Demo](NullByte-2025/vid.gif)

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-l` | *required* | Comma-separated list of raw HTTP/1.1 request files |
| `-re` | *required* | Regex patterns file for value extraction between requests |
| `-h` | | Host:port override (default: extracted from Host header) |
| `-th` | 5 | Thread count |
| `-d` | 0 | Delay in milliseconds between requests |
| `-o` | false | Save last response per thread as `out_<time>_t<id>.txt` |
| `-u` | | Universal replacement: `key=val` or FIFO path (see below) |
| `-px` | | HTTP proxy URL (e.g., `http://127.0.0.1:8080`) |
| `-mode` | async | Execution mode: `sync` or `async` |
| `-k` | true | Keep-alive: persist TLS connections across requests |
| `-x` | 1 | Loop start index (1-based) |
| `-xt` | 1 | Loop count (0 = infinite) |
| `-cli` | 0 | TLS ClientHello fingerprint (see table below) |
| `-tou` | 500 | TLS handshake timeout in milliseconds |
| `-retry` | 3 | Max retries on connection/TLS errors |
| `-v` | false | Verbose debug output |
| `-sw` | 0 | Stats window size (0 = auto: 10 normal, 50 verbose) |
| `-http2` | false | Use HTTP/2 (default: HTTP/1.1) |
| `-fw` | true | FIFO wait: block until first value written to named pipe |
| `-fmode` | 2 | FIFO distribution: 1=Broadcast (all threads), 2=Round-robin |
| `-mtls` | | Client certificate for mTLS: `/path/cert.p12:password` |
| `-dump` | false | Dump thread output to files (`thr<ID>_<H-M>.txt`) |

## ClientHello Fingerprints (`-cli`)

| Value | Fingerprint |
|-------|-------------|
| 0 | Randomized (NoALPN for HTTP/1.1, ALPN for HTTP/2) - default |
| 1 | Chrome_Auto |
| 2 | Firefox_Auto |
| 3 | iOS_Auto |
| 4 | Edge_Auto |
| 5 | Safari_Auto |

> **Note**: Modes 1-5 negotiate ALPN. Mode 0 automatically selects `RandomizedNoALPN` for HTTP/1.1 and `RandomizedALPN` for HTTP/2.

## HTTP/2 Support

TREM supports HTTP/2 natively. Request files remain in HTTP/1.1 raw format - TREM automatically converts them to HTTP/2 frames.

```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -http2 -th 10 -mode sync
```

**Conversion process:**
- `Host` header → `:authority` pseudo-header
- Connection-specific headers stripped (`Connection`, `Keep-Alive`, etc.)
- Headers HPACK encoded
- Body sent as DATA frames
- TLS ALPN set to `h2`

**Limitations:**
- Flow control assumes server window ≥ 64KB
- Server push (PUSH_PROMISE) frames discarded

**When to use HTTP/2:**
- Target server requires HTTP/2
- Testing HTTP/2-specific race conditions
- Leveraging multiplexed streams on single connection

## Operation Modes

### Async Mode (default)

Each thread processes requests independently without synchronization. Best for high-throughput testing.

```bash
./trem -l "login.txt,action.txt" -re patterns.txt -th 5 -mode async
```

### Sync Mode

Threads synchronize at a barrier before sending the first request simultaneously. Essential for race condition exploitation.

```bash
./trem -l "login.txt,race.txt" -re patterns.txt -th 10 -mode sync
```

**Flow:**
1. All threads establish TLS connections
2. All threads wait at barrier
3. Barrier releases → simultaneous request transmission
4. Remaining requests proceed normally

## Request Chaining

### Pattern File Format

Each line extracts values from response N to use in request N+1:
```
regex1`:key1 $ regex2`:key2
regex3`:key3
```

- Use backtick (`) before colon
- Multiple patterns per line separated by ` $ `
- Regex must have one capture group `()`
- Line 1: response 1 → request 2
- Line 2: response 2 → request 3

### Request File Placeholders

Use `$key$` syntax for replacement points:
```http
POST /api/action HTTP/1.1
Host: example.com
Content-Type: application/json

{"session": "$sessionId$", "token": "$csrfToken$"}
```

### Example Chain

**req1.txt** - Login:
```http
POST /api/login HTTP/1.1
Host: target.com
Content-Type: application/json

{"username":"admin","password":"$pass$"}
```

**req2.txt** - Action with extracted session:
```http
POST /api/transfer HTTP/1.1
Host: target.com
Authorization: Bearer $token$
Content-Type: application/json

{"to":"attacker","amount":10000}
```

**regex.txt**:
```
"token":\s*"([^"]+)"`:token
```

## Universal Replacement (`-u`)

### Static Mode

Replace a placeholder across all requests:

```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -u '$user$=victim123'
```

### FIFO Mode (Dynamic Injection)

For dynamic value injection (password spraying, token rotation), use a named pipe:

```bash
# Terminal 1: Start TREM with FIFO
./trem -l "login.txt,action.txt" -re patterns.txt -u /tmp/spray -th 20

# Terminal 2: Feed values
cat passwords.txt > /tmp/spray
```

**passwords.txt format:**
```
$pass$=password123
admin123
letmein
qwerty
123456
```

The first line defines the key (`$pass$`), subsequent lines are values for that key.

### FIFO Distribution Modes (`-fmode`)

| Mode | Behavior | Use Case |
|------|----------|----------|
| 1 | Broadcast: all threads receive all values | Testing same credentials across threads |
| 2 | Round-robin: each value goes to one thread | Password spraying (default) |

**Round-robin example with 3 threads:**
```
FIFO input: pass1, pass2, pass3, pass4, pass5, pass6
Thread 0: pass1, pass4
Thread 1: pass2, pass5
Thread 2: pass3, pass6
```

### Persistent Keys (`$_key$`)

Keys starting with underscore are never consumed and persist across all loops:

```
$_bearer$=eyJhbGciOiJIUzI1NiIs...
$pin$=1234
5678
9012
```

- `$_bearer$` is set once and used in every request
- `$pin$` values are consumed normally (one per request)

This enables scenarios like: authenticate once, then spray PINs.

### Cartesian Product

When multiple keys have multiple values, TREM generates all combinations:

```
$user$=admin
guest
$pass$=123
456
```

Generates 4 requests:
- admin + 123
- admin + 456
- guest + 123
- guest + 456

### FIFO Wait (`-fw`)

| Setting | Behavior |
|---------|----------|
| `-fw=true` (default) | Block threads until first FIFO value arrives |
| `-fw=false` | Start immediately; missing keys left unsubstituted |

## Looping

Execute a subset of requests repeatedly:

```bash
./trem -l "login.txt,setup.txt,race.txt" -re patterns.txt -x 2 -xt 100
```

- `-x 2`: Loop starts from request 2 (setup.txt)
- `-xt 100`: Run 100 iterations
- `-xt 0`: Infinite loops (stop with Q)

**Flow:**
1. Execute requests 1 → 3
2. Loop 100×: requests 2 → 3

## mTLS Support

For targets requiring client certificates:

```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -mtls /path/cert.p12:password
```

The certificate must be in PKCS#12 format.

## Proxy Support

Route traffic through HTTP proxy (e.g., Burp Suite):

```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -px http://127.0.0.1:8080
```

## Thread Output Dump

Save all thread output to files for post-analysis:

```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -dump
```

Creates files: `thr0_15-30.txt`, `thr1_15-30.txt`, etc.

## Verbose Mode

Debug connections, TLS, and regex issues:

```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -v
```

Outputs:
- TLS handshake details (ClientHello, cipher, ALPN)
- Retry attempts with backoff timing
- Connection state changes
- Regex match failures
- Jitter and latency metrics

## Stats Panel

Real-time metrics in the UI:

**Normal mode:**
- Req/s: requests per second
- TotalReq: total requests sent
- HTTP Errs: errors by thread and status (e.g., `T1(401)x5`)
- Univ.: current FIFO path and last value

**Verbose mode (`-v`)** adds:
- Avg Jitter: latency variance
- TLS latency and retry averages
- I/O totals (bytes in/out)

## UI Controls

| Key | Action |
|-----|--------|
| Tab | Next thread tab |
| Shift+Tab | Previous thread tab |
| Q | Quit (stops all threads gracefully) |

## Examples

**Basic race condition (sync mode):**
```bash
./trem -l "auth.txt,race.txt" -re patterns.txt -th 20 -mode sync
```

**Password spray with FIFO:**
```bash
./trem -l "login.txt,dashboard.txt" -re patterns.txt -u /tmp/creds -th 50 -fmode 2
```

**Sustained race with keep-alive:**
```bash
./trem -l "login.txt,setup.txt,race.txt" -re patterns.txt -th 10 -mode sync -k -x 3 -xt 0
```

**HTTP/2 race condition:**
```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -http2 -th 10 -mode sync
```

**mTLS with client certificate:**
```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -mtls ./client.p12:secret
```

**Debug TLS issues:**
```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -cli 1 -v -retry 5
```

**Via Burp proxy with output dump:**
```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -px http://127.0.0.1:8080 -dump
```

**Infinite spray with persistent auth token:**
```bash
# passwords.txt:
# $_token$=eyJhbG...
# $pass$=123456
# password
# admin123

./trem -l "verify.txt" -re patterns.txt -u /tmp/spray -th 100 -xt 0 -fmode 2
```

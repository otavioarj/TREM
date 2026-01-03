# TREM - Transactional Racing Executor Monkey

A high-performance HTTP/1.1 and HTTP/2 race condition testing tool designed for exploiting timing vulnerabilities in web applications. TREM enables security researchers to test race conditions in authentication flows, rate limiters, WAFs, and transactional systems with extreme precision and minimal computational overhead.

## Overview

Race conditions occur when multiple operations access shared resources without proper synchronization, leading to unexpected behavior. TREM exploits these vulnerabilities by orchestrating multiple concurrent HTTP request chains with nanosecond-level timing precision. TREM uses handcrafted [HTTP/2](#http2-support) and [TLS](#clienthello-fingerprints--cli) state machines, by using TLS ClientHello randomization or HTTP/2 server to client sync datagram ignores, it allows TLS fingerprint evasions and special HTTP/2 race scenarios.

### Attack Scenarios

**Classic Race Conditions ([Sync Mode](#sync-mode))**

Multiple threads establish connections, synchronize at a [barrier](#sync-barriers--sb), then fire requests simultaneously. Exploiting classical TOCTOU (Time-of-Check to Time-of-Use) vulnerabilities in:
- Double-spend in financial transactions
- Coupon/voucher redemption bypass
- Inventory manipulation (overselling)
- Privilege escalation via concurrent role assignment

**Havoc Race Conditions ([Async Mode](#async-mode-default))**

Default mode, it abuses in high-throughput race conditions, aiming to cause havoc (_ad absurdum_) in target state machine, where each thread processes requests independently without synchronization. For instance:

- Session puzzling: Rapid sequential requests exploiting session state inconsistencies
- Cache poisoning: Flooding requests to poison CDN/cache before legitimate traffic
- Racer Resource exhaustion: Sustained high-volume requests depleting server connection pools, leading to lost of concurrency managing
- Business logic abuse: High-speed coupon redemption, inventory manipulation without sync timing, leading to corruption of state status at any coupon/inventory manipulation

**Rate Limiter & WAF Bypass**

Abuse the timing window between request arrival and counter increment. When N requests arrive within the same processing window, the rate limiter may count them as fewer attempts than actual, allowing:
- Bypass of login attempt limits
- Circumvention of API throttling
- Evasion of brute-force protections

**Password Spray & Brute-Force Racing**

Via [FIFO-based value distribution](#fifo-mode-dynamic-injection), TREM injects credentials at wire speed while exploiting race windows in authentication systems. A WAF configured for "5 attempts per minute" may allow 50+ attempts if requests arrive faster than the counter synchronization interval.

### Computational Efficiency

TREM is engineered for minimal overhead and maximum throughput:

| Component | Strategy | Complexity |
|-----------|----------|------------|
| Value Distribution | Lock-free reads via `atomic.Pointer` | O(1) |
| Thread Communication | Buffered channels (1000 capacity) | O(1)  |
| FIFO Reader | Non-blocking I/O with 100ms timeout | O(n) per batch |
| Batch Processing | 4KB buffer / 64 lines per dispatch | O(n) amortized |
| Key Extraction | Regex with linear dedup | O(n) amortized|
| Key combination & seek | In-place combination generation | O(log<sub>k</sub>v) for v values, k keys |
| Connection Pooling | Keep-alive with TLS session reuse | O(1) |

The architecture eliminates mutex contention through **copy-on-write semantics** and atomic operations, enabling thousands of concurrent requests with sub-millisecond coordination overhead.

## Build

```bash
# Clone
git clone https://github.com/otavioarj/TREM.git
cd TREM
go mod tidy

# Debug build
go build

# Release build :)
go build -ldflags="-s -w" -trimpath
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
| `-u` | | Universal replacement: `key=val` or FIFO path (see [Universal Replacement](#universal-replacement--u)) |
| `-px` | | HTTP proxy URL (e.g., `http://127.0.0.1:8080`) |
| `-mode` | async | Execution mode: `sync` or `async` (see [Operation Modes](#operation-modes)) |
| `-k` | true | Keep-alive: persist TLS connections across requests |
| `-x` | 1 | Loop start index (1-based, see [Looping](#looping)) |
| `-xt` | 1 | Loop count (0 = infinite) |
| `-cli` | 0 | TLS ClientHello fingerprint (see [table below](#clienthello-fingerprints--cli)) |
| `-tou` | 500 | TLS handshake timeout in milliseconds |
| `-retry` | 3 | Max retries on connection/TLS errors |
| `-v` | false | Verbose debug output (see [Verbose Mode](#verbose-mode)) |
| `-sw` | 10 | Stats window size (0 = auto: 10 normal, 50 verbose) |
| `-http2` | false | Use HTTP/2 (default: HTTP/1.1, see [HTTP/2 Support](#http2-support)) |
| `-fw` | true | FIFO wait: block until first value written to named pipe |
| `-fmode` | 2 | FIFO distribution: 1=Broadcast (all threads), 2=Round-robin |
| `-mtls` | | Client certificate for mTLS: `/path/cert.p12:password` |
| `-dump` | false | Dump thread tab(TUI) output to files (`thr<ID>_<H-M>.txt`) |
| `-sb` | 1 | Sync barrier indices (1-based, comma-separated, see [Sync Barriers](#sync-barriers--sb)) |
| `-ra` | | Response action file (see [Response Actions](#response-actions--ra)) |

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

**Intended Limitations:**
- Flow control assumes server window ≥ 64KB
- Server push (PUSH_PROMISE) frames discarded

**When to use HTTP/2:**
- Target server requires HTTP/2
- Testing HTTP/2-specific race conditions
- Leveraging multiplexed streams on single connection

## Operation Modes

### Async Mode (default)

Each thread processes requests independently without synchronization. Best for high-throughput race condition exploitation.

```bash
./trem -l "login.txt,action.txt" -re patterns.txt -th 5 -mode async
```

### Sync Mode

Threads synchronize at [barrier points](#sync-barriers--sb) before sending requests simultaneously. Essential for classical race condition exploitation.

```bash
./trem -l "login.txt,race.txt" -re patterns.txt -th 10 -mode sync
```

**Default Flow (barrier on first request):**
1. All threads establish TLS connections
2. All threads wait at barrier (request 1)
3. Barrier releases → simultaneous request transmission
4. Remaining requests proceed normally

### Sync Barriers (`-sb`)

By default, sync mode places a barrier only on the first request. Use `-sb` to specify which requests should trigger synchronization barriers.

**Format:** Comma-separated list of 1-based request indices.

```bash
# Barrier only on request 1 (default)
./trem -l "r1.txt,r2.txt,r3.txt" -re patterns.txt -mode sync

# Barrier on requests 1, 2, and 3
./trem -l "r1.txt,r2.txt,r3.txt" -re patterns.txt -mode sync -sb 1,2,3

# Barrier only on requests 2 and 4
./trem -l "r1.txt,r2.txt,r3.txt,r4.txt" -re patterns.txt -mode sync -sb 2,4
```

**Use Cases:**

| Scenario | `-sb` Value | Description |
|----------|-------------|-------------|
| Classic race | `1` (default) | Synchronize first request only |
| Multi-step race | `1,3` | Sync login, then sync critical action |
| Full synchronization | `1,2,3,...` | All requests synchronized |
| Skip login sync | `2` | Login async, sync on second request |

**Example: Two-phase race condition**

Testing a scenario where you need to synchronize both authentication and the vulnerable action:

```bash
./trem -l "login.txt,setup.txt,transfer.txt" -re patterns.txt -th 20 -mode sync -sb 1,3
```

**Flow:**
1. All threads sync → send `login.txt` simultaneously
2. Each thread processes `setup.txt` independently (no barrier)
3. All threads sync → send `transfer.txt` simultaneously

This is useful when the race condition requires both authenticated sessions established at the same time AND the vulnerable action triggered simultaneously.

## Response Actions (`-ra`)

Response actions allow you to execute commands when a regex matches the response at specific request indices. This enables automated detection of successful exploitation, evidence collection, and controlled termination.

**Format:** Each line in the action file follows:
```
indices:regex`:action1, action2, ...
```

Where:
- `indices`: Comma-separated 1-based request indices where the regex will be checked
- `regex`: Golang regular expression to match against response
- `actions`: One or more actions to execute on match

### Available Actions

| Action | Description |
|--------|-------------|
| `pt("msg")` | Print message to the matching thread's tab and pause that thread |
| `pa("msg")` | Print message to ALL thread tabs and pause ALL threads |
| `sre("path")` | Save the request that generated the match to file and pause thread |
| `srp("path")` | Save the response that generated the match to file and pause thread |
| `sa("path")` | Save both request and response to file and pause thread |
| `e` | Gracefully exit the application, it can be combined with other actions, use it at the end :). |

> **Note:** All save actions (`sre`, `srp`, `sa`) automatically append `_idx_epoch` to filenames to prevent overwrites. For example, `/tmp/req.txt` becomes `/tmp/req_2_1704312000.txt`. 
>
> Paused threads generates a message on its tab, requiring pressing **Enter** to resume paused threads.

### Action File Examples

**actions.txt** - Basic match detection:
```
2:"success"\s*:\s*true`:pt("Exploitation successful!")
```

**actions.txt** - Save evidence and exit on match:
```
3:"balance"\s*:\s*[1-9]\d{6}`:sa("/tmp/evidence.txt"), e
```

**actions.txt** - Multiple conditions:
```
2:"token"\s*:\s*"[^"]+`:pt("Got token")
3:"error"\s*:\s*false`:sre("/tmp/winning_request.txt"), e
```

**actions.txt** - Alert all threads on critical finding:
```
2,3,4:"admin"\s*:\s*true`:pa("ADMIN ACCESS ACHIEVED!"), sa("/tmp/admin_poc.txt"), e
```

### Usage Examples

**Detect successful race condition and save evidence:**
```bash
./trem -l "login.txt,race.txt" -re patterns.txt -th 20 -mode sync -ra actions.txt
```

With `actions.txt`:
```
2:"quantity"\s*:\s*-`:sa("/tmp/oversold.txt"), e
```

This will: match responses where quantity went negative (overselling), save both request and response as evidence, then exit.

**Password spray with success detection:**
```bash
./trem -l "login.txt,dashboard.txt" -re patterns.txt -u /tmp/creds -th 50 -ra actions.txt
```

With `actions.txt`:
```
2:"authenticated"\s*:\s*true`:sre("/tmp/valid_creds.txt"), pt("Valid credentials found!"), e
```

**Multi-stage exploitation monitoring:**
```bash
./trem -l "auth.txt,setup.txt,exploit.txt,verify.txt" -re patterns.txt -mode sync -sb 1,3 -ra actions.txt
```

With `actions.txt`:
```
3:"race_won"`:pt("Race condition triggered!")
4:"balance"\s*>\s*1000000`:sa("/tmp/jackpot.txt"), pa("JACKPOT!"), e
```

### Combining with Other Features

Response actions work seamlessly with other TREM features:

**With [looping](#looping):**
```bash
./trem -l "login.txt,race.txt" -re patterns.txt -x 2 -xt 0 -ra actions.txt
```
Actions are checked on every loop iteration until a match triggers exit.

**With [FIFO injection](#fifo-mode-dynamic-injection):**
```bash
./trem -l "login.txt,action.txt" -re patterns.txt -u /tmp/spray -ra actions.txt
```
Each injected value is tested, and actions trigger on first successful match.

**With [HTTP/2](#http2-support):**
```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -http2 -ra actions.txt
```
Saved responses are automatically converted to HTTP/1.1 format for readability.

## Request Chaining

### Pattern File Format

Each line extracts values from response N to use in request N+1, using Golang valid Regular Expressions, example:
```
proof%3D([a-zA-Z0-9%._\-]+?)%26`:proof $ location%3Dhttps%3A%2F%2F([A-Za-z0-9.\-]+)%2F`:host $ location%3Dhttps%3A%2F%2F[A-Za-z0-9.\-]+(%2F.+)"`:url
"access_token":"([^"]+)"`:bearer
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

For dynamic value injection (password spraying, token rotation etc.), use a named pipe. It works on any Unix that Golang supports, e.g. Linux, macOS, BSD, Android, etc. Windows named pipes aren't implemented yet :(. Each entry on FIFO generates an individual request, example:

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

The first line defines the key (`$pass$`), subsequent lines are values for that key. Each value of `$pass$` generates a new request when matching and replacing the key in a given request.

### FIFO Distribution Modes (`-fmode`)

| Mode | Behavior | Use Case |
|------|----------|----------|
| 1 | Broadcast: all threads receive all values | Testing same token across threads |
| 2 | Round-robin: each value goes to one thread | Password spraying (default) |

**Round-robin example with 3 threads:**
```
FIFO input: pass1, pass2, pass3, pass4, pass5, pass6
Thread 0: pass1, pass4
Thread 1: pass2, pass5
Thread 2: pass3, pass6
```

### Round-robin Persistent Keys (`$_key$`)

Keys starting with underscore are never consumed and persist across all loops:

```
$_bearer$=eyJhbGciOiJIUzI1NiIs...
$pin$=1234
5678
9012
```

- `$_bearer$` is set once and used in every request
- `$pin$` values are consumed normally (one per request)

This enables scenarios like: authenticate once, then spray PINs. The `$_key$` can still be updated later:
```
$_bearer$=eyJhbGciOiJIUzI1NiIs...
$pin$=1234
5678
9012
....
$_bearer$=eyJhbGciOiJSUzI1NiIsIn...
```

### Key combinations 

When multiple keys have multiple values in the same batch (64 lines), TREM generates all combinations:

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
- `-xt 0`: Infinite loops (stop with Q or via [response action](#response-actions--ra) exit)

**Flow:**
1. Execute requests 1 → 3
2. Loop 100×: requests 2 → 3

**Combined with response actions:**
```bash
./trem -l "login.txt,race.txt" -re patterns.txt -x 2 -xt 0 -ra actions.txt
```
Loops infinitely until a response action triggers exit.

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

## Thread Tabs Output Dump

Save all thread Tabs output (TUI) to files for post-analysis:

```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -th 2 -dump
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

Real-time metrics in the UI, it uses a windows of events (`-sw`) to control how many events are collected to display metrics, e.g., `-sw 25` will use 25 events (i.e., request sent) to display, default is 10, normal mode, although it can be increased to better granularity if needed. When using [verbose mode](#verbose-mode) (`-v`) windows size is set to 50 if `-sw` is lower than 50.

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
| Enter | Resume paused threads (see [Response Actions](#response-actions--ra)) |
| Q | Quit (stops all threads gracefully) |

## Examples

**Basic race condition (sync mode):**
```bash
./trem -l "auth.txt,race.txt" -re patterns.txt -th 20 -mode sync
```

**Multi-barrier sync race:**
```bash
./trem -l "login.txt,setup.txt,race.txt" -re patterns.txt -th 20 -mode sync -sb 1,3
```

**Password spray with FIFO and success detection:**
```bash
./trem -l "login.txt,dashboard.txt" -re patterns.txt -u /tmp/creds -th 50 -fmode 2 -ra actions.txt
```

**Sustained race with keep-alive and auto-exit on success:**
```bash
./trem -l "login.txt,setup.txt,race.txt" -re patterns.txt -th 10 -mode sync -k -x 3 -xt 0 -ra actions.txt
```

**HTTP/2 race condition with evidence collection:**
```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -http2 -th 10 -mode sync -ra actions.txt
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

**Infinite spray with persistent auth token and auto-exit:**
```bash
# passwords.txt:
# $_token$=eyJhbG...
# $pass$=123456
# password
# admin123

# actions.txt:
# 1`"success"`:sre("/tmp/winner.txt"), e

./trem -l "verify.txt" -re patterns.txt -u /tmp/spray -th 100 -xt 0 -fmode 2 -ra actions.txt
```

**Complete exploitation workflow:**
```bash
# 1. Sync login and critical action
# 2. Loop the exploit infinitely  
# 3. Auto-save evidence and exit on success

./trem -l "login.txt,setup.txt,exploit.txt" -re patterns.txt \
  -th 50 -mode sync -sb 1,3 -x 2 -xt 0 \
  -ra actions.txt
```

With `actions.txt`:
```
3`"exploited"\s*:\s*true`:sa("/tmp/poc.txt"), pa("SUCCESS!"), e
```
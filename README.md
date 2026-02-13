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

**Single-Packet Attack ([Block Mode](#block-mode))**

Sends all requests in a single TCP write operation, exploiting the lowest possible timing window. Uses HTTP/1.1 pipelining or HTTP/2 multiplexing depending on protocol:
- Maximizes request simultaneity at the wire level
- Bypasses network-level timing variations
- Ideal for critical race windows under 1ms
- With [`-ka`](#keep-all-mode--ka): all threads share single TLS connection, enabling true single-packet attacks across entire thread groups

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
| Key Subscription | Per-key round-robin among subscribers | O(S) where S = subscribers |
| Keep-All Flush | Concatenate + single write | O(T×R) where T=threads, R=requests |

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
# Notes:
* Linux ARM64 build runs on Android too :)
* Windows doesn't support FIFO mode yet :(


## Usage

**Single Mode (one group):**
```bash
./trem -l "req1.txt,req2.txt,...,reqN.txt" -re patterns.txt [options]
```

**Groups Mode (multiple independent groups):**
```bash
./trem -l "req1.txt,req2.txt,...,reqN.txt" -thrG groups.txt [options]
```

## Demo
![Demo](NullByte-2025/vid.gif)

## Table of Contents

- [Flags](#flags)
- [ClientHello Fingerprints](#clienthello-fingerprints--cli)
- [Operation Modes](#operation-modes)
  - [Async Mode](#async-mode-default)
  - [Sync Mode](#sync-mode)
  - [Block Mode](#block-mode)
  - [Keep-All Mode](#keep-all-mode--ka)
  - [Sync Barriers](#sync-barriers--sb)
- [Thread Groups](#thread-groups--thrg)
  - [Groups File Format](#groups-file-format)
  - [Cross-Group Value Sharing](#cross-group-value-sharing)
  - [NoFifo Groups](#nofifo-groups)
- [Cross-Group Synchronization](#cross-group-synchronization)
  - [Wait Keys](#wait-keys-wk)
- [Regex Patterns File](#regex-patterns-file--re)
- [Response Actions](#response-actions--ra)
- [Universal Replacement](#universal-replacement--u)
  - [Static Mode](#static-mode)
  - [FIFO Mode](#fifo-mode-dynamic-injection)
  - [FIFO Distribution Modes](#fifo-distribution-modes--fmode)
  - [FIFO Key Distribution](#fifo-key-distribution--fkdist)
- [HTTP/2 Support](#http2-support)
- [Looping](#looping)
- [mTLS Support](#mtls-support)
- [Proxy Support](#proxy-support)
- [Verbose Mode](#verbose-mode)
- [Stats Panel](#stats-panel)
- [UI Controls](#ui-controls)
- [Examples](#examples)

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-l` | *required* | Comma-separated list of raw HTTP/1.1 request files |
| `-re` | | Regex patterns file for value extraction (see [Regex Patterns](#regex-patterns-file--re)) |
| `-h` | | Host:port override (default: extracted from Host header) |
| `-thr` | 1 | Thread count (single mode only, ignored with `-thrG`) |
| `-d` | 0 | Delay in milliseconds between requests |
| `-o` | false | Save last response per thread as `out_<time>_g<G>_t<T>.txt` |
| `-u` | | Universal replacement: `key=val` or FIFO path (see [Universal Replacement](#universal-replacement--u)) |
| `-px` | | HTTP proxy URL (e.g., `http://127.0.0.1:8080`) |
| `-mode` | async | Execution mode: `sync`, `async`, or `block` (see [Operation Modes](#operation-modes)) |
| `-k` | true | Keep-alive: persist TLS connections across requests |
| `-ka` | false | Keep-all: share single TLS connection across all threads (requires `mode=block`, see [Keep-All Mode](#keep-all-mode--ka)) |
| `-x` | -1 | Loop start index (1-based, -1 = no loop, see [Looping](#looping)) |
| `-xt` | 1 | Loop count (0 = infinite) |
| `-cli` | 0 | TLS ClientHello fingerprint (see [table below](#clienthello-fingerprints--cli)) |
| `-tou` | 5000 | TLS handshake timeout in milliseconds |
| `-retry` | 3 | Max retries on connection/TLS errors |
| `-v` | false | Verbose debug output (see [Verbose Mode](#verbose-mode)) |
| `-sw` | 10 | Stats window size (auto-set to 50 if verbose and < 50) |
| `-h2` | false | Use HTTP/2 (default: HTTP/1.1, see [HTTP/2 Support](#http2-support)) |
| `-fw` | true | FIFO wait: block until first value written to named pipe |
| `-fmode` | 2 | FIFO distribution: 1=Broadcast (all threads), 2=Round-robin |
| `-fbck` | 64 | FIFO block consumption: max values to drain per request (0=unlimited) |
| `-fkdist` | true | FIFO key distribution: threads only receive keys from their templates (group mode only, see [FIFO Key Distribution](#fifo-key-distribution--fkdist)) |
| `-mtls` | | Client certificate for mTLS: `/path/cert.p12:password` |
| `-dump` | false | Dump thread tab output to files (`g<G>_t<T>_<H-M>.txt`) |
| `-sb` | 1 | Sync barrier indices (1-based, comma-separated, see [Sync Barriers](#sync-barriers--sb)) |
| `-ra` | | Response action file (see [Response Actions](#response-actions--ra)) |
| `-dsize` | 4096 | Block mode: max TCP data size in bytes before flush |
| `-thrG` | | Thread groups file (see [Thread Groups](#thread-groups--thrg)) |

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

## Operation Modes

### Async Mode (default)

Each thread processes requests independently without synchronization. Best for high-throughput race condition exploitation.

```bash
./trem -l "login.txt,action.txt" -re patterns.txt -thr 5 -mode async
```

### Sync Mode

Threads synchronize at [barrier points](#sync-barriers--sb) before sending requests simultaneously. Essential for classical race condition exploitation.

```bash
./trem -l "login.txt,race.txt" -re patterns.txt -thr 10 -mode sync
```

**Default Flow (barrier on first request):**
1. All threads establish TLS connections
2. All threads wait at barrier (request 1)
3. Barrier releases → simultaneous request transmission
4. Remaining requests proceed normally

### Block Mode

Accumulates all requests and sends them in a **single TCP write**. This achieves the tightest possible timing window by eliminating network round-trip delays between requests.

```bash
./trem -l "r1.txt,r2.txt,r3.txt" -thr 10 -mode block -h2
```

**Protocol behavior:**
- **HTTP/1.1**: Uses pipelining (multiple requests in single write, responses read in order)
- **HTTP/2**: Uses multiplexing (multiple streams in single connection, concurrent responses)

**Key characteristics:**
- Ignores `-d` delay (all requests sent at once)
- Ignores `-re` patterns (no inter-request value extraction)
- Maximum buffer size controlled by `-dsize` (default 4KB)
- Best for exploiting sub-millisecond race windows
- With `-ka` flag: all threads share single TLS connection (see [Keep-All Mode](#keep-all-mode--ka))

**Use Cases:**
- Single-packet attacks where timing is critical
- Bypassing rate limiters that count requests per TCP segment
- Testing race conditions in stateless operations

### Keep-All Mode (`-ka`)

Extends block mode by sharing a **single TLS connection across all threads** in a group. Instead of each thread having its own connection, all threads accumulate requests into their local buffers, then a coordinator performs a **global flush** - concatenating all buffers and sending everything in one TCP write.

```bash
./trem -l "race.txt" -thr 50 -xt 100 -mode block -ka
```

**Requirements:**
- Requires `mode=block`
- All requests in the group must target the same `host:port`

**Flow comparison:**
```
Without -ka (normal block):
  Thread 1: buffer → flush → conn1    (50 connections)
  Thread 2: buffer → flush → conn2
  ...
  Thread 50: buffer → flush → conn50

With -ka:
  Thread 1: buffer ─┐
  Thread 2: buffer ─┼→ global flush → single conn (1 connection)
  ...               │
  Thread 50: buffer─┘
```

**Behavior:**
1. Single TLS handshake for entire group (connection stored in first thread)
2. Each thread accumulates requests in its local buffer
3. Coordinator waits for all threads to finish accumulation
4. Global flush: concatenate all buffers → single TCP write
5. Read responses in order, dispatch to respective threads
6. Repeat for each loop iteration (`-xt`)

**Protocol details:**
- **HTTP/1.1**: All requests concatenated, pipelined in single write
- **HTTP/2**: All requests encoded as frames with unique stream IDs, multiplexed in single write

**Use Cases:**
- True single-packet attacks: 50 threads × 100 iterations = 5000 requests through minimal TCP writes
- Maximum timing precision across thread groups
- Bypassing per-connection rate limits

**Example:** 100 threads, 50 iterations, all through single connection:
```bash
./trem -l "race.txt" -thr 100 -xt 50 -mode block -ka -h2
```
Sends 100 × 50 = 5000 requests through a single TLS connection, with each iteration's requests going out in one TCP write (if total size < `-dsize`).

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
./trem -l "login.txt,setup.txt,transfer.txt" -re patterns.txt -thr 20 -mode sync -sb 1,3
```

**Flow:**
1. All threads sync → send `login.txt` simultaneously
2. Each thread processes `setup.txt` independently (no barrier)
3. All threads sync → send `transfer.txt` simultaneously

This is useful when the race condition requires both authenticated sessions established at the same time AND the vulnerable action triggered simultaneously.

## Thread Groups (`-thrG`)

Thread groups allow you to define **independent execution units** where different subsets of requests run with their own thread count, mode, timing, and patterns. This enables complex attack scenarios like:

- Parallel authentication + exploitation flows
- Multi-phase attacks with different timing requirements
- Coordinated multi-endpoint race conditions

When `-thrG` is used, the following flags are **ignored** (they're defined per group): `-thr`, `-mode`, `-x`, `-xt`, `-sb`, `-re`, `-ka`

### Groups File Format

Each line defines one group:
```
req_indices thr=N mode=sync|async|block s_delay=N [r_delay=N] [x=N] [xt=N] [sb=N,M] [re=file] [nofifo] [ka] [wk=_key1,_key2]
```

| Parameter | Required | Description |
|-----------|----------|-------------|
| `req_indices` | Yes | Comma-separated 1-based request indices (e.g., `1,3,5`) - can overlap between groups |
| `thr=N` | Yes | Thread count for this group |
| `mode=` | Yes | `sync`, `async`, or `block` |
| `s_delay=N` | Yes | Start delay in ms (delay before starting group threads) |
| `r_delay=N` | No | Request delay in ms between requests (defaults to `-d` flag) |
| `x=N` | No | Loop start index (1-based, relative to group) |
| `xt=N` | No | Loop count (0=infinite) |
| `sb=N,M` | No | Sync barriers (1-based, relative to group) |
| `re=file` | No | Regex patterns file for this group |
| `nofifo` | No | Boolean flag - group doesn't receive FIFO values (see [NoFifo Groups](#nofifo-groups)) |
| `ka` | No | Boolean flag - share single TLS connection across all threads (requires `mode=block`, see [Keep-All Mode](#keep-all-mode--ka)) |
| `wk=_k1,_k2` | No | Wait keys - block until these static keys exist (see [Cross-Group Synchronization](#cross-group-synchronization)) |

> **Note**: `mode=block` ignores `r_delay` and `re=` parameters.

> **Note**: Request indices can be **duplicated across groups**, allowing different groups to process the same request with different configurations.

### Groups File Example

```
# groups.txt
# Group 1: Authentication (requests 1,2) - async with 5 threads
1,2 thr=5 mode=async s_delay=0 r_delay=10 re=auth_patterns.txt

# Group 2: Race exploit (requests 3,4,5) - sync with 20 threads, starts 100ms later
3,4,5 thr=20 mode=sync s_delay=100 sb=1 x=1 xt=0 re=race_patterns.txt
```

```bash
./trem -l "login.txt,setup.txt,race1.txt,race2.txt,race3.txt" -thrG groups.txt
```

**Flow:**
1. Group 1 starts immediately (s_delay=0): 5 threads process login.txt → setup.txt
2. Group 2 starts after 100ms: 20 threads sync on race1.txt, then process race2.txt, race3.txt
3. Group 2 loops infinitely (xt=0) from request 1 (x=1) within its group

### Cross-Group Value Sharing

Groups can share values using **static keys** (keys prefixed with `_`):

1. Group 1 extracts `$_session$` from login response (via its `re=` file)
2. Value is stored in `globalStaticVals`
3. Group 2 automatically receives `$_session$` in its requests

This enables patterns like: authenticate once in Group 1, spray exploits in Group 2.

### NoFifo Groups

Groups with `nofifo` flag don't participate in FIFO value distribution. Their threads:
- Don't receive FIFO channel (no buffered values)
- Still have access to static keys (`_key`) via `globalStaticVals`
- Useful for groups that only use regex-extracted values or static keys

**Example:**
```
# groups.txt
# Group 1: Auth - doesn't need FIFO values, only extracts session
1,2 thr=5 mode=async s_delay=0 re=auth.txt nofifo

# Group 2: Spray - uses FIFO for passwords
3 thr=50 mode=async s_delay=100 x=1 xt=0
```

Group 1 extracts `$_session$` which Group 2 uses, but Group 1 threads don't waste channel capacity receiving FIFO values they don't need.

## Cross-Group Synchronization

The `wk=` (wait keys) parameter enables fine-grained synchronization between thread groups by blocking a group until specific static keys become available in `globalStaticVals`.

### Wait Keys (`wk=`)

When a group has `wk=_key1,_key2,...`, all threads in that group will **wait indefinitely** for those keys to appear before processing requests that need them. This creates a dependency chain between groups.

**Format:** Comma-separated list of static keys (must start with `_`)

```
# groups.txt
# Group 1: Auth flow - extracts _vtoken from response
1,2 thr=1 mode=sync s_delay=0 re=auth.txt

# Group 2: Exploit - waits for _vtoken, then races
3 thr=50 mode=block s_delay=100 xt=100 ka wk=_vtoken
```

**Flow:**
```
Group 1: r1 → r2 → extracts _vtoken → globalStaticVals.Store("_vtoken", "eyJ...")
                        ↓
Group 2: [blocked waiting _vtoken]
                        ↓
Group 2: _vtoken available → r3 × 50 threads × 100 iterations
```

**Key characteristics:**
- Only static keys (`_key`) can be waited on
- Wait is infinite (no timeout) for keys in `wk=` list
- Multiple keys can be specified: `wk=_token,_session,_csrf`
- Group starts processing only after ALL wait keys exist

### Use Cases

**1. Dependent authentication flows:**
```
# r1: initial login (uses initial _token from -u)
# r2: refresh token (extracts new _vtoken)
# r3: exploit action (needs _vtoken from r2)

1,2 thr=1 mode=sync s_delay=0 re=rx.txt
3 thr=100 mode=block s_delay=0 xt=50 ka wk=_vtoken
```

**2. Multi-phase coordinated attacks:**
```
# Phase 1: Setup - extracts _session and _csrf
1,2 thr=5 mode=async s_delay=0 re=setup.txt

# Phase 2: Wait for setup, then race (waits for _session AND _csrf)
3,4,5 thr=50 mode=sync s_delay=0 sb=1 wk=_session,_csrf
```

**3. Shared request with different tokens:**
```
# Same request (r1) used in both groups, but with different tokens

# Group 1: Uses initial token, extracts new token
1,2 thr=1 mode=async s_delay=0 re=auth.txt

# Group 2: Same r1 but waits for extracted token
1 thr=50 mode=block s_delay=100 xt=100 ka wk=_newtoken
```

### Combining `wk=` with `ka`

The most powerful combination for race condition attacks:

```
# groups.txt
1,2 thr=1 mode=sync s_delay=0 re=rx.txt
3 thr=100 mode=block s_delay=500 xt=50 ka wk=_vtoken
```

```bash
./trem -l "login.txt,refresh.txt,exploit.txt" -thrG groups.txt -u "_token=eyJ..."
```

**Result:**
1. Group 1 authenticates and extracts `_vtoken`
2. Group 2 waits for `_vtoken`
3. Group 2 sends 100 × 50 = 5000 exploit requests through single TLS connection
4. Each iteration's 100 requests sent in one TCP write (true single-packet attack)

## Regex Patterns File (`-re`)

The patterns file defines how values are extracted from responses and injected into subsequent requests. Each line corresponds to a request transition (line N extracts from response N to populate request N+1).

### Line Format

```
regex`:<key> $ regex2`:<key2> $ ... regexN`:<keyN>
```

- **`regex`**: Golang regular expression with capture group
- **`<key>`**: Placeholder name (will replace `$<key>$` in next request)
- **` $ `**: Separator for multiple patterns on same line (space-dollar-space)

### Special Line Types

| Line Content | Behavior |
|--------------|----------|
| Valid pattern | Extract values, read response |
| Empty line | Read response, skip regex matching |
| `:` (colon only) | **Fire & Forget**: send request, don't wait for response |
| `# comment` | Ignored (comment line) |

### Example

Given requests: `login.txt`, `setup.txt`, `verify.txt`, `action.txt`

```
# patterns.txt
Set-Cookie:\s*session=([^;]+)`_session $ name="csrf" value="([^"]+)"`_csrf
:

token="([^"]+)"`token
```

**Behavior:**
1. Line 1: Extract `_session` and `_csrf` from login response → used in setup.txt
2. Line 2: Fire & Forget - setup.txt sent, response not read
3. Line 3: Empty - verify.txt response read but no extraction
4. Line 4: Extract `token` from verify response → used in action.txt (but line 3 is empty, so this extracts from verify.txt response for action.txt)

> **Note**: Static keys (prefixed with `_`) persist globally and can be shared across thread groups.

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
| `pt("msg")` | Print message to the matching thread's tab (no pause) |
| `ppt("msg")` | Print message to the matching thread's tab and pause that thread |
| `pa("msg")` | Print message to ALL thread tabs and pause ALL threads |
| `sre("path")` | Save the request that generated the match to file and pause thread |
| `srp("path")` | Save the response that generated the match to file and pause thread |
| `sa("path")` | Save both request and response to file and pause thread |
| `e` | Graceful exit (always use as last action if combined) |

### Example

```
# actions.txt
2,3:"success":true`:sa("/tmp/poc.txt"), pa("EXPLOITED!"), e
```

When response 2 or 3 contains `"success":true`:
1. Save request+response to `/tmp/poc.txt`
2. Print "EXPLOITED!" to all threads
3. Exit gracefully

## Universal Replacement (`-u`)

Provides dynamic value injection into requests via static assignment or FIFO pipe.

### Static Mode

```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -u "token=abc123"
```

Replaces every `$token$` in all requests with `abc123`.

> **Note**: Values extracted by regex patterns (via `-re`) take priority over `-u` static values. This allows `-u` to provide initial values that can be overridden by extracted values in subsequent requests or loops.

### FIFO Mode (Dynamic Injection)

```bash
# Terminal 1: Start TREM
./trem -l "login.txt" -re patterns.txt -u /tmp/fifo -thr 50 -fmode 2

# Terminal 2: Feed credentials from a file
cat passwords.txt > /tmp/fifo
# Or from another program
python generate_spraypass.py -in passwords_masks.txt -out /tmp/fifo
```

**FIFO file format:**
```
pass=password123
admin123
letmein
qwerty
123456
```

The first line defines the key (`pass`), subsequent lines are values for that key. Each value of `pass` generates a new request when matching and replacing the key in a given request. Although FIFO keys don't use `$key$` format in feed/consumption, the find&replace algorithm always uses key in format `$key$` in requests! So the FIFO key `pass` will replace request key `$pass$`. FIFO also accept static keys and mutators, as:

```
_user=admin
pass=password123
admin123
letmein
qwerty
123456
```
The key `_user` is globally available (see [Cross-Group Value Sharing](#cross-group-value-sharing)) to all groups threads (see [Thread Groups](#thread-groups--thrg)), while the values of `pass`are shared following the FIFO Distribution Modes. 

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

### Persistent Keys (`_key`)

Keys starting with underscore (globally static values) are also never consumed in the FIFO, persisting across all loops, with the updated value if needed:

```
_bearer=eyJhbGciOiJIUzI1NiIs...
pin=1234
5678
9012
```

- `_bearer` is set once and used in every request
- `pin` values are consumed normally (one per request)

This enables scenarios like: authenticate once, then spray PINs. The `_key` can still be updated later:
```
_bearer=eyJhbGciOiJIUzI1NiIs...
pin=1234
5678
9012
....
_bearer=eyJhbGciOiJSUzI1NiIsIn...
```

### Key Combinations

When multiple keys have multiple values in the same batch (64 lines), TREM generates all combinations:

```
user=admin
guest
pass=123
456
```

Generates 4 requests:
- admin, pass=123
- admin, pass=456
- guest, pass=123
- guest, pass=456


### FIFO Wait (`-fw`)

| Setting | Behavior |
|---------|----------|
| `-fw=true` (default) | Block threads until first FIFO value arrives |
| `-fw=false` | Start immediately; missing keys left unsubstituted |

### FIFO Key Distribution (`-fkdist`)

By default in group mode, TREM analyzes each group's request templates and only sends FIFO keys to threads that actually need them. This **subscription-based distribution** prevents threads from receiving keys they never use, optimizing channel capacity and reducing overhead.

| Setting | Behavior |
|---------|----------|
| `-fkdist=true` (default) | Threads only receive keys present in their group's request templates |
| `-fkdist=false` | All threads receive all keys (legacy behavior) |

> **Note**: In single mode (without `-thrG`), key distribution is always disabled (all threads process all requests, so they need all keys).

**How it works:**

1. Before starting, TREM parses all request templates for each group
2. Extracts non-static keys (`$key$`, not `$_key$`) from each group's requests
3. Builds a subscription map: which threads need which keys
4. FIFO dispatcher only sends keys to subscribed threads

**Example scenario:**

```
# groups.txt
# Group 1: Auth - uses $_session$ (static, always available)
1,2 thr=5 mode=async s_delay=0 re=auth.txt

# Group 2: Spray - uses $pass$ from FIFO  
3 thr=50 mode=async s_delay=100 x=1 xt=0
```

With `-fkdist=true`:
- Group 1 threads (0-4): Don't receive `pass` values (not in their templates)
- Group 2 threads (5-54): Receive `pass` values via round-robin

With `-fkdist=false`:
- All 55 threads receive `pass` values, but Group 1 threads waste them

**Benefits:**
- Reduced channel congestion for threads that don't need certain keys
- More efficient round-robin distribution (values go only to threads that use them)
- Lower memory usage (smaller per-thread buffers)

**Complexity:**

| Operation | Without Subscription | With Subscription |
|-----------|---------------------|-------------------|
| Dispatch key | O(T) all threads | O(S) subscribers only |
| Round-robin | Global index | Per-key index among subscribers |

Where T = total threads, S = subscribers for that key (typically S << T).

## HTTP/2 Support

TREM supports HTTP/2 natively with a handcrafted protocol implementation. Request files remain in HTTP/1.1 raw format - TREM automatically converts them to HTTP/2 frames.

```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -h2 -thr 10 -mode sync
```

**Conversion process:**
- `Host` header → `:authority` pseudo-header
- Connection-specific headers stripped (`Connection`, `Keep-Alive`, etc.)
- Headers HPACK encoded
- Body sent as DATA frames
- TLS ALPN set to `h2` (with `http/1.1` fallback)

**Block mode with HTTP/2:**
- Uses **multiplexing** instead of pipelining
- All requests sent as separate streams in single TCP write
- Responses can arrive out of order (handled by stream ID correlation)

**Intended Limitations:**
- Flow control assumes server window ≥ 10MB
- Server push (PUSH_PROMISE) frames discarded

**When to use HTTP/2:**
- Target server requires HTTP/2
- Testing HTTP/2-specific race conditions
- Leveraging multiplexed streams on single connection
- Block mode attacks (better server compatibility than HTTP/1.1 pipelining)
- Keep-All mode (`-ka`) for maximum single-connection throughput

## Looping

Execute a subset of requests repeatedly:

```bash
./trem -l "login.txt,setup.txt,race.txt" -re patterns.txt -x 2 -xt 100
```

- `-x 2`: Loop starts from request 2 (setup.txt)
- `-xt 100`: Run 100 iterations
- `-xt 0`: Infinite loops (stop with Q or via [response action](#response-actions--ra) exit)
- `-x -1`: No looping (default)

**Flow:**
1. Execute requests 1 → 3
2. Loop 100×: requests 2 → 3

**Combined with response actions:**
```bash
./trem -l "login.txt,race.txt" -re patterns.txt -x 2 -xt 0 -ra actions.txt
```
Loops infinitely until a response action triggers exit.

## mTLS Support

For targets requiring client certificates (Mutual TLS):

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

Save all thread tabs output (TUI) to files for post-analysis:

```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -thr 2 -dump
```

Creates files: `g1_t1_15-30.txt`, `g1_t2_15-30.txt`, etc.

Format: `g<GroupID>_t<ThreadID>_<Hour-Minute>.txt`

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
- HTTP/2 frame debugging

## Stats Panel

Real-time metrics in the UI, it uses a windows of events (`-sw`) to control how many events are collected to display metrics, e.g., `-sw 25` will use 25 events (i.e., request sent) to display, default is 10, normal mode, although it can be increased to better granularity if needed. When using [verbose mode](#verbose-mode) (`-v`) windows size is set to 50 if `-sw` is lower than 50.

**Normal mode:**
- Req/s: requests per second
- TotalReq: total requests sent
- HTTP Errs: errors by group/thread and status (e.g., `G1T1(401)x5`)
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
| Ctrl+P | Next group (jumps to first thread of next group) |
| Enter | Resume paused threads (see [Response Actions](#response-actions--ra)) |
| Q | Quit (stops all threads gracefully) |

The UI displays a **TreeView** on the left showing all groups and their threads. Click or navigate to select a thread and view its output.

## Examples

**Basic race condition (sync mode):**
```bash
./trem -l "auth.txt,race.txt" -re patterns.txt -thr 20 -mode sync
```

**Single-packet attack (block mode with HTTP/2):**
```bash
./trem -l "r1.txt,r2.txt,r3.txt" -thr 10 -mode block -h2
```

**True single-packet attack (keep-all with HTTP/2):**
```bash
./trem -l "race.txt" -thr 100 -xt 50 -mode block -ka -h2
```
Sends 100 × 50 = 5000 requests through single TLS connection, each iteration in one TCP write.

**Multi-barrier sync race:**
```bash
./trem -l "login.txt,setup.txt,race.txt" -re patterns.txt -thr 20 -mode sync -sb 1,3
```

**Password spray with FIFO and success detection:**
```bash
./trem -l "login.txt,dashboard.txt" -re patterns.txt -u /tmp/creds -thr 50 -fmode 2 -ra actions.txt
```

**Sustained race with keep-alive and auto-exit on success:**
```bash
./trem -l "login.txt,setup.txt,race.txt" -re patterns.txt -thr 10 -mode sync -k -x 3 -xt 0 -ra actions.txt
```

**HTTP/2 race condition with evidence collection:**
```bash
./trem -l "req1.txt,req2.txt" -re patterns.txt -h2 -thr 10 -mode sync -ra actions.txt
```

**Fire & Forget with selective response reading:**
```bash
# patterns.txt:
# session=([^;]+)`_session
# :
# (empty line - reads response, no extraction)

./trem -l "login.txt,trigger.txt,verify.txt" -re patterns.txt -thr 5
# login: extract session
# trigger: fire & forget (response not read)
# verify: read response but no extraction
```

**Multi-group coordinated attack:**
```bash
# groups.txt:
# 1,2 thr=5 mode=async s_delay=0 re=auth.txt
# 3,4,5 thr=50 mode=sync s_delay=50 sb=1 x=1 xt=0

./trem -l "login.txt,setup.txt,race1.txt,race2.txt,race3.txt" -thrG groups.txt -ra actions.txt
```

**Multi-group with nofifo optimization:**
```bash
# groups.txt:
# 1,2 thr=5 mode=async s_delay=0 re=auth.txt nofifo
# 3 thr=100 mode=async s_delay=100 x=1 xt=0

./trem -l "login.txt,setup.txt,spray.txt" -thrG groups.txt -u /tmp/fifo -fmode 2
# Group 1: Auth only, doesn't need FIFO values
# Group 2: Spray with all 100 threads receiving passwords via round-robin
```

**Cross-group synchronization with wait keys:**
```bash
# groups.txt:
# 1,2 thr=1 mode=sync s_delay=0 re=auth.txt
# 3 thr=100 mode=block s_delay=100 xt=50 ka wk=_vtoken

./trem -l "login.txt,refresh.txt,exploit.txt" -thrG groups.txt -u "_token=eyJ..."
# Group 1: Auth flow extracts _vtoken
# Group 2: Waits for _vtoken, then races with 100×50=5000 requests via single connection
```

**Shared request across groups with different configs:**
```bash
# groups.txt:
# 1,2 thr=1 mode=async s_delay=0 re=auth.txt
# 1 thr=50 mode=block s_delay=500 xt=100 ka wk=_session

./trem -l "action.txt,setup.txt" -thrG groups.txt
# Group 1: Uses r1 (action.txt) with initial auth, extracts _session
# Group 2: Same r1 but with extracted _session, races 50×100 times
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
# passwords.txt (feed to FIFO):
# _token=eyJhbG...
# pass=123456
# password
# admin123

# actions.txt:
# 1:"success"`:sre("/tmp/winner.txt"), e

./trem -l "verify.txt" -re patterns.txt -u /tmp/spray -thr 100 -xt 0 -fmode 2 -ra actions.txt
```

**Complete exploitation workflow:**
```bash
# 1. Sync login and critical action
# 2. Loop the exploit infinitely  
# 3. Auto-save evidence and exit on success

./trem -l "login.txt,setup.txt,exploit.txt" -re patterns.txt \
  -thr 50 -mode sync -sb 1,3 -x 2 -xt 0 \
  -ra actions.txt
```

With `actions.txt`:
```
3:"exploited"\s*:\s*true`:sa("/tmp/poc.txt"), pa("SUCCESS!"), e
```

**Complete exploitation with keep-all and wait keys:**
```bash
# 1. Auth and extract token in Group 1
# 2. Race exploit with keep-all in Group 2
# 3. Auto-save evidence and exit on success

# groups.txt:
# 1,2 thr=1 mode=sync s_delay=0 re=auth.txt
# 3 thr=50 mode=block s_delay=100 xt=0 ka wk=_token

./trem -l "login.txt,setup.txt,exploit.txt" -thrG groups.txt -ra actions.txt
```

With `actions.txt`:
```
3:"exploited"\s*:\s*true`:sa("/tmp/poc.txt"), pa("SUCCESS!"), e
```
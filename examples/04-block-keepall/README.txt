# Example 04: Block Mode with Keep-All (-ka)

## Purpose
Demonstrates the most aggressive race attack: keep-all mode where ALL
threads in a group share a single TLS connection. All requests from
all threads are concatenated and sent in ONE TCP write operation.

## Attack Scenario
- Without -ka: 20 threads = 20 TLS connections, 20 TCP writes
- With -ka: 20 threads = 1 TLS connection, 1 TCP write per iteration
- Requests arrive at server nearly simultaneously (network level)

## Architecture
```
Normal block (20 connections):     Keep-all (1 connection):
  T1 ──conn1──→ Server              T1 ─┐
  T2 ──conn2──→ Server              T2 ─┼──conn──→ Server
  ...                               ... │
  T20 ─conn20─→ Server              T20─┘
```

## groups.txt Configuration
```
# Auth group
1 thr=1 mode=async s_delay=0 re=auth.txt

# Attack group with ka (keep-all) - covers requests 2 and 3
2,3 thr=20 mode=block s_delay=200 xt=5 ka
```

## Key Features
- `ka` - keep-all flag, shares single connection
- `mode=block` - required for ka
- `-h2` - HTTP/2 for multiplexing (recommended with ka)
- 20 × 5 = 100 coupon redemption attempts through single connection

## Expected Behavior
- Single TLS handshake for all 20 threads
- Each iteration: 20 requests in single TCP write
- Maximum race window exploitation
- Server sees 20 requests arriving "simultaneously"

## Command
./trem -l "req1.txt,req2.txt,req3.txt" -thrG groups.txt -h2

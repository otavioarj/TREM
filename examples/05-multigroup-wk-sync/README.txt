# Example 05: Multi-Group with Wait Keys (wk=)

## Purpose
Demonstrates cross-group synchronization via wait keys. Group 2 blocks
indefinitely until Group 1 extracts the required $_token$. This eliminates
timing guesses (s_delay) and provides deterministic synchronization.

## Comparison: s_delay vs wk=
```
With s_delay=200:              With wk=_token:
  "Wait 200ms, hope auth done"   "Wait until _token exists"
  May fail if auth is slow       Always works, deterministic
  May waste time if auth fast    Starts immediately when ready
```

## Architecture
```
Group 1:                    Group 2:
  login ──→ extract _token    wait for _token ──→ attack
             │                      ↑
             └──────────────────────┘
                 globalStaticVals
```

## groups.txt Configuration
```
# Group 1: Auth
1 thr=1 mode=async s_delay=0 re=auth.txt

# Group 2: Attack with wait key - covers requests 2 and 3
2,3 thr=30 mode=block s_delay=0 xt=10 ka wk=_token
```

## Key Features
- `wk=_token` - blocks until _token exists in globalStaticVals
- `s_delay=0` - no artificial delay, wk handles sync
- `ka` - keep-all for single-connection attack
- 30 threads × 10 iterations = 300 requests

## Expected Behavior
- G1 starts immediately, authenticates, extracts _token
- G2 threads wait (blocked) until _token appears
- Once _token available, G2 fires 300 requests
- All through single TLS connection (ka mode)

## Use Cases
- Complex auth flows with variable timing
- Multi-step token extraction chains
- Dependent group orchestration

## Command
./trem -l "req1.txt,req2.txt,req3.txt" -thrG groups.txt -h2

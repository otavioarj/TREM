# Example 03: Multi-Group - Separated Auth and Race

## Purpose
Demonstrates thread groups (-thrG) to separate authentication from the
attack phase. Group 1 authenticates and extracts the token, Group 2
uses that token for the synchronized race attack.

## Architecture
```
Group 1 (Auth):     Group 2 (Race):
  [T1] ──────────────→ [T1..T10]
  login                 transfer (synced)
  extract _token        uses _token
```

## groups.txt Configuration
```
# Group 1: Auth - 1 thread, async, extracts token
1 thr=1 mode=async s_delay=0 re=auth.txt

# Group 2: Race - 10 threads, sync mode, barrier on req 1 (transfer)
2,3 thr=10 mode=sync s_delay=200 sb=1 xt=5
```

## Key Features
- `-thrG groups.txt` - enables multi-group mode
- `re=auth.txt` - regex file per group
- `s_delay=200` - Group 2 waits 200ms for Group 1 to finish
- Cross-group `$_token$` sharing via globalStaticVals

## Expected Behavior
- G1 authenticates, extracts and stores $_token$
- G2 starts 200ms later, uses stored $_token$
- G2 threads synchronize on transfer request
- Race condition triggered in transfer endpoint

## Command
./trem -l "req1.txt,req2.txt,req3.txt" -thrG groups.txt

# Example 02: Sync Mode - Race Condition Attack

## Purpose
Demonstrates sync mode with barriers to exploit a race condition in the
balance check of the transfer endpoint. All threads fire simultaneously
to bypass the balance validation.

## Attack Scenario
- Admin has 1000 balance
- Transfer endpoint checks balance THEN deducts (TOCTOU vulnerability)
- Multiple threads transferring 500 simultaneously can all pass the check
- Result: More money transferred than available balance

## Flow
1. req1.txt: Login (no barrier - each thread authenticates)
2. req2.txt: Transfer 500 (BARRIER HERE - all fire simultaneously)
3. req3.txt: Get stats to verify exploit

## Key Features
- `mode=sync` - synchronization enabled
- `-sb 2` - barrier on request 2 (transfer)
- `-ra ra.txt` - response actions to detect success
- `-thr 10` - 10 threads for higher race chance

## Expected Behavior (Successful Exploit)
- Multiple threads pass balance check (1000 >= 500)
- All transfers succeed before balance updates
- Final balance goes negative (or multiple success responses)
- evidence.txt created with proof

## Command
./trem -l "req1.txt,req2.txt,req3.txt" -re regex.txt -ra ra.txt -thr 10 -mode sync -sb 2 -xt 3

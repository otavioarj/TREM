# Example 06: Password Spray Attack

## Purpose
Demonstrates a password spray attack on multiple users simultaneously
using FIFO broadcast mode. Both groups receive the same passwords at
the same time, enabling parallel credential testing.

## Flow
1. FIFO receives passwords from spray.txt
2. G1 (req1.txt): Tries password against 'admin' user
3. G2 (req2.txt): Tries password against 'alice' user
4. Response action detects successful login and saves evidence

## Key Features
- `-fmode 1` (broadcast) - same password sent to ALL groups
- `-fbck 1` - process one password at a time
- Multi-group with different request templates (different users)
- `-ra ra.txt` - detect login success, save to evidence.txt

## Expected Behavior
- Both users attacked simultaneously with same passwords
- When correct password found, response action triggers
- Evidence saved with request and response for successful login
- admin123 works for admin, alice123 works for alice

## Command
./trem -l "req1.txt,req2.txt" -thrG groups.txt -u /tmp/spray -fmode 1 -fbck 1 -ra ra.txt

## Manual Execution
# Terminal 1: Start TREM
./trem -l "req1.txt,req2.txt" -thrG groups.txt -u /tmp/spray -fmode 1 -fbck 1 -ra ra.txt

# Terminal 2: Feed passwords
cat spray.txt > /tmp/spray

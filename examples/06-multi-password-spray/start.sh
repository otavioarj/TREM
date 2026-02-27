#!/bin/bash
# Example 6: Password Spray Attack
#
# Demonstrates:
#   - FIFO broadcast mode (fmode=1): same password sent to all groups
#   - Multi-group parallel attack on different users
#   - Response actions to detect and save successful logins
#   - fbck=1: one password at a time per thread
#
# Attack flow:
#   - G1 tries each password against 'admin'
#   - G2 tries each password against 'alice'
#   - Both receive same passwords simultaneously via broadcast

cd "$(dirname "$0")"

# Create FIFO if not exists
FIFO="/tmp/spray"

echo "Password Spray Attack"
echo "G1: Attacking 'admin' user"
echo "G2: Attacking 'alice' user"
echo "Both groups receive same passwords via FIFO broadcast"
echo ""
echo "Starting TREM in background..."

echo "Feeding passwords to FIFO..."
# Here we write FIFO before running TREM, as the cat will block until TREM consumes
#  the FIFO.
cat spray.txt > "$FIFO" &

# Run TREM in background
../../trem -l "req1.txt,req2.txt" \
    -thrG groups.txt \
    -u "$FIFO" \
    -fmode 1 \
    -fbck 1 \
    -ra ra.txt -dump





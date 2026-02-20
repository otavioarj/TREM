#!/bin/bash
# Example 1: Async Mode (Basic)
# 
# Demonstrates:
#   - Default async mode (no sync barriers)
#   - Regex value extraction (_token persists globally)
#   - Simple request chain: login -> transfer -> stats
#
# Each thread runs independently without synchronization

cd "$(dirname "$0")"

../../trem -l "req1.txt,req2.txt,req3.txt" \
    -re regex.txt \
    -thr 3 \
    -mode async \
    -d 100 \
    -xt 5

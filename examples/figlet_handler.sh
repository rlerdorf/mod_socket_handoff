#!/bin/bash
# Extract prompt from HANDOFF_DATA JSON
PROMPT=$(echo "$HANDOFF_DATA" | grep -o '"prompt":"[^"]*"' | cut -d: -f2 | tr -d '"')

# Send HTTP headers
echo -en "HTTP/1.1 200 OK\r\n"
echo -en "Content-Type: text/plain\r\n"
echo -en "Connection: close\r\n"
echo -en "\r\n"

# Run figlet
figlet -w 300 "$PROMPT"
